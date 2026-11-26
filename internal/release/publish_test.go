package release

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestPublishedChartMatchesLayout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "charts", "example")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: example\nversion: 1.0.0\n"), 0644)
	a, err := PackageChart(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	c := &Client{InsecureRegistry: host, PublishRegistry: host + "/aep"}
	var output bytes.Buffer
	if err := c.PublishCharts(context.Background(), root, &output); err != nil {
		t.Fatal(err)
	}
	r, err := c.ref(host + "/aep/charts/example:1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	d, err := remote.Get(r)
	if err != nil || d.Digest.String() != a.Digest {
		t.Fatal("published bytes differ", err)
	}
	if !strings.Contains(output.String(), "@"+a.Digest) {
		t.Fatal(output.String())
	}
	if err := c.PublishCharts(context.Background(), root, &output); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("changed: true\n"), 0644)
	if err := c.PublishCharts(context.Background(), root, &output); err == nil {
		t.Fatal("conflicting chart version accepted")
	}
}
func TestSigningWorkflows(t *testing.T) {
	for _, name := range []string{"images", "charts"} {
		b, err := os.ReadFile("../../.github/workflows/" + name + ".yml")
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"secrets.COSIGN_PRIVATE_KEY", "secrets.COSIGN_PASSWORD", `if [ -n "$COSIGN_PRIVATE_KEY" ]`, "--key env://COSIGN_PRIVATE_KEY", "--tlog-upload=false"} {
			if !strings.Contains(string(b), required) {
				t.Fatalf("%s lacks %s", name, required)
			}
		}
	}
}
