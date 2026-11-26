package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/registrymirror"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"
)

func (o Options) chartSource(source *unstructured.Unstructured) {
	spec := source.Object["spec"].(map[string]any)
	spec["url"] = registrymirror.Rewrite(spec["url"].(string), o.ChartRegistryMirror)
	if o.RegistryInsecure {
		spec["insecure"] = true
	}
	if o.RegistrySecret != "" {
		spec["secretRef"] = M{"name": o.RegistrySecret}
	}
	if o.RegistryCASecret != "" {
		spec["certSecretRef"] = M{"name": o.RegistryCASecret}
	}
}

// MirrorValues uses each pinned upstream chart's own value contract. In particular,
// Kyverno has a different registry for hooks, and kgateway creates proxy pods later.
func MirrorValues(chart, mirror string, values M) M {
	if mirror == "" {
		return values
	}
	switch chart {
	case "openchoreo-data-plane":
		values["global"] = M{"imageRegistry": mirror + "/cr.openchoreo.dev"}
	case "kgateway":
		values["image"] = M{"registry": mirror + "/cr.kgateway.dev/kgateway-dev"}
	case "kyverno":
		// Do not use global.image.registry: hooks use ghcr.io instead of reg.kyverno.io.
		for _, path := range [][]string{{"admissionController", "initContainer", "image"}, {"admissionController", "container", "image"}, {"backgroundController", "image"}, {"cleanupController", "image"}, {"reportsController", "image"}, {"crds", "migration", "image"}} {
			setMap(values, append(path, "registry"), mirror+"/reg.kyverno.io")
		}
		for _, key := range []string{"test", "webhooksCleanup"} {
			setMap(values, []string{key, "image", "registry"}, mirror+"/ghcr.io")
		}
	}
	return values
}
func setMap(m M, path []string, value any) {
	for _, key := range path[:len(path)-1] {
		next, ok := m[key].(map[string]any)
		if !ok {
			next = M{}
			m[key] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

// DeliveryCharts exposes the exact operator chart pins to release inventory generation.
func DeliveryCharts() []struct{ Name, Version, URL string } {
	out := []struct{ Name, Version, URL string }{}
	for _, c := range charts {
		out = append(out, struct{ Name, Version, URL string }{c.name, c.version, c.url + "/" + c.name})
	}
	return append(out, struct{ Name, Version, URL string }{"kyverno", KyvernoChartVersion, "oci://ghcr.io/kyverno/charts/kyverno"})
}

// ReleaseValues is the same chart configuration used by reconciliation, with
// representative identity values (which do not affect image selection).
func ReleaseValues(chart string) M {
	if chart == "kyverno" {
		return policyValues(&api.Organization{}, "prod", "")["kyverno"]
	}
	if chart == "openchoreo-data-plane" {
		return DataPlaneValues("inventory", "wss://gateway.invalid")
	}
	return M{}
}
func DataPlaneValues(plane, gateway string) M {
	return M{"gateway": M{"tls": M{"enabled": false}}, "clusterAgent": M{"planeID": plane, "serverUrl": gateway, "tls": M{"generateCerts": false, "caSecretName": "aep-agent-ca"}}}
}

// Normalize is called once before starting reconciliation.
func (o *Options) Normalize() {
	o.ImageRegistryMirror = strings.TrimRight(strings.TrimPrefix(o.ImageRegistryMirror, "oci://"), "/")
	o.ChartRegistryMirror = strings.TrimRight(strings.TrimPrefix(o.ChartRegistryMirror, "oci://"), "/")
}

// Source credentials are namespace-local in Flux. Copy only explicitly configured
// secrets, using organization ownership checks to avoid adopting tenant data.
func (s *session) mirrorSecrets(ctx context.Context, ns string) error {
	for _, name := range []string{s.r.Options.RegistrySecret, s.r.Options.RegistryCASecret} {
		if name == "" {
			continue
		}
		source, err := s.r.Management.Kube.CoreV1().Secrets("aep-system").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		target := secretObject(ns, name, source.Data)
		target.Object["type"] = string(source.Type)
		if err := s.r.Management.apply(ctx, s.o, "secrets", target); err != nil {
			return err
		}
	}
	return nil
}
