package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testClaim() *api.ServiceClaim {
	return &api.ServiceClaim{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "dp-test", UID: "claim-uid", Generation: 1}, Spec: api.ServiceClaimSpec{Kind: "Postgres", Resource: api.ResourceIdentity{Project: "project", Name: "orders", Environment: "dev"}, Parameters: map[string]ext.JSON{"version": {Raw: []byte(`"v17"`)}, "databases": {Raw: []byte(`{"appdb":{}}`)}, "users": {Raw: []byte(`{"app":{}}`)}}, EnvironmentParameters: map[string]ext.JSON{"resourcesPreset": {Raw: []byte(`"t1.small"`)}, "size": {Raw: []byte(`"2Gi"`)}, "replicas": {Raw: []byte(`1`)}}}}
}
func harness(t *testing.T, c *api.ServiceClaim) (*Reconciler, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = discoveryv1.AddToScheme(s)
	a := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.ServiceClaim{}).WithObjects(c).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if p.Type() != types.ApplyPatchType {
			return cl.Patch(ctx, obj, p, opts...)
		}
		options := &client.PatchOptions{}
		for _, o := range opts {
			o.ApplyToPatch(options)
		}
		if options.FieldManager != Manager {
			t.Fatal("wrong field manager")
		}
		old := obj.DeepCopyObject().(client.Object)
		e := cl.Get(ctx, client.ObjectKeyFromObject(obj), old)
		if errors.IsNotFound(e) {
			return cl.Create(ctx, obj)
		}
		if e != nil {
			return e
		}
		obj.SetResourceVersion(old.GetResourceVersion())
		return cl.Update(ctx, obj)
	}}).Build()
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := &Reconciler{App: a, Management: fake.NewClientBuilder().WithScheme(s).Build(), Dynamic: d, Organization: "org", Tier: "nonprod", TenantNamespace: "tenant-org-nonprod", ClusterID: "cluster", TierDeletionPolicy: "Delete", AllowedKinds: []string{"Postgres", "Redis"}, Policy: policy.Default()}
	return r, d
}
func reconcile(t *testing.T, r *Reconciler, c *api.ServiceClaim) *api.ServiceClaim {
	t.Helper()
	_, e := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	if e != nil {
		t.Fatal(e)
	}
	out := &api.ServiceClaim{}
	if e = r.App.Get(context.Background(), client.ObjectKeyFromObject(c), out); e != nil {
		t.Fatal(e)
	}
	return out
}
func boundApp(t *testing.T, r *Reconciler, c *api.ServiceClaim) *unstructured.Unstructured {
	t.Helper()
	a, e := r.Dynamic.Resource(kinds.Table[c.Spec.Kind].GVR()).Namespace(r.TenantNamespace).Get(context.Background(), policy.Name(r.ClusterID, c.Namespace, c.Name, c.Spec.Resource.Name), metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func TestCreateConflictPauseAndDefaulting(t *testing.T) {
	c := testClaim()
	r, d := harness(t, c)
	// Admission defaults must not cause a perpetual update loop.
	d.PrependReactor("create", "postgreses", func(a ktesting.Action) (bool, runtime.Object, error) {
		u := a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		_ = unstructured.SetNestedField(u.Object, false, "spec", "external")
		return false, nil, nil
	})
	c = reconcile(t, r, c)
	app := boundApp(t, r, c)
	if meta.FindStatusCondition(c.Status.Conditions, "Ready").Reason != "Updating" || c.Status.AppliedSpecHash != app.GetAnnotations()[Domain+"applied-spec-hash"] {
		t.Fatal(c.Status)
	}
	count := len(d.Actions())
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	for _, a := range d.Actions()[count:] {
		if a.GetVerb() == "update" && a.GetResource().Resource == "postgreses" {
			t.Fatal("defaulting caused repeated write")
		}
	}
	annotations := app.GetAnnotations()
	annotations[Domain+"paused"] = "true"
	app.SetAnnotations(annotations)
	_, _ = d.Resource(kinds.Table["Postgres"].GVR()).Namespace(r.TenantNamespace).Update(context.Background(), app, metav1.UpdateOptions{})
	c = reconcile(t, r, c)
	if meta.FindStatusCondition(c.Status.Conditions, "Ready").Reason != "Paused" {
		t.Fatal(c.Status)
	}
	annotations[Domain+"claim-uid"] = "another"
	app.SetAnnotations(annotations)
	_, _ = d.Resource(kinds.Table["Postgres"].GVR()).Namespace(r.TenantNamespace).Update(context.Background(), app, metav1.UpdateOptions{})
	c = reconcile(t, r, c)
	if meta.FindStatusCondition(c.Status.Conditions, "Ready").Reason != "NameConflict" {
		t.Fatal(c.Status)
	}
}
func TestFinalization(t *testing.T) {
	for _, tt := range []struct {
		tier, policy, claimPolicy string
		paused, conflict, retain  bool
	}{{"nonprod", "", "Delete", false, false, false}, {"nonprod", "", "Retain", false, false, true}, {"prod", "", "Delete", false, false, true}, {"prod", "Retain", "Delete", false, false, true}, {"prod", "Delete", "Delete", false, false, true}, {"nonprod", "Retain", "Delete", false, false, true}, {"nonprod", "Delete", "Retain", false, false, true}, {"nonprod", "Delete", "Delete", false, false, false}, {"nonprod", "Delete", "Delete", true, false, true}, {"nonprod", "Delete", "Delete", false, true, true}} {
		t.Run(fmt.Sprint(tt), func(t *testing.T) {
			c := testClaim()
			r, d := harness(t, c)
			r.Tier = tt.tier
			r.TierDeletionPolicy = tt.policy
			c = reconcile(t, r, c)
			c.Spec.DeletionPolicy = tt.claimPolicy
			if e := r.App.Update(context.Background(), c); e != nil {
				t.Fatal(e)
			}
			app := boundApp(t, r, c)
			annotations := app.GetAnnotations()
			if tt.paused {
				annotations[Domain+"paused"] = "true"
			}
			if tt.conflict {
				annotations[Domain+"claim-uid"] = "other"
			}
			app.SetAnnotations(annotations)
			_, _ = d.Resource(kinds.Table["Postgres"].GVR()).Namespace(r.TenantNamespace).Update(context.Background(), app, metav1.UpdateOptions{})
			if e := r.App.Delete(context.Background(), c); e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 2; i++ {
				if _, e := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); e != nil {
					t.Fatal(e)
				}
			}
			got, e := d.Resource(kinds.Table["Postgres"].GVR()).Namespace(r.TenantNamespace).Get(context.Background(), app.GetName(), metav1.GetOptions{})
			if tt.retain && e != nil {
				t.Fatal("retained app missing", e)
			}
			if !tt.retain && !errors.IsNotFound(e) {
				t.Fatal("app not deleted", e)
			}
			if tt.retain && !tt.paused && !tt.conflict && got.GetAnnotations()[Domain+"orphaned"] != "true" {
				t.Fatal("not orphaned")
			}
			for _, a := range d.Actions() {
				if a.GetVerb() == "list" {
					t.Fatal("finalization must never infer deletion from lists")
				}
			}
		})
	}
}
func TestConnectionRotationAndEndpointDrift(t *testing.T) {
	ctx := context.Background()
	c := testClaim()
	r, d := harness(t, c)
	c = reconcile(t, r, c)
	app := boundApp(t, r, c)
	name := app.GetName()
	m := kinds.Table["Postgres"]
	_ = unstructured.SetNestedSlice(app.Object, object("True", "True", 0, 0).Object["status"].(map[string]any)["conditions"].([]any), "status", "conditions")
	_, _ = d.Resource(m.GVR()).Namespace(r.TenantNamespace).Update(ctx, app, metav1.UpdateOptions{})
	hr := object("True", "", 3, 3)
	hr.SetAPIVersion("helm.toolkit.fluxcd.io/v2")
	hr.SetKind("HelmRelease")
	hr.SetName(m.Release(name))
	hr.SetNamespace(r.TenantNamespace)
	spec, _, _ := unstructured.NestedMap(app.Object, "spec")
	_ = unstructured.SetNestedMap(hr.Object, spec, "spec", "values")
	_, _ = d.Resource(HelmReleases).Namespace(r.TenantNamespace).Create(ctx, hr, metav1.CreateOptions{})
	if e := r.Management.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: m.Service(name), Namespace: r.TenantNamespace}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.1.2"}}); e != nil {
		t.Fatal(e)
	}
	for name, data := range map[string]map[string]any{m.Secret(name): {"app": base64.StdEncoding.EncodeToString([]byte("a@:/?#password"))}, m.Release(name) + m.CASuffix: {"ca.crt": base64.StdEncoding.EncodeToString([]byte("test-ca"))}} {
		_, e := d.Resource(TenantSecrets).Namespace(r.TenantNamespace).Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "core.cozystack.io/v1alpha1", "kind": "TenantSecret", "metadata": map[string]any{"name": name}, "data": data}}, metav1.CreateOptions{})
		if e != nil {
			t.Fatal(e)
		}
	}
	c = reconcile(t, r, c)
	if !meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") {
		t.Fatal(c.Status)
	}
	conn := &corev1.Secret{}
	_ = r.App.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name + "-conn"}, conn)
	for _, k := range []string{"host", "port", "database", "username", "password", "uri", "ca.crt"} {
		if len(conn.Data[k]) == 0 {
			t.Fatal("missing", k)
		}
	}
	serialized, _ := json.Marshal(c.Status)
	if string(serialized) == "" || containsPassword(serialized, "a@:/?#password") {
		t.Fatal("password in status")
	}
	endpoint := &discoveryv1.EndpointSlice{}
	_ = r.App.Get(ctx, client.ObjectKeyFromObject(c), endpoint)
	endpoint.Endpoints[0].Addresses = []string{"10.1.1.1"}
	_ = r.App.Update(ctx, endpoint)
	c = reconcile(t, r, c)
	if meta.FindStatusCondition(c.Status.Conditions, "Degraded").Reason != "EndpointDrift" {
		t.Fatal(c.Status)
	}
	_ = r.App.Get(ctx, client.ObjectKeyFromObject(c), endpoint)
	if endpoint.Endpoints[0].Addresses[0] != "10.96.1.2" {
		t.Fatal("drift not corrected")
	}
	credentials, _ := d.Resource(TenantSecrets).Namespace(r.TenantNamespace).Get(ctx, m.Secret(name), metav1.GetOptions{})
	_ = unstructured.SetNestedField(credentials.Object, base64.StdEncoding.EncodeToString([]byte("rotated")), "data", "app")
	_, _ = d.Resource(TenantSecrets).Namespace(r.TenantNamespace).Update(ctx, credentials, metav1.UpdateOptions{})
	c = reconcile(t, r, c)
	_ = r.App.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name + "-conn"}, conn)
	if string(conn.Data["password"]) != "rotated" {
		t.Fatal("password not rotated")
	}
	c.Spec.EnvironmentParameters["size"] = ext.JSON{Raw: []byte(`"3Gi"`)}
	c.Generation++
	_ = r.App.Update(ctx, c)
	c = reconcile(t, r, c)
	if meta.FindStatusCondition(c.Status.Conditions, "Ready").Reason != "Updating" {
		t.Fatal("stale readiness", c.Status)
	}
}
func containsPassword(b []byte, password string) bool {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return findValue(m, password)
}
func findValue(v any, s string) bool {
	switch x := v.(type) {
	case string:
		return x == s
	case map[string]any:
		for _, v := range x {
			if findValue(v, s) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if findValue(v, s) {
				return true
			}
		}
	}
	return false
}

func TestBackupPolicyReference(t *testing.T) {
	c := testClaim()
	r, _ := harness(t, c)
	r.Policy.BackupPolicy = "prod-default"
	r.Policy.Backup = &policy.BackupConfig{ClassName: "aep-acme-prod", Schedule: "0 2 * * *", RetentionPolicy: "30d"}
	c = reconcile(t, r, c)
	if got := boundApp(t, r, c).GetAnnotations()[Domain+"backup-policy"]; got != "prod-default" {
		t.Fatal(got)
	}
	r.Policy.BackupPolicy = ""
	c = reconcile(t, r, c)
	if _, ok := boundApp(t, r, c).GetAnnotations()[Domain+"backup-policy"]; ok {
		t.Fatal("stale backup policy reference")
	}
}
