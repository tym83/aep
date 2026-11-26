package operator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tym83/aep/internal/version"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFleetOwnImagesFollowOperatorVersion(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm required")
	}
	old := version.Version
	version.Version = "sha-1234567"
	defer func() { version.Version = old }()
	dir := t.TempDir()
	chart := filepath.Join(dir, "aep-own")
	if err := os.MkdirAll(filepath.Join(chart, "templates"), 0755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: aep-own\nversion: 0.1.0\n",
		"templates/pod.yaml": `apiVersion: v1
kind: Pod
metadata:
  name: own
spec:
  containers:
    - name: bridge
      image: ghcr.io/tym83/aep-bridge:{{ .Values.global.aepVersion }}
`,
	} {
		if err := os.WriteFile(filepath.Join(chart, path), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("helm", "package", chart, "--destination", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, mirror := range []string{"", "mirror.example/cache"} {
		t.Run(mirror, func(t *testing.T) {
			h := HelmFleetRenderer{Directory: dir, Mirror: mirror}
			out, err := h.Render(context.Background(), "aep-own", "0.1.0", "aep-system", M{"global": M{"aepVersion": "stale"}})
			if err != nil {
				t.Fatal(err)
			}
			containers, _, err := unstructured.NestedSlice(out[0].(map[string]any), "spec", "containers")
			if err != nil {
				t.Fatal(err)
			}
			want := "ghcr.io/tym83/aep-bridge:" + version.Version
			if mirror != "" {
				want = mirror + "/" + want
			}
			if got := containers[0].(map[string]any)["image"]; got != want {
				t.Fatalf("image = %v, want %s", got, want)
			}
		})
	}
	stale := M{"containers": []any{M{"image": "ghcr.io/tym83/aep-bridge:sha-old"}}}
	if err := mirrorFleetImages(stale, ""); err == nil {
		t.Fatal("accepted own image from a different operator build")
	}
}
