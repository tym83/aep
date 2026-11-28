package operator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
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

func goldenSettings() ControlPlaneSettings {
	return ControlPlaneSettings{Domain: "cp.example.org", Issuer: "https://id.example.org/realms/aep", GatewayHost: "cluster-gateway.cp.aep.internal",
		PortalTag: "v1.2.3@sha256:0000000000000000000000000000000000000000000000000000000000000000", PortalClientID: "aep-cp-portal",
		ReadAPIURL: "https://read.example.org", ChangeAPIURL: "https://change.example.org", OIDCCA: true}
}

func TestControlPlaneValuesGolden(t *testing.T) {
	got, err := yaml.Marshal(ControlPlaneValues(goldenSettings()))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile("testdata/controlplane-values.yaml", got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/controlplane-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("ControlPlaneValues drifted from testdata/controlplane-values.yaml (UPDATE_GOLDEN=1 regenerates):\n%s", got)
	}
}

// The control plane never disables TLS verification, and its bootstrap
// mappings grant only system clients, so hardening finds nothing to prune.
func TestControlPlaneValuesSafety(t *testing.T) {
	for _, s := range []ControlPlaneSettings{goldenSettings(), inventoryControlPlane} {
		v := ControlPlaneValues(s)
		b, _ := json.Marshal(v)
		for _, unsafe := range []string{"NODE_TLS_REJECT_UNAUTHORIZED", `"skip_tls_verify":true`, `InsecureSkipVerify":true`} {
			if strings.Contains(string(b), unsafe) {
				t.Fatalf("unsafe %s in %s", unsafe, b)
			}
		}
		if strings.Contains(string(b), "NODE_EXTRA_CA_CERTS") != s.OIDCCA {
			t.Fatal("the extra CA bundle is mounted exactly when configured")
		}
		mappings, _, _ := unstructured.NestedSlice(v, "openchoreoApi", "config", "security", "authorization", "bootstrap", "mappings")
		if len(mappings) == 0 {
			t.Fatal("bootstrap mappings must replace the chart defaults")
		}
		allowed := DefaultBootstrapBindingSystemClients + "," + orDefault(s.PortalClientID, "openchoreo-backstage-client")
		for _, m := range mappings {
			m := m.(M)
			binding := obj(ocAPI, "ClusterAuthzRoleBinding", "", m["name"].(string), M{"entitlement": m["entitlement"]})
			if bootstrapBindingOffends(binding, allowed) {
				t.Fatalf("bootstrap mapping %v would be pruned", m)
			}
		}
	}
	defaults := ControlPlaneValues(ControlPlaneSettings{Domain: "cp.example.org", Issuer: "https://id.example.org/realms/aep"})
	if names, _, _ := unstructured.NestedSlice(defaults, "clusterGateway", "tls", "dnsNames"); len(names) != 2 {
		t.Fatal("an unknown gateway host is not placed in the certificate", names)
	}
	for want, path := range map[string][]string{
		"preferred_username": {"openchoreoApi", "config", "security", "subjects", "user", "mechanisms", "jwt", "readable_id_claim"},
		"https://id.example.org/realms/aep/.well-known/openid-configuration": {"security", "oidc", "wellKnownEndpoint"},
		"openchoreo-backstage-client":                                        {"backstage", "auth", "clientId"},
	} {
		if got, _, _ := unstructured.NestedString(defaults, path...); got != want {
			t.Errorf("%v = %q, want %q", path, got, want)
		}
	}
}

// Render the pinned upstream chart with the control plane values: the chart
// must accept them, and the rendered bootstrap hook must create no group or
// shared publisher binding.
func TestControlPlaneChartRendering(t *testing.T) {
	dir := os.Getenv("AEP_UPSTREAM_CHARTS")
	if dir == "" {
		t.Skip("set AEP_UPSTREAM_CHARTS for upstream Helm rendering")
	}
	values := PullSecretValues(ControlPlaneChart, "openchoreo-control-plane", MirrorValues(ControlPlaneChart, "mirror.local/cache", ControlPlaneValues(goldenSettings())))
	b, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("helm", "template", ControlPlaneChart, filepath.Join(dir, ControlPlaneChart), "--namespace", "openchoreo-control-plane", "--values", "-")
	cmd.Stdin = strings.NewReader(string(b))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	rendered := string(out)
	for _, want := range []string{"mirror.local/cache/ghcr.io/tym83/aep-portal:v1.2.3@sha256:", "app-config.production.yaml", "skip_tls_verify: false", "readable_id_claim: preferred_username",
		"name: " + authzBootstrapName, "serviceAccountName: " + authzBootstrapName, "value: aep-cp-portal", `- "cluster-gateway.cp.aep.internal"`, "NODE_EXTRA_CA_CERTS"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered chart lacks %q", want)
		}
	}
	for _, unsafe := range []string{"NODE_TLS_REJECT_UNAUTHORIZED", "skip_tls_verify: true", "value: admins", "value: developers", "service_mcp_client", "workload-publisher-binding"} {
		if strings.Contains(rendered, unsafe) {
			t.Errorf("rendered chart contains %q", unsafe)
		}
	}
	if n := strings.Count(rendered, "kind: ClusterAuthzRoleBinding"); n != 4 {
		t.Errorf("rendered %d bootstrap bindings, want the 4 system ones", n)
	}
}
