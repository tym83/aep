package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tym83/aep/internal/authn"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

type verifier struct{ id *authn.Identity }

func (v verifier) Verify(context.Context, string) (*authn.Identity, error) { return v.id, nil }
func object(api, kind, ns, name string, fields map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns}}}
	for k, v := range fields {
		o.Object[k] = v
	}
	return o
}
func fixture(t *testing.T, extra ...runtime.Object) *Server {
	t.Helper()
	objects := []runtime.Object{object("aep.aenix.io/v1alpha1", "Organization", "", "acme", map[string]any{"spec": map[string]any{"tiers": map[string]any{"nonprod": map[string]any{}, "prod": map[string]any{}}}}), object("aep.aenix.io/v1alpha1", "Organization", "", "other", nil)}
	objects = append(objects, extra...)
	lists := map[schema.GroupVersionResource]string{organizations: "OrganizationList", events: "EventList", hr: "HelmReleaseList", quotas: "ResourceQuotaList", resource("backups.cozystack.io", "v1alpha1", "backups"): "BackupList"}
	for kind, r := range appResources {
		lists[resource("apps.cozystack.io", "v1alpha1", r)] = kind + "List"
	}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objects...)
	s, err := New(c, verifier{&authn.Identity{Groups: []string{"aep-acme-developer"}}}, Config{MaxRows: 5, MaxBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func scoped() context.Context {
	return context.WithValue(context.Background(), contextKey{}, scope{org: "acme", tiers: []string{"nonprod", "prod"}})
}
func TestScope(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct {
		id authn.Identity
		ok bool
	}{
		{authn.Identity{Groups: []string{"aep-acme-platform"}}, true}, {authn.Identity{ClientID: "aep-acme-sre-agent"}, true},
		{authn.Identity{Groups: []string{"aep-acme-platform", "aep-other-developer"}}, false}, {authn.Identity{ClientID: "aep-other-sre-agent", Groups: []string{"aep-acme-developer"}}, false}, {authn.Identity{ClientID: "aep-acme-sre-agent-evil"}, false}, {authn.Identity{Groups: []string{"aep-acme"}}, false},
	} {
		_, err := s.resolve(context.Background(), &tc.id)
		if (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc.id, err)
		}
	}
	c := s.Client.(*fake.FakeDynamicClient)
	c.ClearActions()
	if _, err := s.query(scoped(), "quota_and_denials", Args{Org: "other"}); err == nil {
		t.Fatal("cross-org accepted")
	}
	if len(c.Actions()) != 0 {
		t.Fatal("cross-org read occurred")
	}
	for _, service := range []string{"../Postgres/db", "other/Postgres/db", "prod/Secret/db"} {
		if _, err := s.query(scoped(), "managed_service_state", Args{Org: "acme", Service: service}); err == nil {
			t.Fatal("invalid service accepted")
		}
	}
}
func TestEvidenceOrderingRedactionLimits(t *testing.T) {
	now := time.Now().UTC()
	extra := []runtime.Object{}
	for i, msg := range []string{"quota exceeded", "password = 'a very long secret'", "token: abc\nextra", "API_KEY xyz", "healthy"} {
		extra = append(extra, object("v1", "Event", "tenant-acme-nonprod", string(rune('a'+i)), map[string]any{"lastTimestamp": now.Add(time.Duration(i-10) * time.Minute).Format(time.RFC3339), "message": msg, "reason": "FailedCreate"}))
	}
	extra = append(extra, object("v1", "Event", "tenant-other-prod", "foreign", map[string]any{"message": "foreign"}))
	s := fixture(t, extra...)
	out, err := s.query(scoped(), "infra_changes", Args{Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	for _, bad := range []string{"long secret", "abc", "xyz", "foreign"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("leaked %s", bad)
		}
	}
	if out.Rows[0]["name"] != "e" {
		t.Fatal("not newest first")
	}
	for _, a := range s.Client.(*fake.FakeDynamicClient).Actions() {
		if a.GetResource().Resource == "secrets" || a.GetNamespace() == "tenant-other-prod" {
			t.Fatal("forbidden read")
		}
	}
	s.config.MaxRows = 1
	out, _ = s.query(scoped(), "infra_changes", Args{Org: "acme"})
	if !out.Truncated || len(out.Rows) != 1 {
		t.Fatal("row cap")
	}
	out = s.bounded(Evidence{Rows: []map[string]any{{"message": strings.Repeat("x", 4000)}}})
	b, _ = json.Marshal(out)
	if len(b) > 2048 || !out.Truncated {
		t.Fatal("byte cap")
	}
}
func TestManagedConditions(t *testing.T) {
	condition := []any{map[string]any{"type": "Ready", "status": "False", "lastTransitionTime": "2026-01-01T00:00:00Z"}}
	s := fixture(t, object("apps.cozystack.io/v1alpha1", "Postgres", "tenant-acme-prod", "db", map[string]any{"spec": map[string]any{"password": "hidden"}, "status": map[string]any{"conditions": condition}}), object("helm.toolkit.fluxcd.io/v2", "HelmRelease", "tenant-acme-prod", "postgres-db", map[string]any{"status": map[string]any{"conditions": condition, "history": []any{map[string]any{"version": int64(2)}}}}))
	out, err := s.query(scoped(), "managed_service_state", Args{Org: "acme", Service: "prod/Postgres/db"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "lastTransitionTime") || strings.Contains(string(b), "hidden") {
		t.Fatal(string(b))
	}
}
func TestHTTPAuthAndRate(t *testing.T) {
	s := fixture(t)
	s.config.RequestsPerMinute = 1
	h := s.Handler()
	for i, status := range []int{401, 200, 429} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if i > 0 {
			r.Header.Set("Authorization", "Bearer valid")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%d: %s", w.Code, w.Body)
		}
	}
}
func TestStreamableRoundTrip(t *testing.T) {
	s := fixture(t)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "quota_and_denials", Arguments: map[string]any{"org": "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("cross-org tool succeeded")
	}
	result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "quota_and_denials", Arguments: map[string]any{"org": "acme"}})
	if err != nil || result.IsError {
		t.Fatalf("own org failed: %v %+v", err, result)
	}
}

type bearerTransport struct{}

func (bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer valid")
	return http.DefaultTransport.RoundTrip(r)
}

func TestLatestBackupAndCNPG(t *testing.T) {
	makeBackup := func(name, taken string) *unstructured.Unstructured {
		return object("backups.cozystack.io/v1alpha1", "Backup", "tenant-acme-prod", name, map[string]any{"spec": map[string]any{"takenAt": taken, "applicationRef": map[string]any{"apiGroup": "apps.cozystack.io", "kind": "Postgres", "name": "db"}}, "status": map[string]any{"phase": "Ready", "artifact": map[string]any{"uri": "s3://private"}}})
	}
	s := fixture(t, object("apps.cozystack.io/v1alpha1", "Postgres", "tenant-acme-prod", "db", nil), object("postgresql.cnpg.io/v1", "Cluster", "tenant-acme-prod", "postgres-db", map[string]any{"status": map[string]any{"currentPrimary": "postgres-db-1", "instances": int64(3), "phase": "Cluster in healthy state"}}), makeBackup("old", "2026-01-01T00:00:00Z"), makeBackup("new", "2026-02-01T00:00:00Z"))
	s.config.MaxBytes = 10000
	out, err := s.query(scoped(), "managed_service_state", Args{Org: "acme", Service: "prod/Postgres/db"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"name":"new"`) || strings.Contains(string(b), `"name":"old"`) || !strings.Contains(string(b), "postgres-db-1") || strings.Contains(string(b), "s3://private") {
		t.Fatal(string(b))
	}
}
func TestMetricsIsolationAndClusterHealth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("query"), `tenant_namespace="tenant-acme-prod"`) {
			t.Error("query not scoped")
		}
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"tenant_namespace":"tenant-other-prod","node":"foreign","condition":"DiskPressure"},"value":[1,"1"]},{"metric":{"tenant_namespace":"tenant-acme-prod","node":"worker-1","condition":"DiskPressure"},"value":[1,"1"]}]}}`))
	}))
	defer up.Close()
	s := fixture(t)
	s.config.MetricsURL = up.URL
	s.config.MaxBytes = 10000
	// Register list kinds for CRDs even when a stand has no instances.
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{resource("kamaji.clastix.io", "v1alpha1", "tenantcontrolplanes"): "TenantControlPlaneList", resource("apps.cozystack.io", "v1alpha1", "kubernetesnodeses"): "KubernetesNodesList", resource("kubevirt.io", "v1", "virtualmachineinstances"): "VirtualMachineInstanceList", quotas: "ResourceQuotaList"}, object("kubevirt.io/v1", "VirtualMachineInstance", "tenant-acme-prod", "vm", map[string]any{"status": map[string]any{"phase": "Running", "nodeName": "physical-secret"}}))
	s.Client = c
	out, err := s.query(scoped(), "cluster_health", Args{Org: "acme", Tier: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "foreign") || strings.Contains(string(b), "physical-secret") || !strings.Contains(string(b), "worker-1") || !strings.Contains(string(b), "Running") {
		t.Fatal(string(b))
	}
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "nodes" || a.GetNamespace() != "tenant-acme-prod" {
			t.Fatal("unscoped read")
		}
	}
}

func TestRCARolesAndInvalidOrganizations(t *testing.T) {
	s := fixture(t, object("aep.aenix.io/v1alpha1", "Organization", "", "platform", nil), object("aep.aenix.io/v1alpha1", "Organization", "", "team-one", nil))
	for _, role := range []string{"developer", "platform", "admin", "auditor", "finance", "approver", "breakglass", "unknown", "developer-extra"} {
		for _, prefix := range []string{"", "/"} {
			_, err := s.resolve(context.Background(), &authn.Identity{Groups: []string{prefix + "aep-acme-" + role}})
			allowed := role == "developer" || role == "platform" || role == "admin" || role == "auditor"
			if (err == nil) != allowed {
				t.Errorf("%s%s: %v", prefix, role, err)
			}
		}
	}
	for _, name := range []string{"platform", "team-one"} {
		for _, id := range []*authn.Identity{{Groups: []string{"aep-" + name + "-admin"}}, {ClientID: "aep-" + name + "-sre-agent"}} {
			if _, err := s.resolve(context.Background(), id); err == nil {
				t.Errorf("accepted invalid org %s", name)
			}
		}
	}
}

func TestPhysicalEventsAndConfiguredRedaction(t *testing.T) {
	extra := []runtime.Object{}
	for i, tc := range []struct{ reason, kind, name string }{
		{"Scheduled", "Pod", "workload"}, {"Started", "Pod", "virt-launcher-vm-abc"},
		{"SuccessfulCreate", "VirtualMachineInstance", "vm"}, {"Ready", "Node", "host-1"},
		{"Updated", "Postgres", "orders"},
	} {
		extra = append(extra, object("v1", "Event", "tenant-acme-nonprod", string(rune('a'+i)), map[string]any{
			"lastTimestamp": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "reason": tc.reason,
			"involvedObject": map[string]any{"kind": tc.kind, "name": tc.name}, "message": "changed on host-42 rack-east"}))
	}
	base := fixture(t, extra...)
	s, err := New(base.Client, base.verifier, Config{RedactPatterns: []string{`host-[0-9]+`, `rack-[a-z]+`}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.query(scoped(), "infra_changes", Args{Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 1 {
		t.Fatalf("unexpected events: %+v", out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "host-") || strings.Contains(string(b), "rack-") {
		t.Fatal(string(b))
	}
	nested := s.bounded(Evidence{Rows: []map[string]any{{"host-42-secret": "hidden", "details": []any{"rack-east"}}}, Unavailable: []string{"host-42/events"}})
	encoded, _ := json.Marshal(nested)
	if strings.Contains(string(encoded), "host-") || strings.Contains(string(encoded), "rack-") || strings.Contains(string(encoded), "hidden") {
		t.Fatal(string(encoded))
	}
	if _, err := New(base.Client, base.verifier, Config{RedactPatterns: []string{"["}}); err == nil {
		t.Fatal("invalid regex accepted")
	}
}
