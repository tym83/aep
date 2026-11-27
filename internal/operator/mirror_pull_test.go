package operator

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPullSecretValuesPerChart(t *testing.T) {
	for chart, path := range map[string][]string{"kyverno": {"global", "imagePullSecrets"}, "argo-cd": {"global", "imagePullSecrets"}, "openchoreo-data-plane": {"global", "imagePullSecrets"}, "kgateway": {"imagePullSecrets"}} {
		v := PullSecretValues(chart, M{})
		got, ok, _ := unstructured.NestedSlice(v, path...)
		if !ok || len(got) != 1 || got[0].(M)["name"] != MirrorPullSecret {
			t.Fatalf("%s: %v", chart, v)
		}
	}
	if v := PullSecretValues("kgateway-crds", M{}); len(v) != 0 {
		t.Fatal("charts without pods must be unchanged", v)
	}
}

func TestDeliverMirrorPull(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	c := s.tiers["nonprod"]
	if err := s.deliverMirrorPull(ctx, c); err != nil {
		t.Fatal("no mirror must be a no-op", err)
	}
	s.r.Options.ImageRegistryMirror, s.r.Options.RegistrySecret = "mirror.example/aep", "mirror-auth"
	kube := s.r.Management.Kube.CoreV1().Secrets("aep-system")
	if _, err := kube.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mirror-auth"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"password": []byte("x")}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverMirrorPull(ctx, c); err == nil || !strings.Contains(err.Error(), "dockerconfigjson") {
		t.Fatal("non-registry Secret must be rejected", err)
	}
	src, _ := kube.Get(ctx, "mirror-auth", metav1.GetOptions{})
	src.Type, src.Data = corev1.SecretTypeDockerConfigJson, map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"mirror.example":{}}}`)}
	if _, err := kube.Update(ctx, src, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverMirrorPull(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"openchoreo-data-plane", "kyverno", "argocd"} {
		u, err := c.get(ctx, "v1", "secrets", ns, MirrorPullSecret)
		if err != nil || u.Object["type"] != "kubernetes.io/dockerconfigjson" || dataValue(t, u, ".dockerconfigjson") != `{"auths":{"mirror.example":{}}}` {
			t.Fatal(ns, err)
		}
	}
	if !s.r.Options.mirrorPull() {
		t.Fatal("mirror with a registry Secret must enable pulls")
	}
}
