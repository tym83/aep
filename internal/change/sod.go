package change

import (
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Well-known groups. Organization roles are per-organization, platform roles
// are installation-wide (design section 4).
const (
	GroupPlatformTeam   = "aep-platform-team"
	GroupSecurity       = "aep-security"
	GroupSecurityOnCall = "aep-security-oncall"
	RoleApprover        = "approver"
	RolePlatform        = "platform"
	RoleSecurity        = "security"
	RoleSecurityOnCall  = "security-oncall"
	RoleStandard        = "standard"
	DecisionApprove     = "approve"
	DecisionReject      = "reject"
)

func GroupApprover(org string) string   { return "aep-" + org + "-approver" }
func GroupBreakglass(org string) string { return "aep-" + org + "-breakglass" }

// Groups allowed to create changes through the HTTP API.
func GroupCreators(org string) []string {
	return []string{"aep-" + org + "-developer", "aep-" + org + "-platform", "aep-" + org + "-admin"}
}

// NormalizeGroup maps a token group to its plain name: Keycloak emits full
// paths like /aep-acme-developer, nested or not.
func NormalizeGroup(g string) string {
	g = strings.Trim(g, "/")
	if i := strings.LastIndex(g, "/"); i >= 0 {
		g = g[i+1:]
	}
	return g
}

func HasGroup(groups []string, want string) bool {
	for _, g := range groups {
		if NormalizeGroup(g) == want {
			return true
		}
	}
	return false
}

// IsServiceAccount reports whether the identity is a service account, which
// can never decide a change: decisions require a human's own token.
func IsServiceAccount(user string) bool {
	return user == "" || strings.HasPrefix(user, "service-account-")
}

// DecisionRequirement classifies which role may decide a change.
type DecisionRequirement int

const (
	// RequireOrgApprover: an approver of the same organization, never the
	// requester; the platform team does not approve application changes.
	RequireOrgApprover DecisionRequirement = iota
	// RequirePlatformOrSecurity: access, quota and profile changes of an
	// organization are decided by the platform team or security.
	RequirePlatformOrSecurity
	// RequireSecurityOnCall: emergency and breakglass changes are confirmed
	// by a second person from the security on-call group.
	RequireSecurityOnCall
)

func RequirementFor(spec api.ChangeRequestSpec) DecisionRequirement {
	if spec.Model == api.ModelEmergency || spec.Operation == api.OperationBreakglass {
		return RequireSecurityOnCall
	}
	if spec.Operation == api.OperationOrganizationChange {
		d, e := DesiredMap(spec)
		if e == nil {
			for _, k := range []string{"access", "quotas", "profile"} {
				if _, ok := d[k]; ok {
					return RequirePlatformOrSecurity
				}
			}
		}
	}
	return RequireOrgApprover
}

// EligibleRole returns the role a decider with these groups acts under for
// this change, or false when the groups grant no decision right.
func EligibleRole(spec api.ChangeRequestSpec, groups []string) (string, bool) {
	switch RequirementFor(spec) {
	case RequireSecurityOnCall:
		if HasGroup(groups, GroupSecurityOnCall) {
			return RoleSecurityOnCall, true
		}
	case RequirePlatformOrSecurity:
		if HasGroup(groups, GroupPlatformTeam) {
			return RolePlatform, true
		}
		if HasGroup(groups, GroupSecurity) {
			return RoleSecurity, true
		}
	default:
		if HasGroup(groups, GroupApprover(spec.Organization)) {
			return RoleApprover, true
		}
	}
	return "", false
}

// ValidateRequester enforces who may ask for a change at all.
func ValidateRequester(spec api.ChangeRequestSpec) error {
	if spec.Operation == api.OperationBreakglass && !HasGroup(spec.RequesterGroups, GroupBreakglass(spec.Organization)) {
		return fmt.Errorf("breakglass requests require membership in %s", GroupBreakglass(spec.Organization))
	}
	return nil
}

// EvaluateApprovals folds the recorded decisions into a phase transition.
// Only eligible decisions count: never the requester, never a service
// account, and only groups matching the required role. The earliest eligible
// decision wins so a race between approvers resolves deterministically.
func EvaluateApprovals(spec api.ChangeRequestSpec, approvals []api.ChangeApproval) (api.ChangePhase, string, string) {
	type eligible struct {
		a    api.ChangeApproval
		role string
	}
	var decisions []eligible
	for _, a := range approvals {
		// Standard changes approve themselves; the controller vetted the
		// template before recording the approval.
		if a.Role == RoleStandard && a.Source == "system" {
			if a.Decision == DecisionApprove {
				decisions = append(decisions, eligible{a, RoleStandard})
			}
			continue
		}
		if a.User == spec.Requester || IsServiceAccount(a.User) {
			continue
		}
		role, ok := EligibleRole(spec, a.Groups)
		if !ok {
			continue
		}
		if a.Decision != DecisionApprove && a.Decision != DecisionReject {
			continue
		}
		decisions = append(decisions, eligible{a, role})
	}
	if len(decisions) == 0 {
		return api.PhasePending, "PendingApproval", "waiting for an eligible approver"
	}
	slices.SortStableFunc(decisions, func(x, y eligible) int { return x.a.Time.Compare(y.a.Time.Time) })
	first := decisions[0]
	if first.a.Decision == DecisionReject {
		return api.PhaseRejected, "Rejected", fmt.Sprintf("rejected by %s (%s)", first.a.User, first.role)
	}
	return api.PhaseApproved, "Approved", fmt.Sprintf("approved by %s (%s)", first.a.User, first.role)
}

// SystemApproval is the record of a standard change approving itself.
func SystemApproval(evidence string, now time.Time) api.ChangeApproval {
	return api.ChangeApproval{
		User:     "system:aep-change",
		Role:     RoleStandard,
		Decision: DecisionApprove,
		Time:     metav1.NewTime(now),
		Source:   "system",
		Evidence: evidence,
	}
}
