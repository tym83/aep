package readapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeChoreo serves the OpenChoreo routes used by the log proxy. allow maps
// an environment-qualified name ("acme/dev") to the logs:view decision.
func fakeChoreo(t *testing.T, allow map[string]bool, owner string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("caller token not forwarded")
		}
		switch {
		case r.URL.Path == "/api/v1/authz/evaluates":
			var body []map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			env := body[0]["context"].(map[string]any)["resource"].(map[string]any)["environment"].(string)
			if body[0]["action"] != "logs:view" {
				t.Errorf("action %v", body[0]["action"])
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"decision": allow[env]}})
		case strings.HasSuffix(r.URL.Path, "/k8sresources/tree"):
			_, _ = w.Write([]byte(`{"renderedReleases":[{"nodes":[{"kind":"Pod","name":"p-old","object":{"status":{"phase":"Failed"}}},{"kind":"Pod","name":"p-live","object":{"status":{"phase":"Running"}}}]}]}`))
		case strings.HasSuffix(r.URL.Path, "/k8sresources/logs"):
			_, _ = w.Write([]byte(`{"logs":[{"log":"line for ` + r.URL.Query().Get("podName") + `"}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/acme/releasebindings/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/acme/releasebindings/")
			env := name[strings.LastIndex(name, "-")+1:]
			_ = json.NewEncoder(w).Encode(map[string]any{"spec": map[string]any{"environment": env, "owner": map[string]any{"projectName": owner, "componentName": "heartbeat"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func withEnvironments(t *testing.T, c client.Client) {
	t.Helper()
	org := &api.Organization{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "acme"}, org); err != nil {
		t.Fatal(err)
	}
	org.Spec.Environments = []api.Environment{{Name: "dev", Tier: "nonprod"}, {Name: "prod", Tier: "prod"}}
	if err := c.Update(context.Background(), org); err != nil {
		t.Fatal(err)
	}
}

func TestLogsAreScopedToTheEnvironment(t *testing.T) {
	s, _, c, sign := fixture(t)
	withEnvironments(t, c)
	choreo, seen := fakeChoreo(t, map[string]bool{"acme/dev": true}, "logdemo")
	WithOpenChoreo(choreo.URL, nil)(s)
	member := sign([]string{"aep-acme-approver"}, nil)
	path := "/v1/organizations/acme/projects/logdemo/components/heartbeat/environments/"
	w := call(s, path+"dev/logs", member)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "line for p-live") {
		t.Fatalf("dev logs: %d %s", w.Code, w.Body)
	}
	if w := call(s, path+"prod/logs?podName=p-1", member); w.Code != 403 {
		t.Fatalf("prod logs without logs:view: %d %s", w.Code, w.Body)
	}
	for _, request := range *seen {
		if strings.Contains(request, "heartbeat-prod/k8sresources/logs") {
			t.Fatal("logs were read before the environment decision")
		}
	}
	if w := call(s, path+"prod/logs", sign([]string{"aep-acme-approver"}, nil)); w.Code != 403 {
		t.Fatalf("denied caller must not learn whether prod is deployed: %d", w.Code)
	}
	if w := call(s, path+"stage/logs", member); w.Code != 404 {
		t.Fatalf("unknown environment: %d", w.Code)
	}
	if w := call(s, path+"dev/logs?podName=Bad_Name", member); w.Code != 400 {
		t.Fatalf("invalid pod name: %d", w.Code)
	}
	if w := call(s, path+"dev/logs?sinceSeconds=0", member); w.Code != 400 {
		t.Fatalf("invalid sinceSeconds: %d", w.Code)
	}
	if w := call(s, path+"dev/logs", sign([]string{"aep-other-developer"}, nil)); w.Code != 403 {
		t.Fatalf("non-member: %d", w.Code)
	}
}

func TestLogsRejectBindingOfAnotherProject(t *testing.T) {
	s, _, c, sign := fixture(t)
	withEnvironments(t, c)
	choreo, seen := fakeChoreo(t, map[string]bool{"acme/dev": true}, "payments")
	WithOpenChoreo(choreo.URL, nil)(s)
	w := call(s, "/v1/organizations/acme/projects/logdemo/components/heartbeat/environments/dev/logs", sign([]string{"aep-acme-developer"}, nil))
	if w.Code != 404 {
		t.Fatalf("binding of another project: %d %s", w.Code, w.Body)
	}
	for _, request := range *seen {
		if strings.Contains(request, "/k8sresources/") {
			t.Fatalf("unexpected request %s", request)
		}
	}
}

func TestLogsDisabledWithoutOpenChoreo(t *testing.T) {
	s, _, c, sign := fixture(t)
	withEnvironments(t, c)
	if w := call(s, "/v1/organizations/acme/projects/p/components/c/environments/dev/logs", sign([]string{"aep-acme-developer"}, nil)); w.Code != 503 {
		t.Fatalf("unconfigured: %d", w.Code)
	}
}
