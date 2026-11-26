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

func TestOperatorChart(t *testing.T) {
	if _, e := exec.LookPath("helm"); e != nil {
		t.Skip("helm unavailable")
	}
	b, e := exec.Command("helm", "template", "operator", "../../charts/aep-operator", "--namespace", "aep-system", "--include-crds", "--set", "aepcpKubeconfigSecret=tenant-aep-cp/kubernetes-aepcp-admin-kubeconfig").CombinedOutput()
	if e != nil {
		t.Fatalf("%v: %s", e, b)
	}
	docs := decode(t, b)
	if len(docs) != 6 {
		t.Fatalf("expected two CRDs, SA, RBAC and Deployment, got %d", len(docs))
	}
	for _, d := range docs {
		if d["kind"] != "ClusterRole" {
			continue
		}
		for _, r := range d["rules"].([]any) {
			rule := r.(map[string]any)
			for _, k := range []string{"apiGroups", "resources", "verbs"} {
				for _, v := range rule[k].([]any) {
					if v == "*" {
						t.Fatalf("wildcard operator privilege in %s", k)
					}
				}
			}
		}
	}
	source, e := os.ReadFile("../../config/crd/aep.aenix.io_organizations.yaml")
	if e != nil {
		t.Fatal(e)
	}
	packaged, e := os.ReadFile("../../charts/aep-operator/crds/aep.aenix.io_organizations.yaml")
	if e != nil {
		t.Fatal(e)
	}
	if string(source) != string(packaged) {
		t.Fatal("operator chart CRD drift")
	}
}

func TestCostLedgerChartReadOnly(t *testing.T) {
	if _, e := exec.LookPath("helm"); e != nil {
		t.Skip("helm unavailable")
	}
	b, e := exec.Command("helm", "template", "cost", "../../charts/aep-cost-ledger", "--namespace", "aep-system", "--set", "credentialsSecret=cost").CombinedOutput()
	if e != nil {
		t.Fatalf("%v: %s", e, b)
	}
	docs := decode(t, b)
	if len(docs) != 4 {
		t.Fatalf("expected deployment and RBAC: %d", len(docs))
	}
	for _, d := range docs {
		if d["kind"] != "ClusterRole" {
			continue
		}
		for _, r := range d["rules"].([]any) {
			rule := r.(map[string]any)
			for _, v := range rule["verbs"].([]any) {
				if v != "get" && v != "list" && v != "watch" {
					t.Fatalf("ledger write permission: %v", v)
				}
			}
		}
	}
}
