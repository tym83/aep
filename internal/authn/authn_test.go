package authn

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func jwksJSON(key *rsa.PublicKey, kid string) []byte {
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	return fmt.Appendf(nil, `{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":%q,"n":%q,"e":%q}]}`, kid, n, e)
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(input))
	sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if e != nil {
		t.Fatal(e)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestOIDCVerifier(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON(&key.PublicKey, "test-key"))
	}))
	t.Cleanup(jwks.Close)
	issuer := "https://keycloak.example/realms/cozy"
	newVerifier := func(audience string) *OIDCVerifier {
		v, e := NewOIDCVerifier(context.Background(), issuer, jwks.URL, audience, "", "")
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	claims := func(mutate func(map[string]any)) map[string]any {
		c := map[string]any{
			"client_id": "aep-acme-sre-agent",
			"iss":       issuer, "sub": "user-1", "preferred_username": "alice",
			"groups": []string{"/aep-acme-developer", "aep-platform-team"},
			"iat":    time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}

	t.Run("valid token", func(t *testing.T) {
		id, e := newVerifier("").Verify(context.Background(), signJWT(t, key, "test-key", claims(nil)))
		if e != nil {
			t.Fatal(e)
		}
		if id.ClientID != "aep-acme-sre-agent" || id.Username != "alice" || id.Subject != "user-1" || len(id.Groups) != 2 {
			t.Fatalf("unexpected identity %+v", id)
		}
	})
	t.Run("expired token", func(t *testing.T) {
		token := signJWT(t, key, "test-key", claims(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }))
		if _, e := newVerifier("").Verify(context.Background(), token); e == nil {
			t.Fatal("expired tokens must be rejected")
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		token := signJWT(t, key, "test-key", claims(func(c map[string]any) { c["iss"] = "https://evil.example" }))
		if _, e := newVerifier("").Verify(context.Background(), token); e == nil {
			t.Fatal("a foreign issuer must be rejected")
		}
	})
	t.Run("audience enforced when configured", func(t *testing.T) {
		token := signJWT(t, key, "test-key", claims(func(c map[string]any) { c["aud"] = "other-client" }))
		if _, e := newVerifier("aep-portal").Verify(context.Background(), token); e == nil {
			t.Fatal("a wrong audience must be rejected")
		}
		token = signJWT(t, key, "test-key", claims(func(c map[string]any) { c["aud"] = "aep-portal" }))
		if _, e := newVerifier("aep-portal").Verify(context.Background(), token); e != nil {
			t.Fatal("the configured audience must be accepted:", e)
		}
	})
	t.Run("username falls back to subject", func(t *testing.T) {
		token := signJWT(t, key, "test-key", claims(func(c map[string]any) { delete(c, "preferred_username") }))
		id, e := newVerifier("").Verify(context.Background(), token)
		if e != nil {
			t.Fatal(e)
		}
		if id.Username != "user-1" {
			t.Fatalf("expected the subject as fallback, got %q", id.Username)
		}
	})
	t.Run("garbage token", func(t *testing.T) {
		if _, e := newVerifier("").Verify(context.Background(), "not-a-jwt"); e == nil {
			t.Fatal("garbage must be rejected")
		}
	})
}

func TestBearerToken(t *testing.T) {
	if BearerToken("Bearer abc") != "abc" || BearerToken("bearer abc") != "" || BearerToken("") != "" {
		t.Fatal("unexpected bearer parsing")
	}
}
