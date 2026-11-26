package charts

import (
	"encoding/json"
	"os/exec"
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
	b, err := exec.Command("helm", args...).CombinedOutput()
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
	out, err := render(t, "aep-sre-agent", agentValues()...)
	if err == nil || !strings.Contains(out, "digest is required") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-sre-agent", append(agentValues(), "tokenProxy.image.digest="+digest)...)
	if err == nil || !strings.Contains(out, "image.digest is required") {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-sre-agent", append(agentValues(), "image.digest="+digest)...)
	if err == nil || !strings.Contains(out, "tokenProxy.image.digest is required") {
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
