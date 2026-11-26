//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPodFromTreePrefersRunning(t *testing.T) {
	raw := `{"renderedReleases":[{"nodes":[{"kind":"Deployment","name":"d"},{"kind":"Pod","name":"old","object":{"status":{"phase":"Pending"}}},{"kind":"Pod","name":"live","object":{"status":{"phase":"Running"}}}]}]}`
	if got := podFromTree(raw); got != "live" {
		t.Fatalf("pod %q, want live", got)
	}
	if got := podFromTree(`{"renderedReleases":[{"nodes":[{"kind":"Pod","name":"only"}]}]}`); got != "only" {
		t.Fatalf("pod %q, want only", got)
	}
	if got := podFromTree(`not json`); got != "" {
		t.Fatalf("pod %q from invalid tree", got)
	}
}

func TestLogsPathResolvesPodOnce(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/a/releasebindings/heartbeat-dev/k8sresources/tree":
			calls++
			if r.Header.Get("Authorization") != "Bearer platform" {
				t.Error("tree must be read with the platform identity")
			}
			_, _ = w.Write([]byte(`{"renderedReleases":[{"nodes":[{"kind":"Pod","name":"heartbeat-1","object":{"status":{"phase":"Running"}}}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := &suite{t: t, pods: map[string]string{}, fixtures: make([]fixture, 1), tokens: map[string]string{"0/platform/false": "platform"}}
	s.c.Endpoints = map[string]string{"openchoreo": server.URL}
	s.c.Organizations = []organization{{Name: "a", LogsProject: "logdemo", LogsComponent: "heartbeat", Users: map[string]string{"developer": "d", "platform": "p"}}}
	s.c.Resources.Environment, s.c.Resources.ProdEnvironment = "dev", "prod"
	s.c.Resources.LogsPath = "/api/v1/namespaces/{org}/releasebindings/{component}-{environment}/k8sresources/logs?podName={podName}"
	for range 2 {
		if got := s.expand("{nonprodLogsPath}", 0, "x"); got != "/api/v1/namespaces/a/releasebindings/heartbeat-dev/k8sresources/logs?podName=heartbeat-1" {
			t.Fatalf("expanded %v", got)
		}
	}
	if got := s.expand("{logsPath}", 0, "x").(string); !strings.HasSuffix(got, "heartbeat-prod/k8sresources/logs?podName=absent") {
		t.Fatalf("missing prod binding expanded to %v", got)
	}
	if calls != 1 {
		t.Fatalf("tree read %d times, want 1 (cached)", calls)
	}
	if got := s.expand("/v1/plain", 0, "x"); got != "/v1/plain" || calls != 1 {
		t.Fatal("paths without log placeholders must not read the tree")
	}
}
