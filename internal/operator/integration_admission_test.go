package operator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArgoRepositoryAdmission(t *testing.T) {
	cli, e := exec.LookPath("kyverno")
	if e != nil {
		t.Skip("Kyverno CLI required")
	}
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.yaml")
	crd := filepath.Join(dir, "crd.yaml")
	crdObject := obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "applications.argoproj.io", M{"group": "argoproj.io", "scope": "Namespaced", "names": M{"kind": "Application", "plural": "applications", "singular": "application"}, "versions": []any{M{"name": "v1alpha1", "served": true, "storage": true, "schema": M{"openAPIV3Schema": M{"type": "object", "x-kubernetes-preserve-unknown-fields": true}}}}})
	data, _ := json.Marshal(crdObject)
	if e = os.WriteFile(crd, data, 0600); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(argoApplicationPolicy())
	if e = os.WriteFile(policy, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, url := range []string{"https://git.example/acme/repo", "https://git.example/acme/../other", "https://git.example/acme/%2e%2e/other", "https://git.example/acme/%2E%2E/other", "https://user@git.example/acme/repo", "git@git.example:acme/repo"} {
		for _, multi := range []bool{false, true} {
			t.Run(url+map[bool]string{true: "/multi"}[multi], func(t *testing.T) {
				spec := M{"source": M{"repoURL": url}}
				if multi {
					spec = M{"sources": []any{M{"repoURL": "https://git.example/acme/safe"}, M{"repoURL": url}}}
				}
				resource := filepath.Join(dir, "resource.json")
				b, _ := json.Marshal(obj("argoproj.io/v1alpha1", "Application", "argocd", "test", spec))
				if e = os.WriteFile(resource, b, 0600); e != nil {
					t.Fatal(e)
				}
				output, err := exec.Command(cli, "apply", policy, "--resource", resource, "--set", "request.operation=CREATE", "--detailed-results", "--crd-paths", crd).CombinedOutput()
				good := url == "https://git.example/acme/repo"
				if good && (err != nil || !strings.Contains(string(output), "pass: 1")) {
					t.Fatalf("expected acceptance: %v %s", err, output)
				}
				if !good && (err == nil || !strings.Contains(string(output), "fail: 1")) {
					t.Fatalf("expected denial: %v %s", err, output)
				}
			})
		}
	}
}
