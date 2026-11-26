package compliance

import (
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
	"time"
)

func TestAggregate(t *testing.T) {
	reports := []unstructured.Unstructured{}
	for i := 59; i >= 0; i-- {
		results := []any{}
		for _, state := range []string{"pass", "fail", "warn", "error", "skip"} {
			results = append(results, map[string]any{"policy": "baseline", "rule": "rule", "result": state})
		}
		reports = append(reports, unstructured.Unstructured{Object: map[string]any{"scope": map[string]any{"apiVersion": "v1", "kind": "Pod", "namespace": "apps", "name": fmt.Sprintf("app-%02d", i)}, "results": results}})
	}
	// Legacy reports carry resources on the result instead of scope.
	reports = append(reports, unstructured.Unstructured{Object: map[string]any{"results": []any{map[string]any{"policy": "other", "result": "fail", "resources": []any{map[string]any{"kind": "Namespace", "name": "apps"}}}}}})
	now := time.Now().UTC()
	out := Aggregate("prod", reports, now)
	if out.Policies["baseline"] != (Counts{60, 60, 60, 60, 60}) {
		t.Fatal(out.Policies)
	}
	if out.Policies["other"].Fail != 1 || len(out.FailingResources) != 50 || !out.FailuresTruncated {
		t.Fatal(out)
	}
	if out.FailingResources[0].Name != "app-00" || out.FailingResources[49].Name != "app-49" || !out.CollectedAt.Equal(now) {
		t.Fatal(out)
	}
	empty := Aggregate("nonprod", nil, now)
	if empty.Policies == nil || empty.FailingResources == nil {
		t.Fatal("null collections")
	}
}
