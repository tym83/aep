package change

import (
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
)

func TestEvaluateApprovals(t *testing.T) {
	at := testNow
	later := testNow.Add(time.Minute)
	cases := []struct {
		name      string
		mutate    func(*api.ChangeRequestSpec)
		approvals []api.ChangeApproval
		want      api.ChangePhase
	}{
		{name: "no decisions stays pending", want: api.PhasePending},
		{name: "org approver approves", approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhaseApproved},
		{name: "keycloak group path is normalized", approvals: []api.ChangeApproval{
			approval("bob", []string{"/teams/aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhaseApproved},
		{name: "requester cannot approve own request", approvals: []api.ChangeApproval{
			approval("alice", []string{"aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhasePending},
		{name: "service account cannot approve", approvals: []api.ChangeApproval{
			approval("service-account-aep-change", []string{"aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhasePending},
		{name: "platform team cannot approve app changes", approvals: []api.ChangeApproval{
			approval("paul", []string{GroupPlatformTeam}, DecisionApprove, at),
		}, want: api.PhasePending},
		{name: "approver of another org cannot approve", approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-other-approver"}, DecisionApprove, at),
		}, want: api.PhasePending},
		{name: "eligible rejection rejects", approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-acme-approver"}, DecisionReject, at),
		}, want: api.PhaseRejected},
		{name: "rejection by a random user is ignored", approvals: []api.ChangeApproval{
			approval("mallory", []string{"aep-acme-developer"}, DecisionReject, at),
		}, want: api.PhasePending},
		{name: "earliest eligible decision wins: approve first", approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-acme-approver"}, DecisionApprove, at),
			approval("carol", []string{"aep-acme-approver"}, DecisionReject, later),
		}, want: api.PhaseApproved},
		{name: "earliest eligible decision wins: reject first", approvals: []api.ChangeApproval{
			approval("carol", []string{"aep-acme-approver"}, DecisionReject, at),
			approval("bob", []string{"aep-acme-approver"}, DecisionApprove, later),
		}, want: api.PhaseRejected},
		{name: "org access change needs platform team", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationOrganizationChange
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"access":{"developer":["AD-X"]}}`)
		}, approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhasePending},
		{name: "org access change approved by platform team", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationOrganizationChange
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"quotas":{"cpu":"100"}}`)
		}, approvals: []api.ChangeApproval{
			approval("paul", []string{GroupPlatformTeam}, DecisionApprove, at),
		}, want: api.PhaseApproved},
		{name: "org access change approved by security", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationOrganizationChange
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"profile":"regulated"}`)
		}, approvals: []api.ChangeApproval{
			approval("sara", []string{GroupSecurity}, DecisionApprove, at),
		}, want: api.PhaseApproved},
		{name: "org displayName change stays with the org approver", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationOrganizationChange
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"displayName":"ACME"}`)
		}, approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhaseApproved},
		{name: "emergency needs security on-call", mutate: func(s *api.ChangeRequestSpec) {
			s.Model = api.ModelEmergency
		}, approvals: []api.ChangeApproval{
			approval("bob", []string{"aep-acme-approver"}, DecisionApprove, at),
		}, want: api.PhasePending},
		{name: "emergency approved by security on-call", mutate: func(s *api.ChangeRequestSpec) {
			s.Model = api.ModelEmergency
		}, approvals: []api.ChangeApproval{
			approval("sara", []string{GroupSecurityOnCall}, DecisionApprove, at),
		}, want: api.PhaseApproved},
		{name: "breakglass approved by security on-call", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationBreakglass
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = nil
		}, approvals: []api.ChangeApproval{
			approval("sara", []string{GroupSecurityOnCall}, DecisionApprove, at),
		}, want: api.PhaseApproved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := promoteSpec(api.ModelNormal, "api-1.0.0")
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			phase, reason, _ := EvaluateApprovals(s, tc.approvals)
			if phase != tc.want {
				t.Fatalf("expected %s, got %s (%s)", tc.want, phase, reason)
			}
		})
	}
}

func TestEligibleRole(t *testing.T) {
	app := promoteSpec(api.ModelNormal, "v1")
	if role, ok := EligibleRole(app, []string{"aep-acme-approver"}); !ok || role != RoleApprover {
		t.Fatalf("expected approver role, got %q %v", role, ok)
	}
	if _, ok := EligibleRole(app, []string{GroupSecurityOnCall}); ok {
		t.Fatal("security on-call must not approve normal app changes")
	}
	em := promoteSpec(api.ModelEmergency, "v1")
	if role, ok := EligibleRole(em, []string{GroupSecurityOnCall}); !ok || role != RoleSecurityOnCall {
		t.Fatalf("expected security-oncall role, got %q %v", role, ok)
	}
}

func TestValidateRequester(t *testing.T) {
	bg := promoteSpec(api.ModelNormal, "v1")
	bg.Operation = api.OperationBreakglass
	bg.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
	bg.RequesterGroups = []string{"aep-acme-developer"}
	if e := ValidateRequester(bg); e == nil {
		t.Fatal("breakglass requester outside the breakglass group must be rejected")
	}
	bg.RequesterGroups = []string{"aep-acme-breakglass"}
	if e := ValidateRequester(bg); e != nil {
		t.Fatal(e)
	}
}

func TestNormalizeGroup(t *testing.T) {
	for in, want := range map[string]string{
		"aep-acme-approver":            "aep-acme-approver",
		"/aep-acme-approver":           "aep-acme-approver",
		"/teams/eng/aep-acme-approver": "aep-acme-approver",
	} {
		if got := NormalizeGroup(in); got != want {
			t.Fatalf("NormalizeGroup(%q) = %q, want %q", in, got, want)
		}
	}
}
