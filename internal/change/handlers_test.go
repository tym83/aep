package change

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/ocapi/octest"
)

func ocClient(s *octest.Server) *ocapi.Client {
	return &ocapi.Client{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "t", nil }}
}

func crFor(spec api.ChangeRequestSpec) *api.ChangeRequest {
	cr := changeRequest("chg-test", spec, api.PhaseApplying, testNow)
	return &cr
}

func writes(s *octest.Server) int {
	n := 0
	for _, c := range s.CallsMade() {
		if c.Method != "GET" {
			n++
		}
	}
	return n
}

func TestPromoteApply(t *testing.T) {
	s := octest.New(t)
	oc := ocClient(s)
	h := HandlerFor(api.OperationPromote, nil)
	spec := promoteSpec(api.ModelNormal, "api-1.0.0")
	// Resource targets promote through ResourceReleaseBinding; keep a component here.
	cr := crFor(spec)
	res, e := h.Apply(context.Background(), oc, cr)
	if e != nil || !res.Executed {
		t.Fatalf("apply failed: %v %v", res, e)
	}
	got := s.Binding("acme", "ReleaseBinding", "api-prod")
	if got["releaseName"] != "api-1.0.0" || got["state"] != "Active" || got["environment"] != "prod" {
		t.Fatalf("unexpected created spec %v", got)
	}
	owner, _ := got["owner"].(map[string]any)
	if owner["projectName"] != "shop" || owner["componentName"] != "api" {
		t.Fatalf("unexpected owner %v", owner)
	}
	// Idempotent: applying again performs no writes.
	before := writes(s)
	if _, e = h.Apply(context.Background(), oc, cr); e != nil {
		t.Fatal(e)
	}
	if writes(s) != before {
		t.Fatal("a repeated apply must not write")
	}
}

func TestPromoteResourceProdRetains(t *testing.T) {
	s := octest.New(t)
	oc := ocClient(s)
	spec := promoteSpec(api.ModelNormal, "db-17")
	spec.Target = api.ChangeTarget{Kind: "Resource", Project: "shop", Name: "orders-db", Environment: "prod"}
	_, e := HandlerFor(api.OperationPromote, nil).Apply(context.Background(), oc, crFor(spec))
	if e != nil {
		t.Fatal(e)
	}
	got := s.Binding("acme", "ResourceReleaseBinding", "orders-db-prod")
	if got["resourceRelease"] != "db-17" || got["retainPolicy"] != "Retain" {
		t.Fatalf("prod resource bindings must retain data, got %v", got)
	}
}

func TestPromotePreservesUnknownSpecFields(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner":             map[string]any{"projectName": "shop", "componentName": "api"},
		"environment":       "prod",
		"releaseName":       "api-0.9.0",
		"workloadOverrides": map[string]any{"replicas": 4},
	})
	oc := ocClient(s)
	cr := crFor(promoteSpec(api.ModelNormal, "api-1.0.0"))
	if _, e := HandlerFor(api.OperationPromote, nil).Apply(context.Background(), oc, cr); e != nil {
		t.Fatal(e)
	}
	got := s.Binding("acme", "ReleaseBinding", "api-prod")
	if got["releaseName"] != "api-1.0.0" {
		t.Fatalf("release pin not advanced: %v", got)
	}
	if ov, _ := got["workloadOverrides"].(map[string]any); ov["replicas"] != float64(4) {
		t.Fatalf("read-modify-write must preserve unknown fields, got %v", got)
	}
}

func TestPromoteRefusesForeignOwner(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner":       map[string]any{"projectName": "other", "componentName": "api"},
		"environment": "prod",
		"releaseName": "api-0.9.0",
	})
	_, e := HandlerFor(api.OperationPromote, nil).Apply(context.Background(), ocClient(s), crFor(promoteSpec(api.ModelNormal, "api-1.0.0")))
	if e == nil {
		t.Fatal("a binding of another owner must never be overwritten")
	}
	if got := s.Binding("acme", "ReleaseBinding", "api-prod"); got["releaseName"] != "api-0.9.0" {
		t.Fatal("foreign binding was modified")
	}
}

func TestResourceUpdateApply(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ResourceReleaseBinding", "orders-db-prod", map[string]any{
		"owner":                          map[string]any{"projectName": "shop", "resourceName": "orders-db"},
		"environment":                    "prod",
		"resourceRelease":                "db-17",
		"resourceTypeEnvironmentConfigs": map[string]any{"replicas": 1, "size": "10Gi"},
	})
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationResourceUpdate
	spec.Target = api.ChangeTarget{Kind: "Resource", Project: "shop", Name: "orders-db", Environment: "prod"}
	spec.Desired = rawJSON(`{"resourceTypeEnvironmentConfigs":{"replicas":2,"size":"20Gi"}}`)
	h := HandlerFor(api.OperationResourceUpdate, nil)
	res, e := h.Apply(context.Background(), ocClient(s), crFor(spec))
	if e != nil || !res.Executed {
		t.Fatalf("apply failed: %v %v", res, e)
	}
	got := s.Binding("acme", "ResourceReleaseBinding", "orders-db-prod")
	cfg, _ := got["resourceTypeEnvironmentConfigs"].(map[string]any)
	if cfg["replicas"] != float64(2) && cfg["replicas"] != 2 {
		t.Fatalf("configs not updated: %v", got)
	}
	if got["resourceRelease"] != "db-17" {
		t.Fatal("untouched fields must survive the update")
	}
	// Idempotent second run performs no writes.
	before := writes(s)
	if _, e = h.Apply(context.Background(), ocClient(s), crFor(spec)); e != nil {
		t.Fatal(e)
	}
	if writes(s) != before {
		t.Fatal("a repeated apply must not write")
	}
}

func TestResourceUpdateRequiresExistingBinding(t *testing.T) {
	s := octest.New(t)
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationResourceUpdate
	spec.Target = api.ChangeTarget{Kind: "Resource", Project: "shop", Name: "orders-db", Environment: "prod"}
	spec.Desired = rawJSON(`{"retainPolicy":"Delete"}`)
	if _, e := HandlerFor(api.OperationResourceUpdate, nil).Apply(context.Background(), ocClient(s), crFor(spec)); e == nil {
		t.Fatal("resource-update of a missing binding must fail")
	}
}

func TestDecommissionApply(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "v1",
	})
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationDecommission
	spec.Desired = nil
	h := HandlerFor(api.OperationDecommission, nil)
	res, e := h.Apply(context.Background(), ocClient(s), crFor(spec))
	if e != nil || !res.Executed {
		t.Fatalf("decommission failed: %v %v", res, e)
	}
	if s.Binding("acme", "ReleaseBinding", "api-prod") != nil {
		t.Fatal("binding must be removed")
	}
	// Idempotent when already gone.
	if res, e = h.Apply(context.Background(), ocClient(s), crFor(spec)); e != nil || !res.Executed {
		t.Fatalf("decommission must be idempotent: %v %v", res, e)
	}
}

func TestDecommissionRefusesForeignOwner(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "other", "componentName": "api"}, "environment": "prod",
	})
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationDecommission
	spec.Desired = nil
	if _, e := HandlerFor(api.OperationDecommission, nil).Apply(context.Background(), ocClient(s), crFor(spec)); e == nil {
		t.Fatal("decommission of a foreign binding must fail")
	}
	if s.Binding("acme", "ReleaseBinding", "api-prod") == nil {
		t.Fatal("foreign binding was deleted")
	}
}

type fakeGranter struct{ executed bool }

func (f fakeGranter) Grant(context.Context, *api.ChangeRequest) (bool, error) { return f.executed, nil }

func TestBreakglassRecordOnly(t *testing.T) {
	spec := promoteSpec(api.ModelEmergency, "")
	spec.Operation = api.OperationBreakglass
	spec.Target = api.ChangeTarget{Kind: "Organization", Name: "acme"}
	spec.Desired = nil
	cr := crFor(spec)
	res, e := HandlerFor(api.OperationBreakglass, RecordOnlyGranter{}).Apply(context.Background(), nil, cr)
	if e != nil || res.Executed {
		t.Fatalf("record-only granter must not execute: %v %v", res, e)
	}
	res, e = HandlerFor(api.OperationBreakglass, fakeGranter{true}).Apply(context.Background(), nil, cr)
	if e != nil || !res.Executed {
		t.Fatalf("fake granter must execute: %v %v", res, e)
	}
	if plan := HandlerFor(api.OperationBreakglass, nil).RollbackPlan(cr, nil); plan.Action != "revoke-grant" {
		t.Fatalf("unexpected rollback plan %v", plan)
	}
}

func TestNotImplementedOperations(t *testing.T) {
	for _, op := range []api.ChangeOperation{
		api.OperationResourceRestore, api.OperationCredentialRotate, api.OperationPolicyException,
		api.OperationOrganizationChange, api.OperationMachineApprove, api.OperationMachineDecommission,
	} {
		if Implemented(op) {
			t.Fatalf("%s must not report implemented", op)
		}
		h := HandlerFor(op, nil)
		if _, e := h.Apply(context.Background(), nil, nil); !errors.Is(e, ErrNotImplemented) {
			t.Fatalf("%s must return ErrNotImplemented, got %v", op, e)
		}
	}
	for _, op := range []api.ChangeOperation{api.OperationPromote, api.OperationRollback, api.OperationResourceUpdate, api.OperationDecommission, api.OperationBreakglass} {
		if !Implemented(op) {
			t.Fatalf("%s must report implemented", op)
		}
	}
}

func TestSnapshotsAndStaleDetection(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "v1",
	})
	oc := ocClient(s)
	h := HandlerFor(api.OperationPromote, nil)
	cr := crFor(promoteSpec(api.ModelNormal, "v2"))
	before, e := h.Before(context.Background(), oc, cr, testNow)
	if e != nil || len(before) != 1 || !before[0].Exists {
		t.Fatalf("capture failed: %v %v", before, e)
	}
	// Unchanged world: not stale.
	live, _ := h.Before(context.Background(), oc, cr, testNow.Add(1))
	if !SnapshotsEqual(before, live) {
		t.Fatal("identical captures must compare equal")
	}
	// Foreign write: stale.
	s.SetSpec("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "v1.5",
	})
	live, _ = h.Before(context.Background(), oc, cr, testNow.Add(2))
	if SnapshotsEqual(before, live) {
		t.Fatal("a modified binding must be detected as stale")
	}
	// Deletion: stale.
	s2 := octest.New(t)
	cr2 := crFor(promoteSpec(api.ModelNormal, "v2"))
	beforeMissing, _ := h.Before(context.Background(), ocClient(s2), cr2, testNow)
	if beforeMissing[0].Exists {
		t.Fatal("missing binding must snapshot as not existing")
	}
	s2.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod"})
	liveMissing, _ := h.Before(context.Background(), ocClient(s2), cr2, testNow)
	if SnapshotsEqual(beforeMissing, liveMissing) {
		t.Fatal("a created binding must be detected as stale")
	}
}

func TestRollbackPlanFromBefore(t *testing.T) {
	h := HandlerFor(api.OperationPromote, nil)
	cr := crFor(promoteSpec(api.ModelNormal, "v2"))
	s := octest.New(t)
	s.Seed("acme", "ReleaseBinding", "api-prod", map[string]any{
		"owner": map[string]any{"projectName": "shop", "componentName": "api"}, "environment": "prod", "releaseName": "v1",
	})
	before, _ := h.Before(context.Background(), ocClient(s), cr, testNow)
	plan := h.RollbackPlan(cr, before)
	if plan.Action != "restore-release" || plan.ReleaseName != "v1" {
		t.Fatalf("unexpected plan %+v", plan)
	}
	s2 := octest.New(t)
	beforeMissing, _ := h.Before(context.Background(), ocClient(s2), cr, testNow)
	if plan = h.RollbackPlan(cr, beforeMissing); plan.Action != "undeploy" {
		t.Fatalf("unexpected plan %+v", plan)
	}
}

func TestPromoteAddsRetainToExistingProdResourceBinding(t *testing.T) {
	s := octest.New(t)
	owner := map[string]any{"projectName": "shop", "resourceName": "orders-db"}
	s.Seed("acme", "ResourceReleaseBinding", "orders-db-prod", map[string]any{"owner": owner, "environment": "prod", "resourceRelease": "db-17"})
	s.Seed("acme", "ResourceReleaseBinding", "cache-prod", map[string]any{"owner": map[string]any{"projectName": "shop", "resourceName": "cache"}, "environment": "prod", "resourceRelease": "c-1", "retainPolicy": "Delete"})
	h := HandlerFor(api.OperationPromote, nil)
	for _, tc := range []struct{ name, release, want string }{{"orders-db", "db-17", "Retain"}, {"cache", "c-1", "Delete"}} {
		spec := promoteSpec(api.ModelNormal, "")
		spec.Target = api.ChangeTarget{Kind: "Resource", Project: "shop", Name: tc.name, Environment: "prod"}
		spec.Desired = rawJSON(`{"releaseName":"` + tc.release + `"}`)
		if res, e := h.Apply(context.Background(), ocClient(s), crFor(spec)); e != nil || !res.Executed {
			t.Fatalf("%s: apply failed: %v %v", tc.name, res, e)
		}
		if got := s.Binding("acme", "ResourceReleaseBinding", tc.name+"-prod"); got["retainPolicy"] != tc.want {
			t.Fatalf("%s: retainPolicy %v, want %s", tc.name, got["retainPolicy"], tc.want)
		}
	}
}

func TestDecommissionRetainedDataRequiresExplicitRelease(t *testing.T) {
	s := octest.New(t)
	s.Seed("acme", "ResourceReleaseBinding", "db-prod", map[string]any{"owner": map[string]any{"projectName": "shop", "resourceName": "db"}, "environment": "prod", "retainPolicy": "Retain"})
	spec := promoteSpec(api.ModelNormal, "")
	spec.Operation = api.OperationDecommission
	spec.Target = api.ChangeTarget{Kind: "Resource", Project: "shop", Name: "db", Environment: "prod"}
	spec.Desired = nil
	h := HandlerFor(spec.Operation, nil)
	for i := 0; i < 2; i++ {
		res, err := h.Apply(context.Background(), ocClient(s), crFor(spec))
		if err != nil || res.Reason != "Retained" || !res.Executed {
			t.Fatalf("retention: %+v %v", res, err)
		}
		if s.Binding("acme", "ResourceReleaseBinding", "db-prod") == nil {
			t.Fatal("retained binding removed")
		}
	}
	spec.Desired = rawJSON(`{"deleteData":true}`)
	s.HoldFinalizers = true
	res, err := h.Apply(context.Background(), ocClient(s), crFor(spec))
	if err != nil || res.Reason != "Finalizing" {
		t.Fatalf("finalizing: %+v %v", res, err)
	}
	if s.Binding("acme", "ResourceReleaseBinding", "db-prod")["retainPolicy"] != "Delete" {
		t.Fatal("data release not requested")
	}
	s.HoldFinalizers = false
	res, err = h.Apply(context.Background(), ocClient(s), crFor(spec))
	if err != nil || !res.Executed || res.Reason != "" || s.Binding("acme", "ResourceReleaseBinding", "db-prod") != nil {
		t.Fatalf("release: %+v %v", res, err)
	}
}

func TestValidateDecommissionPayload(t *testing.T) {
	for _, tc := range []struct {
		payload string
		model   api.ChangeModel
		valid   bool
	}{
		{`{}`, api.ModelNormal, true}, {`{"deleteData":false}`, api.ModelNormal, true},
		{`{"deleteData":true}`, api.ModelNormal, true}, {`{"deleteData":"true"}`, api.ModelNormal, false},
		{`{"retainPolicy":"Delete"}`, api.ModelNormal, false}, {`{"deleteData":true}`, api.ModelStandard, false},
		{`{"deleteData":true}`, api.ModelEmergency, false},
	} {
		spec := promoteSpec(tc.model, "")
		spec.Operation = api.OperationDecommission
		spec.Target = api.ChangeTarget{Kind: "Resource", Project: "shop", Name: "db", Environment: "prod"}
		spec.Desired = rawJSON(tc.payload)
		if err := ValidateSpec(spec, time.Now()); (err == nil) != tc.valid {
			t.Errorf("%s/%s: %v", tc.payload, tc.model, err)
		}
	}
}
