package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestFleetFeedbackIsRequired(t *testing.T) {
	policy := obj("kyverno.io/v1", "ClusterPolicy", "", "baseline", M{})
	dep := obj("apps/v1", "Deployment", "kyverno", "admission", M{})
	work := obj("work.open-cluster-management.io/v1", "ManifestWork", "x", "y", M{})
	cfg := fleetFeedback([]any{policy.Object, dep.Object})
	if len(cfg) != 2 || fleetFeedbackReady(work, cfg) {
		t.Fatal("missing feedback accepted")
	}
	statuses := []any{}
	for _, raw := range cfg {
		id := raw.(M)["resourceIdentifier"].(M)
		values := []any{M{"name": "Ready", "fieldValue": M{"string": "True"}}}
		if id["resource"] == "deployments" {
			values = nil
			for _, name := range []string{"Generation", "ObservedGeneration", "DesiredReplicas", "AvailableReplicas", "ReadyReplicas"} {
				values = append(values, M{"name": name, "fieldValue": M{"integer": int64(2)}})
			}
		}
		statuses = append(statuses, M{"resourceMeta": id, "statusFeedback": M{"values": values}})
	}
	_ = unstructured.SetNestedSlice(work.Object, statuses, "status", "resourceStatus", "manifests")
	if !fleetFeedbackReady(work, cfg) {
		t.Fatal("healthy feedback rejected")
	}
	statuses[0].(M)["statusFeedback"] = M{"values": []any{M{"name": "Ready", "fieldValue": M{"string": "False"}}}}
	_ = unstructured.SetNestedSlice(work.Object, statuses, "status", "resourceStatus", "manifests")
	if fleetFeedbackReady(work, cfg) {
		t.Fatal("unready policy accepted")
	}
}
func TestFleetDeletesStaleBatchesAsOrphans(t *testing.T) {
	ctx := context.Background()
	o := organization()
	stale := obj("work.open-cluster-management.io/v1", "ManifestWork", "ext", "aep-test-001", M{})
	stale.SetLabels(map[string]string{OwnerLabel: string(o.UID), "aep.aenix.io/work-batch": "aep-test"})
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr(stale.GetAPIVersion(), "manifestworks"): "ManifestWorkList"}, stale)
	f := &FleetReconciler{Hub: &Cluster{Dynamic: d}}
	_, err := f.work(ctx, o, "ext", "aep-test", []any{obj("v1", "ConfigMap", "default", "one", nil).Object})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Hub.get(ctx, stale.GetAPIVersion(), "manifestworks", "ext", stale.GetName()); !errors.IsNotFound(err) {
		t.Fatal("stale batch retained", err)
	}
	patched := false
	for _, a := range d.Actions() {
		if a.GetVerb() == "patch" {
			patched = true
		}
		if a.GetVerb() == "delete" && !patched {
			t.Fatal("deleted before setting Orphan")
		}
	}
}
func TestManagedClusterLabelsNeverAuthorizeAssignment(t *testing.T) {
	ctx := context.Background()
	o := organization()
	mc := obj("cluster.open-cluster-management.io/v1", "ManagedCluster", "", "unique", M{"hubAcceptsClient": false})
	mc.SetLabels(map[string]string{OwnerLabel: "forged", "aep.aenix.io/trust": "regulated"})
	f := &FleetReconciler{Hub: &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), mc)}}
	want := mc.DeepCopy()
	want.Object["spec"] = M{"hubAcceptsClient": true}
	want.SetLabels(map[string]string{"aep.aenix.io/trust": "untrusted"})
	if err := f.applyManagedCluster(ctx, o, want); err != nil {
		t.Fatal(err)
	}
	live, err := f.Hub.get(ctx, mc.GetAPIVersion(), "managedclusters", "", mc.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if live.GetLabels()[OwnerLabel] != string(o.UID) || live.GetLabels()["aep.aenix.io/trust"] != "untrusted" {
		t.Fatal("spoke labels trusted")
	}
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "x", UID: "y"}}
	if err = f.revokeHub(ctx, ec); err != nil {
		t.Fatal(err)
	}
}
