package charts

import (
	"encoding/json"
	"fmt"
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
		"aep-mcp":               {"oidc.issuer=https://id.invalid", "tls.existingSecret=mcp", "alertRouter.enabled=true", "alertRouter.tlsSecret=router", "alertRouter.hmacSecret=observer"},
		"aep-read-api":          {"oidc.issuer=https://id.invalid", "oidc.audience=aep"},
		"aep-cost-ledger":       {"credentialsSecret=database"},
		"aep-llm":               {"model=test", "apiKeySecret=key"},
		"aep-sre-agent":         agentValues(),
		"aep-audit-forwarder":   {},
		"aep-cp-backup":         {},
		"aep-component-types":   {},
		"aep-builds":            {"plane=workflow"},
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
			// Own workloads must be able to pull from an authenticated mirror.
			if strings.HasPrefix(chart, "aep-") && chart != "aep-gitlab" && chart != "aep-builds" && len(images) > 0 {
				withSecret, err := render(t, chart, append(values, "global.imageRegistry=mirror.local/cache", "global.imagePullSecrets[0].name=mirror-auth")...)
				if err != nil {
					t.Fatal(withSecret, err)
				}
				pods := strings.Count(withSecret, "containers:")
				if pods == 0 || strings.Count(withSecret, `imagePullSecrets: [{"name":"mirror-auth"}]`) != pods {
					t.Fatalf("%s: every pod spec needs the mirror pull secret", chart)
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

func mcpValues() []string {
	return []string{"oidc.issuer=https://id.invalid", "tls.existingSecret=mcp-tls", "organizations[0].name=acme", "organizations[0].tiers[0]=nonprod", "organizations[0].tiers[1]=prod"}
}

// Keep every document: keying only by kind would silently skip most tier grants.
func mcpDocuments(t *testing.T, out string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, doc := range strings.Split(out, "\n---") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		if m["kind"] != nil {
			docs = append(docs, m)
		}
	}
	return docs
}

func TestMCPRBAC(t *testing.T) {
	values := append(mcpValues(), "alertRouter.enabled=true", "alertRouter.tlsSecret=router", "alertRouter.hmacSecret=observer")
	out, err := render(t, "aep-mcp", values...)
	if err != nil {
		t.Fatal(out, err)
	}
	allowed := map[string]string{
		"events": "", "resourcequotas": "", "persistentvolumeclaims": "",
		"postgreses": "apps.cozystack.io", "redises": "apps.cozystack.io", "kuberneteses": "apps.cozystack.io", "kubernetesnodeses": "apps.cozystack.io",
		"kafkas": "apps.cozystack.io", "rabbitmqs": "apps.cozystack.io", "mysqls": "apps.cozystack.io", "clickhouses": "apps.cozystack.io",
		"helmreleases": "helm.toolkit.fluxcd.io", "clusters": "postgresql.cnpg.io", "tenantcontrolplanes": "kamaji.clastix.io", "virtualmachineinstances": "kubevirt.io", "backups": "backups.cozystack.io",
	}
	roles, bindings, clusterRoles, clusterBindings := 0, 0, 0, 0
	for _, doc := range mcpDocuments(t, out) {
		kind := doc["kind"]
		meta := doc["metadata"].(map[string]any)
		name := meta["name"].(string)
		router := strings.HasSuffix(name, "-alert-router")
		if kind == "RoleBinding" || kind == "ClusterRoleBinding" {
			subjects := doc["subjects"].([]any)
			wantName := "test"
			if router {
				wantName += "-alert-router"
			}
			subject := subjects[0].(map[string]any)
			if len(subjects) != 1 || subject["kind"] != "ServiceAccount" || subject["namespace"] != "aep-system" || subject["name"] != wantName {
				t.Fatal(doc)
			}
			ref := doc["roleRef"].(map[string]any)
			wantKind := "ClusterRole"
			if kind == "RoleBinding" {
				wantKind = "Role"
				bindings++
			} else {
				clusterBindings++
			}
			if ref["kind"] != wantKind || ref["name"] != name {
				t.Fatal(doc)
			}
		}
		if kind != "Role" && kind != "ClusterRole" {
			continue
		}
		want := allowed
		if kind == "ClusterRole" {
			clusterRoles++
			want = map[string]string{"organizations": "aep.aenix.io"}
			if router {
				want["namespaces"] = ""
			}
		} else {
			roles++
			if meta["namespace"] != "tenant-acme-nonprod" && meta["namespace"] != "tenant-acme-prod" {
				t.Fatal(meta)
			}
		}
		seen := map[string]bool{}
		for _, raw := range doc["rules"].([]any) {
			rule := raw.(map[string]any)
			verbs := rule["verbs"].([]any)
			wantVerbs := "[get list]"
			if router {
				wantVerbs = "[get]"
			}
			if fmt.Sprint(verbs) != wantVerbs {
				t.Fatal(rule)
			}
			groups := rule["apiGroups"].([]any)
			if len(groups) != 1 || rule["nonResourceURLs"] != nil {
				t.Fatal(rule)
			}
			for _, resource := range rule["resources"].([]any) {
				r := resource.(string)
				group, ok := want[r]
				if !ok || groups[0] != group || seen[r] {
					t.Fatalf("unexpected grant: %v", rule)
				}
				seen[r] = true
			}
		}
		if len(seen) != len(want) {
			t.Fatalf("missing grants: %v", seen)
		}
	}
	if roles != 2 || bindings != 2 || clusterRoles != 2 || clusterBindings != 2 {
		t.Fatal(roles, bindings, clusterRoles, clusterBindings)
	}
	for resource := range allowed {
		if allowed[resource] == "" {
			continue
		}
		out, err := render(t, "aep-mcp", append(mcpValues(), "rbac.kinds."+resource+"=false")...)
		if err != nil {
			t.Fatal(out, err)
		}
		for _, doc := range mcpDocuments(t, out) {
			if doc["kind"] != "Role" {
				continue
			}
			for _, raw := range doc["rules"].([]any) {
				for _, r := range raw.(map[string]any)["resources"].([]any) {
					if r == resource {
						t.Fatalf("disabled resource granted: %s", resource)
					}
				}
			}
		}
	}
}

func TestMCPFlagsAndTLS(t *testing.T) {
	out, err := render(t, "aep-mcp", append(mcpValues(), "maxRows=42", "maxBytes=4096", "requestsPerMinute=12", "metricsQueryUrlTemplate=https://metrics/{namespace}/api/v1/query", "redactPatterns[0]=host-[0-9]+", "redactPatterns[1]=rack-[a-z]+", "oidc.jwksUrl=https://id.invalid/keys")...)
	if err != nil {
		t.Fatal(out, err)
	}
	for _, flag := range []string{"--listen=:8443", "--issuer=https://id.invalid", "--audience=aep-mcp", "--jwks-url=https://id.invalid/keys", "--max-rows=42", "--max-bytes=4096", "--requests-per-minute=12", "--metrics-query-url-template=https://metrics/{namespace}/api/v1/query", "--redact-pattern=host-[0-9]+", "--redact-pattern=rack-[a-z]+", "--tls-cert=/tls/tls.crt", "--tls-key=/tls/tls.key"} {
		if !strings.Contains(out, flag) {
			t.Fatal("missing flag", flag)
		}
	}
	if strings.Contains(out, "alert-router") || strings.Contains(out, "--tls-termination-proxy") || strings.Contains(out, "kind: Certificate") {
		t.Fatal(out)
	}
	out, err = render(t, "aep-mcp", "oidc.issuer=https://id.invalid", "tls.issuerRef.name=ca")
	if err != nil || !strings.Contains(out, "kind: Certificate") || !strings.Contains(out, "test.aep-system.svc") {
		t.Fatal(out, err)
	}
	proxy := []string{"oidc.issuer=https://id.invalid", "tls.terminationProxy=true", "ingress.enabled=true", "ingress.host=mcp.example.org", "ingress.tls[0].secretName=edge", "ingress.tls[0].hosts[0]=mcp.example.org"}
	out, err = render(t, "aep-mcp", proxy...)
	if err != nil || !strings.Contains(out, "--tls-termination-proxy") || strings.Contains(out, "--tls-cert") {
		t.Fatal(out, err)
	}
	for _, values := range [][]string{
		{"oidc.issuer=https://id.invalid"},
		append(mcpValues(), "tls.issuerRef.name=ca"),
		append(mcpValues(), "tls.terminationProxy=true"),
		{"oidc.issuer=https://id.invalid", "tls.terminationProxy=true", "ingress.enabled=true", "ingress.host=mcp.example.org"},
		append(mcpValues(), "organizations[0].name=platform"),
		append(mcpValues(), "organizations[0].tiers[0]=other"),
		append(mcpValues(), "alertRouter.enabled=true", "alertRouter.tlsSecret=router"),
	} {
		if out, err := render(t, "aep-mcp", values...); err == nil {
			t.Fatal("unsafe configuration accepted", out)
		}
	}
	out, err = render(t, "aep-mcp", append(mcpValues(), "alertRouter.enabled=true", "alertRouter.tlsSecret=router", "alertRouter.hmacSecret=hmac", "alertRouter.clientCASecret=ca", "alertRouter.agentUrlTemplate=https://agent.aep-{org}.svc:8080")...)
	if err != nil {
		t.Fatal(out, err)
	}
	for _, flag := range []string{"--client-ca=/observer-ca/observer-ca.pem", "--hmac-secret-file=/run/observer/hmac", "--agent-url-template=https://agent.aep-{org}.svc:8080"} {
		if !strings.Contains(out, flag) {
			t.Fatal("missing flag", flag)
		}
	}
}

func TestMCPNetworkIsolation(t *testing.T) {
	out, err := render(t, "aep-mcp", append(mcpValues(), "ingress.enabled=true", "ingress.host=mcp.example.org", "networkPolicy.ingressControllerNamespace=edge", "alertRouter.enabled=true", "alertRouter.tlsSecret=router", "alertRouter.hmacSecret=hmac")...)
	if err != nil {
		t.Fatal(out, err)
	}
	for _, doc := range mcpDocuments(t, out) {
		if doc["kind"] != "NetworkPolicy" {
			continue
		}
		spec := doc["spec"].(map[string]any)
		labels := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
		rule := spec["ingress"].([]any)[0].(map[string]any)
		peers := rule["from"].([]any)
		port := rule["ports"].([]any)[0].(map[string]any)["port"]
		wantNS, wantLabel := "aep-acme", "aep-sre-agent"
		if labels["app.kubernetes.io/name"] == "alert-router" {
			wantNS, wantLabel = "openchoreo-observability-plane", "observer"
			if len(peers) != 1 || port != float64(8080) {
				t.Fatal(rule)
			}
		} else {
			if len(peers) != 2 || port != float64(8443) {
				t.Fatal(rule)
			}
			edge := peers[1].(map[string]any)["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)
			if edge["kubernetes.io/metadata.name"] != "edge" {
				t.Fatal(edge)
			}
		}
		peer := peers[0].(map[string]any)
		ns := peer["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)
		pods := peer["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
		if ns["kubernetes.io/metadata.name"] != wantNS || pods["app.kubernetes.io/name"] != wantLabel {
			t.Fatal(peer)
		}
	}
	out, err = render(t, "aep-mcp", "oidc.issuer=https://id.invalid", "tls.existingSecret=tls")
	if err != nil || !strings.Contains(out, "ingress: []") {
		t.Fatal(out, err)
	}
}

func TestMCPSeparateControlPlane(t *testing.T) {
	out, err := render(t, "aep-mcp", "mcp.enabled=false", "alertRouter.enabled=true", "alertRouter.tlsSecret=router", "alertRouter.hmacSecret=observer")
	if err != nil {
		t.Fatal(out, err)
	}
	for _, doc := range mcpDocuments(t, out) {
		if !strings.HasSuffix(doc["metadata"].(map[string]any)["name"].(string), "-alert-router") {
			t.Fatal("management resource in control-plane release", doc)
		}
		if doc["kind"] == "Role" || doc["kind"] == "RoleBinding" {
			t.Fatal(doc)
		}
	}
	if !strings.Contains(out, "ghcr.io/tym83/aep-alert-router:") || strings.Contains(out, "cozystack-mcp") {
		t.Fatal(out)
	}
}

func TestMCPImagePins(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	values := append(mcpValues(), "alertRouter.enabled=true", "alertRouter.tlsSecret=router", "alertRouter.hmacSecret=observer", "global.aepVersion=v1.2.3", "global.aepDigests.ghcr\\.io/tym83/aep-cozystack-mcp="+digest, "global.aepDigests.ghcr\\.io/tym83/aep-alert-router="+digest)
	out, err := render(t, "aep-mcp", values...)
	if err != nil {
		t.Fatal(out, err)
	}
	for _, name := range []string{"cozystack-mcp", "alert-router"} {
		if !strings.Contains(out, "ghcr.io/tym83/aep-"+name+":v1.2.3@"+digest) {
			t.Fatal(out)
		}
	}
	out, err = render(t, "aep-mcp", append(values, "image.tag=v1.2.4", "alertRouter.image.tag=v1.2.4")...)
	if err != nil || strings.Contains(out, "@sha256:") {
		t.Fatal(out, err)
	}
}
