package rcaeval

import (
	"context"
	"encoding/json"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quotaClient() *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "quota", "namespace": "tenant-acme-nonprod", "uid": "quota-uid"}, "status": map[string]any{"hard": map[string]any{"pods": "0"}}, "spec": map[string]any{"hard": map[string]any{"pods": "10", "requests.cpu": "4"}}}})
}
func TestQuotaRecovery(t *testing.T) {
	ctx := context.Background()
	c := quotaClient()
	journal := filepath.Join(t.TempDir(), "recovery.json")
	undo, err := (QuotaInjector{}).Inject(ctx, c, Target{Organization: "acme", Tier: "nonprod", Quota: "quota", WorkloadImage: "probe"}, journal)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := c.Resource(quotaResource).Namespace("tenant-acme-nonprod").Get(ctx, "quota", metav1.GetOptions{})
	if v, _, _ := unstructured.NestedString(q.Object, "spec", "hard", "pods"); v != "0" {
		t.Fatal(v)
	}
	if err = undo(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ = c.Resource(quotaResource).Namespace("tenant-acme-nonprod").Get(ctx, "quota", metav1.GetOptions{})
	if v, _, _ := unstructured.NestedString(q.Object, "spec", "hard", "pods"); v != "10" {
		t.Fatal(v)
	}
	if _, err = os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("journal not removed")
	}
}
func TestRecoveryPreservesConcurrentEdit(t *testing.T) {
	ctx := context.Background()
	c := quotaClient()
	path := filepath.Join(t.TempDir(), "recovery.json")
	undo, err := (QuotaInjector{}).Inject(ctx, c, Target{Organization: "acme", Tier: "nonprod", Quota: "quota", WorkloadImage: "probe"}, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resource(quotaResource).Namespace("tenant-acme-nonprod").Patch(ctx, "quota", types.MergePatchType, []byte(`{"spec":{"hard":{"pods":"7"}}}`), metav1.PatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = undo(ctx); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("journal lost")
	}
}
func TestSimulationOnlyAnnotates(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps.cozystack.io/v1alpha1", "kind": "Postgres", "metadata": map[string]any{"name": "db", "namespace": "tenant-acme-nonprod", "uid": "app-uid", "annotations": map[string]any{"existing": "keep"}}, "spec": map[string]any{"size": "10Gi"}}})
	undo, err := (SimulatedDiskInjector{}).Inject(ctx, c, Target{Organization: "acme", Tier: "nonprod", App: "db"}, filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Actions() {
		if a.GetResource() != appResource {
			t.Fatal("simulation touched another resource")
		}
	}
	if err = undo(ctx); err != nil {
		t.Fatal(err)
	}
	o, _ := c.Resource(appResource).Namespace("tenant-acme-nonprod").Get(ctx, "db", metav1.GetOptions{})
	if len(o.GetAnnotations()) != 1 || o.GetAnnotations()["existing"] != "keep" {
		t.Fatal(o)
	}
}

type nopInjector struct{ cleaned *bool }

func (n nopInjector) Inject(context.Context, dynamic.Interface, Target, string) (Undo, error) {
	return func(context.Context) error { *n.cleaned = true; return nil }, nil
}
func TestReportContractAndTopOne(t *testing.T) {
	polls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing bearer")
		}
		if r.Method == "POST" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			b, _ := json.Marshal(body)
			if strings.Contains(string(b), "expected-cause") {
				t.Error("leaked answer")
			}
			w.Write([]byte(`{"report_id":"report-1","status":"pending"}`))
			return
		}
		if r.URL.Path != "/api/v1/rca-agent/reports/report-1" {
			t.Error(r.URL.Path)
		}
		polls++
		if polls == 1 {
			w.Write([]byte(`{"status":"pending"}`))
			return
		}
		w.Write([]byte(`{"status":"completed","report":{"result":{"root_causes":[{"summary":"wrong cause"},{"summary":"expected-cause"}]}}}`))
	}))
	defer up.Close()
	cleaned := false
	r := Runner{Target: Target{Organization: "acme"}, AgentURL: up.URL, Token: "test", Timeout: time.Second, Poll: time.Millisecond, Injectors: map[string]Injector{"test": nopInjector{&cleaned}}}
	out := r.Run(context.Background(), Case{ID: "test", Injector: "test", Match: "expected-cause", Symptom: "unavailable"})
	if out.Status != "completed" || out.Correct || !cleaned || polls != 2 {
		t.Fatalf("%+v cleaned %v polls %d", out, cleaned, polls)
	}
}
func TestTimeoutCleansUp(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"report_id":"x","status":"pending"}`)) }))
	defer up.Close()
	cleaned := false
	r := Runner{AgentURL: up.URL, Timeout: 30 * time.Millisecond, Poll: 5 * time.Millisecond, Injectors: map[string]Injector{"test": nopInjector{&cleaned}}}
	out := r.Run(context.Background(), Case{Injector: "test", Match: "x"})
	if out.Status != "failed" || !cleaned {
		t.Fatalf("%+v cleaned %v", out, cleaned)
	}
}
func TestSuiteAndScoring(t *testing.T) {
	s, err := Load("../../eval/cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cases) != 30 {
		t.Fatal(len(s.Cases))
	}
	sum := Summarize([]Result{{Status: "completed", Correct: true, Seconds: 2}, {Status: "failed"}, {Status: "completed", Correct: true, Simulated: true}, {Status: "stubbed"}})
	if sum.Accuracy != 0.5 || sum.Eligible != 2 || sum.Acceptance || sum.MeanSeconds != 2 {
		t.Fatalf("%+v", sum)
	}
}

func TestPartialInjectionReturnsRecovery(t *testing.T) {
	c := quotaClient()
	ctx := context.Background()
	_, err := c.Resource(quotaResource).Namespace("tenant-acme-nonprod").Patch(ctx, "quota", types.MergePatchType, []byte(`{"status":{"hard":{"pods":"10"}}}`), metav1.PatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	path := filepath.Join(t.TempDir(), "journal")
	undo, err := (QuotaInjector{}).Inject(cancelled, c, Target{Organization: "acme", Tier: "nonprod", Quota: "quota", WorkloadImage: "probe"}, path)
	if err == nil || undo == nil {
		t.Fatalf("partial mutation must provide undo: %v", err)
	}
	if err = undo(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ := c.Resource(quotaResource).Namespace("tenant-acme-nonprod").Get(ctx, "quota", metav1.GetOptions{})
	if v, _, _ := unstructured.NestedString(q.Object, "spec", "hard", "pods"); v != "10" {
		t.Fatal("quota not restored")
	}
}
