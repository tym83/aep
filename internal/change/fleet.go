package change

import (
	"context"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/ocapi"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

func requireClusterAttach(s api.ChangeRequestSpec, d map[string]any, _ time.Time) error {
	if s.Model != api.ModelNormal {
		return fmt.Errorf("cluster-attach requires the normal model")
	}
	if s.Target.Name == "" || len(s.Target.Name) > 40 {
		return fmt.Errorf("cluster name must contain 1..40 characters")
	}
	for k := range d {
		switch k {
		case "tier", "trustLevel", "provider", "distro":
		default:
			return fmt.Errorf("unknown cluster-attach field %s", k)
		}
	}
	for _, k := range []string{"tier", "trustLevel", "provider", "distro"} {
		if v, ok := d[k].(string); !ok || v == "" {
			return fmt.Errorf("desired.%s is required", k)
		}
	}
	if d["tier"] != "prod" && d["tier"] != "nonprod" {
		return fmt.Errorf("tier must be prod or nonprod")
	}
	switch d["trustLevel"] {
	case "untrusted", "standard", "regulated":
	default:
		return fmt.Errorf("invalid trustLevel")
	}
	if d["tier"] == "prod" && d["trustLevel"] == "untrusted" {
		return fmt.Errorf("untrusted clusters cannot host prod")
	}
	return nil
}
func FleetNeedsSecurity(s api.ChangeRequestSpec) bool {
	d, e := DesiredMap(s)
	return e != nil || d["tier"] == "prod" || d["trustLevel"] == "regulated" || s.Target.Environment == "prod"
}

// EvaluateFleetApprovals requires distinct humans. Group overlap cannot count
// one identity twice, and no system or emergency approval can bypass this gate.
func EvaluateFleetApprovals(s api.ChangeRequestSpec, approvals []api.ChangeApproval) (api.ChangePhase, string, string) {
	if s.Model != api.ModelNormal {
		return api.PhasePending, "InvalidModel", "cluster-attach requires normal approval"
	}
	platforms, securities := map[string]bool{}, map[string]bool{}
	for _, a := range approvals {
		if a.User == s.Requester || IsServiceAccount(a.User) || a.Source == "system" {
			continue
		}
		p, q := HasGroup(a.Groups, GroupPlatformTeam), HasGroup(a.Groups, GroupSecurity)
		if !p && !q {
			continue
		}
		if a.Decision == DecisionReject {
			return api.PhaseRejected, "Rejected", "fleet assignment rejected"
		}
		if a.Decision != DecisionApprove {
			continue
		}
		if p {
			platforms[a.User] = true
		}
		if q {
			securities[a.User] = true
		}
	}
	for p := range platforms {
		if !FleetNeedsSecurity(s) {
			return api.PhaseApproved, "Approved", "platform approved"
		}
		for q := range securities {
			if p != q {
				return api.PhaseApproved, "Approved", "platform and security approved"
			}
		}
	}
	return api.PhasePending, "PendingApproval", "waiting for platform and, for prod/regulated, a distinct security approver"
}
func ExternalSpec(cr *api.ChangeRequest) (api.ExternalClusterSpec, error) {
	d, e := DesiredMap(cr.Spec)
	if e != nil {
		return api.ExternalClusterSpec{}, e
	}
	if e = requireClusterAttach(cr.Spec, d, time.Now()); e != nil {
		return api.ExternalClusterSpec{}, e
	}
	return api.ExternalClusterSpec{Organization: cr.Spec.Organization, Tier: d["tier"].(string), Environment: cr.Spec.Target.Environment, TrustLevel: d["trustLevel"].(string), Provider: d["provider"].(string), Distro: d["distro"].(string), ChangeRequestRef: cr.Name, ClusterProfileRef: api.ClusterProfileReference{APIVersion: "multicluster.x-k8s.io/v1alpha1", Namespace: "aep-" + cr.Spec.Organization, Name: cr.Spec.Target.Name}}, nil
}

// VerifyExternalApproval is shared by the registrar and bootstrap CLI. Neither
// trusts an ExternalCluster annotation or a caller-supplied approved boolean.
func VerifyExternalApproval(ctx context.Context, c client.Reader, ec *api.ExternalCluster) error {
	cr := &api.ChangeRequest{}
	if e := c.Get(ctx, client.ObjectKey{Namespace: ec.Namespace, Name: ec.Spec.ChangeRequestRef}, cr); e != nil {
		return e
	}
	if cr.Spec.Operation != api.OperationClusterAttach || cr.Spec.Target.Name != ec.Name || cr.Status.Phase != api.PhaseApplied {
		return fmt.Errorf("assignment has no applied cluster-attach change")
	}
	s, e := ExternalSpec(cr)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(s, ec.Spec) {
		return fmt.Errorf("assignment differs from approved change")
	}
	if p, _, _ := EvaluateFleetApprovals(cr.Spec, cr.Status.Approvals); p != api.PhaseApproved {
		return fmt.Errorf("fleet approvals missing")
	}
	return ValidateFleetEnvironment(ctx, c, ec.Spec)
}
func ValidateFleetEnvironment(ctx context.Context, c client.Reader, s api.ExternalClusterSpec) error {
	o := &api.Organization{}
	if e := c.Get(ctx, client.ObjectKey{Name: s.Organization}, o); e != nil {
		return e
	}
	for _, env := range o.Spec.Environments {
		if env.Name == s.Environment && env.Tier == s.Tier {
			return nil
		}
	}
	return fmt.Errorf("environment and tier must match Organization.spec.environments")
}

type clusterAttachHandler struct{ c client.Client }

func (clusterAttachHandler) Before(context.Context, *ocapi.Client, *api.ChangeRequest, time.Time) ([]api.BindingSnapshot, error) {
	return nil, nil
}
func (clusterAttachHandler) RollbackPlan(*api.ChangeRequest, []api.BindingSnapshot) *api.RollbackPlan {
	return &api.RollbackPlan{Action: "none", Description: "Drain workloads before platform de-registration; deletion is not an automatic rollback"}
}
func (h clusterAttachHandler) Apply(ctx context.Context, _ *ocapi.Client, cr *api.ChangeRequest) (Result, error) {
	if p, _, _ := EvaluateFleetApprovals(cr.Spec, cr.Status.Approvals); p != api.PhaseApproved {
		return Result{}, fmt.Errorf("fleet approval missing")
	}
	s, e := ExternalSpec(cr)
	if e != nil {
		return Result{}, e
	}
	if e = ValidateFleetEnvironment(ctx, h.c, s); e != nil {
		return Result{}, e
	}
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: cr.Spec.Target.Name, Namespace: cr.Namespace}, Spec: s}
	var assigned api.ExternalClusterList
	if e = h.c.List(ctx, &assigned, client.InNamespace(cr.Namespace)); e != nil {
		return Result{}, e
	}
	for _, existing := range assigned.Items {
		if existing.Name != ec.Name && existing.Spec.Organization == s.Organization && existing.Spec.Environment == s.Environment {
			return Result{}, fmt.Errorf("environment already assigned to %s", existing.Name)
		}
	}
	old := &api.ExternalCluster{}
	e = h.c.Get(ctx, client.ObjectKeyFromObject(ec), old)
	if errors.IsNotFound(e) {
		e = h.c.Create(ctx, ec)
	} else if e == nil && !reflect.DeepEqual(old.Spec, s) {
		e = fmt.Errorf("cluster already assigned by a different change")
	}
	return Result{Executed: e == nil, Message: "external cluster assignment recorded"}, e
}
func (r *Reconciler) handler(op api.ChangeOperation) Handler {
	if op == api.OperationClusterAttach {
		return clusterAttachHandler{r.Client}
	}
	return HandlerFor(op, r.Granter)
}
