package bridge

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func condition(obj *unstructured.Unstructured, name string) metav1.Condition {
	c := metav1.Condition{Type: name, Status: metav1.ConditionUnknown, Reason: "Pending", Message: name + " condition has not been reported", LastTransitionTime: metav1.Now()}
	if obj == nil {
		return c
	}
	entries, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if ok && m["type"] == name {
			if runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c) == nil {
				if c.Reason == "" {
					c.Reason = "Pending"
				}
				if c.LastTransitionTime.IsZero() {
					c.LastTransitionTime = metav1.Now()
				}
				return c
			}
		}
	}
	return c
}
func Readiness(app, hr *unstructured.Unstructured, specMatches bool) []metav1.Condition {
	provisioned := condition(app, "Ready")
	provisioned.Type = "Provisioned"
	ready := condition(app, "WorkloadsReady")
	ready.Type = "Ready"
	if provisioned.Status != metav1.ConditionTrue {
		ready = provisioned
		ready.Type = "Ready"
	} else if ready.Status == metav1.ConditionTrue {
		ready = condition(hr, "Ready")
	}
	observed, found := int64(0), false
	if hr != nil {
		observed, found, _ = unstructured.NestedInt64(hr.Object, "status", "observedGeneration")
	}
	if !specMatches || hr == nil || !found || observed != hr.GetGeneration() {
		ready = metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Updating", Message: "Waiting for the applied spec and current HelmRelease generation", LastTransitionTime: metav1.Now()}
	}
	degraded := metav1.Condition{Type: "Degraded", Status: metav1.ConditionFalse, Reason: "Healthy", Message: "No degradation reported", LastTransitionTime: metav1.Now()}
	for _, c := range []metav1.Condition{condition(app, "Ready"), condition(app, "WorkloadsReady"), condition(hr, "Ready")} {
		if c.Status == metav1.ConditionFalse {
			degraded = c
			degraded.Type = "Degraded"
			degraded.Status = metav1.ConditionTrue
			break
		}
	}
	return []metav1.Condition{provisioned, ready, degraded}
}
