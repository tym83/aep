package operator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	s := &session{o: o}
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
