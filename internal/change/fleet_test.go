package change

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func fleetChange(tier, trust string) *api.ChangeRequest {
	return &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: "attach", Namespace: "tenant-acme", UID: "change-uid"}, Spec: api.ChangeRequestSpec{Organization: "acme", Operation: api.OperationClusterAttach, Model: api.ModelNormal, Requester: "requester", Justification: "add capacity", Target: api.ChangeTarget{Kind: "Cluster", Name: "aws-east", Environment: "prod"}, Desired: &ext.JSON{Raw: []byte(`{"tier":"` + tier + `","trustLevel":"` + trust + `","provider":"aws","distro":"eks","gatewayHost":"edge.example.org"}`)}}}
}
func fleetApproval(user string, groups ...string) api.ChangeApproval {
	return api.ChangeApproval{User: user, Groups: groups, Decision: DecisionApprove, Source: "portal"}
}
func TestFleetSeparationOfDuties(t *testing.T) {
	cr := fleetChange("prod", "standard")
	cases := []struct {
		name      string
		approvals []api.ChangeApproval
		want      api.ChangePhase
	}{
		{"requester", []api.ChangeApproval{fleetApproval("requester", GroupPlatformTeam, GroupSecurity)}, api.PhasePending},
		{"org approver", []api.ChangeApproval{fleetApproval("a", GroupApprover("acme"))}, api.PhasePending},
		{"one dual-role human", []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam, GroupSecurity), fleetApproval("a", GroupSecurity)}, api.PhasePending},
		{"platform only", []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam)}, api.PhasePending},
		{"two humans", []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam), fleetApproval("b", GroupSecurity)}, api.PhaseApproved},
		{"service account", []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam), fleetApproval("service-account-x", GroupSecurity)}, api.PhasePending},
		{"system bypass", []api.ChangeApproval{SystemApproval("forged", time.Now())}, api.PhasePending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := EvaluateApprovals(cr.Spec, tc.approvals)
			if p != tc.want {
				t.Fatalf("got %s", p)
			}
		})
	}
	cr = fleetChange("nonprod", "standard")
	cr.Spec.Target.Environment = "dev"
	if p, _, _ := EvaluateApprovals(cr.Spec, []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam)}); p != api.PhaseApproved {
		t.Fatal(p)
	}
	cr = fleetChange("nonprod", "regulated")
	cr.Spec.Target.Environment = "dev"
	if p, _, _ := EvaluateApprovals(cr.Spec, []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam)}); p != api.PhasePending {
		t.Fatal(p)
	}
}
func TestAttachIdempotenceAndApprovalBinding(t *testing.T) {
	ctx := context.Background()
	cr := fleetChange("prod", "standard")
	cr.Status.Phase = api.PhaseApplied
	cr.Status.Approvals = []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam), fleetApproval("b", GroupSecurity)}
	o := &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: api.OrganizationSpec{Environments: []api.Environment{{Name: "prod", Tier: "prod", DataPlane: api.EnvironmentDataPlane{External: "aws-east"}}}}}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, o).Build()
	h := clusterAttachHandler{c}
	for range 2 {
		if r, e := h.Apply(ctx, nil, cr); e != nil || !r.Executed {
			t.Fatalf("%+v %v", r, e)
		}
	}
	ec := &api.ExternalCluster{}
	if e := c.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: cr.Spec.Target.Name}, ec); e != nil {
		t.Fatal(e)
	}
	if e := VerifyExternalApproval(ctx, c, ec); e != nil {
		t.Fatal(e)
	}
	ec.Spec.TrustLevel = "regulated"
	if e := VerifyExternalApproval(ctx, c, ec); e == nil {
		t.Fatal("tampered assignment accepted")
	}
	cr.Name = "another"
	if _, e := h.Apply(ctx, nil, cr); e == nil {
		t.Fatal("existing assignment adopted")
	}
	cr.Status.Approvals = nil
	if _, e := h.Apply(ctx, nil, cr); e == nil {
		t.Fatal("unapproved attachment")
	}
}
func TestFleetValidation(t *testing.T) {
	for _, model := range []api.ChangeModel{api.ModelStandard, api.ModelEmergency} {
		cr := fleetChange("prod", "standard")
		cr.Spec.Model = model
		if ValidateSpec(cr.Spec, time.Now()) == nil {
			t.Fatal(model)
		}
	}
	cr := fleetChange("prod", "untrusted")
	if ValidateSpec(cr.Spec, time.Now()) == nil {
		t.Fatal("untrusted prod")
	}
	cr = fleetChange("prod", "standard")
	if e := ValidateSpec(cr.Spec, time.Now()); e != nil {
		t.Fatal(e)
	}
}

func TestClusterAttachControllerWaitsForSecurity(t *testing.T) {
	cr := fleetChange("prod", "standard")
	cr.CreationTimestamp = metav1.NewTime(testNow)
	h := newHarness(t, cr)
	h.name = client.ObjectKeyFromObject(cr)
	org := &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: api.OrganizationSpec{Environments: []api.Environment{{Name: "prod", Tier: "prod", DataPlane: api.EnvironmentDataPlane{External: "aws-east"}}}}}
	if e := h.cl.Create(context.Background(), org); e != nil {
		t.Fatal(e)
	}
	phaseIs(t, h.step(t), api.PhasePending)
	h.decide(t, fleetApproval("platform", GroupPlatformTeam))
	phaseIs(t, h.step(t), api.PhasePending)
	var list api.ExternalClusterList
	if e := h.cl.List(context.Background(), &list); e != nil {
		t.Fatal(e)
	}
	if len(list.Items) != 0 {
		t.Fatal("created before security approval")
	}
	h.decide(t, fleetApproval("security", GroupSecurity))
	phaseIs(t, h.run(t, 8), api.PhaseApplied)
	if e := h.cl.List(context.Background(), &list); e != nil || len(list.Items) != 1 {
		t.Fatalf("assignments %d: %v", len(list.Items), e)
	}
}

func TestFleetEnvironmentDeclarationAndNamespace(t *testing.T) {
	ctx := context.Background()
	cr := fleetChange("prod", "standard")
	cr.Status.Phase = api.PhaseApplied
	cr.Status.Approvals = []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam), fleetApproval("b", GroupSecurity)}
	spec, _ := ExternalSpec(cr)
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: cr.Spec.Target.Name, Namespace: cr.Namespace}, Spec: spec}
	o := &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: api.OrganizationSpec{Environments: []api.Environment{{Name: "prod", Tier: "prod", DataPlane: api.EnvironmentDataPlane{External: "aws-east"}}}}}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(o, cr).Build()
	if err := VerifyExternalApproval(ctx, c, ec); err != nil {
		t.Fatal(err)
	}
	for _, env := range []api.Environment{{Name: "prod", Tier: "prod"}, {Name: "prod", Tier: "nonprod", DataPlane: api.EnvironmentDataPlane{External: "aws-east"}}, {Name: "prod", Tier: "prod", DataPlane: api.EnvironmentDataPlane{External: "other"}}} {
		o.Spec.Environments = []api.Environment{env}
		if err := c.Update(ctx, o); err != nil {
			t.Fatal(err)
		}
		if err := VerifyExternalApproval(ctx, c, ec); err == nil {
			t.Fatalf("accepted mismatched declaration: %+v", env)
		}
	}
	ec.Namespace = "foreign"
	cr.Namespace = "foreign"
	cr.ResourceVersion = ""
	_ = c.Create(ctx, cr)
	if err := VerifyExternalApproval(ctx, c, ec); err == nil {
		t.Fatal("accepted foreign namespace")
	}
}

func TestDetachRequiresMatchingUIDAndSecurity(t *testing.T) {
	ctx := context.Background()
	cr := fleetChange("prod", "standard")
	cr.Spec.Operation = api.OperationClusterDetach
	cr.Spec.Desired = &ext.JSON{Raw: []byte(`{"tier":"prod","uid":"assignment-1"}`)}
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: cr.Spec.Target.Name, Namespace: cr.Namespace, UID: "assignment-1", Finalizers: []string{"aep.aenix.io/fleet-cleanup"}}, Spec: api.ExternalClusterSpec{Organization: "acme", Environment: "prod", Tier: "prod"}}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec).Build()
	h := clusterDetachHandler{clusterAttachHandler{c}}
	cr.Status.Approvals = []api.ChangeApproval{fleetApproval("a", GroupPlatformTeam)}
	if _, err := h.Apply(ctx, nil, cr); err == nil {
		t.Fatal("detached without security")
	}
	cr.Status.Approvals = append(cr.Status.Approvals, fleetApproval("b", GroupSecurity))
	if err := ValidateSpec(cr.Spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	cr.Spec.Desired = &ext.JSON{Raw: []byte(`{"tier":"prod","uid":"stale"}`)}
	if _, err := h.Apply(ctx, nil, cr); err == nil {
		t.Fatal("detached replacement assignment")
	}
	current := &api.ExternalCluster{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(ec), current)
	if !current.DeletionTimestamp.IsZero() {
		t.Fatal("invalid request deleted assignment")
	}
	cr.Spec.Desired = &ext.JSON{Raw: []byte(`{"tier":"prod","uid":"assignment-1"}`)}
	if _, err := h.Apply(ctx, nil, cr); err == nil {
		t.Fatal("reported complete before finalization")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(ec), current)
	if current.DeletionTimestamp.IsZero() {
		t.Fatal("approved detach did not start cleanup")
	}
	current.Finalizers = nil
	_ = c.Update(ctx, current)
	if r, err := h.Apply(ctx, nil, cr); err != nil || !r.Executed {
		t.Fatalf("%+v %v", r, err)
	}
}
