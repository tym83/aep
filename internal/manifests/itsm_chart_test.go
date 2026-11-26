package manifests

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestITSMChartSecurityBoundary(t *testing.T) {
	docs := renderAPIChart(t, "aep-itsm-bridge", "itsm.yaml", "--set", "tlsSecret=bridge-tls", "--set", "caBundleConfigMap=corporate-ca")
	deployments := 0
	for _, doc := range docs {
		switch doc["kind"] {
		case "Deployment":
			deployments++
			var d appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(doc, &d); err != nil {
				t.Fatal(err)
			}
			if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
				t.Fatal("delivery worker must be singleton")
			}
			pod := d.Spec.Template.Spec
			sc := pod.SecurityContext
			if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Fatal("pod must meet restricted security")
			}
			c := pod.Containers[0]
			security := c.SecurityContext
			if !strings.Contains(c.Image, "@sha256:") || security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || !(*security.ReadOnlyRootFilesystem) || !slices.Contains(security.Capabilities.Drop, corev1.Capability("ALL")) {
				t.Fatal("container must be pinned, read-only and unprivileged")
			}
			if !slices.Contains(c.Args, "--tls-cert=/tls/tls.crt") {
				t.Fatal("TLS flag missing")
			}
		case "Role", "ClusterRole":
			var role rbacv1.Role
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(doc, &role); err != nil {
				t.Fatal(err)
			}
			for _, rule := range role.Rules {
				if slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Resources, "*") {
					t.Fatal("unexpected credential or wildcard access")
				}
				for _, verb := range []string{"create", "update", "patch", "delete"} {
					if slices.Contains(rule.Verbs, verb) && !slices.Equal(rule.Resources, []string{"configmaps"}) {
						t.Fatal("bridge may only write its cursor ConfigMaps")
					}
				}
				if doc["kind"] == "ClusterRole" && (!slices.Equal(rule.Resources, []string{"organizations"}) || !slices.Equal(rule.ResourceNames, []string{"acme"})) {
					t.Fatal("cluster read must be scoped to the organization")
				}
			}
		}
	}
	if deployments != 1 {
		t.Fatal("missing bridge deployment")
	}
}
