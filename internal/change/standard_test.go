package change

import (
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/policy"
)

func TestStandardEligible(t *testing.T) {
	cfg := policy.Default() // u1.large / 100Gi / 3 replicas ceiling
	rollback := promoteSpec(api.ModelStandard, "api-0.9.0")
	rollback.Operation = api.OperationRollback

	appliedHistory := []api.ChangeRequest{changeRequest("chg-old", promoteSpec(api.ModelNormal, "api-0.9.0"), api.PhaseApplied, testNow.Add(-24*time.Hour))}

	cases := []struct {
		name    string
		mutate  func(*api.ChangeRequestSpec)
		history []api.ChangeRequest
		want    bool
		why     string
	}{
		{name: "credential rotation is standard", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationCredentialRotate
			s.Target.Kind = "Resource"
		}, want: true},
		{name: "rollback to a previously applied release", history: appliedHistory, want: true},
		{name: "rollback to an unknown release is not standard", want: false, why: "never applied"},
		{name: "rollback history of another target does not count", history: func() []api.ChangeRequest {
			other := changeRequest("chg-other", func() api.ChangeRequestSpec {
				s := promoteSpec(api.ModelNormal, "api-0.9.0")
				s.Target.Environment = "stage"
				return s
			}(), api.PhaseApplied, testNow)
			return []api.ChangeRequest{other}
		}(), want: false, why: "never applied"},
		{name: "failed history does not count", history: func() []api.ChangeRequest {
			h := appliedHistory[0].DeepCopy()
			h.Status.Phase = api.PhaseFailed
			return []api.ChangeRequest{*h}
		}(), want: false, why: "never applied"},
		{name: "scale within policy is standard", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{"resourceTypeEnvironmentConfigs":{"resourcesPreset":"t1.small","size":"10Gi","replicas":2}}`)
		}, want: true},
		{name: "scale beyond policy is not standard", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{"resourceTypeEnvironmentConfigs":{"replicas":30}}`)
		}, want: false, why: "replicas"},
		{name: "preset beyond policy is not standard", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{"resourceTypeEnvironmentConfigs":{"resourcesPreset":"u1.4xlarge"}}`)
		}, want: false, why: "resourcesPreset"},
		{name: "release pin change is not a scale", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{"resourceRelease":"db-2","resourceTypeEnvironmentConfigs":{"replicas":2}}`)
		}, want: false, why: "scaling"},
		{name: "promote is never standard", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationPromote
		}, want: false, why: "never a standard change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := rollback
			if tc.mutate != nil {
				s = promoteSpec(api.ModelStandard, "v1")
				tc.mutate(&s)
			}
			ok, why := StandardEligible(s, tc.history, cfg)
			if ok != tc.want {
				t.Fatalf("expected %v, got %v (%s)", tc.want, ok, why)
			}
			if tc.why != "" && !strings.Contains(why, tc.why) {
				t.Fatalf("expected reason containing %q, got %q", tc.why, why)
			}
		})
	}
}
