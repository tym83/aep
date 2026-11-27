package operator

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestSwitchChartSourceDropsStaleField(t *testing.T) {
	old := obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "tenant-acme-nonprod", "aep-argocd", M{
		"chart": M{"spec": M{"chart": "argo-cd", "sourceRef": M{"kind": "HelmRepository", "name": "aep-argocd"}}}})
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}: "HelmReleaseList"}, old)
	c := &Cluster{Dynamic: d}
	want := obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "tenant-acme-nonprod", "aep-argocd", M{"chartRef": M{"kind": "OCIRepository", "name": "aep-argocd"}})
	if err := c.switchChartSource(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := c.get(context.Background(), "helm.toolkit.fluxcd.io/v2", "helmreleases", "tenant-acme-nonprod", "aep-argocd")
	if err != nil {
		t.Fatal(err)
	}
	if _, has, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "chart"); has {
		t.Fatal("stale spec.chart kept")
	}
	if kind, _, _ := unstructured.NestedString(got.Object, "spec", "chartRef", "kind"); kind != "OCIRepository" {
		t.Fatal("chartRef not set", got.Object["spec"])
	}
	// Already switched: no further patch.
	before := len(d.Actions())
	if err := c.switchChartSource(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Actions()[before:] {
		if a.GetVerb() != "get" {
			t.Fatal("patched an object already on the wanted source", a)
		}
	}
}
