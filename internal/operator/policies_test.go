package operator

import (
	"context"
	"encoding/json"
	"errors"
	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"os"
	"os/exec"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPolicyDeliveryContract(t *testing.T) {
	o := organization()
	o.Spec.Policy.Mode = "audit"
	o.Spec.Policy.Registries = []string{"registry.example:5000"}
	for _, tier := range Tiers(o) {
		values := policyValues(o, tier, "PUBLIC KEY")
		if len(values) != 3 {
			t.Fatal(values)
		}
		config := values["kyverno"]["config"].(M)
		if config["excludeKyvernoNamespace"] != false || len(config["resourceFilters"].([]any)) != 1 || len(config["webhooks"].(M)["namespaceSelector"].(M)["matchExpressions"].([]any)) != 0 {
			t.Fatal("engine defaults bypass namespace policies", config)
		}
		expected := int64(1)
		if tier == "prod" {
			expected = 3
		}
		if values["kyverno"]["admissionController"].(M)["replicas"] != expected {
			t.Fatal("wrong HA count")
		}
		for name, v := range values {
			hr := policyRelease(o, tier, name, v)
			if hr.GetNamespace() != TenantNamespace(o.Name, tier) {
				t.Fatal(hr)
			}
			for k, want := range map[string]string{"cozystack.io/target-cluster-name": "kubernetes-apps", "internal.cozystack.io/tenantresource": "true", "sharding.fluxcd.io/key": "tenants"} {
				if hr.GetLabels()[k] != want {
					t.Fatal(hr.GetLabels())
				}
			}
			key, _, _ := unstructured.NestedString(hr.Object, "spec", "kubeConfig", "secretRef", "key")
			secret, _, _ := unstructured.NestedString(hr.Object, "spec", "kubeConfig", "secretRef", "name")
			if key != "super-admin.svc" || secret != "kubernetes-apps-admin-kubeconfig" {
				t.Fatal(hr)
			}
			if name == "app-cluster-prod" && v["cosignPublicKey"] != "PUBLIC KEY" {
				t.Fatal("missing production key")
			}
		}
	}
	o.Spec.Profile = "small"
	if len(Tiers(o)) != 1 || policyValues(o, "nonprod", "")["app-cluster-prod"] != nil {
		t.Fatal("small must use nonprod policies")
	}
}
func TestPolicyChartRendering(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	o := organization()
	o.Spec.Policy.Mode = "audit"
	for _, tier := range Tiers(o) {
		for name, values := range policyValues(o, tier, "test-public-key") {
			if name == "kyverno" {
				continue
			}
			t.Run(tier+"/"+name, func(t *testing.T) {
				data, _ := json.Marshal(values)
				path := filepath.Join(t.TempDir(), "values.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				data, err := exec.Command("helm", "template", "test", "../../policies/"+name, "-f", path).CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, data)
				}
				rendered := string(data)
				if name == "app-cluster-prod" && (!strings.Contains(rendered, "validationFailureAction: Enforce") || !strings.Contains(rendered, "mutateDigest: false") || !strings.Contains(rendered, "verifyDigest: true") || strings.Contains(rendered, "keyless:")) {
					t.Fatal(rendered)
				}
				if name == "app-cluster-baseline" && (!strings.Contains(rendered, "validationFailureAction: Audit") || !strings.Contains(rendered, "validationFailureAction: Enforce") || !strings.Contains(rendered, "org-uid")) {
					t.Fatal(rendered)
				}
				if name == "app-cluster-nonprod" && (!strings.Contains(rendered, "level: restricted") || !strings.Contains(rendered, "validationFailureAction: Enforce")) {
					t.Fatal(rendered)
				}
			})
		}
	}
}

func TestPolicyConfigurationValidation(t *testing.T) {
	o := organization()
	s := &session{o: o, r: &Reconciler{Management: &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}}}
	if err := s.policies(context.Background()); err == nil || !strings.Contains(err.Error(), "cosignPublicKey") {
		t.Fatal("production must fail closed without a key", err)
	}
	for _, mode := range []string{"off", "Enforce"} {
		o.Spec.Policy.Mode = mode
		if Validate(o) == nil {
			t.Fatal("accepted invalid mode", mode)
		}
	}
	o.Spec.Policy.Mode = "enforce"
	for _, registry := range []string{"https://registry.example", "registry.example/team", "*.example", ""} {
		o.Spec.Policy.Registries = []string{registry}
		if Validate(o) == nil {
			t.Fatal("accepted invalid registry", registry)
		}
	}
	o.Spec.Policy.Registries = []string{"registry.example:5000"}
	if err := Validate(o); err != nil {
		t.Fatal(err)
	}
	clone := o.DeepCopy()
	clone.Spec.Policy.Registries[0] = "other.example"
	if o.Spec.Policy.Registries[0] == clone.Spec.Policy.Registries[0] {
		t.Fatal("policy slices alias")
	}
}

func TestMissingKeyDeliversIndependentBundles(t *testing.T) {
	ctx := context.Background()
	o := organization()
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	s := &session{o: o, r: &Reconciler{Management: &Cluster{Dynamic: d}}}
	if err := s.policies(ctx); !errors.Is(err, errCosignKeyMissing) {
		t.Fatal(err)
	}
	for _, tier := range Tiers(o) {
		for _, name := range []string{"kyverno", "app-cluster-baseline", "app-cluster-" + tier} {
			_, err := d.Resource(gvr(helmAPI, "helmreleases")).Namespace(TenantNamespace(o.Name, tier)).Get(ctx, "aep-"+name, metav1.GetOptions{})
			if name == "app-cluster-prod" {
				if err == nil {
					t.Fatal("delivered production without key")
				}
				continue
			}
			if err != nil {
				t.Fatal(name, err)
			}
		}
	}
	d.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		patch := a.(ktesting.PatchAction)
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(patch.GetPatch(), &u.Object); err != nil {
			return true, nil, err
		}
		return true, u, d.Tracker().Update(a.GetResource(), u, a.GetNamespace())
	})
	o.Spec.Policy.CosignPublicKey = "public-key"
	_ = s.policies(ctx)
	if _, err := d.Resource(gvr(helmAPI, "helmreleases")).Namespace(TenantNamespace(o.Name, "prod")).Get(ctx, "aep-app-cluster-prod", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyFailureDoesNotGateAccess(t *testing.T) {
	for _, policyErr := range []error{errCosignKeyMissing, errors.New("release not ready")} {
		o := organization()
		scheme := runtime.NewScheme()
		if err := api.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(o).WithObjects(o).Build()}
		accessed := false
		_, err := r.runSteps(context.Background(), o, []reconcileStep{
			{"TenantsReady", func(context.Context) error { return nil }},
			{"PoliciesReady", func(context.Context) error { return policyErr }},
			{"AccessReady", func(context.Context) error { accessed = true; return nil }},
		})
		if err != nil || !accessed {
			t.Fatal("access blocked", err)
		}
		if !meta.IsStatusConditionTrue(o.Status.Conditions, "AccessReady") || !meta.IsStatusConditionFalse(o.Status.Conditions, "PoliciesReady") || o.Status.Phase != "Pending" {
			t.Fatal(o.Status)
		}
		if errors.Is(policyErr, errCosignKeyMissing) && meta.FindStatusCondition(o.Status.Conditions, "PoliciesReady").Reason != "CosignKeyMissing" {
			t.Fatal(o.Status)
		}
	}
}

func TestPlatformTrustConfiguration(t *testing.T) {
	o := organization()
	o.Spec.Policy.PlatformNamespaces = []string{"addon-monitoring"}
	o.Spec.Policy.PlatformImageTrust = api.PlatformImageTrust{Mode: "enforce", VendorPublicKeys: []string{"vendor-key"}, AllowedImages: []string{"registry.example/agent@sha256:" + strings.Repeat("a", 64)}}
	if err := Validate(o); err != nil {
		t.Fatal(err)
	}
	v := policyValues(o, "prod", "org-key")["app-cluster-prod"]
	if v["platformNamespaces"].([]any)[0] != "addon-monitoring" || v["platformImageTrust"].(M)["mode"] != "enforce" {
		t.Fatal(v)
	}
	clone := o.DeepCopy()
	clone.Spec.Policy.PlatformNamespaces[0] = "changed"
	clone.Spec.Policy.PlatformImageTrust.VendorPublicKeys[0] = "changed"
	clone.Spec.Policy.PlatformImageTrust.AllowedImages[0] = "changed"
	if o.Spec.Policy.PlatformNamespaces[0] == "changed" || o.Spec.Policy.PlatformImageTrust.VendorPublicKeys[0] == "changed" || o.Spec.Policy.PlatformImageTrust.AllowedImages[0] == "changed" {
		t.Fatal("aliased trust config")
	}
	for _, ns := range []string{"dp-app", "dp-*", "*", "user-*", ""} {
		o.Spec.Policy.PlatformNamespaces = []string{ns}
		if Validate(o) == nil {
			t.Fatal("accepted unsafe exclusion", ns)
		}
	}
}

func TestBaselinePlatformNamespacesAreAdditive(t *testing.T) {
	o := organization()
	o.Spec.Policy.PlatformNamespaces = []string{"custom-addon"}
	values := policyValues(o, "nonprod", "")["app-cluster-baseline"]
	namespaces := values["platformNamespaces"].([]any)
	for _, want := range []string{"kube-system", "argocd", "custom-addon"} {
		found := false
		for _, got := range namespaces {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatal("missing platform namespace", want)
		}
	}
}
