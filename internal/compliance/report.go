// Package compliance defines the bounded management-cluster snapshot served to users.
package compliance

import (
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Counts struct {
	Pass  int64 `json:"pass"`
	Fail  int64 `json:"fail"`
	Warn  int64 `json:"warn"`
	Error int64 `json:"error"`
	Skip  int64 `json:"skip"`
}
type Failure struct {
	Policy     string `json:"policy"`
	Rule       string `json:"rule"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}
type Snapshot struct {
	Tier              string            `json:"tier"`
	CollectedAt       time.Time         `json:"collectedAt"`
	Policies          map[string]Counts `json:"policies"`
	FailingResources  []Failure         `json:"failingResources"`
	FailuresTruncated bool              `json:"failuresTruncated"`
}

// Aggregate uses per-result counts because report.summary is not partitioned by policy.
// It deliberately omits arbitrary messages and resource contents from the public snapshot.
func Aggregate(tier string, reports []unstructured.Unstructured, now time.Time) Snapshot {
	out := Snapshot{Tier: tier, CollectedAt: now, Policies: map[string]Counts{}, FailingResources: []Failure{}}
	failures := map[Failure]bool{}
	for _, report := range reports {
		results, _, _ := unstructured.NestedSlice(report.Object, "results")
		scope, _, _ := unstructured.NestedMap(report.Object, "scope")
		for _, raw := range results {
			result, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			policy, _, _ := unstructured.NestedString(result, "policy")
			if policy == "" {
				continue
			}
			state, _, _ := unstructured.NestedString(result, "result")
			count := out.Policies[policy]
			switch state {
			case "pass":
				count.Pass++
			case "fail":
				count.Fail++
			case "warn":
				count.Warn++
			case "error":
				count.Error++
			case "skip":
				count.Skip++
			default:
				continue
			}
			out.Policies[policy] = count
			if state != "fail" {
				continue
			}
			rule, _, _ := unstructured.NestedString(result, "rule")
			resources, _, _ := unstructured.NestedSlice(result, "resources")
			if len(resources) == 0 && scope != nil {
				resources = []any{scope}
			}
			for _, rawResource := range resources {
				resource, ok := rawResource.(map[string]any)
				if !ok {
					continue
				}
				str := func(k string) string { v, _, _ := unstructured.NestedString(resource, k); return v }
				failures[Failure{policy, rule, str("apiVersion"), str("kind"), str("namespace"), str("name")}] = true
			}
		}
	}
	for failure := range failures {
		out.FailingResources = append(out.FailingResources, failure)
	}
	sort.Slice(out.FailingResources, func(i, j int) bool {
		a, b := out.FailingResources[i], out.FailingResources[j]
		return failureKey(a) < failureKey(b)
	})
	if len(out.FailingResources) > 50 {
		out.FailuresTruncated = true
		out.FailingResources = out.FailingResources[:50]
	}
	return out
}
func failureKey(f Failure) string {
	return f.Policy + "\x00" + f.Namespace + "\x00" + f.Kind + "\x00" + f.Name + "\x00" + f.Rule + "\x00" + f.APIVersion
}
