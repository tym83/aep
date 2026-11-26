// Package vcs connects existing corporate source control installations.
package vcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var hookSecrets = struct {
	sync.Mutex
	hashes map[string][32]byte
}{hashes: map[string][32]byte{}}

type Provider interface {
	Verify(context.Context) error
	EnsureGroup(context.Context, string, bool) error
	EnsureWebhook(context.Context, string, string, string) error
}
type Client struct {
	provider, base, token string
	http                  *http.Client
}

func New(provider, origin, token string, ca []byte) (*Client, error) {
	if provider != "gitlab" && provider != "github" {
		return nil, fmt.Errorf("provider %s is not implemented", provider)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("VCS requires an HTTPS URL without credentials, query or fragment")
	}
	if token == "" {
		return nil, fmt.Errorf("VCS token is empty")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(ca) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("invalid VCS CA bundle")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	base := strings.TrimRight(origin, "/") + "/api/v4"
	if provider == "github" {
		base = strings.TrimRight(origin, "/") + "/api/v3"
		if u.Host == "github.com" {
			base = "https://api.github.com"
		}
	}
	return &Client{provider, base, token, &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Never include response bodies, URLs or transport errors in conditions: they can contain credentials.
func (c *Client) request(ctx context.Context, method, path string, body, out any) (int, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("invalid VCS request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("VCS connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("VCS %s: HTTP %d", method, resp.StatusCode)
	}
	if out != nil {
		if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("invalid VCS response")
		}
	}
	return resp.StatusCode, nil
}
func (c *Client) Verify(ctx context.Context) error {
	path := "/user"
	_, err := c.request(ctx, "GET", path, nil, nil)
	return err
}
func (c *Client) EnsureGroup(ctx context.Context, path string, create bool) error {
	if path == "" {
		return nil
	}
	if c.provider == "github" {
		_, err := c.request(ctx, "GET", "/orgs/"+url.PathEscape(path), nil, nil)
		return err
	} // GitHub organizations must be provisioned by their owner.
	parent := int64(0)
	parts := strings.Split(path, "/")
	for i, p := range parts {
		var group struct {
			ID int64 `json:"id"`
		}
		status, err := c.request(ctx, "GET", "/groups/"+url.PathEscape(strings.Join(parts[:i+1], "/")), nil, &group)
		if status == 404 && create {
			body := map[string]any{"name": p, "path": p, "visibility": "private"}
			if parent != 0 {
				body["parent_id"] = parent
			}
			_, err = c.request(ctx, "POST", "/groups", body, &group)
			if err != nil {
				_, err = c.request(ctx, "GET", "/groups/"+url.PathEscape(strings.Join(parts[:i+1], "/")), nil, &group)
			}
		}
		if err != nil {
			return err
		}
		if group.ID == 0 {
			return fmt.Errorf("invalid GitLab group ID")
		}
		parent = group.ID
	}
	return nil
}
func (c *Client) EnsureWebhook(ctx context.Context, repo, target, secret string) error {
	path := "/projects/" + url.PathEscape(repo) + "/hooks"
	body := map[string]any{"url": target, "token": secret, "push_events": true, "enable_ssl_verification": true}
	if c.provider == "github" {
		path = "/repos/" + repo + "/hooks"
		body = map[string]any{"name": "web", "active": true, "events": []string{"push"}, "config": map[string]string{"url": target, "content_type": "json", "secret": secret, "insecure_ssl": "0"}}
	}
	// Paginate to avoid duplicate hooks in repositories with many integrations.
	for page := 1; page <= 100; page++ {
		var hooks []struct {
			ID     int64    `json:"id"`
			URL    string   `json:"url"`
			Push   bool     `json:"push_events"`
			SSL    bool     `json:"enable_ssl_verification"`
			Active bool     `json:"active"`
			Events []string `json:"events"`
			Config struct {
				URL         string `json:"url"`
				ContentType string `json:"content_type"`
				InsecureSSL string `json:"insecure_ssl"`
			} `json:"config"`
		}
		_, err := c.request(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &hooks)
		if err != nil {
			return err
		}
		for _, h := range hooks {
			if h.URL == target || h.Config.URL == target {
				cacheKey := fmt.Sprintf("%s|%s|%d", c.base, repo, h.ID)
				digest := sha256.Sum256([]byte(secret))
				hookSecrets.Lock()
				known, ok := hookSecrets.hashes[cacheKey]
				hookSecrets.Unlock()
				matches := h.Push && h.SSL
				if c.provider == "github" {
					matches = h.Active && len(h.Events) == 1 && h.Events[0] == "push" && h.Config.ContentType == "json" && h.Config.InsecureSSL == "0"
				}
				if matches && ok && known == digest {
					return nil
				}
				method := "PUT"
				if c.provider == "github" {
					method = "PATCH"
				}
				_, err = c.request(ctx, method, fmt.Sprintf("%s/%d", path, h.ID), body, nil)
				if err == nil {
					hookSecrets.Lock()
					hookSecrets.hashes[cacheKey] = digest
					hookSecrets.Unlock()
				}
				return err
			}
		}
		if len(hooks) < 100 {
			_, err = c.request(ctx, "POST", path, body, nil)
			return err
		}
	}
	return fmt.Errorf("webhook pagination limit exceeded")
}
