package itsm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testStore(t *testing.T) Store {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return Store{Client: fake.NewClientBuilder().WithScheme(s).Build(), Namespace: "bridge"}
}
func changeEvent(phase api.ChangePhase) Event {
	return NewEvent("change.updated", "change/acme/test", Change{ID: "acme/test", Request: &api.ChangeRequest{Spec: api.ChangeRequestSpec{Organization: "acme", Model: api.ModelNormal, Operation: api.OperationPromote}, Status: api.ChangeRequestStatus{Phase: phase}}, ConfirmationURL: "https://portal.example/changes/acme/test"})
}
func TestServiceNowLostCreateResponseRecovery(t *testing.T) {
	ctx := context.Background()
	f := &fakeSN{lost: true}
	tokens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			tokens++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"token","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing OAuth")
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	vendor := &ServiceNow{HTTP: HTTP{BaseURL: srv.URL, Client: OAuthClient(ctx, srv.URL+"/oauth_token.do", "id", "secret")}}
	now := time.Now()
	engine := &Engine{Store: testStore(t), Adapter: vendor, Now: func() time.Time { return now }}
	e := changeEvent(api.PhasePending)
	if engine.Submit(ctx, e) == nil {
		t.Fatal("expected transient failure")
	}
	if err := engine.Submit(ctx, e); err != ErrBusy {
		t.Fatalf("expected backoff, got %v", err)
	}
	now = now.Add(3 * time.Second)
	engine = &Engine{Store: engine.Store, Adapter: vendor, Now: func() time.Time { return now }}
	if err := engine.Retry(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Submit(ctx, changeEvent(api.PhaseApplied)); err != nil {
		t.Fatal(err)
	}
	last := f.records["sn1"]
	if last["state"] != "3" || last["close_code"] != "successful" {
		t.Fatalf("bad closure: %v", last)
	}
	if err := engine.Submit(ctx, e); err != nil {
		t.Fatal(err)
	} // delayed duplicate must not reopen
	if len(f.records) != 1 || f.closed != 1 || tokens != 1 {
		t.Fatal("duplicate creation or closure")
	}
	if _, ok := last["approval"]; ok {
		t.Fatal("adapter must not approve ITSM on its own")
	}
}
func sampleGraph() Graph {
	return Graph{Organization: "acme", Epoch: "1", Items: []CI{{UID: "org", Kind: "organization", Name: "acme"}, {UID: "cluster", Kind: "cluster", Name: "prod"}, {UID: "app", Kind: "application-service", Name: "orders/prod"}, {UID: "db", Kind: "database", Name: "orders-db"}}, Relationships: []Relationship{{Parent: "org", Child: "app", Type: "Contains::Contained by"}, {Parent: "app", Child: "cluster", Type: "Runs on::Runs"}, {Parent: "app", Child: "db", Type: "Depends on::Used by"}}}
}
func TestServiceNowCMDBAndEvents(t *testing.T) {
	var graph map[string]any
	var severities []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/now/identifyreconcile":
			_ = json.NewDecoder(r.Body).Decode(&graph)
			if r.URL.Query().Get("sysparm_data_source") != "AEP" {
				t.Error("missing source")
			}
			fmt.Fprint(w, `{"result":{"items":[{},{},{},{}]}}`)
		case "/api/global/em/jsonv2":
			var body struct {
				Records []map[string]string `json:"records"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			severities = append(severities, body.Records[0]["severity"])
			if body.Records[0]["message_key"] != "db/availability" {
				t.Error("missing message key")
			}
			fmt.Fprint(w, `{"result":{}}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &ServiceNow{HTTP: HTTP{BaseURL: srv.URL}}
	for _, e := range []Event{NewEvent("cmdb.sync", "cmdb/acme", sampleGraph()), NewEvent("alert.raised", "alert/db", Alert{Key: "db/availability", Severity: "1"}), NewEvent("alert.resolved", "alert/db", Alert{Key: "db/availability"})} {
		if _, err := s.Deliver(context.Background(), e, ""); err != nil {
			t.Fatal(err)
		}
	}
	items := graph["items"].([]any)
	if items[1].(map[string]any)["className"] != "cmdb_ci_kubernetes_cluster" || items[3].(map[string]any)["className"] != "cmdb_ci_db_instance" || len(graph["relations"].([]any)) != 3 {
		t.Fatalf("bad graph: %v", graph)
	}
	if strings.Join(severities, ",") != "1,0" {
		t.Fatal(severities)
	}
	g := sampleGraph()
	g.Relationships[0].Child = "missing"
	if _, err := IREPayload(g); err == nil {
		t.Fatal("dangling relationship accepted")
	}
}
func TestJiraRequestTransitionsApprovals(t *testing.T) {
	created, transitioned := 0, 0
	issue := ""
	available := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/rest/api/3/search/jql":
			if issue == "" {
				fmt.Fprint(w, `{"issues":[]}`)
			} else {
				fmt.Fprint(w, `{"issues":[{"key":"OPS-1"}]}`)
			}
		case r.URL.Path == "/rest/servicedeskapi/request":
			created++
			issue = "OPS-1"
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["requestFieldValues"].(map[string]any)["labels"] == nil {
				t.Error("correlation must be atomic")
			}
			fmt.Fprint(w, `{"issueKey":"OPS-1"}`)
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			if r.Method == "POST" {
				transitioned++
				available = false
				w.WriteHeader(204)
			} else if available {
				fmt.Fprint(w, `{"transitions":[{"id":"31"}]}`)
			} else {
				fmt.Fprint(w, `{"transitions":[]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/approval"):
			if r.Method != "GET" {
				t.Error("bot must not answer approval")
			}
			fmt.Fprint(w, `{"values":[{"id":"1","finalDecision":"approved"}]}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/OPS-1":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	j := &Jira{HTTP: HTTP{BaseURL: srv.URL}, Project: "OPS", ServiceDesk: "1", RequestType: "2", Transitions: map[string]string{"Applied": "31"}}
	for range 2 {
		if _, err := j.Deliver(context.Background(), changeEvent(api.PhaseApplied), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Approvals(context.Background(), "OPS-1"); err != nil {
		t.Fatal(err)
	}
	if created != 1 || transitioned != 1 {
		t.Fatalf("created=%d transitioned=%d", created, transitioned)
	}
	if _, err := j.Deliver(context.Background(), NewEvent("cmdb.sync", "cmdb/acme", sampleGraph()), ""); err == nil {
		t.Fatal("unsupported Assets must fail explicitly")
	}
}

func TestJiraIncidentForwarding(t *testing.T) {
	created, updated, transitions := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/rest/api/3/search/jql":
			fmt.Fprint(w, `{"issues":[]}`)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue":
			created++
			fmt.Fprint(w, `{"key":"OPS-9"}`)
		case r.Method == "PUT":
			updated++
			w.WriteHeader(204)
		case r.Method == "GET":
			fmt.Fprint(w, `{"transitions":[{"id":"41"}]}`)
		case r.Method == "POST":
			transitions++
			w.WriteHeader(204)
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer srv.Close()
	j := &Jira{HTTP: HTTP{BaseURL: srv.URL}, Project: "OPS", Transitions: map[string]string{"alert.resolved": "41"}}
	ref, err := j.Deliver(context.Background(), NewEvent("alert.raised", "alert/db", Alert{Key: "db", Summary: "Database unavailable"}), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Deliver(context.Background(), NewEvent("alert.resolved", "alert/db", Alert{Key: "db", Summary: "Database recovered"}), ref); err != nil {
		t.Fatal(err)
	}
	if created != 1 || updated != 1 || transitions != 1 {
		t.Fatalf("created=%d updated=%d transitions=%d", created, updated, transitions)
	}
}

func TestIREPartialFailureDoesNotAdvanceCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":{"items":[{},{},{},{}],"relations":[{"errors":[{"error":"INVALID_RELATION"}]}]}}`)
	}))
	defer srv.Close()
	engine := &Engine{Store: testStore(t), Adapter: &ServiceNow{HTTP: HTTP{BaseURL: srv.URL}}}
	ctx := context.Background()
	if err := engine.Submit(ctx, NewEvent("cmdb.sync", "cmdb/acme", sampleGraph())); err == nil {
		t.Fatal("partial failure acknowledged")
	}
	_, record, err := engine.Store.get(ctx, "cmdb/acme")
	if err != nil {
		t.Fatal(err)
	}
	if record.Done || record.Attempts != 1 {
		t.Fatal(record)
	}
}
