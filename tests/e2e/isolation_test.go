//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
func (s *suite) mcpCell(role string, target int, tool string, allow bool) {
	tok := s.auth(0, role, false)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// HTTP authentication failures have an explicit status, not a generic SDK error.
	if !contains([]string{"developer", "platform", "admin", "auditor", "other"}, role) {
		code, _, err := exchange(ctx, s.c.Endpoints["mcp"], "", "POST", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "acceptance", "version": "1"}}}, nil)
		want := 403
		if role == "anonymous" {
			want = 401
		}
		if err != nil || code != want {
			s.t.Errorf("MCP authentication: HTTP %d, want %d", code, want)
		}
		return
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "acceptance", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: s.c.Endpoints["mcp"], HTTPClient: &http.Client{Transport: bearerTransport{tok}, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil)
	if err != nil {
		s.t.Fatal("MCP initialization failed (details redacted)")
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: map[string]any{"org": s.c.Organizations[target].Name, "tier": "nonprod", "service": "nonprod/Postgres/" + s.fixtures[target].resource}})
	if err != nil {
		s.t.Fatal("MCP call failed (details redacted)")
	}
	raw, _ := json.Marshal(result)
	if allow && result.IsError {
		s.t.Error("MCP authorized tool returned an error")
	}
	if !allow {
		if !result.IsError || !strings.Contains(string(raw), "organization mismatch") {
			s.t.Error("MCP expected organization mismatch tool error")
		}
		s.denied(403, string(raw), target)
	}
}
func (s *suite) isolation() {
	a, b := s.c.Organizations[0], s.c.Organizations[1]
	if a.TierKubeconfig == "" || b.TierKubeconfig == "" {
		s.t.Skip("tier kubeconfigs not configured; network/kubectl checks skipped")
	}
	if a.ProbeNamespace == "" || b.ProbeNamespace == "" || s.c.Network.Image == "" || s.c.Network.ControlAddress == "" || b.PostgresAddress == "" || b.APIAddress == "" {
		s.t.Fatal("tier kubeconfigs require probe namespaces, image, positive control and B addresses")
	}
	// Literal addresses distinguish routing isolation from a DNS lookup failure.
	for _, address := range []string{s.c.Network.ControlAddress, b.PostgresAddress, b.APIAddress} {
		host, _, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) == nil {
			s.t.Fatal("network addresses must be literal IP:port")
		}
	}
	probe := s.name("probe")
	kube := []string{"kubectl", "--kubeconfig", a.TierKubeconfig, "--request-timeout=30s", "-n", a.ProbeNamespace}
	s.t.Cleanup(func() {
		if _, err := command(kube, "delete", "pod", probe, "--ignore-not-found", "--wait=false"); err != nil {
			s.t.Error(err)
		}
	})
	if _, err := command(kube, "run", probe, "--image="+s.c.Network.Image, "--restart=Never", "--labels=aep.aenix.io/e2e-run="+s.run, "--command", "--", "sleep", "600"); err != nil {
		s.t.Fatal(err)
	}
	if _, err := command(kube, "wait", "--for=condition=Ready", "pod/"+probe, "--timeout=30s"); err != nil {
		s.t.Fatal("probe pod did not become Ready")
	}
	check := func(address string, want bool) {
		host, port, _ := net.SplitHostPort(address)
		// Exit 1 from nc alone is insufficient: missing tools also fail. Emit a
		// marker only after validating nc and running a bounded TCP check.
		script := `command -v nc >/dev/null || exit 42; if nc -z -w 5 "$1" "$2"; then echo CONNECTED; else echo BLOCKED; fi`
		out, err := command(kube, "exec", probe, "--", "sh", "-c", script, "probe", host, port)
		expected := "BLOCKED"
		if want {
			expected = "CONNECTED"
		}
		if err != nil || out != expected {
			s.t.Errorf("network probe expected %s (output redacted)", expected)
		}
	}
	check(s.c.Network.ControlAddress, true)
	check(b.PostgresAddress, false)
	check(b.APIAddress, false)
	// B's own pod provides a positive control: inaccessible/dead targets must not
	// be mistaken for isolation. This pod has the same image and probe command.
	bkube := []string{"kubectl", "--kubeconfig", b.TierKubeconfig, "--request-timeout=30s", "-n", b.ProbeNamespace}
	s.t.Cleanup(func() {
		if _, err := command(bkube, "delete", "pod", probe, "--ignore-not-found", "--wait=false"); err != nil {
			s.t.Error(err)
		}
	})
	if _, err := command(bkube, "run", probe, "--image="+s.c.Network.Image, "--restart=Never", "--labels=aep.aenix.io/e2e-run="+s.run, "--command", "--", "sleep", "600"); err != nil {
		s.t.Fatal(err)
	}
	if _, err := command(bkube, "wait", "--for=condition=Ready", "pod/"+probe, "--timeout=30s"); err != nil {
		s.t.Fatal(err)
	}
	kube = bkube
	check(b.PostgresAddress, true)
	check(b.APIAddress, true)
	if s.c.ManagementKubeconfig == "" {
		s.t.Skip("management kubeconfig not configured; network checks completed, credential isolation skipped")
	}
	raw, err := command([]string{"kubectl", "--kubeconfig", s.c.ManagementKubeconfig}, "config", "view", "--raw", "--minify", "--flatten", "-o", "json")
	if err != nil {
		s.t.Fatal(err)
	}
	var view struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if json.Unmarshal([]byte(raw), &view) != nil || len(view.Clusters) != 1 {
		s.t.Fatal("invalid management kubeconfig")
	}
	ca, err := base64.StdEncoding.DecodeString(view.Clusters[0].Cluster.CA)
	if err != nil || len(ca) == 0 {
		s.t.Fatal("management CA required")
	}
	caPath := filepath.Join(s.t.TempDir(), "ca.crt")
	if err = os.WriteFile(caPath, ca, 0600); err != nil {
		s.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// Only endpoint and CA come from management. Authentication remains A's tier
	// identity. Explicitly disable an inherited insecure-skip-tls-verify flag.
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", a.TierKubeconfig, "--server", view.Clusters[0].Cluster.Server, "--certificate-authority", caPath, "--insecure-skip-tls-verify=false", "--request-timeout=30s", "get", "namespaces", "--field-selector=metadata.name=tenant-"+b.Name+"-nonprod", "-o", "json")
	out, err := cmd.CombinedOutput()
	if err == nil || (!strings.Contains(string(out), "Forbidden") && !strings.Contains(string(out), "Unauthorized")) {
		s.t.Fatal("expected explicit Kubernetes authentication/authorization denial, not a transport failure")
	}
	s.t.Log(fmt.Sprintf("network and management credential isolation checked for run %s", s.run))
}
