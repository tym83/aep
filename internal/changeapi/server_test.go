package changeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/change"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeVerifier struct{ ids map[string]*authn.Identity }

func (f fakeVerifier) Verify(_ context.Context, token string) (*authn.Identity, error) {
	if id, ok := f.ids[token]; ok {
		return id, nil
	}
	return nil, fmt.Errorf("unknown token")
}

var (
	alice = &authn.Identity{Subject: "u1", Username: "alice", Groups: []string{"aep-acme-developer"}}
	carol = &authn.Identity{Subject: "u6", Username: "carol", Groups: []string{"aep-acme-platform"}}
	bob   = &authn.Identity{Subject: "u2", Username: "bob", Groups: []string{"/aep-acme-approver"}}
	paul  = &authn.Identity{Subject: "u3", Username: "paul", Groups: []string{"aep-platform-team"}}
	mall  = &authn.Identity{Subject: "u4", Username: "mallory", Groups: []string{"aep-other-developer"}}
	sa    = &authn.Identity{Subject: "u5", Username: "service-account-itsm", Groups: []string{"aep-acme-developer"}}
)

func testServer(t *testing.T) (*Server, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.ChangeRequest{}).Build()
	srv := New(cl, fakeVerifier{map[string]*authn.Identity{"alice": alice, "bob": bob, "paul": paul, "mallory": mall, "sa": sa, "carol": carol}}, "tenant-{org}")
	srv.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	return srv, cl
}

func call(t *testing.T, srv *Server, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func createBody() string {
	return `{
		"organization": "acme",
		"operation": "promote",
		"model": "normal",
		"target": {"kind": "Component", "project": "shop", "name": "api", "environment": "prod"},
		"desired": {"releaseName": "api-1.0.0"},
		"justification": "ship the fix",
		"requester": "mallory",
		"externalRef": "CHG0001234"
	}`
}

func createChange(t *testing.T, srv *Server, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, srv, "POST", "/v1/changes", token, createBody(), map[string]string{"Idempotency-Key": key})
}

func TestCreateTakesRequesterFromToken(t *testing.T) {
	srv, cl := testServer(t)
	rec := createChange(t, srv, "alice", "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d %s", rec.Code, rec.Body)
	}
	var out changeView
	if e := json.Unmarshal(rec.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	if out.ID == "" || !strings.HasPrefix(out.ID, "acme/chg-") {
		t.Fatalf("unexpected id %q", out.ID)
	}
	// The body claimed requester mallory; the token says alice and wins.
	if out.Request.Spec.Requester != "alice" {
		t.Fatalf("requester must come from the token, got %q", out.Request.Spec.Requester)
	}
	if len(out.Request.Spec.RequesterGroups) != 1 || out.Request.Spec.RequesterGroups[0] != "aep-acme-developer" {
		t.Fatalf("requester groups must come from the token, got %v", out.Request.Spec.RequesterGroups)
	}
	if out.Request.Namespace != "tenant-acme" || out.Request.Spec.ExternalRef != "CHG0001234" {
		t.Fatalf("unexpected object %s/%s", out.Request.Namespace, out.Request.Name)
	}
	stored := &api.ChangeRequest{}
	if e := cl.Get(context.Background(), client.ObjectKey{Namespace: "tenant-acme", Name: out.Request.Name}, stored); e != nil {
		t.Fatal(e)
	}
}

func TestCreateIdempotency(t *testing.T) {
	srv, cl := testServer(t)
	first := createChange(t, srv, "alice", "key-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d %s", first.Code, first.Body)
	}
	replay := createChange(t, srv, "alice", "key-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("a replay must return 200, got %d %s", replay.Code, replay.Body)
	}
	var a, b changeView
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(replay.Body.Bytes(), &b)
	if a.ID != b.ID {
		t.Fatalf("a replay must return the same change, got %q and %q", a.ID, b.ID)
	}
	var list api.ChangeRequestList
	if e := cl.List(context.Background(), &list); e != nil {
		t.Fatal(e)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly one change, got %d", len(list.Items))
	}
	conflict := call(t, srv, "POST", "/v1/changes", "alice", strings.Replace(createBody(), "api-1.0.0", "api-2.0.0", 1), map[string]string{"Idempotency-Key": "key-1"})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("a reused key with a different body must conflict, got %d", conflict.Code)
	}
	// Another requester with the same key gets their own change.
	other := createChange(t, srv, "carol", "key-1")
	if other.Code != http.StatusCreated {
		t.Fatalf("idempotency keys are scoped per requester, got %d %s", other.Code, other.Body)
	}
}

func TestCreateRejections(t *testing.T) {
	srv, _ := testServer(t)
	if rec := call(t, srv, "POST", "/v1/changes", "alice", createBody(), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key must fail, got %d", rec.Code)
	}
	if rec := createChange(t, srv, "sa", "k"); rec.Code != http.StatusForbidden {
		t.Fatalf("service accounts must not create changes, got %d", rec.Code)
	}
	if rec := createChange(t, srv, "mallory", "k"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-members must not create changes, got %d", rec.Code)
	}
	if rec := call(t, srv, "POST", "/v1/changes", "alice", strings.Replace(createBody(), `"promote"`, `"explode"`, 1), map[string]string{"Idempotency-Key": "k"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid specs must fail, got %d", rec.Code)
	}
	if rec := call(t, srv, "POST", "/v1/changes", "", createBody(), map[string]string{"Idempotency-Key": "k"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing tokens must fail, got %d", rec.Code)
	}
}

func mustCreate(t *testing.T, srv *Server) (string, string) {
	t.Helper()
	rec := createChange(t, srv, "alice", "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d %s", rec.Code, rec.Body)
	}
	var out changeView
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Request.Name, out.Request.ResourceVersion
}

func TestGetChange(t *testing.T) {
	srv, _ := testServer(t)
	name, _ := mustCreate(t, srv)
	if rec := call(t, srv, "GET", "/v1/changes/acme/"+name, "bob", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("org member must read, got %d", rec.Code)
	}
	if rec := call(t, srv, "GET", "/v1/changes/acme/"+name, "paul", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("platform team must read, got %d", rec.Code)
	}
	if rec := call(t, srv, "GET", "/v1/changes/acme/"+name, "mallory", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign org member must not read, got %d", rec.Code)
	}
	if rec := call(t, srv, "GET", "/v1/changes/acme/chg-missing", "bob", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing changes must 404, got %d", rec.Code)
	}
}

// pendingChange creates a change and moves it to Pending as the controller would.
func pendingChange(t *testing.T, srv *Server, cl client.Client) (string, string) {
	t.Helper()
	name, _ := mustCreate(t, srv)
	cr := &api.ChangeRequest{}
	if e := cl.Get(context.Background(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, cr); e != nil {
		t.Fatal(e)
	}
	cr.Status.Phase = api.PhasePending
	if e := cl.Status().Update(context.Background(), cr); e != nil {
		t.Fatal(e)
	}
	return name, cr.ResourceVersion
}

func decide(t *testing.T, srv *Server, token, name, rv, decision string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if rv != "" {
		headers["If-Match"] = rv
	}
	return call(t, srv, "POST", "/v1/changes/acme/"+name+"/decision", token, `{"decision":"`+decision+`","evidence":"CHG0001234"}`, headers)
}

func TestDecisionFlow(t *testing.T) {
	srv, cl := testServer(t)
	name, rv := pendingChange(t, srv, cl)
	if rec := decide(t, srv, "bob", name, "", "approve"); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match must return 428, got %d", rec.Code)
	}
	if rec := decide(t, srv, "bob", name, "1", "approve"); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("a stale If-Match must return 412, got %d", rec.Code)
	}
	if rec := decide(t, srv, "alice", name, rv, "approve"); rec.Code != http.StatusForbidden {
		t.Fatalf("the requester must not decide, got %d", rec.Code)
	}
	if rec := decide(t, srv, "sa", name, rv, "approve"); rec.Code != http.StatusForbidden {
		t.Fatalf("service accounts must not decide, got %d", rec.Code)
	}
	if rec := decide(t, srv, "paul", name, rv, "approve"); rec.Code != http.StatusForbidden {
		t.Fatalf("the platform team must not approve app changes, got %d", rec.Code)
	}
	if rec := decide(t, srv, "bob", name, rv, "maybe"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown decisions must fail, got %d", rec.Code)
	}
	rec := decide(t, srv, "bob", name, rv, "approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body)
	}
	var out changeView
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Request.Status.Approvals) != 1 {
		t.Fatalf("expected one approval, got %+v", out.Request.Status.Approvals)
	}
	a := out.Request.Status.Approvals[0]
	if a.User != "bob" || a.Role != change.RoleApprover || a.Decision != "approve" || a.Source != "portal" || a.Evidence != "CHG0001234" {
		t.Fatalf("unexpected approval %+v", a)
	}
	if len(a.Groups) != 1 || a.Groups[0] != "/aep-acme-approver" {
		t.Fatalf("the approver groups must be recorded from the token, got %v", a.Groups)
	}
	// The same user cannot decide twice.
	if rec = decide(t, srv, "bob", name, out.Request.ResourceVersion, "approve"); rec.Code != http.StatusConflict {
		t.Fatalf("a duplicate decision must conflict, got %d", rec.Code)
	}
}

func TestDecisionOnlyWhilePending(t *testing.T) {
	srv, cl := testServer(t)
	name, _ := pendingChange(t, srv, cl)
	cr := &api.ChangeRequest{}
	if e := cl.Get(context.Background(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, cr); e != nil {
		t.Fatal(e)
	}
	cr.Status.Phase = api.PhaseApplied
	if e := cl.Status().Update(context.Background(), cr); e != nil {
		t.Fatal(e)
	}
	if rec := decide(t, srv, "bob", name, cr.ResourceVersion, "approve"); rec.Code != http.StatusConflict {
		t.Fatalf("decisions on a terminal change must conflict, got %d", rec.Code)
	}
}

func TestIfMatchRaceReturns412(t *testing.T) {
	srv, cl := testServer(t)
	name, rv := pendingChange(t, srv, cl)
	// The controller touches the status between the client's read and write.
	cr := &api.ChangeRequest{}
	if e := cl.Get(context.Background(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, cr); e != nil {
		t.Fatal(e)
	}
	cr.Status.TargetKey = "abc"
	if e := cl.Status().Update(context.Background(), cr); e != nil {
		t.Fatal(e)
	}
	if rec := decide(t, srv, "bob", name, rv, "approve"); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("a concurrent modification must return 412, got %d", rec.Code)
	}
}

func TestNotes(t *testing.T) {
	srv, _ := testServer(t)
	name, _ := mustCreate(t, srv)
	rec := call(t, srv, "POST", "/v1/changes/acme/"+name+"/notes", "bob", `{"text":"looks good"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d %s", rec.Code, rec.Body)
	}
	var out changeView
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Request.Status.Notes) != 1 || out.Request.Status.Notes[0].Source != "portal" {
		t.Fatalf("unexpected notes %+v", out.Request.Status.Notes)
	}
	rec = call(t, srv, "POST", "/v1/changes/acme/"+name+"/notes", "sa", `{"text":"mirrored from ServiceNow"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("adapters may add notes, got %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Request.Status.Notes[1].Source != "itsm" {
		t.Fatalf("service account notes must be marked itsm, got %+v", out.Request.Status.Notes[1])
	}
	if rec = call(t, srv, "POST", "/v1/changes/acme/"+name+"/notes", "mallory", `{"text":"hi"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign users must not comment, got %d", rec.Code)
	}
	if rec = call(t, srv, "POST", "/v1/changes/acme/"+name+"/notes", "bob", `{"text":""}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty notes must fail, got %d", rec.Code)
	}
}

func TestForeignOrganizationDenialsDoNotDiscloseNames(t *testing.T) {
	srv, _ := testServer(t)
	name, _ := mustCreate(t, srv)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/changes/acme/" + name, ""},
		{"POST", "/v1/changes/acme/" + name + "/notes", `{"text":"foreign note"}`},
		{"POST", "/v1/changes/acme/" + name + "/decision", `{"decision":"approve"}`},
		{"POST", "/v1/changes", `{"organization":"acme","operation":"promote","model":"normal","target":{"kind":"Resource","name":"db","project":"p","environment":"prod"},"desired":{"releaseName":"r"},"justification":"isolation"}`},
	} {
		rec := call(t, srv, tc.method, tc.path, "mallory", tc.body, map[string]string{"Idempotency-Key": "isolation", "If-Match": "deliberately-stale"})
		if rec.Code != 403 || strings.Contains(rec.Body.String(), "acme") || strings.Contains(rec.Body.String(), name) {
			t.Fatalf("foreign denial disclosed target or returned %d", rec.Code)
		}
	}
}
