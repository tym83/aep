package vcs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// GitHubApp exchanges a short-lived signed assertion for an installation token.
type installationToken struct {
	token   string
	expires time.Time
}

var installationCache = struct {
	sync.Mutex
	tokens map[[32]byte]installationToken
}{tokens: map[[32]byte]installationToken{}}

func GitHubApp(ctx context.Context, origin, appID, installationID string, key, ca []byte) (string, error) {
	return githubApp(ctx, origin, appID, installationID, key, ca, false)
}
func GitHubAppRead(ctx context.Context, origin, appID, installationID string, key, ca []byte) (string, error) {
	return githubApp(ctx, origin, appID, installationID, key, ca, true)
}
func githubApp(ctx context.Context, origin, appID, installationID string, key, ca []byte, read bool) (string, error) {
	cacheKey := sha256.Sum256([]byte(fmt.Sprintf("%q|%q|%q|%x|%x|%t", origin, appID, installationID, sha256.Sum256(key), sha256.Sum256(ca), read)))
	installationCache.Lock()
	defer installationCache.Unlock()
	if cached, ok := installationCache.tokens[cacheKey]; ok && time.Now().Before(cached.expires.Add(-5*time.Minute)) {
		return cached.token, nil
	}
	for k, v := range installationCache.tokens {
		if time.Now().After(v.expires) {
			delete(installationCache.tokens, k)
		}
	}

	if !regexp.MustCompile(`^[0-9]+$`).MatchString(appID) || !regexp.MustCompile(`^[0-9]+$`).MatchString(installationID) {
		return "", fmt.Errorf("GitHub App IDs must be numeric")
	}
	block, _ := pem.Decode(key)
	if block == nil {
		return "", fmt.Errorf("invalid GitHub App private key")
	}
	var private *rsa.PrivateKey
	private, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return "", fmt.Errorf("invalid GitHub App private key")
		}
		var ok bool
		private, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("GitHub App requires an RSA key")
		}
	}
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "iss": appID})
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("GitHub App signing failed")
	}
	c, err := New("github", origin, unsigned+"."+enc(signature), ca)
	if err != nil {
		return "", err
	}
	var response struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	body := map[string]any{}
	if read {
		body["permissions"] = map[string]string{"contents": "read"}
	}
	_, err = c.request(ctx, "POST", "/app/installations/"+installationID+"/access_tokens", body, &response)
	if err != nil {
		return "", err
	}
	if response.Token == "" {
		return "", fmt.Errorf("GitHub returned an empty installation token")
	}
	if !response.Expires.After(time.Now().Add(5 * time.Minute)) {
		return "", fmt.Errorf("GitHub installation token expiry is missing or too soon")
	}
	installationCache.tokens[cacheKey] = installationToken{response.Token, response.Expires}
	return response.Token, nil
}
