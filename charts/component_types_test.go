package charts

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

func TestComponentTypes(t *testing.T) {
	for _, overrides := range [][]string{nil, {"resources.limits.cpu=2", "resources.requests.memory=64Mi", "allowedWorkflows={custom-builder}"}} {
		out, err := render(t, "aep-component-types", overrides...)
		if err != nil {
			t.Fatal(out, err)
		}
		docs := mcpDocuments(t, out)
		if len(docs) != 4 {
			t.Fatalf("got %d types", len(docs))
		}
		seen := map[string]bool{}
		for _, d := range docs {
			name := d["metadata"].(map[string]any)["name"].(string)
			seen[name] = true
			t.Run(name+fmt.Sprint(overrides), func(t *testing.T) {
				if d["kind"] != "ClusterComponentType" || d["metadata"].(map[string]any)["namespace"] != nil {
					t.Fatal(d)
				}
				if d["metadata"].(map[string]any)["annotations"].(map[string]any)["openchoreo.dev/description"] == "" {
					t.Fatal("missing description")
				}
				spec := d["spec"].(map[string]any)
				props := spec["environmentConfigs"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)
				// Use schema defaults as the sample environment; CEL checks preserve value types.
				var defaults func(map[string]any) any
				defaults = func(s map[string]any) any {
					if s["type"] == "object" {
						m := map[string]any{}
						for k, v := range s["properties"].(map[string]any) {
							m[k] = defaults(v.(map[string]any))
						}
						return m
					}
					return s["default"]
				}
				config := map[string]any{}
				for _, k := range []string{"resources", "runAsUser", "readOnlyRootFilesystem"} {
					config[k] = defaults(props[k].(map[string]any))
				}
				if config["runAsUser"] != float64(65532) || config["readOnlyRootFilesystem"] != true {
					t.Fatal(config)
				}
				env, err := cel.NewEnv(cel.Variable("environmentConfigs", cel.DynType))
				if err != nil {
					t.Fatal(err)
				}
				eval := func(s string) any {
					ast, issues := env.Compile(strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}"))
					if issues.Err() != nil {
						t.Fatal(issues.Err())
					}
					prg, e := env.Program(ast)
					if e != nil {
						t.Fatal(e)
					}
					v, _, e := prg.Eval(map[string]any{"environmentConfigs": config})
					if e != nil {
						t.Fatal(e)
					}
					return v.Value()
				}
				pods := 0
				ids := map[string]bool{}
				for _, r := range spec["resources"].([]any) {
					res := r.(map[string]any)
					ids[res["id"].(string)] = true
					tmpl := res["template"].(map[string]any)
					if _, e := json.Marshal(tmpl); e != nil {
						t.Fatal(e)
					}
					kind := tmpl["kind"]
					if kind != "Deployment" && kind != "CronJob" && kind != "Job" {
						continue
					}
					pods++
					ps := tmpl["spec"].(map[string]any)
					if kind == "CronJob" {
						ps = ps["jobTemplate"].(map[string]any)["spec"].(map[string]any)
					}
					pod := ps["template"].(map[string]any)["spec"].(map[string]any)
					sc := pod["securityContext"].(map[string]any)
					if sc["runAsNonRoot"] != true || sc["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" || pod["automountServiceAccountToken"] != false {
						t.Fatal(pod)
					}
					if eval(sc["runAsUser"].(string)) != float64(65532) {
						t.Fatal(sc)
					}
					if !strings.Contains(pod["volumes"].(string), `"emptyDir": {}`) {
						t.Fatal(pod)
					}
					for _, c := range pod["containers"].([]any) {
						container := c.(map[string]any)
						cs := container["securityContext"].(map[string]any)
						if cs["allowPrivilegeEscalation"] != false || !reflect.DeepEqual(cs["capabilities"].(map[string]any)["drop"], []any{"ALL"}) || eval(cs["readOnlyRootFilesystem"].(string)) != true {
							t.Fatal(cs)
						}
						if !strings.Contains(container["volumeMounts"].(string), `"mountPath": "/tmp"`) {
							t.Fatal(container)
						}
						for _, k := range []string{"requests", "limits"} {
							for _, r := range []string{"cpu", "memory"} {
								value := eval(container["resources"].(map[string]any)[k].(map[string]any)[r].(string))
								if value == nil || value == "" {
									t.Fatal("missing resource", k, r)
								}
								if k == "limits" && r == "cpu" {
									want := "500m"
									if len(overrides) > 0 {
										want = "2"
									}
									if value != want {
										t.Fatal(value, want)
									}
								}
							}
						}
					}
				}
				if pods != 1 {
					t.Fatal("expected one pod template", pods)
				}
				for _, id := range []string{"serviceaccount", "env-config", "file-config", "secret-env-external", "secret-file-external"} {
					if !ids[id] {
						t.Fatal("missing upstream handling", id)
					}
				}
				if (name == "aep-service") != ids["service"] {
					t.Fatal("endpoint resources", ids)
				}
				if name == "aep-migration" && (spec["workloadType"] != "job" || !ids["job"]) {
					t.Fatal(spec)
				}
			})
		}
		for _, name := range []string{"aep-service", "aep-worker", "aep-cron", "aep-migration"} {
			if !seen[name] {
				t.Fatal(name)
			}
		}
	}
	out, err := render(t, "aep-component-types", "types.service=false", "types.worker=false", "types.cron=false", "types.migration=false")
	if err != nil || len(mcpDocuments(t, out)) != 0 {
		t.Fatal(out, err)
	}
	out, err = render(t, "aep-component-types", "allowedWorkflows=null")
	if err != nil {
		t.Fatal(out, err)
	}
	for _, d := range mcpDocuments(t, out) {
		if len(d["spec"].(map[string]any)["allowedWorkflows"].([]any)) != 0 {
			t.Fatal(d)
		}
	}
}
