//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Register before the create request: a lost response must not strand an object.
// Only exact run-owned names are eligible; no namespace-wide deletion exists.
func (s *suite) cleanupObject(org int, kind, name string) {
	s.t.Helper()
	if !strings.Contains(name, "-"+s.run+"-") {
		s.t.Fatal("refusing cleanup of an object without this run id")
	}
	s.t.Cleanup(func() {
		if err := s.cleanupNow(org, kind, name); err != nil {
			s.t.Errorf("cleanup %s/%s failed: %v", kind, name, err)
		}
	})
}

func (s *suite) createOC(org int, role, kind, name string, spec map[string]any) {
	s.cleanupObject(org, kind, name)
	s.require(org, role, "openchoreo", "POST", s.oc(org, "/"+kind), object(name, spec, s.run), nil, 200, 201, 202)
}
func (s *suite) latest(org int, kind, name string) string {
	release := ""
	s.wait("release published", func() bool {
		v := s.require(org, "developer", "openchoreo", "GET", s.oc(org, "/"+kind+"/"+name), nil, nil, 200)
		release = str(v, "status.latestRelease.name")
		return release != ""
	})
	return release
}
func (s *suite) binding(f fixture, environment string, project bool) map[string]any {
	if project {
		return map[string]any{"environment": environment, "mode": "Manage", "owner": map[string]any{"projectName": f.project}, "projectRelease": f.projectRelease}
	}
	return map[string]any{"environment": environment, "owner": map[string]any{"projectName": f.project, "resourceName": f.resource}, "resourceRelease": f.resourceRelease, "retainPolicy": "Delete"}
}
func (s *suite) expand(v any, org int, name string) any {
	f := s.fixtures[org]
	b, _ := json.Marshal(v)
	logsProject, logsComponent := f.project, f.component
	if s.c.Organizations[org].LogsProject != "" {
		logsProject = s.c.Organizations[org].LogsProject
	}
	if s.c.Organizations[org].LogsComponent != "" {
		logsComponent = s.c.Organizations[org].LogsComponent
	}
	logs := func(env string) string {
		if !strings.Contains(string(b), "ogsPath}") {
			return ""
		}
		path := strings.NewReplacer("{org}", s.c.Organizations[org].Name, "{project}", logsProject, "{component}", logsComponent, "{environment}", env).Replace(s.c.Resources.LogsPath)
		if strings.Contains(path, "{podName}") {
			path = strings.ReplaceAll(path, "{podName}", s.logsPod(org, logsComponent, env))
		}
		return path
	}
	r := strings.NewReplacer("{org}", s.c.Organizations[org].Name, "{project}", f.project, "{resource}", f.resource, "{component}", f.component, "{resourceType}", s.c.Resources.ResourceType, "{environment}", s.c.Resources.Environment, "{prod}", s.c.Resources.ProdEnvironment, "{projectRelease}", f.projectRelease, "{resourceRelease}", f.resourceRelease, "{name}", name, "{run}", s.run, "{logsPath}", logs(s.c.Resources.ProdEnvironment), "{nonprodLogsPath}", logs(s.c.Resources.Environment))
	var out any
	if err := json.Unmarshal([]byte(r.Replace(string(b))), &out); err != nil {
		s.t.Fatal("invalid expanded fixture")
	}
	return out
}
func (s *suite) setup(org int) {
	f := fixture{project: s.name(s.c.Resources.Project), resource: s.name("db"), component: s.name("app")}
	s.fixtures[org] = f
	s.createOC(org, "developer", "projects", f.project, map[string]any{"deploymentPipelineRef": map[string]any{"name": "default"}})
	f.projectRelease = s.latest(org, "projects", f.project)
	s.fixtures[org] = f
	s.createOC(org, "developer", "resources", f.resource, map[string]any{"owner": map[string]any{"projectName": f.project}, "type": map[string]any{"kind": "ClusterResourceType", "name": s.c.Resources.ResourceType}, "parameters": s.c.Resources.Parameters})
	f.resourceRelease = s.latest(org, "resources", f.resource)
	s.fixtures[org] = f
	s.createOC(org, "developer", "components", f.component, s.expand(s.c.Resources.ComponentSpec, org, f.component).(map[string]any))
	s.createOC(org, "developer", "projectreleasebindings", f.project+"-"+s.c.Resources.Environment, s.binding(f, s.c.Resources.Environment, true))
	s.createOC(org, "developer", "resourcereleasebindings", f.resource+"-"+s.c.Resources.Environment, s.binding(f, s.c.Resources.Environment, false))
	s.fixtures[org].change = s.newChange(org, "developer", "change", "promote", f.resource, f.resourceRelease)
}
func (s *suite) newChange(org int, role, endpoint, operation, name, release string) string {
	target := map[string]any{"kind": "Resource", "name": name, "project": s.fixtures[org].project, "environment": s.c.Resources.ProdEnvironment}
	desired := map[string]any{}
	if operation == "decommission" {
		desired["deleteData"] = true
	}
	if operation == "promote" {
		desired["releaseName"] = release
	}
	model := "normal"
	if operation == "breakglass" {
		target = map[string]any{"kind": "Organization", "name": s.c.Organizations[org].Name}
		desired = map[string]any{"duration": "5m", "scope": "prod"}
		model = "emergency"
	}
	v := s.require(org, role, endpoint, "POST", "/v1/changes", map[string]any{"organization": s.c.Organizations[org].Name, "operation": operation, "model": model, "target": target, "desired": desired, "justification": "Acceptance " + s.name(operation)}, map[string]string{"Idempotency-Key": s.name("intent")}, 200, 201)
	id := str(v, "request.metadata.name")
	if id == "" {
		s.t.Fatal("change response missing name")
	}
	return id
}
func (s *suite) changePath(org int, id string) string {
	return "/v1/changes/" + s.c.Organizations[org].Name + "/" + id
}
func (s *suite) phase(org int, reader, endpoint, id, want string) map[string]any {
	var result map[string]any
	s.wait("change reaches "+want, func() bool {
		result = s.require(org, reader, endpoint, "GET", s.changePath(org, id), nil, nil, 200)
		phase := str(result, "request.status.phase")
		if phase != want && contains([]string{"Failed", "Rejected", "Expired", "Superseded"}, phase) {
			s.t.Fatalf("unexpected terminal change phase %s", phase)
		}
		return phase == want
	})
	return result
}
func (s *suite) decide(org int, role, endpoint, id, decision string, want int) {
	v := s.require(org, "developer", endpoint, "GET", s.changePath(org, id), nil, nil, 200)
	s.require(org, role, endpoint, "POST", s.changePath(org, id)+"/decision", map[string]any{"decision": decision, "evidence": "Acceptance " + s.run}, map[string]string{"If-Match": str(v, "request.metadata.resourceVersion")}, want)
}
func (s *suite) journeys() {
	s.t.Run("developer-ready-and-outputs", func(t *testing.T) {
		x := *s
		x.t = t
		f := x.fixtures[0]
		x.wait("dev binding Ready", func() bool {
			v := x.require(0, "developer", "openchoreo", "GET", x.oc(0, "/resourcereleasebindings/"+f.resource+"-"+x.c.Resources.Environment), nil, nil, 200)
			return ready(v)
		})
		path := x.expand(x.c.Resources.OutputsPath, 0, "").(string)
		x.wait("connection outputs present", func() bool {
			v := x.require(0, "developer", "openchoreo", "GET", path, nil, nil, 200)
			for _, key := range x.c.Resources.OutputFields {
				value := field(v, key)
				if value == nil || value == "" {
					return false
				}
			}
			return true
		})
	})
	s.t.Run("approver-promotion-and-rejection", func(t *testing.T) {
		x := *s
		x.t = t
		f := x.fixtures[0]
		id := x.newChange(0, "developer", "change", "promote", f.resource, f.resourceRelease)
		// Register approved decommission before approval, including ambiguous responses.
		t.Cleanup(func() {
			cleanup := x.newChange(0, "developer", "change", "decommission", f.resource, "")
			x.phase(0, "developer", "change", cleanup, "Pending")
			x.decide(0, "approver", "change", cleanup, "approve", 200)
			x.phase(0, "developer", "change", cleanup, "Applied")
			x.wait("production binding removed", func() bool {
				code, _ := x.call(0, "developer", "openchoreo", "GET", x.oc(0, "/resourcereleasebindings/"+f.resource+"-"+x.c.Resources.ProdEnvironment), nil, nil)
				return code == 404
			})
		})
		x.phase(0, "developer", "change", id, "Pending")
		x.decide(0, "developer", "change", id, "approve", 403)
		x.decide(0, "approver", "portal", id, "approve", 200)
		x.phase(0, "developer", "change", id, "Applied")
		x.require(0, "developer", "openchoreo", "GET", x.oc(0, "/resourcereleasebindings/"+f.resource+"-"+x.c.Resources.ProdEnvironment), nil, nil, 200)
		rejected := x.newChange(0, "developer", "portal", "promote", f.resource, f.resourceRelease)
		x.phase(0, "developer", "portal", rejected, "Pending")
		x.decide(0, "approver", "portal", rejected, "reject", 200)
		x.phase(0, "developer", "change", rejected, "Rejected")
	})
	s.t.Run("platform-application-secret", func(t *testing.T) {
		x := *s
		x.t = t
		name := x.name("secret")
		x.createOC(0, "platform", "secretreferences", name, x.expand(x.c.Resources.SecretSpec, 0, name).(map[string]any))
	})
	s.t.Run("admin-scoped-binding", func(t *testing.T) {
		x := *s
		x.t = t
		name := x.name("role")
		x.createOC(0, "admin", "authzroles", name, map[string]any{"actions": []string{"project:view"}})
		binding := x.name("binding")
		// Copy the operator's rendered restriction, including every nonprod
		// environment in its original order: the VAP requires exact equality.
		rendered := x.require(0, "admin", "openchoreo", "GET", x.oc(0, "/authzrolebindings/aep-developer"), nil, nil, 200)
		spec, err := delegatedBinding(rendered, name, x.run)
		if err != nil {
			t.Fatal(err)
		}
		unrestricted := x.name("unrestricted")
		x.cleanupObject(0, "authzrolebindings", unrestricted)
		bad := map[string]any{"entitlement": spec["entitlement"], "effect": "allow", "roleMappings": []any{map[string]any{"roleRef": map[string]any{"kind": "AuthzRole", "name": name}}}}
		x.require(0, "admin", "openchoreo", "POST", x.oc(0, "/authzrolebindings"), object(unrestricted, bad, x.run), nil, 422)
		x.createOC(0, "admin", "authzrolebindings", binding, spec)
		x.cleanupObject(1, "authzrolebindings", binding)
		code, body := x.call(0, "admin", "openchoreo", "POST", x.oc(1, "/authzrolebindings"), object(binding, spec, x.run), nil)
		x.denied(code, body, 1)
	})
	s.t.Run("breakglass-recorded", func(t *testing.T) {
		x := *s
		x.t = t
		id := x.newChange(0, "breakglass-requester", "change", "breakglass", "", "")
		x.phase(0, "breakglass", "change", id, "Pending")
		x.decide(0, "breakglass-requester", "change", id, "approve", 403)
		x.decide(0, "security-oncall", "change", id, "approve", 200)
		v := x.phase(0, "breakglass", "change", id, "Applied")
		if !condition(v, "request.status.conditions", "Applied", "Recorded") {
			t.Fatal("expected Applied=True, reason Recorded")
		}
	})
	// Auditor and finance journeys are all corresponding matrix cells, including
	// operational reads, protected writes, costs and portal equivalents.
}
func condition(v map[string]any, path, kind, reason string) bool {
	rows, _ := field(v, path).([]any)
	for _, row := range rows {
		m, _ := row.(map[string]any)
		if m["type"] == kind && m["status"] == "True" && (reason == "" || m["reason"] == reason) {
			return true
		}
	}
	return false
}
func ready(v map[string]any) bool {
	if !condition(v, "status.conditions", "Ready", "") {
		return false
	}
	rows, _ := field(v, "status.conditions").([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["type"] == "Ready" {
			// The OpenChoreo API omits metadata.generation; compare only when it is served.
			generation := field(v, "metadata.generation")
			return generation == nil || m["observedGeneration"] == generation
		}
	}
	return false
}
func (s *suite) denied(code int, body string, org int) {
	s.t.Helper()
	if code != 403 && code != 404 {
		s.t.Errorf("cross-org expected 403/404, got %d", code)
	}
}
func (s *suite) noLeaks(body string, org int) {
	s.t.Helper()
	markers := append([]string{s.c.Organizations[org].Name, s.fixtures[org].project, s.fixtures[org].resource, s.fixtures[org].component}, s.c.Organizations[org].LeakMarkers...)
	for _, marker := range markers {
		if marker != "" && strings.Contains(body, marker) {
			s.t.Error("cross-org response leaked a target marker (body redacted)")
		}
	}
}

func delegatedBinding(rendered map[string]any, role, run string) (map[string]any, error) {
	mappings, _ := field(rendered, "spec.roleMappings").([]any)
	for _, row := range mappings {
		m, _ := row.(map[string]any)
		if str(m, "roleRef.name") != "aep-developer" || str(m, "roleRef.kind") != "AuthzRole" {
			continue
		}
		conditions, _ := m["conditions"].([]any)
		if len(conditions) == 0 {
			break
		}
		return map[string]any{"entitlement": map[string]any{"claim": "groups", "value": "e2e-unused-" + run}, "effect": "allow", "roleMappings": []any{map[string]any{"roleRef": map[string]any{"kind": "AuthzRole", "name": role}, "conditions": conditions}}}, nil
	}
	return nil, fmt.Errorf("operator developer binding has no nonprod restrictions")
}

// logsPod finds a pod of the log component through the binding's resource
// tree: the OpenChoreo logs route requires a pod name, which is generated.
// A missing binding or pod yields "absent", so denial cells still exercise
// authorization while allowed cells fail on the response.
func (s *suite) logsPod(org int, component, env string) string {
	key := fmt.Sprintf("%d/%s/%s", org, component, env)
	if pod, ok := s.pods[key]; ok {
		return pod
	}
	role := "developer"
	if s.c.Organizations[org].Users["platform"] != "" {
		role = "platform"
	}
	tree := s.c.Resources.LogsTreePath
	if tree == "" {
		tree = "/api/v1/namespaces/{org}/releasebindings/{component}-{environment}/k8sresources/tree"
	}
	tree = strings.NewReplacer("{org}", s.c.Organizations[org].Name, "{component}", component, "{environment}", env).Replace(tree)
	pod := "absent"
	if code, raw := s.call(org, role, "openchoreo", "GET", tree, nil, nil); code == 200 {
		if name := podFromTree(raw); name != "" {
			pod = name
		}
	}
	if pod == "absent" {
		s.t.Logf("no pod for logs component in %s of organization %d", env, org)
	}
	s.pods[key] = pod
	return pod
}
func podFromTree(raw string) string {
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
	if json.Unmarshal([]byte(raw), &tree) != nil {
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
