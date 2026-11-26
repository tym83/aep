//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Bindings precede owners; immutable releases are swept after their owners.
var fixtureKinds = []string{"releasebindings", "resourcereleasebindings", "projectreleasebindings", "endpointreleasebindings", "components", "resources", "projects", "secretreferences", "authzrolebindings", "authzroles", "componentreleases", "resourcereleases", "projectreleases", "renderedreleases", "environments", "dataplanes", "resourcetypes"}

func cleanupRank(kind string) int {
	for i, k := range fixtureKinds {
		if k == kind {
			return i
		}
	}
	return len(fixtureKinds)
}

func (s *suite) prepareCleanup() {
	s.t.Helper()
	if err := s.configureCleanup(); err != nil {
		s.t.Fatal(err)
	}
}

func (s *suite) configureCleanup() error {
	s.cleanupKinds = append([]string(nil), fixtureKinds...)
	if len(s.c.Cleanup.TokenCommand) > 0 {
		return nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", s.c.Cleanup.AepcpKubeconfig)
	if err != nil {
		return fmt.Errorf("cleanup aep-cp kubeconfig cannot be loaded (details redacted)")
	}
	cfg.Timeout = 15 * time.Second
	d, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("cleanup aep-cp API client cannot be configured")
	}
	resources, err := d.ServerResourcesForGroupVersion("openchoreo.dev/v1alpha1")
	if err != nil {
		return fmt.Errorf("cleanup aep-cp API is unreachable or OpenChoreo discovery is forbidden; check kubeconfig server, tunnel and credentials before rerunning (details redacted)")
	}
	s.cleanupKinds = nil
	for _, r := range resources.APIResources {
		if r.Namespaced && !strings.Contains(r.Name, "/") && contains(r.Verbs, "list") && contains(r.Verbs, "delete") {
			s.cleanupKinds = append(s.cleanupKinds, r.Name)
		}
	}
	sort.Slice(s.cleanupKinds, func(i, j int) bool { return cleanupRank(s.cleanupKinds[i]) < cleanupRank(s.cleanupKinds[j]) })
	s.cleanupClient, err = dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("cleanup aep-cp API client cannot be configured")
	}
	// A successful discovery alone does not establish cleanup access to namespaces.
	for org := range s.c.Organizations {
		if _, err := s.cleanupList(org, "projects"); err != nil {
			return fmt.Errorf("cleanup aep-cp API cannot list projects in %s: %v", s.c.Organizations[org].Name, err)
		}
	}
	return nil
}

func (s *suite) cleanupResource(org int, kind string) dynamic.ResourceInterface {
	return s.cleanupClient.Resource(schema.GroupVersionResource{Group: "openchoreo.dev", Version: "v1alpha1", Resource: kind}).Namespace(s.c.Organizations[org].Name)
}
func (s *suite) cleanupExchange(org int, method, kind, name string) (int, string, error) {
	tok, err := token(s.c.Cleanup.TokenCommand, "")
	if err != nil {
		return 0, "", err
	}
	path := s.oc(org, "/"+kind)
	if name != "" {
		path += "/" + name
	}
	return exchange(context.Background(), s.c.Endpoints["openchoreo"], path, method, tok, nil, nil)
}
func (s *suite) cleanupList(org int, kind string) ([]map[string]any, error) {
	if s.cleanupClient != nil {
		list, err := s.cleanupResource(org, kind).List(context.Background(), metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list %s failed (details redacted)", kind)
		}
		items := make([]map[string]any, 0, len(list.Items))
		for _, item := range list.Items {
			items = append(items, item.Object)
		}
		return items, nil
	}
	items := []map[string]any{}
	cursor := ""
	seen := map[string]bool{}
	for {
		route := kind
		if cursor != "" {
			route += "?cursor=" + url.QueryEscape(cursor)
		}
		code, raw, err := s.cleanupExchange(org, "GET", route, "")
		if err != nil {
			return nil, err
		}
		if code == 404 {
			return nil, nil
		}
		if code != 200 {
			return nil, fmt.Errorf("list %s returned HTTP %d", kind, code)
		}
		var list struct {
			Items      []map[string]any `json:"items"`
			Pagination struct {
				NextCursor string `json:"nextCursor"`
			} `json:"pagination"`
		}
		if json.Unmarshal([]byte(raw), &list) != nil || list.Items == nil {
			return nil, fmt.Errorf("invalid cleanup list %s", kind)
		}
		items = append(items, list.Items...)
		cursor = list.Pagination.NextCursor
		if cursor == "" {
			return items, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("cleanup list %s repeated its cursor", kind)
		}
		seen[cursor] = true
	}
}

// Report classes only: API errors can embed object contents or credentials.
func cleanupErrorClass(err error) string {
	if err == nil {
		return "object still present (finalization pending)"
	}
	if reason := apierrors.ReasonForError(err); reason != "" && reason != metav1.StatusReasonUnknown {
		return string(reason)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	if errors.Is(err, errTransport) {
		return "transport error"
	}
	return fmt.Sprintf("%T", err)
}

func (s *suite) cleanupTimeout() time.Duration {
	if s.c.TimeoutSeconds <= 0 {
		return 900 * time.Second
	}
	return time.Duration(s.c.TimeoutSeconds) * time.Second
}

func cleanupPause(deadline time.Time) {
	if remaining := time.Until(deadline); remaining > 0 {
		time.Sleep(min(200*time.Millisecond, remaining))
	}
}

// A failed GET never establishes absence. Retry until the configured deadline.
func (s *suite) cleanupGet(org int, kind, name string, deadline time.Time) (map[string]any, bool, error) {
	var last string
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		var obj map[string]any
		var gone bool
		if s.cleanupClient != nil {
			v, err := s.cleanupResource(org, kind).Get(ctx, name, metav1.GetOptions{})
			gone = apierrors.IsNotFound(err)
			last = cleanupErrorClass(err)
			if err == nil {
				obj = v.Object
			}
		} else {
			tok, err := token(s.c.Cleanup.TokenCommand, "")
			if err == nil {
				var code int
				var raw string
				code, raw, err = exchange(ctx, s.c.Endpoints["openchoreo"], s.oc(org, "/"+kind+"/"+name), "GET", tok, nil, nil)
				gone = err == nil && code == 404
				if err == nil && code == 200 {
					err = json.Unmarshal([]byte(raw), &obj)
				}
				last = fmt.Sprintf("HTTP %d", code)
			}
			if err != nil {
				obj = nil
				last = cleanupErrorClass(err)
			}
		}
		cancel()
		if gone || obj != nil {
			return obj, gone, nil
		}
		if !time.Now().Before(deadline) {
			return nil, false, fmt.Errorf("%s", last)
		}
		cleanupPause(deadline)
	}
}

func (s *suite) cleanupDelete(org int, kind, name string) (bool, error) {
	if s.cleanupClient != nil {
		r := s.cleanupResource(org, kind)
		err := r.Delete(context.Background(), name, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("delete %s/%s failed: %s", kind, name, cleanupErrorClass(err))
		}
		_, err = r.Get(context.Background(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("verify %s/%s failed: %s", kind, name, cleanupErrorClass(err))
		}
		return false, nil
	}
	code, _, err := s.cleanupExchange(org, "DELETE", kind, name)
	if err != nil {
		return false, err
	}
	if code == 404 {
		return true, nil
	}
	if code != 200 && code != 202 && code != 204 {
		return false, fmt.Errorf("delete returned HTTP %d", code)
	}
	code, _, err = s.cleanupExchange(org, "GET", kind, name)
	if err != nil {
		return false, err
	}
	if code != 200 && code != 404 {
		return false, fmt.Errorf("verify returned HTTP %d", code)
	}
	return code == 404, nil
}
func (s *suite) cleanupNow(org int, kind, name string) error {
	if !strings.Contains(name, "-"+s.run+"-") {
		return fmt.Errorf("refusing cleanup without exact run id")
	}
	// Deleting a project cascades to its resources and components. Checking
	// only the project's own binding would bypass a failed child decommission.
	if kind == "projects" {
		for _, bindingKind := range []string{"releasebindings", "resourcereleasebindings", "projectreleasebindings"} {
			items, err := s.cleanupList(org, bindingKind)
			if err != nil {
				return err
			}
			for _, item := range items {
				if str(item, "spec.owner.projectName") == name && str(item, "spec.environment") == s.c.Resources.ProdEnvironment {
					if err := s.cleanupProduction(org, bindingKind, str(item, "metadata.name")); err != nil {
						return err
					}
				}
			}
		}
	}
	// Owners and their production bindings must use the normal approval path.
	bindingKind := map[string]string{"resources": "resourcereleasebindings", "components": "releasebindings", "projects": "projectreleasebindings"}[kind]
	if bindingKind != "" {
		if err := s.cleanupProduction(org, bindingKind, name+"-"+s.c.Resources.ProdEnvironment); err != nil {
			return err
		}
	}
	if contains([]string{"resourcereleasebindings", "releasebindings", "projectreleasebindings"}, kind) && strings.HasSuffix(name, "-"+s.c.Resources.ProdEnvironment) {
		return s.cleanupProduction(org, kind, name)
	}
	deadline := time.Now().Add(s.cleanupTimeout())
	var err error
	for {
		var gone bool
		gone, err = s.cleanupDelete(org, kind, name)
		if gone && err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			detail := "object still present (finalization pending)"
			if err != nil {
				detail = err.Error()
			}
			return fmt.Errorf("deletion not confirmed for %s/%s: %s", kind, name, detail)
		}
		cleanupPause(deadline)
	}
}

// Use error-returning requests here so one failed cleanup does not suppress the
// report for other fixtures. Tokens are refreshed on every request and retry.
func (s *suite) cleanupChangeCall(org int, role, method, path string, body any, headers map[string]string) (int, map[string]any, error) {
	var code int
	var raw string
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var tok string
		tok, err = token(s.c.TokenCommand, s.c.Organizations[org].Users[role])
		if err != nil {
			return 0, nil, err
		}
		code, raw, err = exchange(context.Background(), s.c.Endpoints["change"], path, method, tok, body, headers)
		if err == nil || !errors.Is(err, errTransport) {
			break
		}
	}
	out := map[string]any{}
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &out)
	}
	return code, out, err
}
func (s *suite) cleanupProduction(org int, kind, name string) error {
	// Read with the cleanup identity, not a filtered developer list.
	deadline := time.Now().Add(s.cleanupTimeout())
	binding, gone, err := s.cleanupGet(org, kind, name, deadline)
	if err != nil {
		return fmt.Errorf("cannot verify production binding %s: %v", name, err)
	}
	if gone {
		return nil
	}
	targetKind, ownerField := "Resource", "resourceName"
	if kind == "releasebindings" {
		targetKind, ownerField = "Component", "componentName"
	}
	if kind == "projectreleasebindings" {
		return fmt.Errorf("production project binding %s requires manual review: project decommission is unsupported", name)
	}
	owner := str(binding, "spec.owner."+ownerField)
	if !strings.Contains(owner, "-"+s.run+"-") || name != owner+"-"+s.c.Resources.ProdEnvironment {
		return fmt.Errorf("production binding owner does not match run")
	}
	desired := map[string]any{}
	if targetKind == "Resource" {
		desired["deleteData"] = true
	}
	body := map[string]any{"organization": s.c.Organizations[org].Name, "operation": "decommission", "model": "normal", "target": map[string]any{"kind": targetKind, "name": owner, "project": str(binding, "spec.owner.projectName"), "environment": s.c.Resources.ProdEnvironment}, "desired": desired, "justification": "Acceptance cleanup " + s.run}
	code, v, err := s.cleanupChangeCall(org, "developer", "POST", "/v1/changes", body, map[string]string{"Idempotency-Key": s.name("cleanup")})
	if err != nil || (code != 200 && code != 201) {
		return fmt.Errorf("create decommission: HTTP %d, %v", code, err)
	}
	id := str(v, "request.metadata.name")
	if id == "" {
		return fmt.Errorf("decommission response has no name")
	}
	deadline = time.Now().Add(s.cleanupTimeout())
	for time.Now().Before(deadline) {
		code, v, err = s.cleanupChangeCall(org, "developer", "GET", s.changePath(org, id), nil, nil)
		if err == nil && code == 200 {
			phase := str(v, "request.status.phase")
			if phase == "Applied" {
				// Approved deletion may still be finalizing. Never issue a raw prod DELETE.
				// Give finalizers their own full timeout after approval is applied.
				deletionDeadline := time.Now().Add(s.cleanupTimeout())
				for {
					_, gone, readErr := s.cleanupGet(org, kind, name, deletionDeadline)
					if gone {
						return nil
					}
					if readErr != nil {
						return fmt.Errorf("cannot verify production binding %s deletion: %v", name, readErr)
					}
					if !time.Now().Before(deletionDeadline) {
						return fmt.Errorf("production binding %s still finalizing", name)
					}
					cleanupPause(deletionDeadline)
				}
			}
			if contains([]string{"Failed", "Rejected", "Expired", "Superseded"}, phase) {
				return fmt.Errorf("decommission reached %s", phase)
			}
			if phase == "Pending" {
				code, _, err = s.cleanupChangeCall(org, "approver", "POST", s.changePath(org, id)+"/decision", map[string]any{"decision": "approve", "evidence": "Acceptance cleanup " + s.run}, map[string]string{"If-Match": str(v, "request.metadata.resourceVersion")})
				if err != nil || (code != 200 && code != 412 && code != 409) {
					return fmt.Errorf("approve decommission: HTTP %d, %v", code, err)
				}
			}
		}
		cleanupPause(deadline)
	}
	return fmt.Errorf("production decommission %s timed out", id)
}
func (s *suite) sweep() {
	// Registered before setup, so this runs after every per-object cleanup even
	// when setup or a journey fails. Discovery includes controller-created kinds.
	removed, remaining, failures := 0, 0, 0
	for org := range s.c.Organizations {
		blocked := false
		for _, kind := range s.cleanupKinds {
			primaryBinding := contains([]string{"releasebindings", "resourcereleasebindings", "projectreleasebindings"}, kind)
			if blocked && !primaryBinding {
				s.t.Logf("sweep withheld dependent objects in %s: binding cleanup was not confirmed", s.c.Organizations[org].Name)
				break
			}
			items, err := s.cleanupList(org, kind)
			if err != nil {
				failures++
				s.t.Errorf("sweep %s/%s: %v", s.c.Organizations[org].Name, kind, err)
				if primaryBinding {
					blocked = true
				}
				continue
			}
			for _, item := range items {
				name := str(item, "metadata.name")
				if !strings.Contains(name, "-"+s.run+"-") {
					continue
				}
				if err := s.cleanupNow(org, kind, name); err != nil {
					failures++
					s.t.Errorf("sweep %s/%s: %v", kind, name, err)
					if primaryBinding {
						blocked = true
					}
				} else {
					removed++
				}
			}
		}
		for _, kind := range s.cleanupKinds {
			items, err := s.cleanupList(org, kind)
			if err != nil {
				failures++
				s.t.Errorf("sweep verification %s: %v", kind, err)
				continue
			}
			for _, item := range items {
				if name := str(item, "metadata.name"); strings.Contains(name, "-"+s.run+"-") {
					remaining++
					s.t.Errorf("run %s leftover: %s/%s/%s", s.run, s.c.Organizations[org].Name, kind, name)
				}
			}
		}
	}
	apps, appFailures := s.verifyApplicationCleanup()
	remaining += apps
	failures += appFailures
	s.t.Logf("cleanup report run=%s swept=%d remaining=%d errors=%d; ChangeRequests retained as audit records", s.run, removed, remaining, failures)
}
