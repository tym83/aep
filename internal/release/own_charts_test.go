package release

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnChartVersions(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm required")
	}
	paths, err := filepath.Glob("../../charts/*/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			values := LocalValues("")
			values["global"] = map[string]any{"aepVersion": "sha-1234567"}
			rendered, err := render(context.Background(), filepath.Dir(p), "", values)
			if err != nil {
				t.Fatal(err)
			}
			images, err := Images(rendered)
			if err != nil {
				t.Fatal(err)
			}
			for _, image := range images {
				if repo, own := ownRepository(image); own && image != repo+":sha-1234567" {
					t.Fatal(image)
				}
			}
			if strings.Contains(string(rendered), "--bridge-image=") && !strings.Contains(string(rendered), "--bridge-image=ghcr.io/tym83/aep-bridge:sha-1234567") {
				t.Fatal("bridge argument has different version")
			}
		})
	}
	// A tag override cannot accidentally retain the release digest for a different tag.
	values := LocalValues("")
	values["global"] = map[string]any{"aepVersion": "v1.2.3", "aepDigests": map[string]any{"ghcr.io/tym83/aep-bridge": "sha256:" + strings.Repeat("a", 64)}}
	values["image"] = map[string]any{"tag": "sha-7654321"}
	out, err := render(context.Background(), "../../charts/aep-bridge", "", values)
	if err != nil || !strings.Contains(string(out), `image: "ghcr.io/tym83/aep-bridge:sha-7654321"`) {
		t.Fatalf("%s %v", out, err)
	}
}
