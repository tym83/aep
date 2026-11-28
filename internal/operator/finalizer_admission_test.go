package operator

import (
	"testing"

	"github.com/google/cel-go/cel"
)

// Garbage collection and OpenChoreo cleanup must be able to release finalizers on
// reserved objects that are being deleted, without reopening writes to live ones.
func TestFinalizerReleaseExemption(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(finalizerReleaseExemption)
	if issues != nil && issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]any{"roleMappings": []any{map[string]any{"roleRef": map[string]any{"name": "aep-admin"}}}}
	other := map[string]any{"roleMappings": []any{map[string]any{"roleRef": map[string]any{"name": "aep-platform-protected"}}}}
	deleting := map[string]any{"deletionTimestamp": "2026-11-28T04:06:09Z", "finalizers": []any{"foregroundDeletion"}}
	released := map[string]any{"deletionTimestamp": "2026-11-28T04:06:09Z"}
	live := map[string]any{"finalizers": []any{"foregroundDeletion"}}
	for _, tc := range []struct {
		name          string
		op            string
		old, obj      map[string]any
		admissionRuns bool
	}{
		{"release on deleting object", "UPDATE", map[string]any{"metadata": deleting, "spec": spec}, map[string]any{"metadata": released, "spec": spec}, false},
		{"release on object without spec", "UPDATE", map[string]any{"metadata": deleting}, map[string]any{"metadata": released}, false},
		{"spec change while deleting", "UPDATE", map[string]any{"metadata": deleting, "spec": spec}, map[string]any{"metadata": released, "spec": other}, true},
		{"spec added while deleting", "UPDATE", map[string]any{"metadata": deleting}, map[string]any{"metadata": released, "spec": spec}, true},
		{"update of live object", "UPDATE", map[string]any{"metadata": live, "spec": spec}, map[string]any{"metadata": map[string]any{}, "spec": spec}, true},
		{"create", "CREATE", nil, map[string]any{"metadata": map[string]any{}, "spec": spec}, true},
		{"delete", "DELETE", map[string]any{"metadata": deleting, "spec": spec}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var old, obj any
			if tc.old != nil {
				old = tc.old
			}
			if tc.obj != nil {
				obj = tc.obj
			}
			out, _, err := prg.Eval(map[string]any{"request": map[string]any{"operation": tc.op}, "object": obj, "oldObject": old})
			if err != nil {
				t.Fatal(err)
			}
			if out.Value() != tc.admissionRuns {
				t.Fatalf("admission runs = %v, want %v", out.Value(), tc.admissionRuns)
			}
		})
	}
}
