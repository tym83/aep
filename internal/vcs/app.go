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
	"time"
)

// GitHubApp exchanges a short-lived signed assertion for an installation token.
func GitHubApp(ctx context.Context, origin, appID, installationID string, key, ca []byte) (string, error) {
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
		Token string `json:"token"`
	}
	_, err = c.request(ctx, "POST", "/app/installations/"+installationID+"/access_tokens", map[string]any{}, &response)
	if err != nil {
		return "", err
	}
	if response.Token == "" {
		return "", fmt.Errorf("GitHub returned an empty installation token")
	}
	return response.Token, nil
}
