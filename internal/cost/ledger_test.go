package cost

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fake "k8s.io/client-go/dynamic/fake"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

var hour = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) *Collector {
	t.Helper()
	b, e := os.ReadFile("testdata/installation.json")
	if e != nil {
		t.Fatal(e)
	}
	var raw []map[string]any
	if e = json.Unmarshal(b, &raw); e != nil {
		t.Fatal(e)
	}
	objects := []runtime.Object{}
	for _, m := range raw {
		u := &unstructured.Unstructured{}
		b, _ := json.Marshal(m)
		if e = u.UnmarshalJSON(b); e != nil {
			t.Fatal(e)
		}
		objects = append(objects, u)
	}
	lists := map[schema.GroupVersionResource]string{}
	for _, v := range []struct{ g, v, r, k string }{{"aep.aenix.io", "v1alpha1", "ratecards", "RateCard"}, {"aep.aenix.io", "v1alpha1", "organizations", "Organization"}, {"apps.cozystack.io", "v1alpha1", "postgreses", "Postgres"}, {"apps.cozystack.io", "v1alpha1", "redises", "Redis"}, {"apps.cozystack.io", "v1alpha1", "kuberneteses", "Kubernetes"}, {"apps.cozystack.io", "v1alpha1", "buckets", "Bucket"}, {"kubevirt.io", "v1", "virtualmachines", "VirtualMachine"}, {"", "v1", "pods", "Pod"}, {"", "v1", "persistentvolumeclaims", "PersistentVolumeClaim"}, {"", "v1", "services", "Service"}} {
		lists[GVR(v.g, v.v, v.r)] = v.k + "List"
	}
	return &Collector{Client: fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objects...)}
}
func sum(rows []Row) float64 {
	v := 0.0
	for _, r := range rows {
		v += r.Cost
	}
	return v
}
func TestInstallationReconciliation(t *testing.T) {
	c := fixture(t)
	rows, e := c.Collect(context.Background(), hour)
	if e != nil {
		t.Fatal(e)
	}
	// Independent fixture total: VM 2 CPU + 8 GiB + GPU; DB .5 CPU + 2 GiB;
	// 100 GiB disk; 1 IP; 10 GiB bucket; two monthly platform amounts.
	want := 2 + 0.8 + 2 + 0.5 + 0.2 + 0.1 + 0.1 + 0.01 + 2.0
	if math.Abs(sum(rows)-want)/want > 0.02 {
		t.Fatalf("installation=%g organizations=%g", want, sum(rows))
	}
	found := false
	for _, r := range rows {
		if r.ResourceName == "postgres-db-1" {
			found = true
			if r.Project != "orders" || r.Service != "database" || r.Tags[Domain+"environment"] != "dev" {
				t.Fatalf("lost app identity: %+v", r)
			}
		}
		if strings.Contains(r.ResourceName, "virt-launcher") {
			t.Fatal("double counted VM")
		}
	}
	if !found {
		t.Fatal("missing managed service")
	}
	// Reconciliation is exact through OpenCost splitting, even for overcommit.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("window") != "2026-11-24T12:00:00Z,2026-11-24T13:00:00Z" {
			t.Error("wrong window")
		}
		_, _ = w.Write([]byte(`{"code":200,"data":[{"pod":{"cpuCoreRequestAverage":20,"ramByteRequestAverage":0,"minutes":60,"properties":{"labels":{"openchoreo.dev/project":"shop","openchoreo.dev/component":"api"}}}}]}`))
	}))
	defer server.Close()
	c.OpenCost = &OpenCost{URLTemplate: server.URL}
	allocated, e := c.Collect(context.Background(), hour)
	if e != nil || math.Abs(sum(allocated)-want) > 1e-9 {
		t.Fatalf("split reconciliation: %g %v", sum(allocated), e)
	}
}
func TestOpenCostFailureAndIdle(t *testing.T) {
	card := &api.RateCard{Spec: api.RateCardSpec{VCPUHour: 1}}
	rows := []Row{{Cost: 4, Tags: map[string]string{}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":[{"a":{"cpuCoreRequestAverage":1,"minutes":60,"properties":{"labels":{"aep.aenix.io/project":"p","aep.aenix.io/component":"c"}}}}]}`))
	}))
	defer server.Close()
	o := &OpenCost{URLTemplate: server.URL}
	got := o.Split(context.Background(), "a", "prod", hour, rows, card)
	if len(got) != 2 || got[0].Cost != 1 || got[1].Cost != 3 || got[1].Service != "idle" {
		t.Fatalf("%+v", got)
	}
	o.URLTemplate = ":invalid"
	got = o.Split(context.Background(), "a", "prod", hour, rows, card)
	if len(got) != 1 || !got[0].Unallocated || got[0].Cost != 4 {
		t.Fatal(got)
	}
}
func TestRatesAndRollup(t *testing.T) {
	a := api.RateCard{ObjectMeta: metav1.ObjectMeta{Name: "old"}, Spec: api.RateCardSpec{Currency: "USD", EffectiveFrom: metav1.NewTime(hour.Add(-time.Hour))}}
	b := *a.DeepCopy()
	b.Name = "new"
	b.Spec.EffectiveFrom = metav1.NewTime(hour)
	c, e := SelectRate([]api.RateCard{b, a}, hour)
	if e != nil || c.Name != "new" {
		t.Fatal(c, e)
	}
	if _, e = SelectRate([]api.RateCard{a, a}, hour); e == nil {
		t.Fatal("duplicate effective hour accepted")
	}
	rows := Rollup([]Row{{Organization: "a", Cost: 1}, {Organization: "a", Cost: 2}, {Organization: "b", Cost: 5}})
	if len(rows) != 2 || sum(rows) != 8 {
		t.Fatal(rows)
	}
}
func TestPodSchedulingRequests(t *testing.T) {
	restart := core.ContainerRestartPolicyAlways
	req := func(cpu, mem string) core.ResourceRequirements {
		return core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse(cpu), core.ResourceMemory: resource.MustParse(mem)}}
	}
	p := &core.Pod{Spec: core.PodSpec{Containers: []core.Container{{Resources: req("1", "1Gi")}}, InitContainers: []core.Container{{RestartPolicy: &restart, Resources: req("500m", "1Gi")}, {Resources: req("2", "3Gi")}}}}
	cpu, mem := PodRequests(p)
	if cpu != 2.5 || mem != 4 {
		t.Fatalf("%g %g", cpu, mem)
	}
}
func TestFOCUSRequiredColumns(t *testing.T) {
	var b bytes.Buffer
	if e := FocusCSV(&b, []Row{{Organization: "a", Currency: "USD", Category: "Compute", Cost: 1.25, Tags: map[string]string{Domain + "project": "p"}}}, hour, 24, "test"); e != nil {
		t.Fatal(e)
	}
	records, e := csv.NewReader(&b).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	headers := map[string]int{}
	for i, k := range records[0] {
		headers[k] = i
	}
	// Independent list from the FOCUS 1.0 column library (mandatory columns).
	required := strings.Fields("BilledCost BillingAccountId BillingAccountName BillingCurrency BillingPeriodEnd BillingPeriodStart ChargeCategory ChargeClass ChargeDescription ChargeFrequency ChargePeriodEnd ChargePeriodStart ContractedCost EffectiveCost InvoiceIssuerName ListCost PricingCategory PricingQuantity PricingUnit ProviderName PublisherName ServiceCategory ServiceName SubAccountId SubAccountName Tags")
	for _, k := range required {
		if _, ok := headers[k]; !ok {
			t.Errorf("missing required FOCUS column %s", k)
		}
	}
	if records[1][headers["BilledCost"]] != "1.2500000000" || records[1][headers["EffectiveCost"]] != records[1][headers["BilledCost"]] {
		t.Fatal("invalid costs")
	}
	var tags map[string]string
	if e = json.Unmarshal([]byte(records[1][headers["Tags"]]), &tags); e != nil || tags[Domain+"project"] != "p" {
		t.Fatal("tags lost")
	}
}

func TestMissingRateRejectsSnapshot(t *testing.T) {
	c := fixture(t)
	ctx := context.Background()
	card, e := c.Client.Resource(GVR("aep.aenix.io", "v1alpha1", "ratecards")).Get(ctx, "nov", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	_ = unstructured.SetNestedMap(card.Object, map[string]any{}, "spec", "storageGiBMonth")
	if _, e = c.Client.Resource(GVR("aep.aenix.io", "v1alpha1", "ratecards")).Update(ctx, card, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Collect(ctx, hour); e == nil {
		t.Fatal("silently accepted missing storage rate")
	}
}
