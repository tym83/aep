package operator

import (
	"context"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRegistryPullDelivery(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	c := s.tiers["nonprod"]
	// Without a reference nothing is delivered and nothing fails.
	if err := s.registryPull(ctx, "nonprod"); err != nil {
		t.Fatal(err)
	}
	s.o.Spec.Policy.RegistryPullSecretRef = &api.SecretNameReference{Name: "acme-pull"}
	if err := s.registryPull(ctx, "nonprod"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal("missing source must be pending", err)
	}
	kube := s.r.Management.Kube.CoreV1().Secrets("aep-system")
	if _, err := kube.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "acme-pull"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"token": []byte("x")}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.registryPull(ctx, "nonprod"); err == nil || !strings.Contains(err.Error(), "dockerconfigjson") {
		t.Fatal("non-registry Secret must be rejected", err)
	}
	src, _ := kube.Get(ctx, "acme-pull", metav1.GetOptions{})
	src.Type, src.Data = corev1.SecretTypeDockerConfigJson, map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)}
	if _, err := kube.Update(ctx, src, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.registryPull(ctx, "nonprod"); err != nil {
		t.Fatal(err)
	}
	copy, err := c.get(ctx, "v1", "secrets", "aep-system", RegistryPullSecret)
	if err != nil {
		t.Fatal(err)
	}
	if copy.Object["type"] != "kubernetes.io/dockerconfigjson" || dataValue(t, copy, ".dockerconfigjson") != `{"auths":{}}` || copy.GetLabels()[OwnerLabel] != string(s.o.UID) {
		t.Fatal(copy.Object)
	}
	// Removing the reference removes the owned tier copy.
	s.o.Spec.Policy.RegistryPullSecretRef = nil
	if err := s.registryPull(ctx, "nonprod"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(ctx, "v1", "secrets", "aep-system", RegistryPullSecret); err == nil {
		t.Fatal("tier copy must be removed")
	}
}

func TestRegistryPullPolicyValues(t *testing.T) {
	o := organization()
	if policyValues(o, "nonprod", "")["app-cluster-baseline"]["registryPull"].(M)["enabled"] != false {
		t.Fatal("generation must be off without a reference")
	}
	o.Spec.Policy.RegistryPullSecretRef = &api.SecretNameReference{Name: "acme-pull"}
	v := policyValues(o, "prod", "")["app-cluster-baseline"]["registryPull"].(M)
	if v["enabled"] != true || v["secretName"] != RegistryPullSecret || v["organizationNamespace"] != o.Name {
		t.Fatal(v)
	}
}
