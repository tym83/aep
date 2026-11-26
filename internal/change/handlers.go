package change

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/policy"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNotImplemented marks operations that are validated and stored but whose
// execution is not built yet; the change stays approved with a condition.
var ErrNotImplemented = errors.New("operation handler is not implemented")

// Result reports what Apply did.
type Result struct {
	// Executed is false when the change was recorded but the platform side is
	// a manual step (breakglass grant).
	Executed bool
	Message  string
}

// Handler executes one operation kind against OpenChoreo.
type Handler interface {
	// Before captures the state the change is approved against; the stale
	// check compares it with the live state before every apply attempt.
	Before(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest, now time.Time) ([]api.BindingSnapshot, error)
	// RollbackPlan derives how to undo the change from the before state.
	RollbackPlan(cr *api.ChangeRequest, before []api.BindingSnapshot) *api.RollbackPlan
	// Apply converges the target to the desired state. It must be idempotent:
	// it is retried with backoff until it succeeds or fails permanently.
	Apply(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest) (Result, error)
}

// Granter executes an approved breakglass access grant.
//
// TODO(design section 4): implement the actual grant — temporary membership
// in the breakglass groups, prod role bindings expiring with the window, an
// immediate SIEM alert, and rotation of the accessed prod credentials after
// the window closes.
type Granter interface {
	Grant(ctx context.Context, cr *api.ChangeRequest) (executed bool, err error)
}

// RecordOnlyGranter records approved breakglass changes without granting
// anything; the grant is performed manually until Granter is implemented.
type RecordOnlyGranter struct{}

func (RecordOnlyGranter) Grant(context.Context, *api.ChangeRequest) (bool, error) {
	return false, nil
}

func Implemented(op api.ChangeOperation) bool {
	switch op {
	case api.OperationPromote, api.OperationRollback, api.OperationResourceUpdate, api.OperationDecommission, api.OperationBreakglass:
		return true
	}
	return false
}

func HandlerFor(op api.ChangeOperation, g Granter) Handler {
	switch op {
	case api.OperationPromote, api.OperationRollback:
		return releaseHandler{}
	case api.OperationResourceUpdate:
		return resourceUpdateHandler{}
	case api.OperationDecommission:
		return decommissionHandler{}
	case api.OperationBreakglass:
		if g == nil {
			g = RecordOnlyGranter{}
		}
		return breakglassHandler{g}
	}
	return notImplementedHandler{}
}

func snapshotOf(b *ocapi.Binding, kind, name string, now time.Time) api.BindingSnapshot {
	snap := api.BindingSnapshot{BindingKind: kind, Name: name, Exists: b != nil, CapturedAt: metav1.NewTime(now)}
	if b != nil {
		raw, _ := json.Marshal(b.Spec)
		snap.Spec = &apiextensionsv1.JSON{Raw: raw}
		snap.Hash = policy.Hash(b.Spec)
	}
	return snap
}

// SnapshotsEqual compares a fresh capture with the stored before state.
func SnapshotsEqual(a, b []api.BindingSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].BindingKind != b[i].BindingKind || a[i].Name != b[i].Name || a[i].Exists != b[i].Exists || a[i].Hash != b[i].Hash {
			return false
		}
	}
	return true
}

func bindingRef(cr *api.ChangeRequest) (kind, name string, e error) {
	kind, e = BindingKind(cr.Spec.Target.Kind)
	if e != nil {
		return "", "", e
	}
	return kind, BindingName(cr.Spec.Target), nil
}

func captureBinding(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest, now time.Time) ([]api.BindingSnapshot, error) {
	kind, name, e := bindingRef(cr)
	if e != nil {
		return nil, e
	}
	b, e := oc.Get(ctx, cr.Spec.Organization, kind, name)
	if e != nil {
		return nil, e
	}
	return []api.BindingSnapshot{snapshotOf(b, kind, name, now)}, nil
}

// releaseHandler promotes or rolls back a release pin. Rollback differs only
// in approval rules, which live outside the handler.
type releaseHandler struct{}

func (releaseHandler) Before(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest, now time.Time) ([]api.BindingSnapshot, error) {
	return captureBinding(ctx, oc, cr, now)
}

func (releaseHandler) RollbackPlan(cr *api.ChangeRequest, before []api.BindingSnapshot) *api.RollbackPlan {
	if len(before) == 0 || !before[0].Exists {
		return &api.RollbackPlan{Action: "undeploy", Description: "the binding did not exist before this change; deleting it undeploys the release"}
	}
	field := ocapi.ReleaseField(before[0].BindingKind)
	var spec map[string]any
	if before[0].Spec != nil {
		_ = json.Unmarshal(before[0].Spec.Raw, &spec)
	}
	if prev, _ := spec[field].(string); prev != "" {
		return &api.RollbackPlan{Action: "restore-release", ReleaseName: prev, Description: "roll back to the previously pinned release " + prev}
	}
	return &api.RollbackPlan{Action: "restore-spec", Description: "re-apply the binding spec captured in the before snapshot"}
}

func (releaseHandler) Apply(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest) (Result, error) {
	kind, name, e := bindingRef(cr)
	if e != nil {
		return Result{}, e
	}
	release, e := DesiredRelease(cr.Spec)
	if e != nil {
		return Result{}, e
	}
	org, target := cr.Spec.Organization, cr.Spec.Target
	b, e := oc.Get(ctx, org, kind, name)
	if e != nil {
		return Result{}, e
	}
	if b == nil {
		nb := ocapi.NewBinding(kind, name, target.Environment, release, Owner(target))
		if kind == ocapi.KindReleaseBinding {
			nb.Spec["state"] = "Active"
		}
		defaultRetain(kind, target.Environment, nb.Spec)
		_, e = oc.Create(ctx, org, kind, nb)
		return Result{Executed: e == nil, Message: "binding created"}, e
	}
	if !b.MatchesOwner(Owner(target)) {
		return Result{}, fmt.Errorf("binding %s/%s belongs to another owner; refusing to overwrite it", kind, name)
	}
	field := ocapi.ReleaseField(kind)
	retainAdded := defaultRetain(kind, target.Environment, b.Spec)
	if b.Spec[field] == release && !retainAdded {
		return Result{Executed: true, Message: "already at the requested release"}, nil
	}
	b.Spec[field] = release
	if _, e = oc.Update(ctx, org, kind, name, b); e != nil {
		return Result{}, e
	}
	return Result{Executed: true, Message: "release pin updated"}, nil
}

// defaultRetain makes protected resources retain their data unless a change
// explicitly chose otherwise. Bindings created outside the change service
// (before protection was in place) get the default on their next promotion.
// It reports whether the spec was changed.
func defaultRetain(kind, environment string, spec map[string]any) bool {
	if kind != ocapi.KindResourceReleaseBinding || environment != "prod" {
		return false
	}
	if v, _ := spec["retainPolicy"].(string); v != "" {
		return false
	}
	spec["retainPolicy"] = "Retain"
	return true
}

// resourceUpdateHandler changes the environment configs, release pin or
// retention of an existing ResourceReleaseBinding.
type resourceUpdateHandler struct{}

func (resourceUpdateHandler) Before(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest, now time.Time) ([]api.BindingSnapshot, error) {
	return captureBinding(ctx, oc, cr, now)
}

func (resourceUpdateHandler) RollbackPlan(_ *api.ChangeRequest, before []api.BindingSnapshot) *api.RollbackPlan {
	return &api.RollbackPlan{Action: "restore-spec", Description: "re-apply the environment configs captured in the before snapshot"}
}

func (resourceUpdateHandler) Apply(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest) (Result, error) {
	kind, name, e := bindingRef(cr)
	if e != nil {
		return Result{}, e
	}
	d, e := DesiredMap(cr.Spec)
	if e != nil {
		return Result{}, e
	}
	org := cr.Spec.Organization
	b, e := oc.Get(ctx, org, kind, name)
	if e != nil {
		return Result{}, e
	}
	if b == nil {
		return Result{}, fmt.Errorf("binding %s/%s does not exist; promote the resource first", kind, name)
	}
	if !b.MatchesOwner(Owner(cr.Spec.Target)) {
		return Result{}, fmt.Errorf("binding %s/%s belongs to another owner; refusing to overwrite it", kind, name)
	}
	changed := false
	for _, k := range []string{"resourceRelease", "retainPolicy", "resourceTypeEnvironmentConfigs"} {
		v, ok := d[k]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(b.Spec[k], v) {
			b.Spec[k] = v
			changed = true
		}
	}
	if !changed {
		return Result{Executed: true, Message: "binding already matches the requested configuration"}, nil
	}
	if _, e = oc.Update(ctx, org, kind, name, b); e != nil {
		return Result{}, e
	}
	return Result{Executed: true, Message: "environment configuration updated"}, nil
}

// decommissionHandler removes the protected binding of a component or
// resource so the object itself can then be deleted: admission on aep-cp
// blocks deletions while protected bindings exist.
type decommissionHandler struct{}

func (decommissionHandler) Before(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest, now time.Time) ([]api.BindingSnapshot, error) {
	return captureBinding(ctx, oc, cr, now)
}

func (decommissionHandler) RollbackPlan(_ *api.ChangeRequest, before []api.BindingSnapshot) *api.RollbackPlan {
	return &api.RollbackPlan{Action: "recreate-binding", Description: "re-create the binding from the before snapshot through a new approved change"}
}

func (decommissionHandler) Apply(ctx context.Context, oc *ocapi.Client, cr *api.ChangeRequest) (Result, error) {
	kind, name, e := bindingRef(cr)
	if e != nil {
		return Result{}, e
	}
	org := cr.Spec.Organization
	b, e := oc.Get(ctx, org, kind, name)
	if e != nil {
		return Result{}, e
	}
	if b == nil {
		return Result{Executed: true, Message: "binding already removed"}, nil
	}
	if !b.MatchesOwner(Owner(cr.Spec.Target)) {
		return Result{}, fmt.Errorf("binding %s/%s belongs to another owner; refusing to delete it", kind, name)
	}
	if e = oc.Delete(ctx, org, kind, name); e != nil {
		return Result{}, e
	}
	return Result{Executed: true, Message: "protected binding removed; the object can now be deleted"}, nil
}

// breakglassHandler records approved breakglass access; the actual grant is
// delegated to a Granter, which is record-only until implemented.
type breakglassHandler struct{ granter Granter }

func (breakglassHandler) Before(context.Context, *ocapi.Client, *api.ChangeRequest, time.Time) ([]api.BindingSnapshot, error) {
	return nil, nil
}

func (breakglassHandler) RollbackPlan(_ *api.ChangeRequest, _ []api.BindingSnapshot) *api.RollbackPlan {
	return &api.RollbackPlan{Action: "revoke-grant", Description: "revoke the temporary access; after the window the platform rotates the credentials of the accessed prod services"}
}

func (h breakglassHandler) Apply(ctx context.Context, _ *ocapi.Client, cr *api.ChangeRequest) (Result, error) {
	executed, e := h.granter.Grant(ctx, cr)
	if e != nil {
		return Result{}, e
	}
	if !executed {
		return Result{Executed: false, Message: "breakglass access recorded; the grant itself is a manual step until the granter is implemented"}, nil
	}
	return Result{Executed: true, Message: "breakglass access granted"}, nil
}

// notImplementedHandler stores validated operations without executing them.
type notImplementedHandler struct{}

func (notImplementedHandler) Before(context.Context, *ocapi.Client, *api.ChangeRequest, time.Time) ([]api.BindingSnapshot, error) {
	return nil, nil
}

func (notImplementedHandler) RollbackPlan(*api.ChangeRequest, []api.BindingSnapshot) *api.RollbackPlan {
	return &api.RollbackPlan{Action: "none"}
}

func (notImplementedHandler) Apply(context.Context, *ocapi.Client, *api.ChangeRequest) (Result, error) {
	return Result{}, ErrNotImplemented
}
