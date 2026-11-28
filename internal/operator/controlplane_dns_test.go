package operator

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCoreDNSHostsAddon(t *testing.T) {
	got := CoreDNSHostsAddon("cluster-gateway.cp.aep.internal", "10.96.57.50")
	// Rendered by the Cozystack coredns chart as a server block before the
	// default one, indented like the rest of its Corefile.
	want := M{"valuesOverride": M{"coredns": M{"extraConfig": M{"cluster-gateway.cp.aep.internal:53": M{
		"parameters": "{\n      hosts {\n        10.96.57.50 cluster-gateway.cp.aep.internal\n        ttl 30\n      }\n    }"}}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

// Tier clusters of an Organization on a ControlPlane with a fixed gateway
// host resolve that host to the gateway mirror; other tier clusters keep the
// Cozystack CoreDNS configuration.
func TestTierClustersResolveFixedGatewayHost(t *testing.T) {
	f := newCPFixture(t)
	f.seedGateway()
	for _, fixed := range []bool{true, false} {
		cp := servingControlPlane("default")
		cp.Status.Gateway.MirrorService = cpNS + "/kubernetes-aepcp-cluster-gateway-external"
		if fixed {
			cp.Spec.ClusterGateway.Host = cp.Status.Gateway.Host
		}
		mgmt, mgmtDyn, _ := fakeCluster()
		mgmt.Kube = f.mgmtKube
		s := &session{r: &Reconciler{Management: mgmt}, o: organization(), resolved: &controlPlaneTarget{cp: cp, namespace: cpNS, cluster: "aepcp"}, tiers: map[string]*Cluster{}}
		if err := s.clusters(f.ctx); err == nil || !strings.Contains(err.Error(), "TenantControlPlane") {
			t.Fatal(err)
		}
		k, err := mgmtDyn.Resource(gvr(appsAPI, "kuberneteses")).Namespace("tenant-acme-nonprod").Get(f.ctx, "apps", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		dns, has := k.Object["spec"].(map[string]any)["addons"].(map[string]any)["coredns"]
		if has != fixed {
			t.Fatalf("fixed=%v coredns addon %v", fixed, dns)
		}
		if fixed && !reflect.DeepEqual(dns, CoreDNSHostsAddon("cluster-gateway.default.aep.internal", "10.96.57.50")) {
			t.Fatalf("coredns addon %#v", dns)
		}
	}
	cp := servingControlPlane("default")
	cp.Spec.ClusterGateway.Host = cp.Status.Gateway.Host
	cp.Status.Gateway.MirrorService = ""
	s := &session{r: &Reconciler{Management: f.mgmt}, o: organization(), resolved: &controlPlaneTarget{cp: cp}}
	if _, err := s.gatewayDNS(f.ctx); err == nil {
		t.Fatal("tier cluster DNS without a published gateway mirror")
	}
}
