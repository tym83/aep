package manifests

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func decode(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	var docs []map[string]any
	for {
		var d map[string]any
		e := decoder.Decode(&d)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if len(d) == 0 {
			continue
		}
		if d["apiVersion"] == nil || d["kind"] == nil {
			t.Fatal("missing GVK")
		}
		docs = append(docs, d)
	}
	return docs
}
func TestYAML(t *testing.T) {
	for _, dir := range []string{"config/crd", "config/app-cluster", "config/mgmt", "resource-types"} {
		paths, e := filepath.Glob(filepath.Join("../..", dir, "*.yaml"))
		if e != nil {
			t.Fatal(e)
		}
		for _, p := range paths {
			if strings.HasSuffix(p, "policy.yaml") {
				continue
			}
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			t.Run(p, func(t *testing.T) { decode(t, b) })
		}
	}
}
func TestCharts(t *testing.T) {
	if _, e := exec.LookPath("helm"); e != nil {
		t.Skip("helm unavailable")
	}
	for _, chart := range []string{"aep-bridge", "aep-bridge-appcluster"} {
		args := []string{"template", "bridge", filepath.Join("../../charts", chart), "--namespace", "tenant-test-prod"}
		if chart == "aep-bridge" {
			args = append(args, "--set", "organization=test,clusterID=test,tier=prod,prodAdmissionDraft=true")
		} else {
			args = append(args, "--set", "bridgeNamespace=custom-ns,bridgeServiceAccount=custom-sa")
		}
		b, e := exec.Command("helm", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s", e, b)
		}
		docs := decode(t, b)
		if len(docs) < 5 {
			t.Fatal("missing manifests")
		}
		for _, d := range docs {
			if chart == "aep-bridge" && d["kind"] == "ClusterRoleBinding" {
				t.Fatal("management privileges escaped namespace")
			}
		}
		if chart == "aep-bridge-appcluster" && !strings.Contains(string(b), "system:serviceaccount:custom-ns:custom-sa") {
			t.Fatal("admission identity not configured")
		}
	}
}
