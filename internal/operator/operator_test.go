package operator

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"os"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
	"time"
)

func organization() *api.Organization {
	return &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "org-uid"}, Spec: api.OrganizationSpec{Profile: "standard", DisplayName: "ACME", Quotas: api.Quotas{"cpu": "32", "memory": "64Gi", "storage": "1Ti"}, Environments: []api.Environment{{Name: "dev", Tier: "nonprod", ChangeModel: "none"}, {Name: "prod", Tier: "prod", ChangeModel: "required"}}, Tiers: map[string]api.TierSpec{"nonprod": tier(), "prod": tier()}}}
}
func tier() api.TierSpec {
	return api.TierSpec{Cluster: api.ClusterSpec{Version: "v1.35", NodeGroups: map[string]api.NodeGroup{"md0": {MinReplicas: 2, MaxReplicas: 3, InstanceType: "u1.large"}}}, Services: api.ServicesSpec{AllowedKinds: []string{"Postgres", "Redis"}, MaxPreset: "u1.large"}}
}
func TestQuotaAllocation(t *testing.T) {
	for _, n := range []int{1, 2} {
		tiers, parent, e := QuotaAllocation(api.Quotas{"cpu": "33", "memory": "65Gi", "gpu": "3"}, n)
		if e != nil {
			t.Fatal(e)
		}
		for _, k := range []string{"cpu", "memory", "gpu"} {
			sum := resource.MustParse("0")
			for _, q := range tiers {
				sum.Add(resource.MustParse(q[k]))
			}
			if sum.Cmp(resource.MustParse(parent[k])) != 0 {
				t.Fatalf("%s budget not conserved", k)
			}
		}
		expected := resource.MustParse("33")
		for i := 0; i < n; i++ {
			expected.Add(resource.MustParse("20"))
		}
		if expected.Cmp(resource.MustParse(parent["cpu"])) != 0 {
			t.Fatal(parent)
		}
	}
	q, _, _ := QuotaAllocation(api.Quotas{"cpu": "32", "memory": "64Gi"}, 2)
	mem := resource.MustParse(q[0]["memory"])
	expectedMem := resource.MustParse("40Gi")
	if q[0]["cpu"] != "36" || mem.Cmp(expectedMem) != 0 {
		t.Fatal(q)
	}
	for _, q := range []api.Quotas{{"cpu": "invalid", "memory": "1Gi"}, {"cpu": "-1", "memory": "1Gi"}, {"cpu": "1"}} {
		if _, _, e := QuotaAllocation(q, 2); e == nil {
			t.Fatal("accepted invalid quota", q)
		}
	}
}
func TestValidation(t *testing.T) {
	for _, name := range []string{"aep", "root", "cozy", "system", "Acme", "a-cme", "", strings.Repeat("a", 33)} {
		o := organization()
		o.Name = name
		if Validate(o) == nil {
			t.Fatal("accepted", name)
		}
	}
	o := organization()
	if e := Validate(o); e != nil {
		t.Fatal(e)
	}
	if TenantNamespace("acme", "prod") != "tenant-acme-prod" {
		t.Fatal("bad name")
	}
	o.Spec.Environments[0].Tier = "prod"
	if Validate(o) == nil {
		t.Fatal("prod root accepted")
	}
	o = organization()
	o.Spec.Profile = "regulated"
	if e := Validate(o); e == nil || !strings.Contains(e.Error(), "not implemented") {
		t.Fatal(e)
	}
	o = organization()
	o.Spec.Profile = "small"
	if Validate(o) != nil || PhysicalTier(o, "prod") != "nonprod" {
		t.Fatal("small must share one physical tier")
	}
	o.Spec.Environments = o.Spec.Environments[:1]
	if e := Validate(o); e != nil {
		t.Fatal(e)
	}
}
func TestPipeline(t *testing.T) {
	p := Pipeline(organization())
	paths := p["promotionPaths"].([]any)
	if len(paths) != 2 {
		t.Fatal(p)
	}
	first := paths[0].(M)
	if first["sourceEnvironmentRef"].(M)["name"] != "dev" || first["targetEnvironmentRefs"].([]any)[0].(M)["name"] != "prod" {
		t.Fatal(p)
	}
}
func TestAuthzGolden(t *testing.T) {
	o := organization()
	var b strings.Builder
	for _, u := range RenderAuthz(o) {
		data, e := yaml.Marshal(u.Object)
		if e != nil {
			t.Fatal(e)
		}
		b.Write(data)
		b.WriteString("---\n")
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if e := os.WriteFile("testdata/authz.yaml", []byte(b.String()), 0644); e != nil {
			t.Fatal(e)
		}
	}
	want, e := os.ReadFile("testdata/authz.yaml")
	if e != nil {
		t.Fatal(e)
	}
	if string(want) != b.String() {
		t.Fatal("authz golden mismatch")
	}
	for _, u := range RenderAuthz(o) {
		if u.GetKind() != "AuthzRoleBinding" || u.GetName() != "aep-developer" {
			continue
		}
		m, _, _ := unstructured.NestedSlice(u.Object, "spec", "roleMappings")
		conditions := m[0].(M)["conditions"].([]any)
		condition := conditions[0].(M)
		if condition["expression"] != `resource.environment in ["acme/dev"]` {
			t.Fatal(condition)
		}
		actions := condition["actions"].([]any)
		for _, kind := range []string{"releasebinding", "resourcereleasebinding", "projectreleasebinding", "endpointreleasebinding"} {
			for _, verb := range []string{"create", "update", "delete"} {
				found := false
				for _, a := range actions {
					if a == kind+":"+verb {
						found = true
					}
				}
				if !found {
					t.Fatal("unguarded", kind, verb)
				}
			}
		}
	}
}
func TestNetworkServiceFixture(t *testing.T) {
	data, e := os.ReadFile("testdata/gateway-service.yaml")
	if e != nil {
		t.Fatal(e)
	}
	svc := &corev1.Service{}
	if e = yaml.Unmarshal(data, svc); e != nil {
		t.Fatal(e)
	}
	policies, e := RenderNetwork(organization(), svc, "aepcp")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(policies)
	for _, expected := range []string{"30268", "tenant-acme-prod", "tenant-acme-nonprod", "kubernetes-aepcp", "tenant-aep-cp"} {
		if !strings.Contains(string(b), expected) {
			t.Fatal("missing", expected)
		}
	}
	svc.Spec.Ports[0].TargetPort = intstr.FromInt32(30269)
	p, _ := RenderNetwork(organization(), svc, "aepcp")
	b, _ = json.Marshal(p)
	if !strings.Contains(string(b), "30269") || strings.Contains(string(b), "30268") {
		t.Fatal("stale targetPort")
	}
	svc.Spec.Ports[0].TargetPort = intstr.FromString("https")
	if _, e = RenderNetwork(organization(), svc, "aepcp"); e == nil {
		t.Fatal("accepted named targetPort")
	}
}
func TestReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, status  string
		gen, observed int64
		want          bool
	}{{"green", "True", 2, 2, true}, {"stale", "True", 2, 1, false}, {"false", "False", 1, 1, false}, {"unknown", "Unknown", 1, 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			u := obj(helmAPI, "HelmRelease", "ns", "apps", nil)
			u.SetGeneration(tc.gen)
			u.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": tc.status, "observedGeneration": tc.observed}}}
			if ready(u) != tc.want {
				t.Fatal(tc)
			}
		})
	}
	u := obj("kamaji.clastix.io/v1alpha1", "TenantControlPlane", "ns", "apps", nil)
	if ControlPlaneReady(u) {
		t.Fatal("empty TCP ready")
	}
	u.Object["status"] = M{"kubernetesResources": M{"version": M{"status": "Ready"}}}
	if !ControlPlaneReady(u) {
		t.Fatal("ready TCP rejected")
	}
	node := corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if NodesReady([]corev1.Node{node}, 2) || !NodesReady([]corev1.Node{node, node}, 2) {
		t.Fatal("incorrect node count")
	}
}
func TestBootstrapRemediation(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		cp    bool
		nodes int
		token bool
		age   time.Duration
		want  bool
	}{{true, 0, false, 11 * time.Minute, true}, {false, 0, false, time.Hour, false}, {true, 1, false, time.Hour, false}, {true, 0, true, time.Hour, false}, {true, 0, false, 9 * time.Minute, false}} {
		if got := BootstrapRemediation(tc.cp, tc.nodes, tc.token, now.Add(-tc.age), now); got != tc.want {
			t.Fatal(tc)
		}
	}
	if BootstrapRemediation(true, 0, false, time.Time{}, now) {
		t.Fatal("unknown start remediated")
	}
}
func TestRefuseAdoption(t *testing.T) {
	u := obj(appsAPI, "Tenant", "tenant-root", "acme", M{})
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), u)
	c := &Cluster{Dynamic: d}
	if e := c.apply(context.Background(), organization(), "tenants", u.DeepCopy()); e == nil || !strings.Contains(e.Error(), "refusing to adopt") {
		t.Fatal(e)
	}
	for _, a := range d.Actions() {
		if a.GetVerb() != "get" {
			t.Fatal("mutated unowned tenant", a)
		}
	}
}
func TestDeepCopy(t *testing.T) {
	o := organization()
	copy := o.DeepCopy()
	copy.Spec.Quotas["cpu"] = "999"
	tier := copy.Spec.Tiers["prod"]
	tier.Cluster.NodeGroups["md0"] = api.NodeGroup{}
	if o.Spec.Quotas["cpu"] == "999" || o.Spec.Tiers["prod"].Cluster.NodeGroups["md0"].MinReplicas == 0 {
		t.Fatal("aliased deepcopy")
	}
}

func TestSequentialTenants(t *testing.T) {
	o := organization()
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	s := &session{o: o, r: &Reconciler{Management: &Cluster{Dynamic: d}}}
	if e := s.tenants(context.Background()); e == nil {
		t.Fatal("new parent considered ready")
	}
	created := 0
	for _, a := range d.Actions() {
		if a.GetVerb() == "create" {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d tenants before parent readiness", created)
	}
	parent, e := d.Resource(gvr(appsAPI, "tenants")).Namespace("tenant-root").Get(context.Background(), "acme", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	parent.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True", "observedGeneration": int64(0)}}}
	if _, e = d.Resource(gvr(appsAPI, "tenants")).Namespace("tenant-root").Update(context.Background(), parent, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	d.ClearActions()
	if e = s.tenants(context.Background()); e == nil {
		t.Fatal("new child considered ready")
	}
	created = 0
	for _, a := range d.Actions() {
		if a.GetVerb() == "create" {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("expected one child, created %d", created)
	}
}
func TestDeleteConfirmation(t *testing.T) {
	s := &session{o: organization(), r: &Reconciler{}}
	if e := s.delete(context.Background()); e == nil || !strings.Contains(e.Error(), "confirm-delete") {
		t.Fatal(e)
	}
}
func TestDeleteBlockedByComponent(t *testing.T) {
	o := organization()
	o.Annotations = map[string]string{"aep.aenix.io/confirm-delete": "acme"}
	component := obj(ocAPI, "Component", "acme", "checkout", M{})
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), component)
	s := &session{o: o, r: &Reconciler{}, cp: &Cluster{Dynamic: d}}
	if e := s.delete(context.Background()); e == nil || !strings.Contains(e.Error(), "checkout") {
		t.Fatal(e)
	}
	for _, a := range d.Actions() {
		if a.GetVerb() != "list" {
			t.Fatal("mutation before preflight", a)
		}
	}
}
