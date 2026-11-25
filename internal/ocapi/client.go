package ocapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIError is a non-2xx response from the OpenChoreo API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openchoreo api: status %d: %s", e.StatusCode, e.Body)
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	e, ok := err.(*APIError)
	return ok && e.StatusCode == http.StatusNotFound
}

// Permanent reports whether retrying the same request can never succeed:
// client errors other than throttling. Network errors and 5xx are transient.
func Permanent(err error) bool {
	e, ok := err.(*APIError)
	if !ok {
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// Client calls the OpenChoreo REST API with a bearer token per request.
type Client struct {
	BaseURL string
	// Token returns a valid access token; typically an oauth2.TokenSource
	// backed by the per-organization client-credentials client.
	Token      func(ctx context.Context) (string, error)
	HTTPClient *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, e := json.Marshal(in)
		if e != nil {
			return e
		}
		body = bytes.NewReader(raw)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, body)
	if e != nil {
		return e
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		token, e := c.Token(ctx)
		if e != nil {
			return fmt.Errorf("openchoreo token: %w", e)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := c.http().Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return e
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func bindingPath(namespace, kind, name string) (string, error) {
	plural, e := Plural(kind)
	if e != nil {
		return "", e
	}
	p := "/api/v1/namespaces/" + namespace + "/" + plural
	if name != "" {
		p += "/" + name
	}
	return p, nil
}

// Get returns the binding, or (nil, nil) when it does not exist.
func (c *Client) Get(ctx context.Context, namespace, kind, name string) (*Binding, error) {
	p, e := bindingPath(namespace, kind, name)
	if e != nil {
		return nil, e
	}
	b := &Binding{}
	if e := c.do(ctx, http.MethodGet, p, nil, b); e != nil {
		if IsNotFound(e) {
			return nil, nil
		}
		return nil, e
	}
	return b, nil
}

// Create posts a new binding and returns the server-side object.
func (c *Client) Create(ctx context.Context, namespace, kind string, b *Binding) (*Binding, error) {
	p, e := bindingPath(namespace, kind, "")
	if e != nil {
		return nil, e
	}
	out := &Binding{}
	if e := c.do(ctx, http.MethodPost, p, b, out); e != nil {
		return nil, e
	}
	return out, nil
}

// Update replaces the whole binding spec; the API has no precondition, so
// callers must have serialized writers and verified freshness beforehand.
func (c *Client) Update(ctx context.Context, namespace, kind, name string, b *Binding) (*Binding, error) {
	p, e := bindingPath(namespace, kind, name)
	if e != nil {
		return nil, e
	}
	out := &Binding{}
	if e := c.do(ctx, http.MethodPut, p, b, out); e != nil {
		return nil, e
	}
	return out, nil
}

// Delete removes the binding; a missing binding is not an error so retries
// stay idempotent.
func (c *Client) Delete(ctx context.Context, namespace, kind, name string) error {
	p, e := bindingPath(namespace, kind, name)
	if e != nil {
		return e
	}
	if e := c.do(ctx, http.MethodDelete, p, nil, nil); e != nil && !IsNotFound(e) {
		return e
	}
	return nil
}

// DefaultHTTPClient bounds every call; applies must not hang a reconcile.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
