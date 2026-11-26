package audit

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestDeletionAndIntentEvidence(t *testing.T) {
	spec := map[string]any{"operation": "update", "model": "standard", "desired": map[string]any{"replicas": int64(3)}, "justification": "capacity", "window": map[string]any{"start": "tomorrow"}, "externalRef": "INC-1", "requesterGroups": []any{"operators"}}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "aep.aenix.io/v1alpha1", "kind": "ChangeRequest", "metadata": map[string]any{"name": "change"}, "spec": spec, "status": map[string]any{"phase": "Applied"}}}
	events := Normalize(u, true)
	if len(events) != 1 || events[0].Action != "changerequest.deleted" {
		t.Fatalf("%+v", events)
	}
	for k, v := range spec {
		if Digest(events[0].Details[k]) != Digest(v) {
			t.Fatal("lost evidence", k)
		}
	}
	if events[0].Details["desiredHash"] != Digest(spec["desired"]) {
		t.Fatal("wrong desired hash")
	}
	if organizationInitialAction(false) != "organization.observed" || organizationInitialAction(true) != "organization.created" {
		t.Fatal("initial observation mislabeled")
	}
}
