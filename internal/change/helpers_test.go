package change

import (
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func rawJSON(s string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(s)} }

func promoteSpec(model api.ChangeModel, release string) api.ChangeRequestSpec {
	return api.ChangeRequestSpec{
		Organization:    "acme",
		Operation:       api.OperationPromote,
		Model:           model,
		Target:          api.ChangeTarget{Kind: "Component", Project: "shop", Name: "api", Environment: "prod"},
		Desired:         rawJSON(`{"releaseName":"` + release + `"}`),
		Justification:   "ship the fix",
		Requester:       "alice",
		RequesterGroups: []string{"aep-acme-developer"},
	}
}

func changeRequest(name string, spec api.ChangeRequestSpec, phase api.ChangePhase, created time.Time) api.ChangeRequest {
	return api.ChangeRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme", CreationTimestamp: metav1.NewTime(created)},
		Spec:       spec,
		Status:     api.ChangeRequestStatus{Phase: phase, TargetKey: TargetKey(spec)},
	}
}

func approval(user string, groups []string, decision string, at time.Time) api.ChangeApproval {
	return api.ChangeApproval{User: user, Groups: groups, Role: "x", Decision: decision, Time: metav1.NewTime(at), Source: "portal"}
}
