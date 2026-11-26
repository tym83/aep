//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/operator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransportPreservesIdentityAndRefusesRedirects(t *testing.T) {
	destinationCalled := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalled = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("If-Match") != "42" {
			t.Error("request identity or precondition lost")
		}
		var v map[string]any
		if json.NewDecoder(r.Body).Decode(&v) != nil || v["decision"] != "approve" {
			t.Error("body lost")
		}
		http.Redirect(w, r, destination.URL, 302)
	}))
	defer origin.Close()
	code, _, err := exchange(context.Background(), origin.URL, "/decision", "POST", "fixture", map[string]any{"decision": "approve"}, map[string]string{"If-Match": "42"})
	if err != nil || code != 302 || destinationCalled {
		t.Fatalf("redirect boundary failed: %d %v", code, err)
	}
}
func TestResponseLimitAndRedactedTransportErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", (4<<20)+2))) }))
	if _, _, err := exchange(context.Background(), server.URL, "/", "GET", "", nil, nil); err == nil {
		t.Fatal("oversize response accepted")
	}
	server.Close()
	_, _, err := exchange(context.Background(), server.URL, "/probe", "GET", "secret", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "/probe") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("transport details leaked")
	}
}
func TestPortalUsesSeparateCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/aep/v1/me" || r.Header.Get("Authorization") != "Bearer backstage" || r.Header.Get("x-openchoreo-token") != "oidc" {
			t.Error("portal identity propagation failed")
		}
		_, _ = w.Write([]byte(`{"organizations":[]}`))
	}))
	defer server.Close()
	s := &suite{t: t, c: config{Endpoints: map[string]string{"portal": server.URL}}, tokens: map[string]string{"0/developer/false": "oidc", "0/developer/true": "backstage"}}
	s.require(0, "developer", "portal", "GET", "/v1/me", nil, nil, 200)
}
func TestReadinessRequiresCurrentGeneration(t *testing.T) {
	v := map[string]any{"metadata": map[string]any{"generation": float64(2)}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": float64(1)}}}}
	if ready(v) {
		t.Fatal("stale Ready accepted")
	}
	field(v, "status.conditions").([]any)[0].(map[string]any)["observedGeneration"] = float64(2)
	if !ready(v) {
		t.Fatal("current Ready rejected")
	}
}
func TestMatrixContract(t *testing.T) {
	m, err := readMatrix()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Roles) != 9 || len(m.Actions) < 30 {
		t.Fatal("incomplete matrix")
	}
	for name, a := range m.Actions {
		if a.Endpoint == "" || a.Path == "" || a.Method == "" || len(a.Success) == 0 {
			t.Errorf("invalid action %s", name)
		}
		for _, role := range a.Allow {
			if !contains(m.Roles, role) {
				t.Errorf("unknown role %s", role)
			}
		}
	}
	if contains(m.Actions["read-services"].Allow, "finance") || len(m.Actions["prod-logs"].Allow) != 0 {
		t.Fatal("resolved expectations regressed")
	}
}
func TestConfigRejectsMissingCleanupAndUnknownFields(t *testing.T) {
	c, err := loadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.Cleanup.TokenCommand = nil
	c.Cleanup.AepcpKubeconfig = ""
	b, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfig(path); err == nil {
		t.Fatal("unsafe cleanup config accepted")
	}
	if err = os.WriteFile(path, []byte(`{"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfig(path); err == nil {
		t.Fatal("unknown config accepted")
	}
}
func TestChangeLifecycleAgainstFake(t *testing.T) {
	phase := "Pending"
	version := "1"
	reads := 0
	approved := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/changes" {
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("missing idempotency")
			}
			w.WriteHeader(201)
		} else if r.Method == "POST" {
			if r.Header.Get("If-Match") != version {
				t.Error("missing fresh version")
			}
			if r.Header.Get("Authorization") == "Bearer developer" {
				w.WriteHeader(403)
				return
			}
			phase = "Applied"
			approved = true
		} else {
			reads++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"request": map[string]any{"metadata": map[string]any{"name": "change", "resourceVersion": version}, "status": map[string]any{"phase": phase}}})
	}))
	defer server.Close()
	s := &suite{t: t, run: "local", c: config{Endpoints: map[string]string{"change": server.URL}, Organizations: []organization{{Name: "a"}}, TimeoutSeconds: 1}, fixtures: []fixture{{project: "p"}}, tokens: map[string]string{"0/developer/false": "developer", "0/approver/false": "approver"}}
	id := s.newChange(0, "developer", "change", "decommission", "db-local-1", "")
	s.phase(0, "developer", "change", id, "Pending")
	s.decide(0, "developer", "change", id, "approve", 403)
	s.decide(0, "approver", "change", id, "approve", 200)
	s.phase(0, "developer", "change", id, "Applied")
	if !approved || reads < 3 {
		t.Fatal("approval flow did not re-read version and verify outcome")
	}
}

func TestCleanupUsesDedicatedIdentityAndExactRunName(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		if r.Method != "DELETE" || r.URL.Path != "/api/v1/namespaces/a/resources/db-local-1" || r.Header.Get("Authorization") != "Bearer cleanup" {
			t.Error("cleanup scope or identity incorrect")
		}
		deleted = true
		w.WriteHeader(204)
	}))
	defer server.Close()
	t.Run("run", func(t *testing.T) {
		s := &suite{t: t, run: "local", c: config{Endpoints: map[string]string{"openchoreo": server.URL}, Organizations: []organization{{Name: "a"}}}, tokens: map[string]string{"0/developer/false": "developer"}}
		s.c.Cleanup.TokenCommand = []string{"printf", "%s", "cleanup"}
		s.cleanupObject(0, "resources", "db-local-1")
	})
	if !deleted {
		t.Fatal("cleanup did not execute")
	}
}
func TestTemplateExpansionUsesConfiguredLogFixtures(t *testing.T) {
	s := &suite{t: t, run: "local", c: config{Organizations: []organization{{Name: "a", LogsProject: "running", LogsComponent: "service"}}}, fixtures: []fixture{{project: "new", component: "new-app"}}}
	s.c.Resources.LogsPath = "/namespaces/{org}/projects/{project}/components/{component}?env={environment}"
	s.c.Resources.ProdEnvironment = "prod"
	if got := s.expand("{logsPath}", 0, "x"); got != "/namespaces/a/projects/running/components/service?env=prod" {
		t.Fatalf("incorrect log fixture: %v", got)
	}
}

func TestFilteredListDenial(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		deny bool
	}{
		{`{"items":[]}`, true}, {`{"items":[{"metadata":{"namespace":"other"}}]}`, true},
		{`{"items":[{"metadata":{"namespace":"target"}}]}`, false}, {`{"items":[{}]}`, false},
		{`{}`, false}, {`{"items":null}`, false}, {`{"items":"bad"}`, false},
	} {
		if got := emptyTargetList(tc.raw, "target"); got != tc.deny {
			t.Errorf("%s: %v", tc.raw, got)
		}
	}
}

func TestSecretFixtureValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"data":[]}`, `{"template":{},"data":[{}]}`, `{"template":{},"data":[{"secretKey":"p","remoteRef":{"key":""}}]}`} {
		var spec map[string]any
		_ = json.Unmarshal([]byte(raw), &spec)
		if validateSecretSpec(spec) == nil {
			t.Errorf("invalid fixture accepted: %s", raw)
		}
	}
}

func TestTransportRetriesWithFreshCredentials(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Error("cached token used")
		}
		if calls == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	s := &suite{t: t, c: config{Endpoints: map[string]string{"change": server.URL}, TokenCommand: []string{"sh", "-c", "printf fresh"}, Organizations: []organization{{Users: map[string]string{"developer": "dev"}}}}, tokens: map[string]string{"0/developer/false": "expired"}}
	s.require(0, "developer", "change", "POST", "/probe", map[string]any{}, nil, 200)
	if calls != 2 {
		t.Fatalf("requests: %d", calls)
	}
}

func TestDelegatedBindingUsesRenderedRestrictions(t *testing.T) {
	org := &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: api.OrganizationSpec{Environments: []api.Environment{{Name: "dev", Tier: "nonprod"}, {Name: "stage", Tier: "nonprod"}, {Name: "prod", Tier: "prod"}}}}
	for _, obj := range operator.RenderAuthz(org) {
		if obj.GetKind() != "AuthzRoleBinding" || obj.GetName() != "aep-developer" {
			continue
		}
		spec, err := delegatedBinding(obj.Object, "custom", "local")
		if err != nil {
			t.Fatal(err)
		}
		mappings := spec["roleMappings"].([]any)
		mapping := mappings[0].(map[string]any)
		if str(mapping, "roleRef.name") != "custom" || str(mapping, "roleRef.kind") != "AuthzRole" {
			t.Fatal("delegated role changed")
		}
		b, _ := json.Marshal(mapping["conditions"])
		if !strings.Contains(string(b), `acme/stage`) || strings.Contains(string(b), `acme/prod`) {
			t.Fatalf("wrong restriction: %s", b)
		}
		return
	}
	t.Fatal("operator binding missing")
}
