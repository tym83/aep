package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestComponentTypeImagesAreNotReleaseArtifacts(t *testing.T) {
	data := []byte(`apiVersion: openchoreo.dev/v1alpha1
kind: ClusterComponentType
spec:
  resources:
  - template:
      spec:
        containers:
        - image: ${workload.container.image}
---
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
      - image: example.com/platform:v1
`)
	images, err := Images(data)
	if err != nil || !reflect.DeepEqual(images, []string{"example.com/platform:v1"}) {
		t.Fatal(images, err)
	}
}

func TestRefreshLocalDiscoversNewCharts(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	dir := filepath.Join(root, "charts", "new-type")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: new-type\nversion: 0.1.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	upstream := Artifact{Kind: "chart", Ref: "oci://ghcr.io/tym83/aep/charts/klusterlet", Version: "1.4.0", Digest: "sha256:upstream"}
	m, err := RefreshLocal(root, Manifest{Schema: 1, Artifacts: []Artifact{upstream}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 2 {
		t.Fatal(m)
	}
	if !reflect.DeepEqual(m.Artifacts[0], upstream) {
		t.Fatal("upstream pin changed", m)
	}
	if m.Artifacts[1].Ref != "oci://ghcr.io/tym83/aep/charts/new-type" || m.Artifacts[1].Signature != SignatureRequiredAtPublish {
		t.Fatal(m)
	}
	again, err := RefreshLocal(root, m)
	if err != nil || !reflect.DeepEqual(m, again) {
		t.Fatal("refresh is not idempotent", again, err)
	}
}
