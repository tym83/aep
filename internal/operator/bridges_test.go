package operator

import (
	"reflect"
	"testing"
)

func TestBridgeArgsDeletionPolicy(t *testing.T) {
	s := &session{o: organization()}
	for _, tc := range []struct{ tier, policy string }{{"nonprod", "Delete"}, {"prod", "Retain"}} {
		t.Run(tc.tier, func(t *testing.T) {
			want := []string{"--organization=acme", "--tier=" + tc.tier,
				"--tenant-namespace=tenant-acme-" + tc.tier, "--cluster-id=org-uid-" + tc.tier,
				"--allowed-kinds=Postgres,Redis", "--app-kubeconfig=/credentials/kubeconfig",
				"--app-token-file=/credentials/token", "--config=/policy/policy.yaml",
				"--tier-deletion-policy=" + tc.policy}
			if got := s.bridgeArgs(tc.tier); !reflect.DeepEqual(got, want) {
				t.Fatalf("bridge args = %v, want %v", got, want)
			}
		})
	}
}
