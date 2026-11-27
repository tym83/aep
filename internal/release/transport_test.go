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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"sigs.k8s.io/yaml"
)

func TestMirrorBundleLoad(t *testing.T) {
	ctx := context.Background()
	s := httptest.NewServer(registry.New())
	defer s.Close()
	host := strings.TrimPrefix(s.URL, "http://")
	c := &Client{InsecureRegistry: host}
	if e := c.Init(); e != nil {
		t.Fatal(e)
	}
	img, e := mutate.AppendLayers(empty.Image, static.NewLayer([]byte("payload"), types.OCILayer))
	if e != nil {
		t.Fatal(e)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
	r, _ := name.NewTag(host+"/source/image:v1.0.0", name.Insecure)
	if e = remote.WriteIndex(r, index); e != nil {
		t.Fatal(e)
	}
	h, _ := index.Digest()
	a := Artifact{Kind: "image", Ref: host + "/source/image", Version: "v1.0.0", Digest: h.String()}
	// Exercise a chart's non-container config and layer media types too.
	chart := mutate.ConfigMediaType(empty.Image, types.MediaType("application/vnd.cncf.helm.config.v1+json"))
	chart, e = mutate.AppendLayers(chart, static.NewLayer([]byte("chart archive"), types.MediaType("application/vnd.cncf.helm.chart.content.v1.tar+gzip")))
	if e != nil {
		t.Fatal(e)
	}
	cr, _ := name.NewTag(host+"/source/chart:1.0.0", name.Insecure)
	if e = remote.Write(cr, chart); e != nil {
		t.Fatal(e)
	}
	ch, _ := chart.Digest()
	m := Manifest{Schema: 1, Artifacts: []Artifact{a, {Kind: "chart", Ref: "oci://" + host + "/source/chart", Version: "1.0.0", Digest: ch.String()}}}
	for i := 0; i < 2; i++ {
		if e = c.Mirror(ctx, m, host+"/mirror"); e != nil {
			t.Fatal(e)
		}
	}
	dir := t.TempDir()
	if e = c.Bundle(ctx, m, dir); e != nil {
		t.Fatal(e)
	}
	// A bundle's self-supplied checksums do not establish trust.
	c.Key = "trusted.pub"
	if err := c.Load(ctx, dir, host+"/untrusted"); err == nil || !strings.Contains(err.Error(), "signature required") {
		t.Fatalf("unsigned bundle accepted: %v", err)
	}
	c.Key = ""
	c.MaxSize = 1
	if err := c.Load(ctx, dir, host+"/oversize"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize bundle accepted: %v", err)
	}
	c.MaxSize = 0
	c.WorkDir = t.TempDir()
	// Source server can be gone for load; the destination is independent.
	s.Close()
	dest := httptest.NewServer(registry.New())
	defer dest.Close()
	to := strings.TrimPrefix(dest.URL, "http://")
	c.InsecureRegistry = to
	for i := 0; i < 2; i++ {
		if e = c.Load(ctx, dir, to); e != nil {
			t.Fatal(e)
		}
	}
	for _, a := range m.Artifacts {
		dst, e := c.destination(a, to, false)
		if e != nil {
			t.Fatal(e)
		}
		d, e := remote.Get(dst)
		if e != nil || d.Digest.String() != a.Digest {
			t.Fatalf("digest not preserved: %v", e)
		}
	}
	if e = os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = c.Load(ctx, dir, to); e == nil || !strings.Contains(e.Error(), "checksum mismatch") {
		t.Fatalf("tampered bundle accepted: %v", e)
	}
}
func TestSignatureVerification(t *testing.T) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	pub, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if e = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0600); e != nil {
		t.Fatal(e)
	}
	a := Artifact{Ref: "example.org/a", Digest: "sha256:" + strings.Repeat("a", 64)}
	payload := []byte(fmt.Sprintf(`{"critical":{"type":"cosign container image signature","image":{"docker-manifest-digest":%q}}}`, a.Digest))
	hash := sha256.Sum256(payload)
	sig, e := ecdsa.SignASN1(rand.Reader, key, hash[:])
	if e != nil {
		t.Fatal(e)
	}
	img, e := mutate.Append(empty.Image, mutate.Addendum{Layer: static.NewLayer(payload, types.OCILayer), Annotations: map[string]string{"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString(sig)}})
	if e != nil {
		t.Fatal(e)
	}
	c := &Client{Key: path}
	if e = c.verify(a, img); e != nil {
		t.Fatal(e)
	}
	a.Digest = "sha256:" + strings.Repeat("b", 64)
	if e = c.verify(a, img); e == nil {
		t.Fatal("wrong subject accepted")
	}
	c.Key = ""
	if e = c.verify(a, img); e != nil {
		t.Fatal("optional signature blocked transfer")
	}
}
func TestImages(t *testing.T) {
	got, e := Images([]byte("kind: Pod\nspec:\n  initContainers:\n  - image: a.io/init:v1\n  containers:\n  - image: b.io/main@sha256:abc\n---\nkind: Job\nspec:\n  image: a.io/init:v1\n"))
	if e != nil || len(got) != 2 {
		t.Fatal(got, e)
	}
	got, e = Images([]byte("kind: ClusterWorkflowTemplate\nspec:\n  templates:\n  - container:\n      image: c.io/build@sha256:abc\n"))
	if e != nil || len(got) != 1 || got[0] != "c.io/build@sha256:abc" {
		t.Fatal("Argo template images must be inventoried", got, e)
	}
}
func TestManifestValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.yaml")
	m := Manifest{Schema: 1, Artifacts: []Artifact{{Kind: "chart", Ref: "oci://example.org/chart", Version: "1", Digest: "unresolved"}}}
	b, _ := yaml.Marshal(m)
	os.WriteFile(p, b, 0600)
	if _, e := Read(p); e == nil {
		t.Fatal("unresolved digest accepted")
	}
}

// Keep the test compiler checking the interface used for multi-platform copies.
var _ v1.ImageIndex = empty.Index

func TestLocalChartDeterministic(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "charts", "example")
	os.MkdirAll(filepath.Join(dir, "templates"), 0755)
	os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: example\nversion: 1.0.0\n"), 0644)
	os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte("kind: ConfigMap\n"), 0644)
	a, e := PackageChart(root, dir)
	if e != nil {
		t.Fatal(e)
	}
	b, e := PackageChart(root, dir)
	if e != nil || a.Digest != b.Digest {
		t.Fatal("nondeterministic chart", e)
	}
	a.Layout = filepath.Join(root, a.Layout)
	c := &Client{}
	d, e := c.fetch(context.Background(), a, false)
	if e != nil {
		t.Fatal(e)
	}
	img, e := d.Image()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := img.RawConfigFile()
	if e != nil || !strings.Contains(string(raw), `"name":"example"`) {
		t.Fatalf("invalid Helm config: %s %v", raw, e)
	}
}

func TestHelmOCICompatibility(t *testing.T) {
	if _, e := exec.LookPath("helm"); e != nil {
		t.Skip("Helm not installed")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "charts", "example")
	os.MkdirAll(filepath.Join(dir, "templates"), 0755)
	os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: example\nversion: 1.0.0\n"), 0644)
	os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example\n"), 0644)
	a, e := PackageChart(root, dir)
	if e != nil {
		t.Fatal(e)
	}
	a.Layout = filepath.Join(root, a.Layout)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	to := strings.TrimPrefix(server.URL, "http://")
	c := &Client{InsecureRegistry: to}
	if e = c.Mirror(context.Background(), Manifest{Schema: 1, Artifacts: []Artifact{a}}, to); e != nil {
		t.Fatal(e)
	}
	out, e := exec.Command("helm", "template", "example", "oci://"+to+"/ghcr.io/tym83/aep/charts/example", "--version", "1.0.0", "--plain-http").CombinedOutput()
	if e != nil || !strings.Contains(string(out), "kind: ConfigMap") {
		t.Fatalf("Helm cannot consume packaged OCI chart: %v %s", e, out)
	}
}

func TestRetagRequiresOptIn(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	c := &Client{InsecureRegistry: host}
	dst, err := c.ref(host + "/image:v1")
	if err != nil {
		t.Fatal(err)
	}
	first := empty.Image
	second, err := mutate.AppendLayers(first, static.NewLayer([]byte("changed"), types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := first.Digest()
	h2, _ := second.Digest()
	ctx := context.Background()
	if err = c.put(ctx, dst, h1.String(), first, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.put(ctx, dst, h2.String(), second, nil); err == nil {
		t.Fatal("retag accepted")
	}
	got, err := remote.Get(dst)
	if err != nil || got.Digest != h1 {
		t.Fatal("existing tag changed", err)
	}
	c.AllowRetag = true
	if err = c.put(ctx, dst, h2.String(), second, nil); err != nil {
		t.Fatal(err)
	}
}
func TestInsecureRegistryScope(t *testing.T) {
	c := &Client{InsecureRegistry: "mirror.example:5000"}
	for _, tc := range []struct{ ref, scheme string }{
		{"mirror.example:5000/app:v1", "http"},
		{"upstream.example/app:v1", "https"},
		{"mirror.example:5001/app:v1", "https"},
	} {
		r, err := c.ref(tc.ref)
		if err != nil || r.Context().Registry.Scheme() != tc.scheme {
			t.Fatal(tc, r, err)
		}
	}
}
func TestSignaturePolicy(t *testing.T) {
	c := &Client{Keys: map[string]string{"ghcr.io/team": "team.pub"}, Verify: "docker.io/library"}
	for _, tc := range []struct {
		ref      string
		required bool
		key      string
	}{
		{"ghcr.io/team/image", true, "team.pub"},
		{"ghcr.io/team-other/image", false, ""},
		{"docker.io/library/alpine", true, ""},
		{"other.example/image", false, ""},
	} {
		a := Artifact{Ref: tc.ref}
		key, required := c.signaturePolicy(a)
		if key != tc.key || required != tc.required {
			t.Fatal(tc, key, required)
		}
		if err := c.requireSignature(a); (err != nil) != tc.required {
			t.Fatal(tc, err)
		}
	}
	c = &Client{Key: "trusted.pub"}
	if err := c.Mirror(context.Background(), Manifest{Artifacts: []Artifact{{Ref: "example.org/unsigned"}}}, "mirror.example"); err == nil {
		t.Fatal("unsigned artifact accepted with --key")
	}
}

func TestFingerprintTrackedInputs(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	for _, path := range []string{"internal/operator/mirror.go", "charts/example/values.yaml", "release/extra-images.yaml", "Dockerfile.operator"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("original"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	initial, err := Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "charts/example/.DS_Store"), []byte("untracked"), 0644)
	after, err := Fingerprint(root)
	if err != nil || initial != after {
		t.Fatal("untracked input changed hash", err)
	}
	for _, path := range []string{"internal/operator/mirror.go", "charts/example/values.yaml", "release/extra-images.yaml", "Dockerfile.operator"} {
		full := filepath.Join(root, path)
		os.WriteFile(full, []byte("modified"), 0644)
		got, err := Fingerprint(root)
		if err != nil || got == initial {
			t.Fatal("missed tracked input", path, err)
		}
		os.WriteFile(full, []byte("original"), 0644)
	}
}
func TestOfflineImageInventory(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm required")
	}
	root := "../.."
	m, err := Read(filepath.Join(root, "release/manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "release/.cache/kyverno/Chart.yaml")); err != nil {
		t.Skip("upstream cache required")
	}
	if err := CheckImages(context.Background(), root, m); err != nil {
		t.Fatal(err)
	}
	for i, a := range m.Artifacts {
		if a.Kind == "image" {
			m.Artifacts = append(m.Artifacts[:i], m.Artifacts[i+1:]...)
			break
		}
	}
	if err := CheckImages(context.Background(), root, m); err == nil {
		t.Fatal("missing image accepted")
	}
}

func TestMutableReleaseTagsRejected(t *testing.T) {
	for _, version := range []string{"main", "latest", "latest-dev", "dev", "master", "v1"} {
		m := Manifest{Schema: 1, Artifacts: []Artifact{{Kind: "image", Ref: "example.org/image", Version: version, Digest: "sha256:" + strings.Repeat("a", 64)}}}
		b, _ := yaml.Marshal(m)
		path := filepath.Join(t.TempDir(), "manifest.yaml")
		os.WriteFile(path, b, 0600)
		if _, err := Read(path); err == nil {
			t.Fatal("mutable tag accepted", version)
		}
	}
}

func TestHTTPRequiresExactOptIn(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	c := &Client{InsecureRegistry: "different.example"}
	r, err := c.ref(strings.TrimPrefix(server.URL, "http://") + "/image:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(r, c.options(context.Background())...); err == nil || !strings.Contains(err.Error(), "requires --insecure-registry=") {
		t.Fatalf("implicit HTTP fallback: %v", err)
	}
}

func TestResolveOwnBuild(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	c := &Client{InsecureRegistry: host}
	if err := c.Init(); err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewTag(host+"/own:sha-1234567", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, empty.Image); err != nil {
		t.Fatal(err)
	}
	upstream := Artifact{Kind: "image", Ref: "unreachable.invalid/upstream", Version: "v1.2.3", Digest: "sha256:" + strings.Repeat("a", 64)}
	m := Manifest{Schema: 1, Artifacts: []Artifact{{Kind: "image", Own: true, Ref: host + "/own", Version: "@build"}, upstream}}
	for _, v := range []string{"", "main", "latest"} {
		if _, err := c.ResolveOwn(context.Background(), m, v); err == nil {
			t.Fatal("accepted build", v)
		}
	}
	frozen, err := c.ResolveOwn(context.Background(), m, "sha-1234567")
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := empty.Image.Digest()
	if frozen.Artifacts[0].Digest != digest.String() || !frozen.Artifacts[0].Own || frozen.Artifacts[0].Version != "sha-1234567" || frozen.Artifacts[1] != upstream || m.Artifacts[0].Version != "@build" {
		t.Fatal(frozen, m)
	}
	server.Close()
	if _, err := c.ResolveOwn(context.Background(), frozen, "sha-7654321"); err == nil {
		t.Fatal("relabelled a frozen digest")
	}
	// Frozen inventories never resolve the tag again, including during later transfers.
	if _, err := c.ResolveOwn(context.Background(), frozen, ""); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorRerunSkipsCopiedArtifacts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "charts", "example")
	os.MkdirAll(filepath.Join(dir, "templates"), 0755)
	os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: example\nversion: 1.0.0\n"), 0644)
	os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example\n"), 0644)
	a, e := PackageChart(root, dir)
	if e != nil {
		t.Fatal(e)
	}
	a.Layout = filepath.Join(root, a.Layout)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	to := strings.TrimPrefix(server.URL, "http://")
	c := &Client{InsecureRegistry: to}
	m := Manifest{Schema: 1, Artifacts: []Artifact{a}}
	if e = c.Mirror(context.Background(), m, to); e != nil {
		t.Fatal(e)
	}
	// The source is gone, as when a public registry rate-limits a rerun.
	m.Artifacts[0].Layout = filepath.Join(root, "missing")
	if e = c.Mirror(context.Background(), m, to); e != nil {
		t.Fatal("rerun pulled an artifact the mirror already holds:", e)
	}
}
