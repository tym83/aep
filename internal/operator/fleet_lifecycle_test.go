package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"testing"
)

func TestFleetConditionsDoNotLoopOrResetRegistration(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "tenant-acme", UID: "first", Generation: 1, Finalizers: []string{fleetFinalizer}}, Spec: api.ExternalClusterSpec{Organization: "acme", ChangeRequestRef: "missing"}}
	fleetCondition(ec, "Registered", true, "Accepted", "Accepted")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ec).WithObjects(ec).Build()
	f := &FleetReconciler{Reconciler: &Reconciler{Client: c}, Hub: &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), Kube: kubefake.NewClientset()}}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ec)}
	_, _ = f.Reconcile(ctx, req)
	first := &api.ExternalCluster{}
	_ = c.Get(ctx, req.NamespacedName, first)
	_, _ = f.Reconcile(ctx, req)
	second := &api.ExternalCluster{}
	_ = c.Get(ctx, req.NamespacedName, second)
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatal("unchanged failure rewrote status")
	}
	if !meta.IsStatusConditionTrue(second.Status.Conditions, "Registered") {
		t.Fatal("later failure reset registration")
	}
	if meta.IsStatusConditionTrue(second.Status.Conditions, "Ready") {
		t.Fatal("failure reported ready")
	}
	if FleetPredicate().Update(event.UpdateEvent{ObjectOld: first, ObjectNew: second}) {
		t.Fatal("status event triggers reconcile")
	}
	deleting := second.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if !FleetPredicate().Update(event.UpdateEvent{ObjectOld: second, ObjectNew: deleting}) {
		t.Fatal("deletion ignored")
	}
}
func TestFleetIdentityAndNames(t *testing.T) {
	a := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "tenant-acme", UID: "one"}}
	b := a.DeepCopy()
	b.UID = "two"
	if FleetID(a) == FleetID(b) {
		t.Fatal("recreated assignment reuses identity")
	}
	name := shortName("agent-ca-abcdefghijklmnopqrstuvwxyzabcdef-" + FleetID(a))
	if len(name) > 63 {
		t.Fatal(name)
	}
	if name == shortName("agent-ca-abcdefghijklmnopqrstuvwxyzabcdeg-"+FleetID(a)) {
		t.Fatal("name collision")
	}
}
