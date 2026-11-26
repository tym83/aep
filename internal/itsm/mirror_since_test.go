package itsm

import (
	"context"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Enabling the bridge must not mirror an organization's history into the ITSM.
func TestPollerSkipsChangesBeforeMirrorSince(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cutoff := time.Date(2026, 11, 26, 22, 0, 0, 0, time.UTC)
	change := func(name string, created time.Time) *api.ChangeRequest {
		return &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme", CreationTimestamp: metav1.NewTime(created)}, Spec: api.ChangeRequestSpec{Organization: "acme"}}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(change("old", cutoff.Add(-time.Hour)), change("new", cutoff.Add(time.Minute))).Build()
	var delivered []string
	engine := &Engine{Store: Store{Client: cl, Namespace: "itsm", Scope: "acme"}, Adapter: deliveryFunc(func(_ context.Context, e Event, _ string) (string, error) {
		delivered = append(delivered, e.Subject)
		return "", nil
	})}
	p := &Poller{Client: cl, Engine: engine, Organization: "acme", Namespace: "tenant-acme", MirrorSince: cutoff}
	if err := p.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || delivered[0] != "change/acme/new" {
		t.Fatalf("delivered %v", delivered)
	}
}
