package operator

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestFleetCleanupWaitsBetweenDependencyLayers(t *testing.T) {
	ctx := context.Background()
	o := organization()
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "west", Namespace: "tenant-acme", UID: "assignment", Finalizers: []string{fleetFinalizer}}, Spec: api.ExternalClusterSpec{Organization: o.Name, ClusterProfileRef: api.ClusterProfileReference{Name: "west"}}}
	id := FleetID(ec)
	owned := func(u *unstructured.Unstructured) *unstructured.Unstructured {
		u.SetLabels(map[string]string{OwnerLabel: string(o.UID)})
		return u
	}
	work := owned(obj("work.open-cluster-management.io/v1", "ManifestWork", id, "aep-policies-000", M{"deleteOption": M{"propagationPolicy": "Foreground"}}))
	profile := owned(obj("multicluster.x-k8s.io/v1alpha1", "ClusterProfile", "aep-acme", "west", M{}))
	mc := obj("cluster.open-cluster-management.io/v1", "ManagedCluster", "", id, M{}) // deliberately untrusted labels
	hub := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr(work.GetAPIVersion(), "manifestworks"): "ManifestWorkList"}, work, profile, mc)
	dp := owned(obj(ocAPI, "DataPlane", o.Name, fleetDataPlaneName(ec), M{}))
	cp := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), dp)
	ca := owned(secretObject("aep-system", shortName("agent-ca-"+o.Name+"-"+id), nil))
	leaf := owned(secretObject("aep-system", "agent-leaf-"+id, nil))
	mgmt := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), ca, leaf)
	sequence := []string{}
	for _, d := range []*dynamicfake.FakeDynamicClient{hub, cp, mgmt} {
		d.PrependReactor("delete", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
			sequence = append(sequence, a.GetResource().Resource)
			return false, nil, nil
		})
	}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec, o).Build()
	f := &FleetReconciler{Reconciler: &Reconciler{Client: c, Management: &Cluster{Dynamic: mgmt}, Options: Options{Namespace: "aep-system"}}, Hub: &Cluster{Dynamic: hub, Kube: kubefake.NewClientset()}}
	s := &session{r: f.Reconciler, o: o, cp: &Cluster{Dynamic: cp}}
	finished := false
	for range 10 {
		res, err := f.cleanupFleetObjects(ctx, ec, s)
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter == 0 {
			finished = true
			break
		}
	}
	if !finished {
		t.Fatal("cleanup never completed")
	}
	expected := []string{"manifestworks", "dataplanes", "secrets", "secrets", "clusterprofiles", "managedclusters"}
	if len(sequence) != len(expected) {
		t.Fatal(sequence)
	}
	for i, want := range expected {
		if sequence[i] != want {
			t.Fatalf("cleanup order %v", sequence)
		}
	}
	patched := false
	for _, a := range hub.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "manifestworks" {
			p := a.(ktesting.PatchAction)
			patched = string(p.GetPatch()) == `{"spec":{"deleteOption":{"propagationPolicy":"Orphan"}}}`
		}
		if a.GetVerb() == "delete" && a.GetResource().Resource == "manifestworks" && !patched {
			t.Fatal("work deleted without Orphan")
		}
	}
}

func TestDetachReattachKeepsImmutableEnvironmentReference(t *testing.T) {
	ctx := context.Background()
	o := organization()
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "first"}, Spec: api.ExternalClusterSpec{Organization: o.Name, Environment: "dev"}}
	cp := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
	cp.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("patch", "environments", func(a ktesting.Action) (bool, runtime.Object, error) {
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(a.(ktesting.PatchAction).GetPatch(), &u.Object); err != nil {
			return true, nil, err
		}
		oldObject, err := cp.Dynamic.(*dynamicfake.FakeDynamicClient).Tracker().Get(a.GetResource(), a.GetNamespace(), u.GetName())
		if err != nil {
			return true, nil, err
		}
		old := oldObject.(*unstructured.Unstructured)
		before, _, _ := unstructured.NestedString(old.Object, "spec", "dataPlaneRef", "name")
		after, _, _ := unstructured.NestedString(u.Object, "spec", "dataPlaneRef", "name")
		if before != after {
			t.Fatal("immutable reference changed")
		}
		return true, u, cp.Dynamic.(*dynamicfake.FakeDynamicClient).Tracker().Update(a.GetResource(), u, a.GetNamespace())
	})

	first := fleetDataPlane(o, ec, []byte("first-ca"))
	if err := cp.apply(ctx, o, "dataplanes", first); err != nil {
		t.Fatal(err)
	}
	env := fleetEnvironment(o, ec)
	if err := cp.apply(ctx, o, "environments", env); err != nil {
		t.Fatal(err)
	}
	if err := cp.resource(ocAPI, "dataplanes", o.Name).Delete(ctx, first.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	ec.UID = "second"
	second := fleetDataPlane(o, ec, []byte("second-ca"))
	if second.GetName() != first.GetName() {
		t.Fatal("immutable reference changed")
	}
	if err := cp.apply(ctx, o, "dataplanes", second); err != nil {
		t.Fatal(err)
	}
	if err := cp.apply(ctx, o, "environments", fleetEnvironment(o, ec)); err != nil {
		t.Fatal(err)
	}
	live, err := cp.get(ctx, ocAPI, "dataplanes", o.Name, first.GetName())
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := unstructured.NestedString(live.Object, "spec", "planeID")
	if id != FleetID(ec) {
		t.Fatal("assignment identity not rotated")
	}
}
