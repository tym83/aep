package vcs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeClient(t *testing.T, provider string, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	c, e := New(provider, s.URL, "private-token", ca)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestProviders(t *testing.T) {
	for _, provider := range []string{"gitlab", "github"} {
		t.Run(provider, func(t *testing.T) {
			posts, updates := 0, 0
			c := fakeClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("missing credentials")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/user"):
					fmt.Fprint(w, `{"id":1}`)
				case strings.Contains(r.URL.Path, "/groups/"):
					fmt.Fprint(w, `{"id":12}`)
				case strings.Contains(r.URL.Path, "/orgs/"):
					fmt.Fprint(w, `{"login":"acme"}`)
				case strings.HasSuffix(r.URL.Path, "/hooks") && r.Method == "GET":
					fmt.Fprint(w, `[{"id":4,"url":"https://cp.example/api/v1alpha1/autobuild","config":{"url":"https://cp.example/api/v1alpha1/autobuild"}}]`)
				case r.Method == "PUT" || r.Method == "PATCH":
					updates++
					w.WriteHeader(200)
				case r.Method == "POST":
					posts++
					w.WriteHeader(201)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					w.WriteHeader(404)
				}
			})
			ctx := context.Background()
			if e := c.Verify(ctx); e != nil {
				t.Fatal(e)
			}
			if e := c.EnsureGroup(ctx, "acme", false); e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 2; i++ {
				if e := c.EnsureWebhook(ctx, "acme/repo", "https://cp.example/api/v1alpha1/autobuild", "hook-secret"); e != nil {
					t.Fatal(e)
				}
			}
			if posts != 0 || updates != 2 {
				t.Fatalf("not idempotent: posts %d updates %d", posts, updates)
			}
		})
	}
}
func TestGitLabCreateNestedGroup(t *testing.T) {
	created := 0
	c := fakeClient(t, "gitlab", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			created++
			fmt.Fprintf(w, `{"id":%d}`, created)
			return
		}
		w.WriteHeader(404)
	})
	if e := c.EnsureGroup(context.Background(), "acme/team", true); e != nil {
		t.Fatal(e)
	}
	if created != 2 {
		t.Fatal(created)
	}
}
func TestCredentialFailuresAreRedacted(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		for _, status := range []int{401, 403, 429, 500, 302} {
			c := fakeClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://attacker.invalid")
				w.WriteHeader(status)
				fmt.Fprint(w, "private-token")
			})
			e := c.Verify(context.Background())
			if e == nil || strings.Contains(e.Error(), "private-token") {
				t.Fatalf("unsafe error %v", e)
			}
		}
	}
}
func TestUntrustedCA(t *testing.T) {
	c := fakeClient(t, "github", func(w http.ResponseWriter, r *http.Request) {})
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
	if c.Verify(context.Background()) == nil {
		t.Fatal("accepted untrusted certificate")
	}
}
func TestRejectInvalidConfiguration(t *testing.T) {
	for _, url := range []string{"http://git.example", "https://token@git.example", "https://git.example?token=abc"} {
		if _, e := New("github", url, "token", nil); e == nil {
			t.Fatal(url)
		}
	}
}

func TestGitHubAppExchange(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/app/installations/42/access_tokens" || r.Method != "POST" {
			t.Error(r.Method, r.URL.Path)
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Fatal("invalid JWT")
		}
		signature, e := base64.RawURLEncoding.DecodeString(parts[2])
		if e != nil {
			t.Fatal(e)
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if e = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); e != nil {
			t.Error(e)
		}
		fmt.Fprint(w, `{"token":"installation-token"}`)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	token, e := GitHubApp(context.Background(), server.URL, "123", "42", private, ca)
	if e != nil || token != "installation-token" {
		t.Fatal(token, e)
	}
}
