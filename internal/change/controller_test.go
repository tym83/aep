package change

import (
	"context"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/ocapi/octest"
	"github.com/tym83/aep/internal/policy"
	coordinationv1 "k8s.io/api/coordination/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeFactory struct{ c *ocapi.Client }

func (f fakeFactory) For(context.Context, string) (*ocapi.Client, error) { return f.c, nil }

type harness struct {
	r    *Reconciler
	cl   client.Client
	oc   *octest.Server
	occ  *ocapi.Client
	now  time.Time
	name types.NamespacedName
}

func newHarness(t *testing.T, crs ...*api.ChangeRequest) *harness {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = coordinationv1.AddToScheme(s)
	objs := []client.Object{}
	for _, cr := range crs {
		objs = append(objs, cr)
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.ChangeRequest{}).WithObjects(objs...).Build()
	server := octest.New(t)
	h := &harness{cl: cl, oc: server, now: testNow}
	h.occ = &ocapi.Client{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "t", nil }}
	h.r = &Reconciler{
		Client:               cl,
		API:                  fakeFactory{h.occ},
		Policy:               policy.Default(),
		InstanceID:           "test-instance",
		OrgNamespaceTemplate: "tenant-{org}",
		Now:                  func() time.Time { return h.now },
	}
	return h
}

func (h *harness) step(t *testing.T) *api.ChangeRequest {
	t.Helper()
	_, e := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.name})
	if e != nil {
		t.Fatal(e)
	}
	return h.get(t)
}

func (h *harness) get(t *testing.T) *api.ChangeRequest {
	t.Helper()
	cr := &api.ChangeRequest{}
	if e := h.cl.Get(context.Background(), h.name, cr); e != nil {
		t.Fatal(e)
	}
	return cr
}

// run steps the reconciler until the phase is terminal or maxSteps is hit.
func (h *harness) run(t *testing.T, maxSteps int) *api.ChangeRequest {
	t.Helper()
	var cr *api.ChangeRequest
	for i := 0; i < maxSteps; i++ {
		cr = h.step(t)
		if cr.Status.Phase.Terminal() {
			return cr
		}
	}
	return cr
}

func (h *harness) decide(t *testing.T, a api.ChangeApproval) {
	t.Helper()
	cr := h.get(t)
	cr.Status.Approvals = append(cr.Status.Approvals, a)
	if e := h.cl.Status().Update(context.Background(), cr); e != nil {
		t.Fatal(e)
	}
}

func phaseIs(t *testing.T, cr *api.ChangeRequest, want api.ChangePhase) {
	t.Helper()
	if cr.Status.Phase != want {
		cond := meta.FindStatusCondition(cr.Status.Conditions, "Applied")
		msg := ""
		if cond != nil {
			msg = cond.Reason + ": " + cond.Message
		}
		t.Fatalf("expected phase %s, got %s (%s)", want, cr.Status.Phase, msg)
	}
}

func TestPromoteFlowEndToEnd(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}

	got := h.step(t)
	phaseIs(t, got, api.PhasePending)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Validated"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("expected Validated condition, got %+v", got.Status.Conditions)
	}

	// No approval yet: stays pending, and the before state is captured.
	got = h.step(t)
	phaseIs(t, got, api.PhasePending)
	if got.Status.TargetKey == "" || len(got.Status.Before) != 1 || got.Status.Before[0].Exists {
		t.Fatalf("expected target key and a missing-binding snapshot, got %+v", got.Status)
	}
	if got.Status.RollbackPlan == nil || got.Status.RollbackPlan.Action != "undeploy" {
		t.Fatalf("expected an undeploy rollback plan, got %+v", got.Status.RollbackPlan)
	}

	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	got = h.run(t, 5)
	phaseIs(t, got, api.PhaseApplied)
	if got.Status.Attempts != 1 {
		t.Fatalf("expected one attempt, got %d", got.Status.Attempts)
	}
	binding := h.oc.Binding("acme", "ReleaseBinding", "api-prod")
	if binding["releaseName"] != "api-1.0.0" {
		t.Fatalf("expected the release to be pinned, got %v", binding)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Applied"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("expected Applied condition, got %+v", got.Status.Conditions)
	}
	// Terminal: further reconciles are no-ops.
	before := len(h.oc.CallsMade())
	h.step(t)
	if len(h.oc.CallsMade()) != before {
		t.Fatal("terminal changes must not touch OpenChoreo")
	}
}

func TestStandardRollbackAutoApproved(t *testing.T) {
	history := changeRequest("chg-old", promoteSpec(api.ModelNormal, "api-0.9.0"), api.PhaseApplied, testNow.Add(-48*time.Hour))
	rollback := promoteSpec(api.ModelStandard, "api-0.9.0")
	rollback.Operation = api.OperationRollback
	cr := changeRequest("chg-rb", rollback, "", testNow)
	h := newHarness(t, &history, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-rb"}
	h.oc.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "api-1.0.0",
	})
	got := h.run(t, 6)
	phaseIs(t, got, api.PhaseApplied)
	if len(got.Status.Approvals) != 1 || got.Status.Approvals[0].Role != RoleStandard || got.Status.Approvals[0].Source != "system" {
		t.Fatalf("expected a recorded system approval, got %+v", got.Status.Approvals)
	}
	if b := h.oc.Binding("acme", "ReleaseBinding", "api-prod"); b["releaseName"] != "api-0.9.0" {
		t.Fatalf("expected the rollback to be applied, got %v", b)
	}
}

func TestStandardIneligibleNeedsHuman(t *testing.T) {
	spec := promoteSpec(api.ModelStandard, "api-9.9.9")
	spec.Operation = api.OperationRollback
	cr := changeRequest("chg-rb", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-rb"}
	got := h.run(t, 3)
	phaseIs(t, got, api.PhasePending)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Approved"); c == nil || c.Reason != "StandardNotEligible" {
		t.Fatalf("expected StandardNotEligible, got %+v", got.Status.Conditions)
	}
}

func TestStaleBindingFails(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.oc.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "api-0.9.0",
	})
	h.step(t) // validate
	h.step(t) // pending: snapshot taken at api-0.9.0
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	h.step(t) // approved
	h.step(t) // applying scheduled
	// A foreign write lands before the apply attempt.
	h.oc.SetSpec("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "api-0.9.5",
	})
	got := h.run(t, 3)
	phaseIs(t, got, api.PhaseFailed)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Applied"); c == nil || c.Reason != "Stale" {
		t.Fatalf("expected Stale, got %+v", got.Status.Conditions)
	}
	if b := h.oc.Binding("acme", "ReleaseBinding", "api-prod"); b["releaseName"] != "api-0.9.5" {
		t.Fatal("the foreign write must not be overwritten")
	}
}

func TestSupersedeOlderRequest(t *testing.T) {
	old := changeRequest("chg-old", promoteSpec(api.ModelNormal, "api-1.0.0"), api.PhasePending, testNow.Add(-time.Hour))
	newer := changeRequest("chg-new", promoteSpec(api.ModelNormal, "api-2.0.0"), api.PhasePending, testNow)
	h := newHarness(t, &old, &newer)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-new"}
	got := h.step(t)
	phaseIs(t, got, api.PhasePending)
	oldAfter := &api.ChangeRequest{}
	if e := h.cl.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "chg-old"}, oldAfter); e != nil {
		t.Fatal(e)
	}
	phaseIs(t, oldAfter, api.PhaseSuperseded)
	// The superseded change is terminal and never applies.
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-old"}
	h.step(t)
	if b := h.oc.Binding("acme", "ReleaseBinding", "api-prod"); b != nil {
		t.Fatalf("a superseded change must not apply, got %v", b)
	}
}

func TestNewerRequestIsSupersededByNothing(t *testing.T) {
	old := changeRequest("chg-old", promoteSpec(api.ModelNormal, "api-1.0.0"), api.PhasePending, testNow.Add(-time.Hour))
	newer := changeRequest("chg-new", promoteSpec(api.ModelNormal, "api-2.0.0"), api.PhasePending, testNow)
	h := newHarness(t, &old, &newer)
	// Reconciling the older one first also supersedes it.
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-old"}
	got := h.step(t)
	phaseIs(t, got, api.PhaseSuperseded)
}

func TestExpiry(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), api.PhasePending, testNow.Add(-73*time.Hour))
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	got := h.step(t)
	phaseIs(t, got, api.PhaseExpired)
}

func TestApprovalExpiresAtWindowEnd(t *testing.T) {
	spec := promoteSpec(api.ModelNormal, "api-1.0.0")
	spec.Window = &api.ChangeWindow{Start: metav1.NewTime(testNow.Add(-2 * time.Hour)), End: metav1.NewTime(testNow.Add(-time.Hour))}
	cr := changeRequest("chg-1", spec, api.PhaseApproved, testNow.Add(-3*time.Hour))
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	got := h.step(t)
	phaseIs(t, got, api.PhaseExpired)
	if b := h.oc.Binding("acme", "ReleaseBinding", "api-prod"); b != nil {
		t.Fatal("an expired approval must not be applied")
	}
}

func TestWindowScheduling(t *testing.T) {
	spec := promoteSpec(api.ModelNormal, "api-1.0.0")
	spec.Window = &api.ChangeWindow{Start: metav1.NewTime(testNow.Add(time.Hour)), End: metav1.NewTime(testNow.Add(2 * time.Hour))}
	cr := changeRequest("chg-1", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.step(t) // validate
	h.step(t) // pending: snapshot
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	h.step(t) // approved
	got := h.step(t)
	phaseIs(t, got, api.PhaseScheduled)
	h.now = testNow.Add(90 * time.Minute)
	got = h.run(t, 3)
	phaseIs(t, got, api.PhaseApplied)
}

func TestNotImplementedOperationStaysApproved(t *testing.T) {
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationOrganizationChange
	spec.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
	spec.Desired = rawJSON(`{"displayName":"ACME Payments"}`)
	cr := changeRequest("chg-1", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.step(t) // validate
	h.step(t) // pending
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	got := h.run(t, 4)
	phaseIs(t, got, api.PhaseApproved)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Applied"); c == nil || c.Reason != "NotImplemented" {
		t.Fatalf("expected NotImplemented condition, got %+v", got.Status.Conditions)
	}
}

func TestBreakglassFlow(t *testing.T) {
	spec := promoteSpec(api.ModelEmergency, "")
	spec.Operation = api.OperationBreakglass
	spec.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
	spec.Desired = rawJSON(`{"duration":"4h"}`)
	spec.RequesterGroups = []string{"aep-acme-breakglass"}
	cr := changeRequest("chg-1", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.step(t)
	h.decide(t, approval("sara", []string{GroupSecurityOnCall}, DecisionApprove, testNow))
	got := h.run(t, 4)
	phaseIs(t, got, api.PhaseApplied)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Applied"); c == nil || c.Reason != "Recorded" {
		t.Fatalf("record-only breakglass must end with reason Recorded, got %+v", got.Status.Conditions)
	}
}

func TestBreakglassRequesterNeedsGroup(t *testing.T) {
	spec := promoteSpec(api.ModelEmergency, "")
	spec.Operation = api.OperationBreakglass
	spec.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
	spec.RequesterGroups = []string{"aep-acme-developer"}
	cr := changeRequest("chg-1", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	got := h.step(t)
	phaseIs(t, got, api.PhaseFailed)
}

func TestRetryBackoffAndSuccess(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.step(t) // validate
	h.step(t) // pending: snapshot
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	h.step(t) // approved
	h.step(t) // -> applying
	h.oc.FailWrites = 2
	got := h.step(t) // attempt 1 fails
	phaseIs(t, got, api.PhaseApplying)
	if got.Status.Attempts != 1 {
		t.Fatalf("expected one failed attempt, got %d", got.Status.Attempts)
	}
	got = h.step(t) // attempt 2 fails
	if got.Status.Attempts != 2 {
		t.Fatalf("expected two failed attempts, got %d", got.Status.Attempts)
	}
	got = h.step(t) // attempt 3 succeeds
	phaseIs(t, got, api.PhaseApplied)
	if got.Status.Attempts != 3 {
		t.Fatalf("expected three attempts, got %d", got.Status.Attempts)
	}
}

func TestMaxAttemptsExhausted(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), "", testNow)
	h := newHarness(t, &cr)
	h.r.MaxAttempts = 2
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.step(t)
	h.step(t)
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	h.step(t)
	h.step(t)
	h.oc.FailWrites = 100
	h.step(t)        // attempt 1
	got := h.step(t) // attempt 2 -> give up
	phaseIs(t, got, api.PhaseFailed)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Applied"); c == nil || c.Reason != "ApplyFailed" {
		t.Fatalf("expected ApplyFailed, got %+v", got.Status.Conditions)
	}
}

func TestPermanentFailureDoesNotRetry(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.step(t)
	h.step(t)
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	h.step(t)
	h.step(t)
	h.oc.Token = "different" // every call is a 401
	got := h.step(t)
	phaseIs(t, got, api.PhaseFailed)
	if got.Status.Attempts != 1 {
		t.Fatalf("permanent errors must not be retried, got %d attempts", got.Status.Attempts)
	}
}

func TestLeaseSerializesApplies(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), api.PhaseApplying, testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	// A valid snapshot is required for the stale check.
	snap, e := HandlerFor(api.OperationPromote, nil).Before(context.Background(), h.occ, &cr, testNow)
	if e != nil {
		t.Fatal(e)
	}
	cr.Status.Before = snap
	if e := h.cl.Status().Update(context.Background(), &cr); e != nil {
		t.Fatal(e)
	}
	other := "other-instance"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: leaseName(&cr)},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &other,
			LeaseDurationSeconds: ptr(int32(60)),
			RenewTime:            &metav1.MicroTime{Time: testNow},
		},
	}
	if e := h.cl.Create(context.Background(), lease); e != nil {
		t.Fatal(e)
	}
	got := h.step(t)
	phaseIs(t, got, api.PhaseApplying)
	if b := h.oc.Binding("acme", "ReleaseBinding", "api-prod"); b != nil {
		t.Fatal("apply must wait while another instance holds the lease")
	}
	// The lease expires; the apply proceeds.
	h.now = testNow.Add(2 * time.Minute)
	got = h.run(t, 3)
	phaseIs(t, got, api.PhaseApplied)
}

func TestInvalidSpecFails(t *testing.T) {
	spec := promoteSpec(api.ModelNormal, "api-1.0.0")
	spec.Desired = rawJSON(`{"unexpected":true}`)
	cr := changeRequest("chg-1", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	got := h.step(t)
	phaseIs(t, got, api.PhaseFailed)
	if c := meta.FindStatusCondition(got.Status.Conditions, "Validated"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("expected invalid validation, got %+v", got.Status.Conditions)
	}
}

func TestWrongNamespaceFails(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), "", testNow)
	cr.Namespace = "tenant-other"
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-other", Name: "chg-1"}
	got := h.step(t)
	phaseIs(t, got, api.PhaseFailed)
}

func TestRejectedByApprover(t *testing.T) {
	cr := changeRequest("chg-1", promoteSpec(api.ModelNormal, "api-1.0.0"), api.PhasePending, testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionReject, testNow))
	got := h.step(t)
	phaseIs(t, got, api.PhaseRejected)
}

func TestDecommissionFlow(t *testing.T) {
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationDecommission
	spec.Desired = nil
	cr := changeRequest("chg-1", spec, "", testNow)
	h := newHarness(t, &cr)
	h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
	h.oc.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "v1",
	})
	h.step(t)
	h.step(t)
	h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
	got := h.run(t, 4)
	phaseIs(t, got, api.PhaseApplied)
	if b := h.oc.Binding("acme", "ReleaseBinding", "api-prod"); b != nil {
		t.Fatal("decommission must remove the protected binding")
	}
	if got.Status.RollbackPlan == nil || got.Status.RollbackPlan.Action != "recreate-binding" {
		t.Fatalf("unexpected rollback plan %+v", got.Status.RollbackPlan)
	}
}

func TestDecommissionRetentionOutcomeAndApprovalGate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		deleteData, hold bool
		reason           string
	}{
		{name: "retained", reason: "Retained"},
		{name: "finalizing", deleteData: true, hold: true, reason: "Finalizing"},
		{name: "removed", deleteData: true, reason: "Applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := promoteSpec(api.ModelNormal, "")
			spec.Operation = api.OperationDecommission
			spec.Target = api.ChangeTarget{Kind: "Resource", Name: "db", Project: "shop", Environment: "prod"}
			spec.Desired = nil
			if tc.deleteData {
				spec.Desired = rawJSON(`{"deleteData":true}`)
			}
			cr := changeRequest("chg-1", spec, "", testNow)
			h := newHarness(t, &cr)
			h.name = types.NamespacedName{Namespace: "tenant-acme", Name: "chg-1"}
			h.oc.HoldFinalizers = tc.hold
			h.oc.Seed("acme", "ResourceReleaseBinding", "db-prod", map[string]any{"owner": map[string]any{"projectName": "shop", "resourceName": "db"}, "environment": "prod", "retainPolicy": "Retain"})
			h.step(t)
			phaseIs(t, h.step(t), api.PhasePending)
			if writes(h.oc) != 0 {
				t.Fatal("decommission mutated binding before human approval")
			}
			h.decide(t, approval("bob", []string{"aep-acme-approver"}, DecisionApprove, testNow))
			got := h.run(t, 4)
			phaseIs(t, got, api.PhaseApplied)
			cond := meta.FindStatusCondition(got.Status.Conditions, "Applied")
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tc.reason {
				t.Fatalf("untruthful condition: %+v", cond)
			}
		})
	}
}
