package operator

import (
	"encoding/json"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
)

// TestLegacyControlPlaneGolden pins what an Organization without a
// ControlPlane does through the legacy --aepcp-* flags: the network policies,
// the gateway URL, the tier clusters and the hardening result. The golden
// file was produced by the operator before ControlPlane resolution existed;
// set AEP_UPDATE_GOLDEN=1 to rewrite it only for an intended change.
func TestLegacyControlPlaneGolden(t *testing.T) {
	f := newCPFixture(t)
	f.seedGateway()
	opts := Options{Namespace: "aep-system", CPSecret: "aep-system/aepcp-admin", CPNamespace: cpNS, CPCluster: "aepcp",
		GatewayHostTemplate: "cluster-gateway.{{.DashedIP}}.nip.io", TenantEgressWorkaround: true, PruneBootstrapBindings: true,
		BootstrapBindingSystemClients: DefaultBootstrapBindingSystemClients}
	s := &session{r: &Reconciler{Management: f.mgmt, Options: opts}, o: organization(), cp: f.remote, tiers: map[string]*Cluster{}}
	result := map[string]any{}
	errText := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	result["network"] = errText(s.network(f.ctx))
	result["gateway"] = s.gateway
	result["workaround"] = errText(s.workaround(f.ctx))
	result["clusters"] = errText(s.clusters(f.ctx))
	s.hardenControlPlane(f.ctx)
	c := meta.FindStatusCondition(s.o.Status.Conditions, "ControlPlaneHardened")
	result["hardened"] = []any{string(c.Status), c.Reason, c.Message}
	objects := map[string]any{}
	for _, r := range []struct{ av, resource, ns string }{
		{"cilium.io/v2", "ciliumclusterwidenetworkpolicies", ""},
		{appsAPI, "kuberneteses", "tenant-acme-nonprod"},
		{appsAPI, "kubernetesnodeses", "tenant-acme-nonprod"},
	} {
		items, err := f.mgmt.list(f.ctx, r.av, r.resource, r.ns)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range items {
			objects[r.resource+"/"+u.GetName()] = u.Object
		}
	}
	result["objects"] = objects
	got, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/legacy-control-plane.golden.json"
	if os.Getenv("AEP_UPDATE_GOLDEN") == "1" {
		if err = os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got)+"\n" {
		t.Fatalf("legacy behaviour changed:\n%s", got)
	}
}
