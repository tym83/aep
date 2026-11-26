package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestFleetRegistrationRequiresAppliedApproval(t *testing.T) {
	ctx := context.Background()
	o := organization()
	cr := &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: "join", Namespace: "tenant-acme"}, Spec: api.ChangeRequestSpec{Organization: "acme", Operation: api.OperationClusterAttach, Model: api.ModelNormal, Requester: "requester", Target: api.ChangeTarget{Kind: "Cluster", Name: "eks", Environment: "dev"}, Desired: &ext.JSON{Raw: []byte(`{"tier":"nonprod","trustLevel":"standard","provider":"aws","distro":"eks"}`)}}, Status: api.ChangeRequestStatus{Phase: api.PhaseApplied, Approvals: []api.ChangeApproval{{User: "platform", Groups: []string{change.GroupPlatformTeam}, Decision: change.DecisionApprove, Source: "portal"}}}}
	spec, e := change.ExternalSpec(cr)
	if e != nil {
		t.Fatal(e)
	}
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "eks", Namespace: "tenant-acme", UID: "external-uid"}, Spec: spec}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(o, cr, ec).Build()
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	hub := &Cluster{Dynamic: d, Kube: kubefake.NewClientset()}
	f := &FleetReconciler{Reconciler: &Reconciler{Client: c}, Hub: hub}
	bad := ec.DeepCopy()
	bad.Spec.TrustLevel = "regulated"
	if e = f.reconcileFleet(ctx, bad); e == nil {
		t.Fatal("tampered assignment accepted")
	}
	if len(d.Actions()) != 0 {
		t.Fatal("unapproved assignment touched hub")
	}
	if e = f.reconcileFleet(ctx, ec); e != nil {
		t.Fatal(e)
	}
	for _, item := range []struct{ av, r, ns, name string }{
		{"cluster.open-cluster-management.io/v1", "managedclusters", "", FleetID(ec)},
		{"cluster.open-cluster-management.io/v1beta2", "managedclustersets", "", "aep-acme"},
		{"cluster.open-cluster-management.io/v1beta2", "managedclustersetbindings", "aep-acme", "aep-acme"},
		{"cluster.open-cluster-management.io/v1beta1", "placements", "aep-acme", FleetID(ec)},
		{"multicluster.x-k8s.io/v1alpha1", "clusterprofiles", "aep-acme", "eks"},
	} {
		if _, e = hub.get(ctx, item.av, item.r, item.ns, item.name); e != nil {
			t.Fatal(e)
		}
	}
	for _, a := range d.Actions() {
		if a.GetResource().Resource == "manifestworks" {
			t.Fatal("work delivered before join")
		}
	}
}
