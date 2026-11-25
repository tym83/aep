package bridge

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func object(ready, workloads string, generation, observed int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready, "reason": "UpstreamReason", "message": "Upstream message", "lastTransitionTime": "2026-11-25T00:00:00Z"}, map[string]any{"type": "WorkloadsReady", "status": workloads, "reason": "WorkloadsReason", "message": "Workload message", "lastTransitionTime": "2026-11-25T00:00:00Z"}}}}}
	if generation > 0 {
		u.SetGeneration(generation)
		_ = unstructured.SetNestedField(u.Object, observed, "status", "observedGeneration")
	}
	return u
}
func TestReadiness(t *testing.T) {
	for _, tt := range []struct {
		name, app, workloads, hr string
		gen, observed            int64
		match, want              bool
	}{{"ready without app generation", "True", "True", "True", 2, 2, true, true}, {"app pending", "False", "True", "True", 2, 2, true, false}, {"workloads pending", "True", "False", "True", 2, 2, true, false}, {"helm pending", "True", "True", "False", 2, 2, true, false}, {"stale helm", "True", "True", "True", 2, 1, true, false}, {"spec drift", "True", "True", "True", 2, 2, false, false}, {"missing generation", "True", "True", "True", 0, 0, true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			conditions := Readiness(object(tt.app, tt.workloads, 0, 0), object(tt.hr, "", tt.gen, tt.observed), tt.match)
			if (conditions[1].Status == metav1.ConditionTrue) != tt.want {
				t.Fatal(conditions)
			}
			if tt.want && conditions[1].LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-25T00:00:00Z" {
				t.Fatal("upstream transition lost")
			}
			if tt.hr == "False" && conditions[2].Message != "Upstream message" {
				t.Fatal("upstream failure lost")
			}
		})
	}
	if Readiness(nil, nil, true)[1].Status == metav1.ConditionTrue {
		t.Fatal("missing resources ready")
	}
}
func TestConnectionIdentity(t *testing.T) {
	d, u := ConnectionIdentity(map[string]any{"databases": map[string]any{"appdb": map[string]any{"roles": map[string]any{"admin": []any{"zuser"}}}}, "users": map[string]any{"auser": map[string]any{}, "zuser": map[string]any{}}}, "Postgres")
	if d != "appdb" || u != "zuser" {
		t.Fatal(d, u)
	}
	d, u = ConnectionIdentity(nil, "Redis")
	if d != "0" || u != "default" {
		t.Fatal(d, u)
	}
}
