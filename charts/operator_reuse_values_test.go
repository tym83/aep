package charts

import (
	"os/exec"
	"strings"
	"testing"
)

// helm upgrade --reuse-values keeps values from older releases, which have no
// controlPlane map; the chart must still render.
func TestOperatorRendersWithoutControlPlaneValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm required")
	}
	out, err := exec.Command("helm", "template", "operator", "./aep-operator", "--set", "aepcpKubeconfigSecret=cp/access", "--set", "controlPlane=null").CombinedOutput()
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "--default-control-plane") {
		t.Fatal("default control plane flag rendered without a value")
	}
}
