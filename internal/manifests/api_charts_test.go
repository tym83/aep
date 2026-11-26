package manifests

import (
	"os/exec"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func renderAPIChart(t *testing.T, chart, fixture string, extra ...string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
	args := []string{"template", "test", "../../charts/" + chart, "--namespace", "management", "--include-crds", "-f", "../../charts/ci/" + fixture}
	args = append(args, extra...)
	raw, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm: %v: %s", err, raw)
	}
	return decode(t, raw)
}

// Check the security boundary, including nondefault namespace/Secret templates.
func TestChangeChartRBAC(t *testing.T) {
	docs := renderAPIChart(t, "aep-change", "change.yaml", "-f", "../../charts/ci/change-tls.yaml")
	roles, clusterRoles, crds := 0, 0, 0
	for _, doc := range docs {
		switch doc["kind"] {
		case "CustomResourceDefinition":
			if doc["metadata"].(map[string]any)["name"] != "changerequests.aep.aenix.io" {
				t.Fatal("unexpected bundled CRD")
			}
			crds++
		case "Role", "ClusterRole":
			var role rbacv1.Role
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(doc, &role); err != nil {
				t.Fatal(err)
			}
			if doc["kind"] == "ClusterRole" {
				clusterRoles++
				if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].APIGroups, []string{"aep.aenix.io"}) ||
					!slices.Equal(role.Rules[0].Resources, []string{"changerequests", "organizations"}) ||
					!slices.Equal(role.Rules[0].Verbs, []string{"get", "list", "watch"}) {
					t.Fatal("cluster permissions must only read ChangeRequests and Organizations")
				}
			} else {
				roles++
				if !slices.Contains([]string{"org-acme", "org-example", "change-leases"}, role.Namespace) {
					t.Fatalf("unexpected RBAC namespace %q", role.Namespace)
				}
			}
			for _, rule := range role.Rules {
				for _, items := range [][]string{rule.APIGroups, rule.Resources, rule.Verbs, rule.ResourceNames} {
					if slices.Contains(items, "*") {
						t.Fatal("wildcard privilege")
					}
				}
				if slices.Contains(rule.Resources, "secrets") {
					want := "credentials-" + role.Namespace[len("org-"):]
					if !slices.Equal(rule.ResourceNames, []string{want}) || !slices.Equal(rule.Verbs, []string{"get"}) {
						t.Fatal("credentials must be restricted to an exact Secret and get only")
					}
				}
				if role.Name == "management-test-acme-api" || role.Name == "management-test-example-api" {
					if !slices.Equal(rule.APIGroups, []string{"aep.aenix.io"}) {
						t.Fatal("API must not have credentials or lease permissions")
					}
				}
			}
		}
	}
	if roles != 5 || clusterRoles != 1 || crds != 1 {
		t.Fatalf("incomplete RBAC/CRD: roles=%d clusterRoles=%d crds=%d", roles, clusterRoles, crds)
	}
}

func TestAPIChartIngress(t *testing.T) {
	for chart, fixture := range map[string]string{"aep-change": "change.yaml", "aep-read-api": "read.yaml"} {
		t.Run(chart, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				var extra []string
				if enabled {
					extra = []string{"-f", "../../charts/ci/ingress.yaml"}
				}
				docs := renderAPIChart(t, chart, fixture, extra...)
				services := map[string]bool{}
				var ingress map[string]any
				for _, doc := range docs {
					if doc["kind"] == "Service" {
						services[doc["metadata"].(map[string]any)["name"].(string)] = true
					}
					if doc["kind"] == "Ingress" {
						ingress = doc
					}
				}
				if (ingress != nil) != enabled {
					t.Fatal("ingress enabled setting ignored")
				}
				if !enabled {
					continue
				}
				spec := ingress["spec"].(map[string]any)
				rule := spec["rules"].([]any)[0].(map[string]any)
				path := rule["http"].(map[string]any)["paths"].([]any)[0].(map[string]any)
				backend := path["backend"].(map[string]any)["service"].(map[string]any)
				if !services[backend["name"].(string)] {
					t.Fatal("ingress does not target a rendered Service")
				}
				tls := spec["tls"].([]any)[0].(map[string]any)
				annotations := ingress["metadata"].(map[string]any)["annotations"].(map[string]any)
				if spec["ingressClassName"] != "nginx" || rule["host"] != "api.example.org" ||
					tls["secretName"] != "api-tls" || tls["hosts"].([]any)[0] != rule["host"] ||
					annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
					t.Fatal("ingress routing or certificate settings lost")
				}
			}
		})
	}
}
