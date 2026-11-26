//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestRestrictedProbeOverrides(t *testing.T) {
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(probeOverrides("probe")), &pod); err != nil {
		t.Fatal(err)
	}
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65534 || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("unsafe pod security context")
	}
	c := pod.Spec.Containers[0]
	if c.Name != "probe" || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("unsafe container security context")
	}
}

func TestIsolationDiagnostics(t *testing.T) {
	for _, secret := range []string{"Bearer private-token", "--token private-token", "Authorization: Bearer private-token", "password: \"private-token with spaces\"", "--token=private-token", "password: private-token", "eyJhbGci.eyJzdWI.signature", "https://user:private-token@example.org"} {
		got := diagnosticLine("policy denied " + secret + "\nsecond-line-secret")
		if strings.Contains(got, "private-token") || strings.Contains(got, "eyJ") || strings.Contains(got, "second-line") || !strings.Contains(got, "policy denied") {
			t.Fatalf("unsafe diagnostic %q", got)
		}
	}
	_, err := isolationCommand([]string{"sh", "-c", "printf 'policy denied Bearer private-token\n' >&2; exit 1"})
	if err == nil || !strings.Contains(err.Error(), "policy denied") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("diagnostic %v", err)
	}
}
