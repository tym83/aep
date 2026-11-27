package release

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type rejectInventoryNetwork struct{ calls int }

func (r *rejectInventoryNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	return nil, fmt.Errorf("unexpected registry access: %s", req.URL)
}

func TestInventoryOwnImagesDoNotDiscoverSignatures(t *testing.T) {
	// A frozen release has concrete own-image pins; the source inventory now
	// contains @build placeholders and must not supply historical commit pins.
	root := t.TempDir()
	for _, dir := range []string{".github/workflows", "release"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{".github/workflows/images.yml", "Dockerfile.operator"} {
		data, err := os.ReadFile(filepath.Join("../..", path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "schema: 1\nartifacts:\n- kind: image\n  own: true\n  ref: ghcr.io/tym83/aep-operator\n  version: sha-618d4f85c1e90f6afcaf3cbcb26a066f21155e83\n  digest: sha256:" + strings.Repeat("a", 64) + "\n"
	if err := os.WriteFile(filepath.Join(root, "release/manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	transport := &rejectInventoryNetwork{}
	c := &Client{transport: transport}
	for _, ref := range []string{
		"ghcr.io/tym83/aep-operator:sha-618d4f85c1e90f6afcaf3cbcb26a066f21155e83",
		"ghcr.io/tym83/aep-operator@sha256:" + strings.Repeat("b", 64),
	} {
		a, err := c.resolveInventoryImage(context.Background(), root, ref)
		if err != nil {
			t.Fatal(err)
		}
		if a.Signature != SignatureRequiredAtPublish || a.Digest == "" {
			t.Fatalf("missing local pin/publish requirement: %+v", a)
		}
	}
	if _, err := c.resolveInventoryImage(context.Background(), root, "ghcr.io/tym83/aep-operator:latest@sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("mutable local image tag accepted")
	}
	if transport.calls != 0 {
		t.Fatal("own images accessed registry")
	}
	for _, ref := range []string{"ghcr.io/tym83/aep-operator-unrelated:v1.0.0", "ghcr.io/other/aep-operator:v1.0.0"} {
		if _, err := c.resolveInventoryImage(context.Background(), root, ref); err == nil {
			t.Fatal("upstream lookup skipped")
		}
	}
	if transport.calls == 0 {
		t.Fatal("upstream images did not access registry")
	}
}

func TestPublishRequirementPreservesStrictTransfers(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{InsecureRegistry: host, Key: keyPath}
	for _, kind := range []string{"image", "chart"} {
		t.Run(kind, func(t *testing.T) {
			img, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte(kind), types.OCILayer))
			if err != nil {
				t.Fatal(err)
			}
			ref, _ := name.NewTag(host+"/own/"+kind+":1.0.0", name.Insecure)
			if err := remote.Write(ref, img); err != nil {
				t.Fatal(err)
			}
			digest, _ := img.Digest()
			a := Artifact{Kind: kind, Ref: ref.Context().Name(), Version: "1.0.0", Digest: digest.String(), Signature: SignatureRequiredAtPublish}
			if kind == "chart" {
				a.Ref = "oci://" + a.Ref
			}
			m := Manifest{Schema: 1, Artifacts: []Artifact{a}}
			if err := c.requireSignature(a); err == nil {
				t.Fatal("publish marker treated as signature")
			}
			if err := c.Mirror(ctx, m, host+"/missing"); err == nil {
				t.Fatal("unsigned mirror succeeded")
			}
			if err := c.Bundle(ctx, m, t.TempDir()); err == nil {
				t.Fatal("unsigned bundle succeeded")
			}
			sigRef := ref.Context().Tag("sha256-" + digest.Hex + ".sig")
			// A published but invalid signature must also fail with a trusted key.
			if err := remote.Write(sigRef, empty.Image); err != nil {
				t.Fatal(err)
			}
			if err := c.Mirror(ctx, m, host+"/invalid"); err == nil {
				t.Fatal("invalid signature mirrored")
			}
			if err := c.Bundle(ctx, m, t.TempDir()); err == nil {
				t.Fatal("invalid signature bundled")
			}
			payload := []byte(fmt.Sprintf(`{"critical":{"type":"cosign container image signature","image":{"docker-manifest-digest":%q}}}`, a.Digest))
			hash := sha256.Sum256(payload)
			sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
			if err != nil {
				t.Fatal(err)
			}
			signed, err := mutate.Append(empty.Image, mutate.Addendum{Layer: static.NewLayer(payload, types.OCILayer), Annotations: map[string]string{"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString(sig)}})
			if err != nil {
				t.Fatal(err)
			}
			if err := remote.Write(sigRef, signed); err != nil {
				t.Fatal(err)
			}
			if err := c.Mirror(ctx, m, host+"/verified"); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := c.Bundle(ctx, m, dir); err != nil {
				t.Fatal(err)
			}
			bundled, err := Read(filepath.Join(dir, "manifest.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if bundled.Artifacts[0].Signature == SignatureRequiredAtPublish {
				t.Fatal("bundle retained unresolved signature")
			}
			if m.Artifacts[0].Signature != SignatureRequiredAtPublish {
				t.Fatal("transport mutated inventory")
			}
			if err := c.Load(ctx, dir, host+"/loaded"); err != nil {
				t.Fatal(err)
			}
			c.Key = filepath.Join(t.TempDir(), "missing-key")
			if err := c.Load(ctx, dir, host+"/untrusted"); err == nil {
				t.Fatal("load bypassed key verification")
			}
			c.Key = keyPath
		})
	}
}

func TestDiscoverSignaturesToleratesDeniedOptionalLookups(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`, http.StatusForbidden)
	}))
	defer s.Close()
	host := strings.TrimPrefix(s.URL, "http://")
	c := &Client{InsecureRegistry: host}
	if err := c.Init(); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	optional := Manifest{Schema: 1, Artifacts: []Artifact{{Kind: "chart", Ref: "oci://" + host + "/charts/layout-only", Version: "1.4.0", Digest: digest}}}
	if err := c.DiscoverSignatures(context.Background(), &optional); err != nil || optional.Artifacts[0].Signature != "" {
		t.Fatal("an unpublished optional artifact must not fail discovery", err)
	}
	required := Manifest{Schema: 1, Artifacts: []Artifact{{Kind: "chart", Ref: "oci://" + host + "/charts/own", Version: "1.0.0", Digest: digest, Signature: SignatureRequiredAtPublish}}}
	if err := c.DiscoverSignatures(context.Background(), &required); err == nil {
		t.Fatal("a denied lookup of a required signature must fail")
	}
}
