package changeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/ocapi/octest"
	"github.com/tym83/aep/internal/policy"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type staticFactory struct{ c *ocapi.Client }

func (f staticFactory) For(context.Context, string) (*ocapi.Client, error) { return f.c, nil }

// TestRequestToAppliedEndToEnd drives the real HTTP API and the real
// reconciler against one store: request, validate, decide with If-Match,
// apply, and the binding lands in OpenChoreo.
func TestRequestToAppliedEndToEnd(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.ChangeRequest{}).Build()
	srv, _ := testServer(t)
	srv.Client = cl
	oc := octest.New(t)
	reconciler := &change.Reconciler{
		Client: cl,
		API: staticFactory{&ocapi.Client{BaseURL: oc.URL, Token: func(context.Context) (string, error) {
			return "t", nil
		}}},
		Policy:               policy.Default(),
		InstanceID:           "e2e",
		OrgNamespaceTemplate: "tenant-{org}",
	}
	step := func(name string) *api.ChangeRequest {
		t.Helper()
		if _, e := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-acme", Name: name}}); e != nil {
			t.Fatal(e)
		}
		cr := &api.ChangeRequest{}
		if e := cl.Get(context.Background(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, cr); e != nil {
			t.Fatal(e)
		}
		return cr
	}

	rec := createChange(t, srv, "alice", "e2e-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out changeView
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	name := out.Request.Name
	// The fake client does not default creationTimestamp; a real apiserver
	// always sets it, and expiry is computed from it.
	created := &api.ChangeRequest{}
	if e := cl.Get(context.Background(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, created); e != nil {
		t.Fatal(e)
	}
	created.CreationTimestamp = metav1.Now()
	if e := cl.Update(context.Background(), created); e != nil {
		t.Fatal(e)
	}

	if cr := step(name); cr.Status.Phase != api.PhasePending {
		t.Fatalf("expected Pending, got %s", cr.Status.Phase)
	}
	step(name) // capture the before state

	rec = decide(t, srv, "bob", name, step(name).ResourceVersion, "approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	var applied *api.ChangeRequest
	for i := 0; i < 5; i++ {
		applied = step(name)
		if applied.Status.Phase.Terminal() {
			break
		}
	}
	if applied.Status.Phase != api.PhaseApplied {
		t.Fatalf("expected Applied, got %s (%+v)", applied.Status.Phase, applied.Status.Conditions)
	}
	binding := oc.Binding("acme", "ReleaseBinding", "api-prod")
	if binding["releaseName"] != "api-1.0.0" {
		t.Fatalf("expected the promoted release, got %v", binding)
	}
	if len(applied.Status.Approvals) != 1 || applied.Status.Approvals[0].User != "bob" {
		t.Fatalf("expected bob's recorded approval, got %+v", applied.Status.Approvals)
	}
}
