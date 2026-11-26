package itsm

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestInventoryRelationshipsRetirementAndFailedList(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	org := &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "org"}, Spec: api.OrganizationSpec{Tiers: map[string]api.TierSpec{"prod": {}, "nonprod": {}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(org).Build()
	object := func(kind, name, ns, uid string, labels map[string]string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps.cozystack.io/v1alpha1", "kind": kind}}
		o.SetName(name)
		o.SetNamespace(ns)
		o.SetUID(types.UID(uid))
		o.SetLabels(labels)
		return o
	}
	cluster := object("Kubernetes", "apps", "tenant-acme-prod", "cluster", map[string]string{"aep.aenix.io/organization": "org"})
	db := object("Postgres", "orders-db", "tenant-acme-prod", "db", map[string]string{"aep.aenix.io/managed-by": "bridge"})
	gvr := func(r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: r}
	}
	apps := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{gvr("kuberneteses"): "KubernetesList", gvr("postgreses"): "PostgresList", gvr("redises"): "RedisList"})
	if _, err := apps.Resource(gvr("kuberneteses")).Namespace(cluster.GetNamespace()).Create(ctx, cluster, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Resource(gvr("postgreses")).Namespace(db.GetNamespace()).Create(ctx, db, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Store: Store{Client: cl, Namespace: "itsm", Scope: "acme"}, Adapter: deliveryFunc(func(context.Context, Event, string) (string, error) { return "", nil })}
	p := &Poller{Client: cl, Apps: apps, Engine: engine, Organization: "acme", Services: []ApplicationService{{UID: "app", Name: "orders", Environment: "prod", Tier: "prod", Databases: []string{"tenant-acme-prod/Postgres/orders-db"}, BusinessApplication: "existing123"}}}
	g, err := p.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Items) != 5 || len(g.Relationships) != 6 {
		t.Fatalf("unexpected graph: %+v", g)
	}
	if err := engine.Submit(ctx, NewEvent("cmdb.sync", "cmdb/acme", g)); err != nil {
		t.Fatal(err)
	}
	if err := apps.Resource(gvr("postgreses")).Namespace(db.GetNamespace()).Delete(ctx, db.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Inventory(ctx); err == nil {
		t.Fatal("missing dependency must prevent partial graph sync")
	}
	p.Services = nil
	g, err = p.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retired := map[string]bool{}
	for _, ci := range g.Items {
		if ci.Retired {
			retired[ci.UID] = true
		}
		if ci.Kind == "business-application" {
			t.Fatal("customer business application must not be retired")
		}
	}
	if !retired["db"] || !retired["app"] {
		t.Fatal(retired)
	}
	if err := engine.Submit(ctx, NewEvent("cmdb.sync", "cmdb/acme", g)); err != nil {
		t.Fatal(err)
	}
	next, err := p.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range next.Items {
		if ci.Retired {
			t.Fatal("confirmed retirement retained")
		}
	}

}
func TestDurableOrderingAndScope(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	store.Scope = "acme"
	now := time.Now()
	calls := 0
	fail := true
	adapter := deliveryFunc(func(context.Context, Event, string) (string, error) {
		calls++
		if fail {
			return "", errors.New("offline")
		}
		return "remote1", nil
	})
	engine := &Engine{Store: store, Adapter: adapter, Now: func() time.Time { return now }}
	first := changeEvent(api.PhasePending)
	second := changeEvent(api.PhaseApplied)
	if engine.Submit(ctx, first) == nil {
		t.Fatal("expected failure")
	}
	if engine.Submit(ctx, second) == nil {
		t.Fatal("new state overtook pending state")
	}
	other := store
	other.Scope = "other"
	if err := (&Engine{Store: other, Adapter: adapter}).Retry(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("other organization retried this delivery")
	}
	fail = false
	now = now.Add(3 * time.Second)
	engine = &Engine{Store: store, Adapter: adapter, Now: func() time.Time { return now }}
	if err := engine.Retry(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Submit(ctx, second); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
	corrupt := second
	corrupt.Data = first.Data
	if engine.Submit(ctx, corrupt) == nil {
		t.Fatal("event ID payload conflict accepted")
	}
}
