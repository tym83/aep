package release

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// Discover charts instead of maintaining a second list that silently misses new
// components. Required identity values must work in both lint and rendering.
func TestAllLocalChartsLint(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm required")
	}
	for _, base := range []string{"charts", "policies"} {
		paths, err := filepath.Glob(filepath.Join("../..", base, "*", "Chart.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			dir := filepath.Dir(path)
			t.Run(filepath.Base(dir), func(t *testing.T) {
				b, err := json.Marshal(LocalValues(filepath.Base(dir)))
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("helm", "lint", "--strict", dir, "--values", "-")
				cmd.Stdin = bytes.NewReader(b)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
			})
		}
	}
}

func TestChartInventoryCoverage(t *testing.T) {
	m, err := Read("../../release/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckCharts("../..", m); err != nil {
		t.Fatal(err)
	}
	for i, a := range m.Artifacts {
		if a.Kind != "chart" {
			continue
		}
		missing := m
		missing.Artifacts = append(append([]Artifact{}, m.Artifacts[:i]...), m.Artifacts[i+1:]...)
		if err := CheckCharts("../..", missing); err == nil {
			t.Fatalf("missing chart accepted: %s", a.Ref)
		}
	}
}
