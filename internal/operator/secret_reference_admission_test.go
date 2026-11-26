package operator

import (
	"encoding/json"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runSecretReferenceAdmission(t *testing.T, policy *unstructured.Unstructured) {
	t.Helper()
	cli, e := exec.LookPath("kyverno")
	if e != nil {
		t.Skip("Kyverno CLI required")
	}
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	b, _ := json.Marshal(policy)
	if e = os.WriteFile(policyPath, b, 0600); e != nil {
		t.Fatal(e)
	}
	crd := filepath.Join(dir, "crd.yaml")
	b, _ = json.Marshal(obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "secretreferences.openchoreo.dev", M{"group": "openchoreo.dev", "scope": "Namespaced", "names": M{"kind": "SecretReference", "plural": "secretreferences", "singular": "secretreference"}, "versions": []any{M{"name": "v1alpha1", "served": true, "storage": true, "schema": M{"openAPIV3Schema": M{"type": "object", "x-kubernetes-preserve-unknown-fields": true}}}}}))
	if e = os.WriteFile(crd, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"ordinary", "vcs-acme-build", "vcs-another-build"} {
		t.Run(key, func(t *testing.T) {
			resource := filepath.Join(dir, "resource.yaml")
			b, _ := json.Marshal(obj(ocAPI, "SecretReference", "acme", "user-reference", M{"data": []any{M{"secretKey": "password", "remoteRef": M{"key": key, "property": "password"}}}}))
			if e = os.WriteFile(resource, b, 0600); e != nil {
				t.Fatal(e)
			}
			info := filepath.Join(dir, "userinfo.yaml")
			if e = os.WriteFile(info, []byte(`{"apiVersion":"cli.kyverno.io/v1alpha1","kind":"UserInfo","metadata":{"name":"user"},"userInfo":{"username":"developer"}}`), 0600); e != nil {
				t.Fatal(e)
			}
			output, err := exec.Command(cli, "apply", policyPath, "--resource", resource, "--crd-paths", crd, "--userinfo", info, "--set", "request.operation=CREATE").CombinedOutput()
			good := key == "ordinary"
			if good && (err != nil || !strings.Contains(string(output), "pass: 1")) {
				t.Fatalf("expected acceptance: %v %s", err, output)
			}
			if !good && (err == nil || !strings.Contains(string(output), "fail: 1")) {
				t.Fatalf("expected denial: %v %s", err, output)
			}
		})
	}
}
