package operator

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestControlPlaneValues(t *testing.T) {
	v := ControlPlaneValues(ControlPlaneSettings{Domain: "cp.example.org", Issuer: "https://id.example.org/realms/aep", GatewayHost: "gw.cp.example.org", PortalTag: "v1.2.3"})
	for want, path := range map[string][]string{
		"https://api.cp.example.org":        {"openchoreoApi", "config", "server", "publicUrl"},
		"https://console.cp.example.org":    {"backstage", "baseUrl"},
		PortalSecret:                        {"backstage", "secretName"},
		PortalImage:                         {"backstage", "image", "repository"},
		"v1.2.3":                            {"backstage", "image", "tag"},
		"https://id.example.org/realms/aep": {"security", "oidc", "issuer"},
		"https://id.example.org/realms/aep/protocol/openid-connect/certs": {"security", "oidc", "jwksUrl"},
		"https://id.example.org/realms/aep/protocol/openid-connect/token": {"security", "oidc", "tokenUrl"},
		"LoadBalancer": {"clusterGateway", "service", "type"},
	} {
		if got, _, _ := unstructured.NestedString(v, path...); got != want {
			t.Errorf("%v = %q, want %q", path, got, want)
		}
	}
	if tls, _, _ := unstructured.NestedBool(v, "gateway", "tls", "enabled"); tls {
		t.Error("gateway TLS ends at the management ingress")
	}
	names, _, _ := unstructured.NestedSlice(v, "clusterGateway", "tls", "dnsNames")
	if !reflect.DeepEqual(names, []any{"cluster-gateway.openchoreo-control-plane.svc", "cluster-gateway.openchoreo-control-plane.svc.cluster.local", "gw.cp.example.org"}) {
		t.Error("gateway certificate must keep in-cluster names and add the agent host", names)
	}
	b, _ := json.Marshal(ReleaseValues(ControlPlaneChart))
	if strings.Contains(string(b), ".invalid") {
		t.Error("the chart rejects .invalid placeholder domains", string(b))
	}
}

func TestControlPlaneMirrorValues(t *testing.T) {
	v := MirrorValues(ControlPlaneChart, "mirror.local/cache", ReleaseValues(ControlPlaneChart))
	if _, ok := v["global"]; ok {
		t.Fatal("global.imageRegistry would move aep-portal under the OpenChoreo registry path", v["global"])
	}
	if repo, _, _ := unstructured.NestedString(v, "backstage", "image", "repository"); repo != "mirror.local/cache/"+PortalImage {
		t.Fatal(repo)
	}
	for _, image := range controlPlaneImages {
		repo, _, _ := unstructured.NestedString(v, append(append([]string{}, image.path...), "repository")...)
		tag, _, _ := unstructured.NestedString(v, append(append([]string{}, image.path...), "tag")...)
		if repo != "mirror.local/cache/"+image.ref || !strings.HasPrefix(tag, "v1.4.0-m.3@sha256:") {
			t.Fatal(image.path, repo, tag)
		}
	}
	bootstrap, _, _ := unstructured.NestedString(v, authzBootstrapImagePath...)
	if !strings.HasPrefix(bootstrap, "mirror.local/cache/"+AuthzBootstrapImage+":v1.36.0@sha256:") {
		t.Fatal(bootstrap)
	}
	eso := MirrorValues(ExternalSecretsChart, "mirror.local/cache", ReleaseValues(ExternalSecretsChart))
	if repo, _, _ := unstructured.NestedString(eso, "global", "repository"); repo != "mirror.local/cache/"+externalSecretsImage {
		t.Fatal(eso)
	}
	for _, path := range externalSecretsImages {
		if tag, _, _ := unstructured.NestedString(eso, append(append([]string{}, path...), "tag")...); !strings.HasPrefix(tag, "v2.0.1@sha256:") {
			t.Fatal(path, tag)
		}
	}
}

// Reconciliation applies values on every pass; mirroring twice must not add a
// second registry prefix.
func TestMirrorValuesIdempotent(t *testing.T) {
	for _, chart := range DeliveryCharts() {
		once := MirrorValues(chart.Name, "mirror.local/cache", ReleaseValues(chart.Name))
		a, _ := json.Marshal(once)
		b, _ := json.Marshal(MirrorValues(chart.Name, "mirror.local/cache", once))
		if string(a) != string(b) {
			t.Fatalf("%s: %s != %s", chart.Name, a, b)
		}
		if strings.Contains(string(a), "mirror.local/cache/mirror.local/cache") {
			t.Fatalf("%s: double prefix %s", chart.Name, a)
		}
	}
}
