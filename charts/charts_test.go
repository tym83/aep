package charts

import (
	"encoding/json"
	"github.com/tym83/aep/internal/release"
	"os"
	"os/exec"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func render(t *testing.T, chart string, values ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart tests")
	}
	args := []string{"template", "test", "./" + chart}
	for _, v := range values {
		args = append(args, "--set", v)
	}
	cmd := exec.Command("helm", args...)
	if chart == "aep-gitlab" {
		version, _ := exec.Command("helm", "version", "--short").Output()
		cmd.Env = os.Environ()
		if strings.HasPrefix(string(version), "v4.") {
			plugins := t.TempDir()
			source, err := filepath.Abs("aep-gitlab/postrenderer")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(source, filepath.Join(plugins, "aep-images")); err != nil {
				t.Fatal(err)
			}
			cmd.Env = append(cmd.Env, "HELM_PLUGINS="+plugins)
			cmd.Args = append(cmd.Args, "--post-renderer", "aep-images")
		} else {
			// Helm 3 excludes hooks from its built-in post-renderer.
			cmd.Args = append([]string{"./aep-gitlab/render.sh"}, args[1:]...)
			cmd.Path = "./aep-gitlab/render.sh"
		}
		for _, v := range values {
			if strings.HasPrefix(v, "global.imageRegistry=") {
				cmd.Env = append(cmd.Env, "AEP_REGISTRY_MIRROR="+strings.TrimPrefix(v, "global.imageRegistry="))
			}
		}
	}
	b, err := cmd.CombinedOutput()
	return string(b), err
}
func agentValues() []string {
	return []string{"organization=acme", "secretName=agent", "llm.modelName=model", "llm.baseUrl=http://llm:8000/v1", "oauth.tokenUrl=https://issuer/token", "jwt.issuer=https://issuer", "jwt.audience=agent", "jwt.jwksUrl=https://issuer/certs"}
}
func resources(t *testing.T, rendered string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, doc := range strings.Split(rendered, "\n---") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		if kind, ok := m["kind"].(string); ok {
			out[kind] = m
		}
	}
	return out
}
func TestAgentTokenProxy(t *testing.T) {
	out, err := render(t, "aep-sre-agent", append(agentValues(), "dev=true")...)
	if err != nil {
		t.Fatal(out, err)
	}
	docs := resources(t, out)
	data := docs["ConfigMap"]["data"].(map[string]any)
	var config struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(data["mcp.json"].(string)), &config); err != nil {
		t.Fatal(err)
	}
	if config.Servers["cozystack"].URL != "http://127.0.0.1:8081/mcp" || len(config.Servers["cozystack"].Headers) != 0 {
		t.Fatal(config)
	}
	if strings.Contains(out, "COZYSTACK_MCP_TOKEN") || !strings.Contains(out, "name: token-proxy") || !strings.Contains(out, "key: OAUTH_CLIENT_SECRET") || !strings.Contains(out, "--listen=127.0.0.1:8081") {
		t.Fatal(out)
	}
}

func TestLLMAuthenticationAndIsolation(t *testing.T) {
	out, err := render(t, "aep-llm", "model=test")
	if err == nil || !strings.Contains(out, "apiKeySecret is required") {
		t.Fatal(out, err)
	}
	for _, auth := range []string{"apiKeySecret=llm-key", "allowUnauthenticated=true"} {
		out, err := render(t, "aep-llm", "model=test", auth)
		if err != nil {
			t.Fatal(out, err)
		}
		docs := resources(t, out)
		pod := docs["Deployment"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		sc := pod["securityContext"].(map[string]any)
		if sc["runAsNonRoot"] != true || sc["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" {
			t.Fatal(sc)
		}
		c := pod["containers"].([]any)[0].(map[string]any)
		cs := c["securityContext"].(map[string]any)
		if cs["allowPrivilegeEscalation"] != false || cs["capabilities"].(map[string]any)["drop"].([]any)[0] != "ALL" {
			t.Fatal(cs)
		}
		np := docs["NetworkPolicy"]["spec"].(map[string]any)
		ingress := np["ingress"].([]any)[0].(map[string]any)
		peers := ingress["from"].([]any)
		if len(peers) != 1 || peers[0].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)["app.kubernetes.io/name"] != "aep-sre-agent" {
			t.Fatal(peers)
		}
		if ingress["ports"].([]any)[0].(map[string]any)["port"] != float64(8000) {
			t.Fatal(ingress)
		}
	}
	out, err = render(t, "aep-llm", "model=test", "apiKeySecret=key", "networkPolicy.allowedSources[0].namespaceSelector.matchLabels.team=operations")
	if err != nil || !strings.Contains(out, "team: operations") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-llm", "model=test", "apiKeySecret=key", "networkPolicy.allowedSources=null")
	if err != nil || !strings.Contains(out, "ingress: []") {
		t.Fatal(out, err)
	}
}

func TestDigestRequirements(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	out, err := render(t, "aep-sre-agent", append(agentValues(), "image.digest=", "tokenProxy.image.digest=")...)
	if err == nil || !strings.Contains(out, "digest is required") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-sre-agent", append(agentValues(), "image.digest=", "tokenProxy.image.digest="+digest)...)
	if err == nil || !strings.Contains(out, "image.digest is required") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-sre-agent", append(agentValues(), "tokenProxy.image.digest=", "image.digest="+digest)...)
	if err != nil {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-sre-agent", append(agentValues(), "image.digest="+digest, "tokenProxy.image.digest="+digest)...)
	if err != nil || strings.Count(out, "@"+digest) != 2 {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-llm", "model=test", "apiKeySecret=key", "image.digest=")
	if err == nil || !strings.Contains(out, "image.digest is required") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-llm", "model=test", "apiKeySecret=key", "image.digest=", "dev=true")
	if err != nil || !strings.Contains(out, "vllm/vllm-openai:v0.11.0") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-llm", "model=test", "apiKeySecret=key")
	if err != nil || !strings.Contains(out, "vllm/vllm-openai@sha256:014a95f21c9edf6abe0aea6b07353f96baa4ec291c427bb1176dc7c93a85845c") {
		t.Fatal(out, err)
	}
}

func TestChartLint(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart tests")
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		chart  string
		values []string
	}{
		{"aep-sre-agent", append(agentValues(), "image.digest="+digest, "tokenProxy.image.digest="+digest)},
		{"aep-llm", []string{"model=test", "apiKeySecret=key"}},
	} {
		args := []string{"lint", "--strict", "./" + tc.chart}
		for _, v := range tc.values {
			args = append(args, "--set", v)
		}
		if out, err := exec.Command("helm", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", tc.chart, err, out)
		}
	}
}

func TestMirrorAllCharts(t *testing.T) {
	cases := map[string][]string{
		"aep-gitlab":            {"gitlab.global.ingress.class=nginx"},
		"aep-operator":          {"aepcpKubeconfigSecret=cp/access"},
		"aep-bridge":            {"organization=acme", "clusterID=acme"},
		"aep-bridge-appcluster": {},
		"aep-change":            {"organizations[0]=acme", "oidc.issuer=https://id.invalid", "oidc.audience=aep", "openchoreo.apiUrl=https://oc.invalid", "openchoreo.tokenUrl=https://id.invalid/token"},
		"aep-read-api":          {"oidc.issuer=https://id.invalid", "oidc.audience=aep"},
		"aep-cost-ledger":       {"credentialsSecret=database"},
		"aep-llm":               {"model=test", "apiKeySecret=key"},
		"aep-sre-agent":         agentValues(),
		"aep-audit-forwarder":   {},
		"aep-cp-backup":         {},
		"aep-itsm-bridge":       {"credentialsSecret=itsm", "config.organization=acme", "config.changeNamespace=tenant-acme"},
	}
	paths, err := filepath.Glob("*/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, ok := cases[filepath.Dir(path)]; !ok {
			t.Fatalf("chart missing mirror coverage: %s", path)
		}
	}
	for chart, values := range cases {
		t.Run(chart, func(t *testing.T) {
			out, err := render(t, chart, append(values, "global.imageRegistry=mirror.local/cache")...)
			if err != nil {
				t.Fatal(out, err)
			}
			images, e := release.Images([]byte(out))
			if e != nil {
				t.Fatal(e)
			}
			for _, image := range images {
				if !strings.HasPrefix(image, "mirror.local/cache/") || (!strings.Contains(image, "@sha256:") && !strings.Contains(image, "ghcr.io/tym83/aep-")) {
					t.Fatalf("non-mirrored or mutable image: %s", image)
				}
			}
			for _, host := range []string{"argoproj.github.io/", "ghcr.io/", "cr.openchoreo.dev/", "docker.io/", "quay.io/", "registry.k8s.io/"} {
				if strings.Contains(strings.ReplaceAll(out, "mirror.local/cache/"+host, ""), host) {
					t.Fatalf("public reference in %s: %s", chart, out)
				}
			}
		})
	}
}

func TestDockerHubAliases(t *testing.T) {
	for _, host := range []string{"docker.io", "index.docker.io", "registry-1.docker.io"} {
		out, err := render(t, "aep-llm", "model=test", "apiKeySecret=key", "image.repository="+host+"/ubuntu", "image.tag=v24", "image.digest=", "dev=true", "global.imageRegistry=mirror.local/cache")
		if err != nil || !strings.Contains(out, "mirror.local/cache/docker.io/library/ubuntu:v24") {
			t.Fatalf("%s: %v\n%s", host, err, out)
		}
	}
}

func TestHelm4PostrendererPlugin(t *testing.T) {
	version, err := exec.Command("helm", "version", "--short").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(version), "v4.") {
		t.Skip("Helm 4 plugin contract")
	}
	plugins := t.TempDir()
	source, err := filepath.Abs("aep-gitlab/postrenderer")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(source, filepath.Join(plugins, "aep-images")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("helm", "template", "operator", "./aep-operator", "--set", "aepcpKubeconfigSecret=cp/access", "--post-renderer", "aep-images")
	cmd.Env = append(os.Environ(), "HELM_PLUGINS="+plugins, "AEP_REGISTRY_MIRROR=mirror.local/cache")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, b)
	}
	images, err := release.Images(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) == 0 {
		t.Fatal("no image rendered")
	}
	for _, image := range images {
		if !strings.HasPrefix(image, "mirror.local/cache/") || !strings.Contains(image, "@sha256:") {
			t.Fatal(image)
		}
	}
}
