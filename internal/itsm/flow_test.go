package itsm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/change"
	"github.com/tym83/aep/internal/changeapi"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/ocapi/octest"
	"github.com/tym83/aep/internal/policy"
	"golang.org/x/oauth2"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type flowVerifier struct{}

func (flowVerifier) Verify(_ context.Context, token string) (*authn.Identity, error) {
	switch token {
	case "developer":
		return &authn.Identity{Username: "alice", Subject: "directory-alice", Groups: []string{"aep-acme-developer"}}, nil
	case "approver":
		return &authn.Identity{Username: "bob", Subject: "directory-bob", Groups: []string{"aep-acme-approver"}}, nil
	case "adapter":
		return &authn.Identity{Username: "service-account-itsm", Subject: "adapter", Groups: []string{"aep-acme-developer"}}, nil
	}
	return nil, fmt.Errorf("invalid token")
}

type flowFactory struct{ API *ocapi.Client }

func (f flowFactory) For(context.Context, string) (*ocapi.Client, error) { return f.API, nil }
func TestScenario3ServiceNow(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.ChangeRequest{}).Build()
	aep := httptest.NewServer(changeapi.New(cl, flowVerifier{}, "tenant-{org}"))
	defer aep.Close()
	oc := octest.New(t)
	reconciler := &change.Reconciler{Client: cl, API: flowFactory{&ocapi.Client{BaseURL: oc.URL, Token: func(context.Context) (string, error) { return "token", nil }}}, Policy: policy.Default(), InstanceID: "scenario3", OrgNamespaceTemplate: "tenant-{org}"}
	records := map[string]map[string]any{}
	closed := 0
	cmdb := false
	sn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/now/identifyreconcile":
			cmdb = true
			fmt.Fprint(w, `{"result":{"items":[{},{},{},{}]}}`)
		case r.Method == "GET":
			fmt.Fprint(w, `{"result":[]}`)
		case r.Method == "POST":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			id := fmt.Sprintf("sn%d", len(records)+1)
			records[id] = b
			fmt.Fprintf(w, `{"result":{"sys_id":%q}}`, id)
		case r.Method == "PATCH":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["state"] == "3" {
				closed++
			}
			fmt.Fprint(w, `{"result":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer sn.Close()
	hc := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "adapter"}))
	engine := &Engine{Store: Store{Client: cl, Namespace: "itsm-acme"}, Adapter: &Router{Organization: "acme", Vendor: &ServiceNow{HTTP: HTTP{BaseURL: sn.URL}}, AEP: HTTP{BaseURL: aep.URL, Client: hc}}}
	secret := []byte("01234567890123456789012345678901ab")
	receiver := &Receiver{Engine: engine, Secret: secret, Organization: "acme"}
	call := func(method, path, token, key, rv string, body any) (int, Change) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, aep.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("If-Match", rv)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v Change
		_ = json.NewDecoder(res.Body).Decode(&v)
		return res.StatusCode, v
	}
	step := func(name string) *api.ChangeRequest {
		t.Helper()
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-acme", Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		cr := &api.ChangeRequest{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "tenant-acme", Name: name}, cr); err != nil {
			t.Fatal(err)
		}
		return cr
	}
	for _, target := range []struct{ Kind, Name, Release string }{{"Component", "orders", "orders-v2"}, {"Resource", "orders-db", "db-v2"}} {
		status, v := call("POST", "/v1/changes", "developer", target.Name, "", map[string]any{"organization": "acme", "model": "normal", "operation": "promote", "target": map[string]string{"kind": target.Kind, "name": target.Name, "project": "shop", "environment": "prod"}, "desired": map[string]string{"releaseName": target.Release}, "justification": "Scenario 3"})
		if status != 201 {
			t.Fatalf("create: %d", status)
		}
		cr := v.Request
		cr.CreationTimestamp = metav1.Now()
		if err := cl.Update(ctx, cr); err != nil {
			t.Fatal(err)
		}
		cr = step(cr.Name)
		cr = step(cr.Name)
		ev := NewEvent("change.created", "change/"+v.ID, Change{ID: v.ID, Request: cr, ConfirmationURL: "https://portal.example/changes/" + v.ID})
		if err := engine.Submit(ctx, ev); err != nil {
			t.Fatal(err)
		}
		// CAB approval is evidence only; it cannot change AEP state.
		note := NewEvent("change.note", "ignored", Note{Organization: "acme", Change: cr.Name, Text: "CAB approved", ExternalID: "approval-" + target.Name, ApproverID: "directory-bob", Decision: "approve"})
		for range 2 {
			body, _ := json.Marshal(note)
			req := httptest.NewRequest("POST", "/v1/events", bytes.NewReader(body))
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			req.Header.Set("X-AEP-Timestamp", ts)
			req.Header.Set("X-AEP-Signature", Signature(secret, ts, body))
			w := httptest.NewRecorder()
			receiver.ServeHTTP(w, req)
			if w.Code != 204 {
				t.Fatalf("webhook: %d %s", w.Code, w.Body)
			}
		}
		cr = step(cr.Name)
		if cr.Status.Phase != api.PhasePending || len(cr.Status.Approvals) != 0 || len(cr.Status.Notes) != 1 {
			t.Fatalf("evidence bypassed human confirmation: %+v", cr.Status)
		}
		path := "/v1/changes/" + v.ID + "/decision"
		for _, token := range []string{"developer", "adapter"} {
			status, _ = call("POST", path, token, "", cr.ResourceVersion, map[string]string{"decision": "approve"})
			if status != 403 {
				t.Fatalf("%s can approve: %d", token, status)
			}
		}
		status, _ = call("POST", path, "approver", "", "stale", map[string]string{"decision": "approve"})
		if status != 412 {
			t.Fatal("stale decision accepted")
		}
		status, _ = call("POST", path, "approver", "", cr.ResourceVersion, map[string]string{"decision": "approve", "evidence": "approval-" + target.Name})
		if status != 200 {
			t.Fatalf("human decision: %d", status)
		}
		for range 8 {
			cr = step(cr.Name)
			if cr.Status.Phase.Terminal() {
				break
			}
		}
		if cr.Status.Phase != api.PhaseApplied {
			t.Fatalf("not applied: %+v", cr.Status)
		}
		kind := "ReleaseBinding"
		field := "releaseName"
		if target.Kind == "Resource" {
			kind = "ResourceReleaseBinding"
			field = "resourceRelease"
		}
		if binding := oc.Binding("acme", kind, target.Name+"-prod"); binding[field] != target.Release {
			t.Fatalf("binding missing: %v", binding)
		}
		if err := engine.Submit(ctx, NewEvent("change.updated", "change/"+v.ID, Change{ID: v.ID, Request: cr})); err != nil {
			t.Fatal(err)
		}
		// A standard rollback to this applied release requires no CAB decision.
		status, rollback := call("POST", "/v1/changes", "developer", "rollback-"+target.Name, "", map[string]any{"organization": "acme", "model": "standard", "operation": "rollback", "target": map[string]string{"kind": target.Kind, "name": target.Name, "project": "shop", "environment": "prod"}, "desired": map[string]string{"releaseName": target.Release}, "justification": "Known approved release"})
		if status != 201 {
			t.Fatal(status)
		}
		rc := rollback.Request
		rc.CreationTimestamp = metav1.Now()
		if err := cl.Update(ctx, rc); err != nil {
			t.Fatal(err)
		}
		for range 8 {
			rc = step(rc.Name)
			if rc.Status.Phase.Terminal() {
				break
			}
		}
		if rc.Status.Phase != api.PhaseApplied || len(rc.Status.Approvals) != 1 || rc.Status.Approvals[0].Source != "system" {
			t.Fatalf("standard rollback: %+v", rc.Status)
		}
		if err := engine.Submit(ctx, NewEvent("change.updated", "change/"+rollback.ID, Change{ID: rollback.ID, Request: rc})); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Submit(ctx, NewEvent("cmdb.sync", "cmdb/acme", sampleGraph())); err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || closed != 4 || !cmdb {
		t.Fatalf("records=%d closed=%d cmdb=%v", len(records), closed, cmdb)
	}
}
