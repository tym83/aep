package operator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorSources(t *testing.T) {
	o := Options{ChartRegistryMirror: "mirror.local/cache", RegistrySecret: "registry-auth", RegistryCASecret: "registry-ca"}
	for _, c := range DeliveryCharts() {
		u := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "tenant", "chart", M{"url": c.URL})
		o.chartSource(u)
		spec := u.Object["spec"].(map[string]any)
		if !strings.HasPrefix(spec["url"].(string), "oci://mirror.local/cache/") {
			t.Fatal(spec)
		}
		if spec["secretRef"].(map[string]any)["name"] != "registry-auth" || spec["certSecretRef"].(map[string]any)["name"] != "registry-ca" {
			t.Fatal(spec)
		}
	}
}

// Set AEP_UPSTREAM_CHARTS to a directory containing the pinned, unpacked charts.
// CI supplies these; developers can run the rest of the suite without registries.
func TestUpstreamMirrorRendering(t *testing.T) {
	dir := os.Getenv("AEP_UPSTREAM_CHARTS")
	if dir == "" {
		t.Skip("set AEP_UPSTREAM_CHARTS for upstream Helm rendering")
	}
	for _, chart := range DeliveryCharts() {
		t.Run(chart.Name, func(t *testing.T) {
			values := MirrorValues(chart.Name, "mirror.local/cache", ReleaseValues(chart.Name))
			b, e := json.Marshal(values)
			if e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command("helm", "template", "aep", filepath.Join(dir, chart.Name), "--values", "-", "--include-crds")
			cmd.Stdin = strings.NewReader(string(b))
			out, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("%v: %s", e, out)
			}
			// Scan all rendered text, including hooks and controller environment defaults.
			for _, host := range []string{"ghcr.io/", "cr.kgateway.dev/", "cr.openchoreo.dev/", "reg.kyverno.io/", "quay.io/", "registry.k8s.io/", "docker.io/"} {
				stripped := strings.ReplaceAll(string(out), "mirror.local/cache/"+host, "")
				// Descriptions and CRD examples can contain unrelated public URLs; only
				// manifests with workloads are scanned below, CRDs have no executable images.
				for _, doc := range strings.Split(stripped, "\n---") {
					if strings.Contains(doc, "kind: CustomResourceDefinition") {
						continue
					}
					if strings.Contains(doc, host) {
						t.Fatalf("public reference %s in %s", host, doc)
					}
				}
			}
		})
	}
}
