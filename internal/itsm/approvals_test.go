package itsm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A CAB decision recorded in ServiceNow reaches AEP once as an evidence note;
// requested approvals and non-pending changes produce nothing.
func TestPollerForwardsCABDecisionsOnce(t *testing.T) {
	sn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":[{"sys_id":"appr1","approver":{"value":"cab1"},"state":"approved"},{"sys_id":"appr2","approver":"cab2","state":"requested"}]}`))
	}))
	defer sn.Close()
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	pending := &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: "chg-1", Namespace: "tenant-acme"}, Spec: api.ChangeRequestSpec{Organization: "acme"}, Status: api.ChangeRequestStatus{Phase: api.PhasePending}}
	applied := &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: "chg-2", Namespace: "tenant-acme"}, Spec: api.ChangeRequestSpec{Organization: "acme"}, Status: api.ChangeRequestStatus{Phase: api.PhaseApplied}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pending, applied).Build()
	vendor := deliveryFunc(func(context.Context, Event, string) (string, error) { return "chgref1", nil })
	engine := &Engine{Store: Store{Client: cl, Namespace: "itsm", Scope: "acme"}, Adapter: vendor}
	var notes []Note
	noteEngine := &Engine{Store: Store{Client: cl, Namespace: "itsm", Scope: "acme-notes"}, Adapter: deliveryFunc(func(_ context.Context, e Event, ref string) (string, error) {
		var n Note
		_ = json.Unmarshal(e.Data, &n)
		notes = append(notes, n)
		return ref, nil
	})}
	reader := &ServiceNow{HTTP: HTTP{BaseURL: sn.URL, Client: sn.Client()}}
	p := &Poller{Client: cl, Engine: engine, Organization: "acme", Namespace: "tenant-acme", Approvals: reader, Notes: noteEngine}
	for range 2 {
		if err := p.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(notes) != 1 || notes[0].Change != "chg-1" || notes[0].Decision != "approve" || notes[0].ApproverID != "cab1" || notes[0].ExternalID != "appr1" {
		t.Fatalf("notes %+v", notes)
	}
}
