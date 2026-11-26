package tokenproxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshAndProxy(t *testing.T) {
	var calls atomic.Int32
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "agent" || r.Form.Get("client_secret") != "secret" || r.Form.Get("scope") != "mcp" {
			t.Error("incorrect grant")
		}
		n := calls.Add(1)
		fmt.Fprintf(w, `{"access_token":"token%d","token_type":"Bearer","expires_in":100}`, n)
	}))
	defer tokens.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "/upstream/mcp?fixed=yes" || r.Header.Get("Cookie") != "" {
			t.Error("incorrect proxy target or cookies")
		}
		fmt.Fprint(w, r.Header.Get("Authorization"))
	}))
	defer upstream.Close()
	p, err := New(Config{TokenURL: tokens.URL, MCPURL: upstream.URL + "/upstream/mcp?fixed=yes", ClientID: "agent", ClientSecret: "secret", Scope: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.now = func() time.Time { return now }
	request := func(want string) {
		r := httptest.NewRequest("POST", "/mcp?injected=yes", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer caller")
		r.Header.Set("Cookie", "private=1")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != want {
			t.Errorf("%d %s", w.Code, w.Body)
		}
		if r.Header.Get("Authorization") != "Bearer caller" {
			t.Error("mutated caller")
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { request("Bearer token1") })
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	now = now.Add(79 * time.Second)
	request("Bearer token1")
	now = now.Add(time.Second)
	request("Bearer token2")
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}

func TestTokenFailuresDoNotReachUpstream(t *testing.T) {
	for _, body := range []string{`{"access_token":"x","token_type":"Bearer","expires_in":0}`, `{"access_token":"x","token_type":"Basic","expires_in":10}`, `{`, strings.Repeat("x", 65537)} {
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		p, err := New(Config{TokenURL: endpoint.URL, MCPURL: "http://unreachable.invalid/mcp", ClientID: "id", ClientSecret: "secret"})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("POST", "/mcp", nil))
		endpoint.Close()
		if w.Code != 502 || strings.Contains(w.Body.String(), "secret") {
			t.Fatal(w.Code, w.Body)
		}
	}
}
