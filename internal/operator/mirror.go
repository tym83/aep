package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/registrymirror"
	inventory "github.com/tym83/aep/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"
)

func (o Options) chartSource(source *unstructured.Unstructured) error {
	spec := source.Object["spec"].(map[string]any)
	url := spec["url"].(string)
	version := ""
	if ref, ok := spec["ref"].(map[string]any); ok {
		version, _ = ref["tag"].(string)
	}
	canonical := url
	if o.ChartRegistryMirror != "" {
		canonical = strings.Replace(canonical, "oci://"+o.ChartRegistryMirror+"/", "oci://", 1)
	}
	a, err := inventory.Lookup("chart", canonical, version)
	if err != nil {
		return err
	}
	spec["ref"] = M{"digest": a.Digest}
	spec["url"] = registrymirror.Rewrite(spec["url"].(string), o.ChartRegistryMirror)
	if o.RegistryInsecure && o.ChartRegistryMirror != "" {
		spec["insecure"] = true
	}
	if o.RegistrySecret != "" {
		spec["secretRef"] = M{"name": o.RegistrySecret}
	}
	if o.RegistryCASecret != "" {
		spec["certSecretRef"] = M{"name": o.RegistryCASecret}
	}
	return nil
}

// MirrorValues uses each pinned upstream chart's own value contract. In particular,
// Kyverno has a different registry for hooks, and kgateway creates proxy pods later.
func MirrorValues(chart, mirror string, values M) M {
	values = PinValues(chart, values)
	if mirror == "" {
		return values
	}
	switch chart {
	case "argo-cd":
		setMap(values, []string{"global", "image", "repository"}, mirror+"/quay.io/argoproj/argocd")
		setMap(values, []string{"redis", "image", "repository"}, mirror+"/ecr-public.aws.com/docker/library/redis")
	case "openchoreo-data-plane":
		values["global"] = M{"imageRegistry": mirror + "/cr.openchoreo.dev"}
	case "kgateway":
		setMap(values, []string{"image", "registry"}, mirror+"/cr.kgateway.dev/kgateway-dev")
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
	return append(out, struct{ Name, Version, URL string }{"kyverno", KyvernoChartVersion, "oci://ghcr.io/kyverno/charts/kyverno"}, struct{ Name, Version, URL string }{"argo-cd", ArgoCDChartVersion, ArgoCDChartOCI})
}

// ReleaseValues is the same chart configuration used by reconciliation, with
// representative identity values (which do not affect image selection).
func ReleaseValues(chart string) M {
	if chart == "argo-cd" {
		return PinValues(chart, argoValues(&api.Organization{}, "argo.inventory.invalid", "https://id.invalid", nil))
	}
	if chart == "kyverno" {
		return PinValues(chart, policyValues(&api.Organization{}, "prod", "")["kyverno"])
	}
	if chart == "openchoreo-data-plane" {
		return PinValues(chart, DataPlaneValues("inventory", "wss://gateway.invalid"))
	}
	return PinValues(chart, M{})
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

// Each upstream chart accepts a tag string; tag@digest preserves the release
// label while making runtime pulls independent of mutable registry tags.
func PinValues(chart string, values M) M {
	pin := func(ref string, path ...string) {
		a, err := inventory.Lookup("image", ref, "")
		if err != nil {
			panic(err)
		} // Embedded inventory is validated by release checks.
		setMap(values, path, a.Version+"@"+a.Digest)
	}
	switch chart {
	case "argo-cd":
		pin("quay.io/argoproj/argocd", "global", "image", "tag")
		pin("ecr-public.aws.com/docker/library/redis", "redis", "image", "tag")
	case "kgateway":
		pin("cr.kgateway.dev/kgateway-dev/kgateway", "controller", "image", "tag")
		pin("cr.kgateway.dev/kgateway-dev/envoy-wrapper", "image", "tag")
	case "openchoreo-data-plane":
		pin("cr.openchoreo.dev/openchoreo/cluster-agent", "clusterAgent", "image", "tag")
	case "kyverno":
		for _, p := range []struct {
			repo string
			path []string
		}{
			{"kyvernopre", []string{"admissionController", "initContainer", "image", "tag"}},
			{"kyverno", []string{"admissionController", "container", "image", "tag"}},
			{"background-controller", []string{"backgroundController", "image", "tag"}},
			{"cleanup-controller", []string{"cleanupController", "image", "tag"}},
			{"reports-controller", []string{"reportsController", "image", "tag"}},
			{"kyverno-cli", []string{"crds", "migration", "image", "tag"}},
		} {
			pin("reg.kyverno.io/kyverno/"+p.repo, p.path...)
		}
		for _, p := range []string{"test", "webhooksCleanup"} {
			pin("ghcr.io/kyverno/readiness-checker", p, "image", "tag")
		}
	}
	return values
}

// Helm may already have rewritten an operator image argument. Validate the
// original repository and digest before returning it through the mirror again.
func (o Options) pinnedImage(ref string) (string, error) {
	if o.ImageRegistryMirror != "" {
		ref = strings.TrimPrefix(ref, o.ImageRegistryMirror+"/")
	}
	pinned, err := inventory.Image(ref)
	if err != nil {
		return "", err
	}
	return registrymirror.Rewrite(pinned, o.ImageRegistryMirror), nil
}
