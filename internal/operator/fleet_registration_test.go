package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestFleetRegistrationRequiresAppliedApproval(t *testing.T) {
	ctx := context.Background()
	o := organization()
	o.Spec.Environments[0].DataPlane.External = "eks"
	cr := &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: "join", Namespace: "tenant-acme"}, Spec: api.ChangeRequestSpec{Organization: "acme", Operation: api.OperationClusterAttach, Model: api.ModelNormal, Requester: "requester", Target: api.ChangeTarget{Kind: "Cluster", Name: "eks", Environment: "dev"}, Desired: &ext.JSON{Raw: []byte(`{"tier":"nonprod","trustLevel":"standard","provider":"aws","distro":"eks","gatewayHost":"edge.example.org"}`)}}, Status: api.ChangeRequestStatus{Phase: api.PhaseApplied, Approvals: []api.ChangeApproval{{User: "platform", Groups: []string{change.GroupPlatformTeam}, Decision: change.DecisionApprove, Source: "portal"}}}}
	spec, e := change.ExternalSpec(cr)
	if e != nil {
		t.Fatal(e)
	}
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "eks", Namespace: "tenant-acme", UID: "external-uid"}, Spec: spec}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(o, cr, ec).Build()
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	k := kubefake.NewClientset()
	k.Resources = []*metav1.APIResourceList{{GroupVersion: "multicluster.x-k8s.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "clusterprofiles"}}}}
	hub := &Cluster{Dynamic: d, Kube: k}
	f := &FleetReconciler{Reconciler: &Reconciler{Client: c}, Hub: hub}
	bad := ec.DeepCopy()
	bad.Spec.TrustLevel = "regulated"
	if e = f.reconcileFleet(ctx, bad); e == nil {
		t.Fatal("tampered assignment accepted")
	}
	for _, a := range d.Actions() {
		if a.GetVerb() != "get" {
			t.Fatal("unapproved assignment mutated hub")
		}
	}
	if e = f.reconcileFleet(ctx, ec); e != nil {
		t.Fatal(e)
	}
	for _, item := range []struct{ av, r, ns, name string }{
		{"cluster.open-cluster-management.io/v1", "managedclusters", "", FleetID(ec)},
		{"cluster.open-cluster-management.io/v1beta2", "managedclustersets", "", "aep-acme"},
		{"cluster.open-cluster-management.io/v1beta2", "managedclustersetbindings", "aep-acme", "aep-acme"},
		{"multicluster.x-k8s.io/v1alpha1", "clusterprofiles", "aep-acme", "eks"},
	} {
		if _, e = hub.get(ctx, item.av, item.r, item.ns, item.name); e != nil {
			t.Fatal(e)
		}
	}

	newer := ec.DeepCopy()
	newer.Name = "newer"
	newer.ResourceVersion = ""
	newer.UID = "newer-uid"
	newer.Spec.ChangeRequestRef = "newer-change"
	newer.CreationTimestamp = metav1.NewTime(time.Now())
	if err := c.Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	d.ClearActions()
	if err := f.reconcileFleet(ctx, newer); err != nil {
		t.Fatal(err)
	}
	pending := meta.FindStatusCondition(newer.Status.Conditions, "Registered")
	if pending == nil || pending.Reason != "Pending" || len(d.Actions()) != 0 {
		t.Fatal("new assignment did not wait", pending, d.Actions())
	}
	// The established assignment ignores an unapproved competitor.
	if err := c.Delete(ctx, newer); err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Actions() {
		if a.GetResource().Resource == "manifestworks" {
			t.Fatal("work delivered before join")
		}
	}
}
