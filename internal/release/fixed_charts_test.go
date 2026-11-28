package release

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tym83/aep/internal/operator"
	inventory "github.com/tym83/aep/release"
	"sigs.k8s.io/yaml"
)

// A fixed-version chart is published once per version. Changing its content
// must come with a version bump and a new entry in charts/fixed-versions.yaml;
// existing entries are history and never change.
func TestFixedVersionCharts(t *testing.T) {
	b, err := os.ReadFile("../../charts/fixed-versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	committed := map[string]map[string]string{}
	if err := yaml.UnmarshalStrict(b, &committed); err != nil {
		t.Fatal(err)
	}
	operatorPins := map[string]string{operator.ComponentTypesChart: operator.ComponentTypesChartVersion, operator.BuildsChart: operator.BuildsChartVersion}
	paths, err := filepath.Glob("../../charts/*/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		dir := filepath.Dir(p)
		fixed, err := FixedVersion(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !fixed {
			continue
		}
		a, err := PackageChart(t.TempDir(), dir)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(dir)
		seen[name] = true
		want, ok := committed[name][a.Version]
		if !ok {
			t.Errorf("%s %s: add %q under this version to charts/fixed-versions.yaml", name, a.Version, a.Digest)
			continue
		}
		if want != a.Digest {
			t.Errorf("%s %s content changed (%s, committed %s): bump the chart version and the operator constant", name, a.Version, a.Digest, want)
		}
		if operatorPins[name] != a.Version {
			t.Errorf("%s: operator pins %q, Chart.yaml has %q", name, operatorPins[name], a.Version)
		}
		if pinned, err := inventory.Lookup("chart", operator.OwnChartRepository+"/"+name, a.Version); err != nil || pinned.Digest != a.Digest {
			t.Errorf("%s: embedded inventory %v %v, refresh release/manifest.yaml", name, pinned, err)
		}
	}
	for name := range operatorPins {
		if !seen[name] {
			t.Errorf("%s: operator pins a version, but Chart.yaml lacks %s", name, FixedVersionAnnotation)
		}
	}
}

func TestWriteBuildValuesKeepsFixedCharts(t *testing.T) {
	root := t.TempDir()
	write := func(chart, meta string) {
		dir := filepath.Join(root, "charts", chart)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("replicas: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fixed := "apiVersion: v2\nname: fixed\nversion: 0.1.0\nannotations:\n  aep.aenix.io/fixed-version: \"true\"\n"
	write("fixed", fixed)
	write("moving", "apiVersion: v2\nname: moving\nversion: 0.1.0\n")
	if err := WriteBuildValues(root, Manifest{}, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"Chart.yaml": fixed, "values.yaml": "replicas: 1\n"} {
		if b, _ := os.ReadFile(filepath.Join(root, "charts/fixed", file)); string(b) != want {
			t.Fatalf("fixed chart %s rewritten: %s", file, b)
		}
	}
	var meta struct{ Version string }
	b, _ := os.ReadFile(filepath.Join(root, "charts/moving/Chart.yaml"))
	if err := yaml.Unmarshal(b, &meta); err != nil || meta.Version != "1.2.3" {
		t.Fatalf("moving chart kept its version: %s %v", b, err)
	}
}
