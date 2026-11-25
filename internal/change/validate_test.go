package change

import (
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateSpec(t *testing.T) {
	window := &api.ChangeWindow{Start: metav1.NewTime(testNow), End: metav1.NewTime(testNow.Add(time.Hour))}
	cases := []struct {
		name   string
		mutate func(*api.ChangeRequestSpec)
		want   string // empty means valid
	}{
		{name: "valid promote", mutate: func(s *api.ChangeRequestSpec) {}},
		{name: "valid with window", mutate: func(s *api.ChangeRequestSpec) { s.Window = window }},
		{name: "bad organization", mutate: func(s *api.ChangeRequestSpec) { s.Organization = "Acme_Corp" }, want: "DNS-1123"},
		{name: "bad operation", mutate: func(s *api.ChangeRequestSpec) { s.Operation = "explode" }, want: "unsupported operation"},
		{name: "bad model", mutate: func(s *api.ChangeRequestSpec) { s.Model = "yolo" }, want: "unsupported model"},
		{name: "missing requester", mutate: func(s *api.ChangeRequestSpec) { s.Requester = "" }, want: "requester"},
		{name: "missing justification", mutate: func(s *api.ChangeRequestSpec) { s.Justification = "" }, want: "justification"},
		{name: "inverted window", mutate: func(s *api.ChangeRequestSpec) {
			s.Window = &api.ChangeWindow{Start: metav1.NewTime(testNow.Add(time.Hour)), End: metav1.NewTime(testNow)}
		}, want: "window start"},
		{name: "wrong target kind", mutate: func(s *api.ChangeRequestSpec) { s.Target.Kind = "Organization" }, want: "does not accept target kind"},
		{name: "missing project", mutate: func(s *api.ChangeRequestSpec) { s.Target.Project = "" }, want: "target.project"},
		{name: "missing environment", mutate: func(s *api.ChangeRequestSpec) { s.Target.Environment = "" }, want: "target.environment"},
		{name: "bad target name", mutate: func(s *api.ChangeRequestSpec) { s.Target.Name = "API_Server" }, want: "DNS-1123"},
		{name: "missing desired", mutate: func(s *api.ChangeRequestSpec) { s.Desired = nil }, want: "desired payload is required"},
		{name: "missing releaseName", mutate: func(s *api.ChangeRequestSpec) { s.Desired = rawJSON(`{"other":"x"}`) }, want: "releaseName"},
		{name: "extra desired key", mutate: func(s *api.ChangeRequestSpec) { s.Desired = rawJSON(`{"releaseName":"v1","state":"Undeploy"}`) }, want: "not allowed"},
		{name: "desired not object", mutate: func(s *api.ChangeRequestSpec) { s.Desired = rawJSON(`[1,2]`) }, want: "JSON object"},
		{name: "binding name too long", mutate: func(s *api.ChangeRequestSpec) {
			s.Target.Name = strings.Repeat("a", 40)
			s.Target.Environment = strings.Repeat("b", 30)
		}, want: "at most 63"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := promoteSpec(api.ModelNormal, "api-1.0.0")
			tc.mutate(&s)
			e := ValidateSpec(s, testNow)
			if tc.want == "" && e != nil {
				t.Fatalf("expected valid, got %v", e)
			}
			if tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
				t.Fatalf("expected error containing %q, got %v", tc.want, e)
			}
		})
	}
}

func TestValidateSpecOperations(t *testing.T) {
	base := promoteSpec(api.ModelNormal, "v1")
	cases := []struct {
		name   string
		mutate func(*api.ChangeRequestSpec)
		want   string
	}{
		{name: "rollback ok", mutate: func(s *api.ChangeRequestSpec) { s.Operation = api.OperationRollback }},
		{name: "project promote ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Target = api.ChangeTarget{Kind: "Project", Name: "shop", Environment: "prod"}
		}},
		{name: "project target with foreign project field", mutate: func(s *api.ChangeRequestSpec) {
			s.Target = api.ChangeTarget{Kind: "Project", Name: "shop", Project: "other", Environment: "prod"}
		}, want: "target.project"},
		{name: "resource-update ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{"resourceTypeEnvironmentConfigs":{"replicas":2}}`)
		}},
		{name: "resource-update empty desired", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{}`)
		}, want: "desired payload is required"},
		{name: "resource-update bad retainPolicy", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Target.Kind = "Resource"
			s.Desired = rawJSON(`{"retainPolicy":"Keep"}`)
		}, want: "retainPolicy"},
		{name: "resource-update on component", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationResourceUpdate
			s.Desired = rawJSON(`{"retainPolicy":"Retain"}`)
		}, want: "does not accept target kind"},
		{name: "decommission ok without desired", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationDecommission
			s.Desired = nil
		}},
		{name: "decommission needs environment", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationDecommission
			s.Target.Environment = ""
		}, want: "environment"},
		{name: "credential-rotate ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationCredentialRotate
			s.Target.Kind = "Resource"
			s.Desired = nil
		}},
		{name: "breakglass ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationBreakglass
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"duration":"2h"}`)
		}},
		{name: "breakglass too long", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationBreakglass
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"duration":"8h"}`)
		}, want: "4h"},
		{name: "breakglass foreign org", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationBreakglass
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "other"}
		}, want: "itself"},
		{name: "organization-change ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationOrganizationChange
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"quotas":{"cpu":"100"}}`)
		}},
		{name: "organization-change unknown key", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationOrganizationChange
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"rename":"x"}`)
		}, want: "not an organization setting"},
		{name: "policy-exception ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationPolicyException
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"policy":"no-latest-tag","owner":"bob","expires":"2027-01-01T00:00:00Z"}`)
		}},
		{name: "policy-exception expired", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationPolicyException
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"policy":"p","owner":"bob","expires":"2020-01-01T00:00:00Z"}`)
		}, want: "future"},
		{name: "policy-exception without owner", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationPolicyException
			s.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
			s.Desired = rawJSON(`{"policy":"p","expires":"2027-01-01T00:00:00Z"}`)
		}, want: "owner"},
		{name: "cluster-attach ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationClusterAttach
			s.Target = api.ChangeTarget{Kind: "Cluster", Name: "eks-1"}
			s.Desired = rawJSON(`{"tier":"nonprod"}`)
		}},
		{name: "machine-approve ok", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationMachineApprove
			s.Target = api.ChangeTarget{Kind: "Machine", Name: "node-7"}
			s.Desired = nil
		}},
		{name: "machine-approve wrong kind", mutate: func(s *api.ChangeRequestSpec) {
			s.Operation = api.OperationMachineApprove
			s.Target = api.ChangeTarget{Kind: "Cluster", Name: "node-7"}
		}, want: "does not accept"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			e := ValidateSpec(s, testNow)
			if tc.want == "" && e != nil {
				t.Fatalf("expected valid, got %v", e)
			}
			if tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
				t.Fatalf("expected error containing %q, got %v", tc.want, e)
			}
		})
	}
}
