package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFingerprintArtifactInputs(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("init")
	files := []string{"internal/bridge/reconcile.go", "cmd/operator/main.go", "internal/operator/mirror.go", "charts/example/values.yaml", "release/extra-images.yaml", "Dockerfile.operator", ".github/workflows/images.yml"}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("before"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	before, err := Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		p := filepath.Join(root, f)
		if err := os.WriteFile(p, []byte("after"), 0644); err != nil {
			t.Fatal(err)
		}
		after, err := Fingerprint(root)
		if err != nil {
			t.Fatal(err)
		}
		if (before == after) != (i < 2) {
			t.Fatalf("unexpected fingerprint effect: %s", f)
		}
		if err := os.WriteFile(p, []byte("before"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
