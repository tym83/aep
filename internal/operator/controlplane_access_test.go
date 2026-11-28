package operator

import (
	"slices"
	"testing"
)

func grants(rules []any, resource, verb string) bool {
	for _, r := range rules {
		m := r.(M)
		if slices.Contains(m["resources"].([]any), any(resource)) && slices.Contains(m["verbs"].([]any), any(verb)) {
			return true
		}
	}
	return false
}

// The shared cluster wait lists nodes and kube-system Secrets through the
// control plane identity, before any other step runs.
func TestControlPlaneContractCoversClusterWait(t *testing.T) {
	if !grants(controlPlaneOperatorRules(), "nodes", "list") {
		t.Fatal("control plane identity cannot list nodes")
	}
	if !grants(controlPlaneTokenRules(), "secrets", "list") {
		t.Fatal("control plane identity cannot list kube-system Secrets")
	}
}
