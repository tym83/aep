//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func readMatrix() (matrix, error) {
	var m matrix
	b, err := os.ReadFile("matrix.yaml")
	if err == nil {
		err = yaml.UnmarshalStrict(b, &m)
	}
	return m, err
}
func (s *suite) matrix() {
	m, err := readMatrix()
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Log("role | action | target | expected HTTP | actual HTTP; response bodies redacted")
	for _, role := range m.Roles {
		for _, name := range sortedActions(m) {
			a := m.Actions[name]
			for target := range s.c.Organizations {
				if role == "other" && target == 1 {
					continue
				}
				if a.Path == "/v1/me" && target == 1 {
					continue
				}
				s.t.Run(role+"/"+name+"/"+s.c.Organizations[target].Name, func(t *testing.T) {
					x := *s
					x.t = t
					if a.Fixture == "self" && (!contains([]string{"developer", "platform", "admin"}, role) || target != 0) {
						t.Skip("approve-own applies only to creator roles in their own organization")
					}
					if strings.Contains(name, "logs") && (x.c.Organizations[target].LogsProject == "" || x.c.Organizations[target].LogsComponent == "") {
						t.Skip("logsProject and logsComponent are not configured for this organization")
					}
					if a.Endpoint == "mcp" && x.c.Endpoints["mcp"] == "" {
						t.Skip("MCP not configured")
					}
					cross := target == 1 || role == "other"
					allow := contains(a.Allow, role) && !cross
					if a.Path == "/v1/me" {
						allow = role != "anonymous"
					}
					expected := a.Success
					if !allow {
						expected = []int{403}
						if cross {
							expected = []int{403, 404}
						}
					}
					if role == "anonymous" {
						expected = []int{401}
						// The entry point denies the direct log route before authentication.
						if strings.HasPrefix(name, "direct-") {
							expected = []int{401, 403}
						}
					}
					unique := x.name("cell")
					body := x.expand(a.Body, target, unique)
					path := x.expand(a.Path, target, unique).(string)
					if spec := x.c.Resources.ProtectedSpecs[name]; spec != nil && contains([]string{"create-environments", "create-dataplanes", "create-resourcetypes"}, name) {
						body = object(unique, x.expand(spec, target, unique).(map[string]any), x.run)
					}
					if name == "create-application-secret" {
						body = object(unique, x.expand(x.c.Resources.SecretSpec, target, unique).(map[string]any), x.run)
					}
					if name == "create-component" {
						body = object(unique, x.expand(x.c.Resources.ComponentSpec, target, unique).(map[string]any), x.run)
					}
					if name == "create-resource" {
						body.(map[string]any)["spec"].(map[string]any)["parameters"] = x.c.Resources.Parameters
					}
					if name == "bind-nonprod" {
						resource := x.name("matrix-db")
						x.createOC(target, "developer", "resources", resource, map[string]any{"owner": map[string]any{"projectName": x.fixtures[target].project}, "type": map[string]any{"kind": "ClusterResourceType", "name": x.c.Resources.ResourceType}, "parameters": x.c.Resources.Parameters})
						release := x.latest(target, "resources", resource)
						spec := body.(map[string]any)["spec"].(map[string]any)
						spec["owner"].(map[string]any)["resourceName"] = resource
						spec["resourceRelease"] = release
						spec["retainPolicy"] = "Delete"
						body.(map[string]any)["metadata"].(map[string]any)["name"] = resource + "-" + x.c.Resources.Environment
					}
					if name == "bind-prod-direct" {
						body.(map[string]any)["metadata"].(map[string]any)["name"] = x.fixtures[target].resource + "-" + x.c.Resources.ProdEnvironment
						t.Cleanup(func() {
							code, _ := x.call(target, "developer", "openchoreo", "GET", x.oc(target, "/resourcereleasebindings/"+x.fixtures[target].resource+"-"+x.c.Resources.ProdEnvironment), nil, nil)
							if code == 404 {
								return
							}
							if code != 200 {
								t.Error("cannot verify production cleanup")
								return
							}
							id := x.newChange(target, "developer", "change", "decommission", x.fixtures[target].resource, "")
							x.phase(target, "developer", "change", id, "Pending")
							x.decide(target, "approver", "change", id, "approve", 200)
							x.phase(target, "developer", "change", id, "Applied")
						})
					}
					headers := map[string]string{"Idempotency-Key": x.name("key")}
					if a.Fixture == "change" || a.Fixture == "decision" || a.Fixture == "self" {
						id := x.fixtures[target].change
						if a.Fixture == "decision" || a.Fixture == "self" {
							// An approval of a decommission for an absent run-owned target cannot
							// create production data. It still exercises the real Pending/SoD path.
							creator := "developer"
							if a.Fixture == "decision" && role == "developer" && !cross {
								creator = "admin"
							}
							if a.Fixture == "self" && contains([]string{"developer", "platform", "admin"}, role) && !cross {
								creator = role
							}
							id = x.newChange(target, creator, "change", "decommission", x.name("absent"), "")
							v := x.phase(target, "developer", "change", id, "Pending")
							headers["If-Match"] = str(v, "request.metadata.resourceVersion")
						}
						path = strings.ReplaceAll(path, "{change}", id)
					}
					if a.Endpoint == "openchoreo" && a.Method == "POST" {
						// Register exact names even for expected denials, so regressions do not
						// leave newly created nonprod objects behind.
						if spec, ok := body.(map[string]any); ok {
							if md, ok := spec["metadata"].(map[string]any); ok {
								n, _ := md["name"].(string)
								if n != "" && !strings.Contains(name, "prod-direct") {
									parts := strings.Split(path, "/")
									x.cleanupObject(target, parts[len(parts)-1], n)
								}
							}
						}
					}
					if a.Endpoint == "mcp" {
						x.mcpCell(role, target, a.Path, allow)
						return
					}
					code, raw := x.call(0, role, a.Endpoint, a.Method, path, body, headers)
					// Pending status can change between the fixture read and decision.
					for attempt := 0; code == 412 && attempt < 2 && (a.Fixture == "self" || a.Fixture == "decision"); attempt++ {
						v := x.require(target, "developer", "change", "GET", strings.TrimSuffix(path, "/decision"), nil, nil, 200)
						headers["If-Match"] = str(v, "request.metadata.resourceVersion")
						code, raw = x.call(0, role, a.Endpoint, a.Method, path, body, headers)
					}
					filtered := !allow && role != "anonymous" && a.Endpoint == "openchoreo" && a.Method == "GET" && a.List && code == 200 && emptyTargetList(raw, x.c.Organizations[target].Name)
					ok := filtered
					for _, want := range expected {
						ok = ok || code == want
					}
					if !ok {
						t.Errorf("%s | %s | %d | %v | %d", role, name, target, expected, code)
					}
					if filtered {
						x.noLeaks(raw, target)
					} else if cross && a.Path != "/v1/me" && role != "anonymous" {
						x.denied(code, raw, target)
					}
					if a.Path == "/v1/me" && role != "anonymous" {
						var v map[string]any
						if json.Unmarshal([]byte(raw), &v) != nil {
							t.Error("invalid me JSON")
						}
						foreign := 1
						if role == "other" {
							foreign = 0
						}
						if strings.Contains(raw, x.c.Organizations[foreign].Name) {
							t.Error("me leaked foreign organization")
						}
					}
				})
			}
		}
	}
}

// A namespaced list must explicitly contain items; malformed responses never
// count as denial. Missing namespace on an item belongs to the requested scope.
func emptyTargetList(raw, organization string) bool {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &envelope) != nil || envelope["items"] == nil || string(envelope["items"]) == "null" {
		return false
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return false
	}
	for _, item := range list.Items {
		ns := str(item, "metadata.namespace")
		if ns == "" || ns == organization {
			return false
		}
	}
	return true
}
