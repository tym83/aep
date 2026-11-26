package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func TestFleetProfileDiscoveryPrefersServedVersion(t *testing.T) {
	for _, versions := range [][]string{{"multicluster.x-k8s.io/v1alpha1"}, {"multicluster.x-k8s.io/v1alpha2", "multicluster.x-k8s.io/v1alpha1"}} {
		k := kubefake.NewClientset()
		for _, v := range versions {
			k.Resources = append(k.Resources, &metav1.APIResourceList{GroupVersion: v, APIResources: []metav1.APIResource{{Name: "clusterprofiles"}}})
		}
		f := &FleetReconciler{Hub: &Cluster{Kube: k}}
		v, err := f.profileVersion()
		if err != nil || v != versions[0] {
			t.Fatal(v, err)
		}
	}
}
func TestSuspensionNeverReacceptsHub(t *testing.T) {
	ctx := context.Background()
	o := organization()
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "tenant-acme", UID: "one"}, Spec: api.ExternalClusterSpec{Organization: o.Name, Suspended: true}}
	mc := obj("cluster.open-cluster-management.io/v1", "ManagedCluster", "", FleetID(ec), M{"hubAcceptsClient": true})
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(o, ec).Build()
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), mc)
	f := &FleetReconciler{Reconciler: &Reconciler{Client: c}, Hub: &Cluster{Dynamic: d, Kube: kubefake.NewClientset()}}
	// Missing CP credentials may block CA revocation, but must not preserve hub acceptance.
	_ = f.reconcileFleet(ctx, ec)
	live, err := f.Hub.get(ctx, mc.GetAPIVersion(), "managedclusters", "", FleetID(ec))
	if err != nil {
		t.Fatal(err)
	}
	accepted, _, _ := unstructured.NestedBool(live.Object, "spec", "hubAcceptsClient")
	if accepted {
		t.Fatal("suspended assignment accepted")
	}
	if meta.IsStatusConditionTrue(ec.Status.Conditions, "PoliciesReady") {
		t.Fatal("suspended assignment ready")
	}
}
