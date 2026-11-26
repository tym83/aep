package change

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/policy"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// APIFactory resolves the per-organization OpenChoreo client.
type APIFactory interface {
	For(ctx context.Context, org string) (*ocapi.Client, error)
}

// Reconciler drives the ChangeRequest state machine.
type Reconciler struct {
	Client  client.Client
	API     APIFactory
	Granter Granter
	Policy  policy.Config
	// ApprovalTTL bounds approvals of changes without a window (default 72h).
	ApprovalTTL time.Duration
	MaxAttempts int
	// InstanceID identifies this controller in per-binding leases.
	InstanceID string
	// OrgNamespaceTemplate maps an organization to its management namespace;
	// a ChangeRequest outside its organization's namespace is rejected.
	OrgNamespaceTemplate string
	Now                  func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) ttl() time.Duration {
	if r.ApprovalTTL > 0 {
		return r.ApprovalTTL
	}
	return DefaultApprovalTTL
}

func (r *Reconciler) maxAttempts() int32 {
	if r.MaxAttempts > 0 {
		return int32(r.MaxAttempts)
	}
	return 8
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &api.ChangeRequest{}
	if e := r.Client.Get(ctx, req.NamespacedName, cr); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	if cr.Status.Phase.Terminal() {
		return ctrl.Result{}, nil
	}
	now := r.now()
	switch cr.Status.Phase {
	case "", api.PhaseDraft:
		return r.validate(ctx, cr, now)
	case api.PhasePending:
		return r.pending(ctx, cr, now)
	case api.PhaseApproved, api.PhaseScheduled:
		return r.approved(ctx, cr, now)
	case api.PhaseApplying:
		return r.applying(ctx, cr, now)
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) orgNamespace(org string) string {
	return OrgNamespace(r.OrgNamespaceTemplate, org)
}

func (r *Reconciler) validate(ctx context.Context, cr *api.ChangeRequest, now time.Time) (ctrl.Result, error) {
	e := ValidateSpec(cr.Spec, now)
	if e == nil {
		e = ValidateRequester(cr.Spec)
	}
	if e == nil && cr.Namespace != r.orgNamespace(cr.Spec.Organization) {
		e = fmt.Errorf("change for organization %q must live in namespace %q", cr.Spec.Organization, r.orgNamespace(cr.Spec.Organization))
	}
	if e != nil {
		r.setCondition(cr, "Validated", metav1.ConditionFalse, "Invalid", e.Error())
		return r.transition(ctx, cr, api.PhaseFailed, "Invalid", e.Error())
	}
	r.setCondition(cr, "Validated", metav1.ConditionTrue, "Valid", "spec is valid")
	cr.Status.TargetKey = TargetKey(cr.Spec)
	return r.transition(ctx, cr, api.PhasePending, "Validated", "waiting for approval")
}

func (r *Reconciler) pending(ctx context.Context, cr *api.ChangeRequest, now time.Time) (ctrl.Result, error) {
	if done, res, e := r.resolveSupersede(ctx, cr); done || e != nil {
		return res, e
	}
	if Expired(cr, now, r.ttl()) {
		return r.expire(ctx, cr)
	}
	if e := r.captureBefore(ctx, cr, now); e != nil {
		r.setCondition(cr, "Applied", metav1.ConditionFalse, "SnapshotUnavailable", e.Error())
		_ = r.save(ctx, cr)
		return ctrl.Result{}, e
	}
	if cr.Spec.Model == api.ModelStandard && !hasSystemApproval(cr) {
		eligible, why := r.standardEligible(ctx, cr)
		if eligible {
			cr.Status.Approvals = append(cr.Status.Approvals, SystemApproval(why, now))
		} else {
			r.setCondition(cr, "Approved", metav1.ConditionFalse, "StandardNotEligible", why)
		}
	}
	phase, reason, msg := EvaluateApprovals(cr.Spec, cr.Status.Approvals)
	switch phase {
	case api.PhaseApproved:
		conditionReason := "HumanApproval"
		if hasSystemApproval(cr) {
			conditionReason = "StandardModel"
		}
		r.setCondition(cr, "Approved", metav1.ConditionTrue, conditionReason, msg)
		return r.transition(ctx, cr, api.PhaseApproved, reason, msg)
	case api.PhaseRejected:
		r.setCondition(cr, "Approved", metav1.ConditionFalse, "Rejected", msg)
		return r.transition(ctx, cr, api.PhaseRejected, reason, msg)
	}
	if e := r.save(ctx, cr); e != nil {
		return ctrl.Result{}, e
	}
	return ctrl.Result{RequeueAfter: time.Until(Deadline(cr, r.ttl()))}, nil
}

func (r *Reconciler) approved(ctx context.Context, cr *api.ChangeRequest, now time.Time) (ctrl.Result, error) {
	if done, res, e := r.resolveSupersede(ctx, cr); done || e != nil {
		return res, e
	}
	if Expired(cr, now, r.ttl()) {
		return r.expire(ctx, cr)
	}
	if !Implemented(cr.Spec.Operation) {
		r.setCondition(cr, "Applied", metav1.ConditionFalse, "NotImplemented",
			"the change is validated, approved and stored; execution of "+string(cr.Spec.Operation)+" is not implemented yet")
		if e := r.save(ctx, cr); e != nil {
			return ctrl.Result{}, e
		}
		return ctrl.Result{RequeueAfter: time.Until(Deadline(cr, r.ttl()))}, nil
	}
	if w := cr.Spec.Window; w != nil && now.Before(w.Start.Time) {
		if cr.Status.Phase != api.PhaseScheduled {
			r.setCondition(cr, "Applied", metav1.ConditionFalse, "Scheduled", "waiting for the change window to open")
			return r.transition(ctx, cr, api.PhaseScheduled, "Scheduled", "waiting for the change window")
		}
		return ctrl.Result{RequeueAfter: w.Start.Sub(now)}, r.save(ctx, cr)
	}
	return r.transition(ctx, cr, api.PhaseApplying, "Applying", "applying the change")
}

func (r *Reconciler) applying(ctx context.Context, cr *api.ChangeRequest, now time.Time) (ctrl.Result, error) {
	if Expired(cr, now, r.ttl()) {
		return r.expire(ctx, cr)
	}
	handler := HandlerFor(cr.Spec.Operation, r.Granter)
	var oc *ocapi.Client
	if IsBindingOp(cr.Spec.Operation) {
		var e error
		oc, e = r.API.For(ctx, cr.Spec.Organization)
		if e != nil {
			return r.retry(ctx, cr, "ClientUnavailable", e, now)
		}
		// A change that reached Applying without a recorded before state
		// (only possible for objects written out of band) captures it now.
		if e := r.captureBefore(ctx, cr, now); e != nil {
			return r.retry(ctx, cr, "SnapshotUnavailable", e, now)
		}
		// The approval covered the captured before state; a binding that
		// changed since must not be overwritten blindly.
		live, e := handler.Before(ctx, oc, cr, now)
		if e != nil {
			if ocapi.Permanent(e) {
				cr.Status.Attempts++
				r.setCondition(cr, "Applied", metav1.ConditionFalse, "ApplyFailed", e.Error())
				return r.transition(ctx, cr, api.PhaseFailed, "ApplyFailed", e.Error())
			}
			return r.retry(ctx, cr, "SnapshotUnavailable", e, now)
		}
		if !SnapshotsEqual(cr.Status.Before, live) {
			r.setCondition(cr, "Applied", metav1.ConditionFalse, "Stale", "the target binding changed after this change was approved; request a new approval")
			return r.transition(ctx, cr, api.PhaseFailed, "Stale", "target binding changed after approval")
		}
		locked, e := r.acquireLease(ctx, cr)
		if e != nil {
			return r.retry(ctx, cr, "LeaseUnavailable", e, now)
		}
		if !locked {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	res, e := handler.Apply(ctx, oc, cr)
	if e == nil {
		cr.Status.Attempts++
		reason := res.Reason
		if reason == "" {
			reason = "Applied"
		}
		if !res.Executed {
			reason = "Recorded"
		}
		r.setCondition(cr, "Applied", metav1.ConditionTrue, reason, res.Message)
		r.releaseLease(ctx, cr)
		return r.transition(ctx, cr, api.PhaseApplied, reason, res.Message)
	}
	if errors.Is(e, ErrNotImplemented) {
		r.setCondition(cr, "Applied", metav1.ConditionFalse, "NotImplemented", e.Error())
		return ctrl.Result{}, r.save(ctx, cr)
	}
	r.releaseLease(ctx, cr)
	if ocapi.Permanent(e) {
		cr.Status.Attempts++
		r.setCondition(cr, "Applied", metav1.ConditionFalse, "ApplyFailed", e.Error())
		return r.transition(ctx, cr, api.PhaseFailed, "ApplyFailed", e.Error())
	}
	return r.retry(ctx, cr, "Retrying", e, now)
}

// retry records a transient failure and requeues with backoff until the
// attempt budget is exhausted.
func (r *Reconciler) retry(ctx context.Context, cr *api.ChangeRequest, reason string, cause error, now time.Time) (ctrl.Result, error) {
	cr.Status.Attempts++
	if cr.Status.Attempts >= r.maxAttempts() {
		r.setCondition(cr, "Applied", metav1.ConditionFalse, "ApplyFailed", fmt.Sprintf("giving up after %d attempts: %v", cr.Status.Attempts, cause))
		return r.transition(ctx, cr, api.PhaseFailed, "ApplyFailed", cause.Error())
	}
	r.setCondition(cr, "Applied", metav1.ConditionFalse, reason, cause.Error())
	if e := r.save(ctx, cr); e != nil {
		return ctrl.Result{}, e
	}
	return ctrl.Result{RequeueAfter: Backoff(cr.Status.Attempts)}, nil
}

func (r *Reconciler) expire(ctx context.Context, cr *api.ChangeRequest) (ctrl.Result, error) {
	r.setCondition(cr, "Approved", metav1.ConditionFalse, "Expired", "the approval is no longer valid")
	return r.transition(ctx, cr, api.PhaseExpired, "Expired", "approval expired at "+Deadline(cr, r.ttl()).Format(time.RFC3339))
}

// captureBefore records the state the change is approved against, once.
func (r *Reconciler) captureBefore(ctx context.Context, cr *api.ChangeRequest, now time.Time) error {
	if cr.Status.Before != nil {
		return nil
	}
	handler := HandlerFor(cr.Spec.Operation, r.Granter)
	var oc *ocapi.Client
	if IsBindingOp(cr.Spec.Operation) {
		var e error
		oc, e = r.API.For(ctx, cr.Spec.Organization)
		if e != nil {
			return e
		}
	}
	before, e := handler.Before(ctx, oc, cr, now)
	if e != nil {
		return e
	}
	if before == nil {
		before = []api.BindingSnapshot{}
	}
	cr.Status.Before = before
	cr.Status.RollbackPlan = handler.RollbackPlan(cr, before)
	return nil
}

func (r *Reconciler) standardEligible(ctx context.Context, cr *api.ChangeRequest) (bool, string) {
	var list api.ChangeRequestList
	if e := r.Client.List(ctx, &list, client.InNamespace(cr.Namespace)); e != nil {
		return false, "cannot evaluate standard eligibility: " + e.Error()
	}
	return StandardEligible(cr.Spec, list.Items, r.Policy)
}

// resolveSupersede enforces one active change per target: the newest change
// wins, older supersedable ones are marked Superseded. It returns true when
// the reconciled change itself was superseded.
func (r *Reconciler) resolveSupersede(ctx context.Context, cr *api.ChangeRequest) (bool, ctrl.Result, error) {
	key := cr.Status.TargetKey
	if key == "" {
		key = TargetKey(cr.Spec)
	}
	var list api.ChangeRequestList
	if e := r.Client.List(ctx, &list, client.InNamespace(cr.Namespace)); e != nil {
		return false, ctrl.Result{}, e
	}
	all := []api.ChangeRequest{*cr}
	for _, c := range list.Items {
		if c.Name == cr.Name {
			continue
		}
		k := c.Status.TargetKey
		if k == "" {
			k = TargetKey(c.Spec)
		}
		if k == key {
			all = append(all, c)
		}
	}
	winner := SupersedeWinner(all)
	if winner != nil && winner.Name != cr.Name {
		r.setCondition(cr, "Approved", metav1.ConditionFalse, "Superseded", "superseded by "+winner.Name)
		_, e := r.transition(ctx, cr, api.PhaseSuperseded, "Superseded", "superseded by "+winner.Name)
		return true, ctrl.Result{}, e
	}
	var firstErr error
	for i := range all {
		c := &all[i]
		if c.Name == cr.Name || !Supersedable(c.Status.Phase) {
			continue
		}
		fresh := &api.ChangeRequest{}
		if e := r.Client.Get(ctx, client.ObjectKeyFromObject(c), fresh); e != nil {
			firstErr = errors.Join(firstErr, e)
			continue
		}
		if !Supersedable(fresh.Status.Phase) {
			continue
		}
		fresh.Status.Phase = api.PhaseSuperseded
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{Type: "Approved", Status: metav1.ConditionFalse, Reason: "Superseded", Message: "superseded by " + cr.Name, LastTransitionTime: metav1.Now()})
		if e := r.Client.Status().Update(ctx, fresh); e != nil {
			firstErr = errors.Join(firstErr, e)
		}
	}
	return false, ctrl.Result{}, firstErr
}

func hasSystemApproval(cr *api.ChangeRequest) bool {
	for _, a := range cr.Status.Approvals {
		if a.Role == RoleStandard {
			return true
		}
	}
	return false
}

func (r *Reconciler) setCondition(cr *api.ChangeRequest, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: cr.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
}

func (r *Reconciler) transition(ctx context.Context, cr *api.ChangeRequest, phase api.ChangePhase, reason, msg string) (ctrl.Result, error) {
	cr.Status.Phase = phase
	if e := r.save(ctx, cr); e != nil {
		return ctrl.Result{}, e
	}
	return ctrl.Result{Requeue: !phase.Terminal()}, nil
}

func (r *Reconciler) save(ctx context.Context, cr *api.ChangeRequest) error {
	return r.Client.Status().Update(ctx, cr)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&api.ChangeRequest{}).Complete(r)
}
