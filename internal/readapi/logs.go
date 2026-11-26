package readapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"k8s.io/apimachinery/pkg/util/validation"
)

// maxLogBytes bounds a proxied log response.
const maxLogBytes = 4 << 20

// WithOpenChoreo enables environment-scoped log access through the OpenChoreo API.
//
// OpenChoreo authorizes its pod log route with releasebinding:view and no
// environment context, so environment conditions on logs:view never apply
// there. This route evaluates logs:view for the exact environment with the
// caller's own token first and only then reads the logs as the caller.
func WithOpenChoreo(baseURL string, httpClient *http.Client) func(*Server) {
	return WithOpenChoreoHost(baseURL, "", httpClient)
}

// WithOpenChoreoHost is WithOpenChoreo for an internal address of the
// OpenChoreo gateway that routes by host name. The public entry point denies
// the direct pod log route, so the read API must reach OpenChoreo past it.
func WithOpenChoreoHost(baseURL, host string, httpClient *http.Client) func(*Server) {
	return func(s *Server) {
		s.openChoreoHost = host
		s.openChoreoURL = strings.TrimRight(baseURL, "/")
		s.openChoreo = httpClient
		if s.openChoreo == nil {
			s.openChoreo = &http.Client{Timeout: 20 * time.Second}
		}
	}
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, org *api.Organization) {
	if s.openChoreoURL == "" {
		fail(w, 503, "log access is not configured")
		return
	}
	project, component, environment := r.PathValue("project"), r.PathValue("component"), r.PathValue("environment")
	pod, container := r.URL.Query().Get("podName"), r.URL.Query().Get("container")
	for _, v := range []string{project, component, environment} {
		if len(validation.IsDNS1123Label(v)) != 0 {
			fail(w, 400, "invalid project, component or environment")
			return
		}
	}
	for _, v := range []string{pod, container} {
		if v != "" && len(validation.IsDNS1123Subdomain(v)) != 0 {
			fail(w, 400, "invalid pod or container name")
			return
		}
	}
	since := r.URL.Query().Get("sinceSeconds")
	if since != "" {
		if n, err := strconv.Atoi(since); err != nil || n < 1 || n > 7*24*3600 {
			fail(w, 400, "sinceSeconds must be between 1 and 604800")
			return
		}
	}
	known := false
	for _, e := range org.Spec.Environments {
		known = known || e.Name == environment
	}
	if !known {
		fail(w, 404, "environment not found")
		return
	}
	token := authn.BearerToken(r.Header.Get("Authorization"))
	// Decide first, so a denied caller learns nothing about which deployments exist.
	allowed, err := s.canViewLogs(r.Context(), token, org.Name, project, component, environment)
	if err != nil {
		fail(w, 502, "cannot evaluate log access")
		return
	}
	if !allowed {
		fail(w, 403, "log access denied for this environment")
		return
	}
	binding := component + "-" + environment
	base := "/api/v1/namespaces/" + org.Name + "/releasebindings/" + binding
	// The decision covers the requested hierarchy; the binding must belong to it.
	var rb struct {
		Spec struct {
			Environment string
			Owner       struct{ ProjectName, ComponentName string }
		}
	}
	code, raw, err := s.choreo(r.Context(), token, "GET", base, nil)
	if err != nil {
		fail(w, 502, "OpenChoreo is unavailable")
		return
	}
	if code != 200 || json.Unmarshal(raw, &rb) != nil || rb.Spec.Owner.ProjectName != project || rb.Spec.Owner.ComponentName != component || rb.Spec.Environment != environment {
		fail(w, 404, "deployment not found")
		return
	}
	if pod == "" {
		code, raw, err = s.choreo(r.Context(), token, "GET", base+"/k8sresources/tree", nil)
		if err != nil {
			fail(w, 502, "OpenChoreo is unavailable")
			return
		}
		if pod = podFromTree(raw); code != 200 || pod == "" {
			fail(w, 404, "no running pod")
			return
		}
	}
	q := url.Values{"podName": {pod}}
	if container != "" {
		q.Set("container", container)
	}
	if since != "" {
		q.Set("sinceSeconds", since)
	}
	code, raw, err = s.choreo(r.Context(), token, "GET", base+"/k8sresources/logs?"+q.Encode(), nil)
	if err != nil {
		fail(w, 502, "OpenChoreo is unavailable")
		return
	}
	if code != 200 {
		fail(w, code, "cannot read logs")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

func (s *Server) canViewLogs(ctx context.Context, token, org, project, component, environment string) (bool, error) {
	body := []map[string]any{{
		"action":   "logs:view",
		"resource": map[string]any{"type": "component", "id": component, "hierarchy": map[string]any{"namespace": org, "project": project, "component": component}},
		"context":  map[string]any{"resource": map[string]any{"environment": org + "/" + environment}},
	}}
	code, raw, err := s.choreo(ctx, token, "POST", "/api/v1/authz/evaluates", body)
	if err != nil {
		return false, err
	}
	if code != 200 {
		return false, fmt.Errorf("evaluation returned HTTP %d", code)
	}
	var decisions []struct {
		Decision bool `json:"decision"`
	}
	if err := json.Unmarshal(raw, &decisions); err != nil || len(decisions) != 1 {
		return false, fmt.Errorf("unexpected evaluation response")
	}
	return decisions[0].Decision, nil
}

func (s *Server) choreo(ctx context.Context, token, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.openChoreoURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if s.openChoreoHost != "" {
		req.Host = s.openChoreoHost
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.openChoreo.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLogBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(raw) > maxLogBytes {
		return 0, nil, fmt.Errorf("response exceeds %d bytes", maxLogBytes)
	}
	return resp.StatusCode, raw, nil
}

// podFromTree picks a pod from a release binding's resource tree, preferring
// a running one.
func podFromTree(raw []byte) string {
	var tree struct {
		RenderedReleases []struct {
			Nodes []struct {
				Kind, Name string
				Object     struct {
					Status struct{ Phase string }
				}
			}
		}
	}
	if json.Unmarshal(raw, &tree) != nil {
		return ""
	}
	first := ""
	for _, r := range tree.RenderedReleases {
		for _, n := range r.Nodes {
			if n.Kind != "Pod" || n.Name == "" {
				continue
			}
			if n.Object.Status.Phase == "Running" {
				return n.Name
			}
			if first == "" {
				first = n.Name
			}
		}
	}
	return first
}
