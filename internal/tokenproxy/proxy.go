// Package tokenproxy adds short-lived client credentials to MCP requests.
package tokenproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Config struct{ TokenURL, MCPURL, ClientID, ClientSecret, Scope string }
type Proxy struct {
	config    Config
	client    *http.Client
	upstream  *httputil.ReverseProxy
	mu        sync.Mutex
	token     string
	refreshAt time.Time
	now       func() time.Time
}

func New(cfg Config) (*Proxy, error) {
	for _, raw := range []string{cfg.TokenURL, cfg.MCPURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("invalid upstream URL")
		}
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("OAuth client ID and secret required")
	}
	target, _ := url.Parse(cfg.MCPURL)
	p := &Proxy{config: cfg, now: time.Now, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	p.upstream = &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme = target.Scheme
		r.Out.URL.Host = target.Host
		r.Out.URL.Path = target.Path
		r.Out.URL.RawPath = target.RawPath
		r.Out.URL.RawQuery = target.RawQuery
		r.Out.Host = target.Host
		// No caller-controlled credentials or cookies cross the proxy boundary.
		r.Out.Header.Del("Cookie")
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "MCP upstream unavailable", http.StatusBadGateway)
	}}
	return p, nil
}

func (p *Proxy) accessToken(r *http.Request) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.now().Before(p.refreshAt) {
		return p.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {p.config.ClientID}, "client_secret": {p.config.ClientSecret}}
	if p.config.Scope != "" {
		form.Set("scope", p.config.Scope)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	started := p.now()
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint rejected request")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(b) > 65536 {
		return "", fmt.Errorf("invalid token response")
	}
	var data struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(b, &data) != nil || data.AccessToken == "" || !strings.EqualFold(data.TokenType, "Bearer") || strings.ContainsAny(data.AccessToken, "\r\n") || data.ExpiresIn <= 0 || data.ExpiresIn > 31536000 {
		return "", fmt.Errorf("invalid token response")
	}
	refreshAt := started.Add(time.Duration(data.ExpiresIn) * time.Second * 4 / 5)
	if !p.now().Before(refreshAt) {
		return "", fmt.Errorf("token lifetime too short")
	}
	p.token, p.refreshAt = data.AccessToken, refreshAt
	return p.token, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	token, err := p.accessToken(r)
	if err != nil {
		http.Error(w, "OAuth token unavailable", http.StatusBadGateway)
		return
	}
	req := r.Clone(r.Context())
	req.Header.Set("Authorization", "Bearer "+token)
	p.upstream.ServeHTTP(w, req)
}
