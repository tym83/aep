package operator

import (
	"testing"

	"github.com/google/cel-go/cel"
)

// argocd-server must be able to create the default project at startup, and
// nothing else: Argo CD service accounts cannot change or remove projects.
func TestArgoProjectAdmission(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(argoProjectAdmission)
	if issues != nil && issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	server := "system:serviceaccount:argocd:argocd-server"
	for _, tc := range []struct {
		user, op, name string
		allow          bool
	}{
		{server, "CREATE", "default", true},
		{server, "CREATE", "aep-payments", false},
		{server, "UPDATE", "default", false},
		{server, "DELETE", "default", false},
		{"system:serviceaccount:argocd:argocd-application-controller", "UPDATE", "aep-payments", false},
		{"aep-operator", "UPDATE", "default", true},
		{"aep-operator", "CREATE", "aep-payments", true},
	} {
		out, _, err := prg.Eval(map[string]any{"request": map[string]any{"userInfo": map[string]any{"username": tc.user}, "operation": tc.op}, "object": map[string]any{"metadata": map[string]any{"name": tc.name}}})
		if err != nil {
			t.Fatal(err)
		}
		if out.Value() != tc.allow {
			t.Errorf("%s %s %s: allowed=%v", tc.user, tc.op, tc.name, out.Value())
		}
	}
}
