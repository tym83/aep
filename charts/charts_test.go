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
	out, err := render(t, "aep-sre-agent", agentValues()...)
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
