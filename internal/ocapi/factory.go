package ocapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Credentials are the OAuth client credentials of one organization's
// aep-change-<org> client, resolved from a Secret by the caller.
type Credentials struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
}

// Factory builds per-organization API clients. Protected environments accept
// writes only from the organization's own change client, so one client per
// organization keeps the blast radius of a leaked credential at one org.
type Factory struct {
	BaseURL    string
	HTTPClient *http.Client
	// Credentials resolves the client credentials of an organization.
	// It is called on every For; token sources are cached per credential set,
	// so Secret rotation takes effect without a restart.
	Credentials func(ctx context.Context, org string) (Credentials, error)

	mu      sync.Mutex
	sources map[string]cachedSource
}

type cachedSource struct {
	key string
	ts  oauth2.TokenSource
}

func (f *Factory) For(ctx context.Context, org string) (*Client, error) {
	creds, e := f.Credentials(ctx, org)
	if e != nil {
		return nil, fmt.Errorf("credentials for %s: %w", org, e)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.TokenURL == "" {
		return nil, fmt.Errorf("credentials for %s are incomplete", org)
	}
	sum := sha256.Sum256([]byte(creds.ClientID + "\x00" + creds.ClientSecret + "\x00" + creds.TokenURL))
	key := hex.EncodeToString(sum[:])
	f.mu.Lock()
	if f.sources == nil {
		f.sources = map[string]cachedSource{}
	}
	entry, ok := f.sources[org]
	if !ok || entry.key != key {
		cfg := &clientcredentials.Config{
			ClientID:     creds.ClientID,
			ClientSecret: creds.ClientSecret,
			TokenURL:     creds.TokenURL,
			AuthStyle:    oauth2.AuthStyleInHeader,
		}
		entry = cachedSource{key: key, ts: cfg.TokenSource(ctx)}
		f.sources[org] = entry
	}
	f.mu.Unlock()
	ts := entry.ts
	return &Client{
		BaseURL:    f.BaseURL,
		HTTPClient: f.HTTPClient,
		Token: func(ctx context.Context) (string, error) {
			t, e := ts.Token()
			if e != nil {
				return "", e
			}
			return t.AccessToken, nil
		},
	}, nil
}
