package release

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// verify checks cosign's simple-signing payload and signature offline. The key is
// supplied independently by the operator, never trusted from the transferred bundle.
func (c *Client) verify(a Artifact, img v1.Image) error {
	keyPath, required := c.signaturePolicy(a)
	if !required {
		return nil
	}
	if keyPath == "" {
		return fmt.Errorf("no trusted key for %s", a.Ref)
	}
	if img == nil {
		return fmt.Errorf("missing signature for %s", a.Ref)
	}
	b, e := os.ReadFile(keyPath)
	if e != nil {
		return e
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return fmt.Errorf("invalid public key PEM")
	}
	key, e := x509.ParsePKIXPublicKey(block.Bytes)
	if e != nil {
		return e
	}
	manifest, e := img.Manifest()
	if e != nil {
		return e
	}
	layers, e := img.Layers()
	if e != nil {
		return e
	}
	for i, l := range layers {
		sig, e := base64.StdEncoding.DecodeString(manifest.Layers[i].Annotations["dev.cosignproject.cosign/signature"])
		if e != nil || len(sig) == 0 {
			continue
		}
		r, e := l.Uncompressed()
		if e != nil {
			return e
		}
		payload, e := io.ReadAll(io.LimitReader(r, 4<<20))
		r.Close()
		if e != nil {
			return e
		}
		var p struct {
			Critical struct {
				Image struct {
					Digest string `json:"docker-manifest-digest"`
				} `json:"image"`
				Type string `json:"type"`
			} `json:"critical"`
		}
		if json.Unmarshal(payload, &p) != nil || p.Critical.Image.Digest != a.Digest || p.Critical.Type != "cosign container image signature" {
			continue
		}
		hash := sha256.Sum256(payload)
		valid := false
		switch k := key.(type) {
		case *ecdsa.PublicKey:
			valid = ecdsa.VerifyASN1(k, hash[:], sig)
		case *rsa.PublicKey:
			valid = rsa.VerifyPKCS1v15(k, crypto.SHA256, hash[:], sig) == nil
		case ed25519.PublicKey:
			valid = ed25519.Verify(k, payload, sig)
		}
		if valid {
			return nil
		}
	}
	return fmt.Errorf("no valid cosign signature for %s@%s with provided key", a.Ref, a.Digest)
}

// Prefix matching respects repository boundaries, avoiding trust in lookalike names.
func matchesPrefix(ref, prefix string) bool {
	ref = strings.TrimPrefix(ref, "oci://")
	prefix = strings.TrimRight(strings.TrimPrefix(prefix, "oci://"), "/")
	return ref == prefix || strings.HasPrefix(ref, prefix+"/")
}
func (c *Client) signaturePolicy(a Artifact) (string, bool) {
	key, longest := c.Key, 0
	for prefix, path := range c.Keys {
		if matchesPrefix(a.Ref, prefix) && len(prefix) > longest {
			key, longest = path, len(prefix)
		}
	}
	required := key != ""
	for _, prefix := range strings.Split(c.Verify, ",") {
		if prefix != "" && matchesPrefix(a.Ref, prefix) {
			required = true
		}
	}
	return key, required
}
func (c *Client) requireSignature(a Artifact) error {
	key, required := c.signaturePolicy(a)
	if a.Signature == SignatureRequiredAtPublish || required && (key == "" || a.Signature == "") {
		return fmt.Errorf("trusted signature required for %s", a.Ref)
	}
	return nil
}
