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
	if len(docs) != 8 {
		t.Fatalf("expected four CRDs, SA, RBAC and Deployment, got %d", len(docs))
	}
	for _, want := range []string{"controlplanes/status", "controlplanes/finalizers", "ingresses", "controlplanes.aep.aenix.io"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("operator chart lacks %s", want)
		}
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
	packagedCRDs, e := filepath.Glob("../../charts/aep-operator/crds/*.yaml")
	if e != nil || len(packagedCRDs) != 4 {
		t.Fatalf("operator chart CRDs %v: %v", packagedCRDs, e)
	}
	for _, path := range packagedCRDs {
		source, e := os.ReadFile("../../config/crd/" + filepath.Base(path))
		if e != nil {
			t.Fatal(e)
		}
		packaged, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if string(source) != string(packaged) {
			t.Fatalf("operator chart CRD drift: %s", filepath.Base(path))
		}
	}
}

// The legacy aep-cp kubeconfig is required unless a default ControlPlane is set.
func TestOperatorChartControlPlaneSources(t *testing.T) {
	if _, e := exec.LookPath("helm"); e != nil {
		t.Skip("helm unavailable")
	}
	if b, e := exec.Command("helm", "template", "operator", "../../charts/aep-operator").CombinedOutput(); e == nil || !strings.Contains(string(b), "aepcpKubeconfigSecret required unless controlPlane.default is set") {
		t.Fatalf("missing source accepted: %v: %s", e, b)
	}
	for _, tc := range []struct {
		set       []string
		want, not []string
	}{
		{[]string{"aepcpKubeconfigSecret=cp/access"}, []string{`"--aepcp-kubeconfig-secret=cp/access"`}, []string{"--default-control-plane"}},
		{[]string{"controlPlane.default=default"}, []string{`"--default-control-plane=default"`}, []string{"--aepcp-kubeconfig-secret"}},
		{[]string{"controlPlane.default=default", "aepcpKubeconfigSecret=cp/access"}, []string{`"--default-control-plane=default"`, `"--aepcp-kubeconfig-secret=cp/access"`}, nil},
	} {
		args := []string{"template", "operator", "../../charts/aep-operator", "--show-only", "templates/deployment.yaml"}
		for _, s := range tc.set {
			args = append(args, "--set", s)
		}
		b, e := exec.Command("helm", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s", e, b)
		}
		for _, w := range tc.want {
			if !strings.Contains(string(b), w) {
				t.Errorf("%v: missing %s", tc.set, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(string(b), n) {
				t.Errorf("%v: unexpected %s", tc.set, n)
			}
		}
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

func TestPlatformTrustAcceptsTaggedDigests(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	ref := "registry.example/platform:v1@" + digest
	cmd := exec.Command("helm", "template", "test", "../../policies/app-cluster-prod", "--set", "cosignPublicKey=test", "--set", "platformImageTrust.mode=enforce", "--set", "platformImageTrust.allowedImages[0]="+ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), `regex_replace_all(':[^/:]+@', element.image, '@')`) || !strings.Contains(string(out), "registry.example/platform@"+digest) {
		t.Fatal("tagged digest is not normalized in trust policy")
	}
	cmd = exec.Command("helm", "template", "test", "../../policies/app-cluster-prod", "--set", "cosignPublicKey=test", "--set", "platformImageTrust.allowedImages[0]=registry.example/platform:v1")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("tag-only trust accepted: %s", out)
	}
}
