package bridge

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	"k8s.io/apimachinery/pkg/api/equality"
	errors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	Domain    = "aep.aenix.io/"
	Finalizer = Domain + "bridge"
	Manager   = "aep-bridge"
)

var HelmReleases = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
var TenantSecrets = schema.GroupVersionResource{Group: "core.cozystack.io", Version: "v1alpha1", Resource: "tenantsecrets"}

type Reconciler struct {
	App                                                                client.Client
	Management                                                         client.Client
	Dynamic                                                            dynamic.Interface
	Organization, Tier, TenantNamespace, ClusterID, TierDeletionPolicy string
	AllowedKinds                                                       []string
	Policy                                                             policy.Config
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if !strings.HasPrefix(req.Namespace, "dp-") {
		return ctrl.Result{}, nil
	}
	c := &api.ServiceClaim{}
	if e := r.App.Get(ctx, req.NamespacedName, c); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	// Finalization uses the persisted binding, even if the operator has disabled the kind.
	if !c.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, c)
	}
	desired, e := policy.Validate(c, r.Policy, r.AllowedKinds)
	if e != nil {
		return r.report(ctx, c, "ParameterNotAllowed", e.Error())
	}
	m := kinds.Table[c.Spec.Kind]
	name := policy.Name(r.ClusterID, c.Namespace, c.Name, c.Spec.Resource.Name)
	apps := r.Dynamic.Resource(m.GVR()).Namespace(r.TenantNamespace)
	app, e := apps.Get(ctx, name, metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return r.reportError(ctx, c, "ManagementUnavailable", e)
	}
	exists := e == nil
	if exists && app.GetAnnotations()[Domain+"claim-uid"] != string(c.UID) {
		return r.report(ctx, c, "NameConflict", "Application belongs to a different claim UID")
	}
	immutable := map[string]any{}
	for _, key := range []string{"version", "users", "databases"} {
		if value, ok := desired[key]; ok {
			immutable[key] = value
		}
	}
	immutableHash := policy.Hash(immutable)
	if exists && app.GetAnnotations()[Domain+"immutable-spec-hash"] != "" && app.GetAnnotations()[Domain+"immutable-spec-hash"] != immutableHash {
		return r.report(ctx, c, "ParameterNotAllowed", "version, users and databases are immutable; create a new resource or use an approved platform operation")
	}
	if exists && app.GetAnnotations()[Domain+"paused"] == "true" {
		return r.report(ctx, c, "Paused", "Application reconciliation is paused")
	}
	if !controllerutil.ContainsFinalizer(c, Finalizer) {
		base := c.DeepCopy()
		controllerutil.AddFinalizer(c, Finalizer)
		if e := r.App.Patch(ctx, c, client.MergeFrom(base)); e != nil {
			return ctrl.Result{}, e
		}
	}
	hash := policy.Hash(desired)
	current := map[string]any{}
	if exists {
		current, _, _ = unstructured.NestedMap(app.Object, "spec")
	}
	changed := !exists || app.GetAnnotations()[Domain+"backup-policy"] != r.Policy.BackupPolicy || app.GetAnnotations()[Domain+"claim-spec-hash"] != hash || policy.Hash(current) != app.GetAnnotations()[Domain+"applied-spec-hash"]
	if changed {
		if _, e := r.report(ctx, c, "Updating", "Applying the requested specification"); e != nil {
			return ctrl.Result{}, e
		}
		if !exists {
			app = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps.cozystack.io/v1alpha1", "kind": c.Spec.Kind, "metadata": map[string]any{"name": name, "namespace": r.TenantNamespace}}}
		}
		app.Object["spec"] = desired
		a := app.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		if r.Policy.BackupPolicy != "" {
			a[Domain+"backup-policy"] = r.Policy.BackupPolicy
		} else {
			delete(a, Domain+"backup-policy")
		}
		a[Domain+"claim-uid"] = string(c.UID)
		a[Domain+"cluster-id"] = r.ClusterID
		a[Domain+"claim-ref"] = c.Namespace + "/" + c.Name
		a[Domain+"applied-spec-hash"] = hash
		a[Domain+"claim-spec-hash"] = hash
		a[Domain+"immutable-spec-hash"] = immutableHash
		app.SetAnnotations(a)
		labels := app.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range map[string]string{"organization": r.Organization, "tier": r.Tier, "project": c.Spec.Resource.Project, "environment": c.Spec.Resource.Environment, "resource": c.Spec.Resource.Name, "managed-by": "bridge"} {
			labels[Domain+k] = v
		}
		app.SetLabels(labels)
		if exists {
			app, e = apps.Update(ctx, app, metav1.UpdateOptions{})
		} else {
			app, e = apps.Create(ctx, app, metav1.CreateOptions{})
		}
		if e != nil {
			return r.reportError(ctx, c, "ApplyFailed", e)
		}
	}
	// Cozystack defaults spec at admission. Hash the accepted spec, not just the request.
	accepted, _, _ := unstructured.NestedMap(app.Object, "spec")
	appliedHash := policy.Hash(accepted)
	if changed && app.GetAnnotations()[Domain+"applied-spec-hash"] != appliedHash {
		annotations := app.GetAnnotations()
		annotations[Domain+"applied-spec-hash"] = appliedHash
		app.SetAnnotations(annotations)
		app, e = apps.Update(ctx, app, metav1.UpdateOptions{})
		if e != nil {
			return r.reportError(ctx, c, "ApplyFailed", e)
		}
	}
	c.Status.Application = &api.ApplicationReference{Kind: c.Spec.Kind, Name: name, Namespace: r.TenantNamespace, UID: string(app.GetUID())}
	c.Status.AppliedSpecHash = appliedHash
	hr, e := r.Dynamic.Resource(HelmReleases).Namespace(r.TenantNamespace).Get(ctx, m.Release(name), metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return r.reportError(ctx, c, "ManagementUnavailable", e)
	}
	actual, _, _ := unstructured.NestedMap(app.Object, "spec")
	// Checking values closes the window where the aggregated API still exposes an old, Ready release.
	values, _, _ := unstructured.NestedMap(hrObject(hr), "spec", "values")
	matches := !changed && policy.Hash(actual) == appliedHash && app.GetAnnotations()[Domain+"applied-spec-hash"] == appliedHash && contains(values, desired)
	conditions := Readiness(app, hr, matches)
	for _, v := range conditions {
		v.ObservedGeneration = c.Generation
		meta.SetStatusCondition(&c.Status.Conditions, v)
		if v.Reason != "Updating" && v.Reason != "Pending" && v.Reason != "Healthy" {
			meta.FindStatusCondition(c.Status.Conditions, v.Type).LastTransitionTime = v.LastTransitionTime
		}
	}
	if !changed {
		drift, e := r.connect(ctx, c, m, name)
		if e != nil {
			return r.reportError(ctx, c, "ConnectionUnavailable", e)
		}
		if drift {
			r.set(c, "Degraded", metav1.ConditionTrue, "EndpointDrift", "EndpointSlice differed from the management Service; the endpoint was restored")
		}
	}
	return r.save(ctx, c)
}
func hrObject(u *unstructured.Unstructured) map[string]any {
	if u == nil {
		return nil
	}
	return u.Object
}
func contains(actual, desired map[string]any) bool {
	for k, v := range desired {
		if m, ok := v.(map[string]any); ok {
			a, ok := actual[k].(map[string]any)
			if !ok || !contains(a, m) {
				return false
			}
		} else if !reflect.DeepEqual(actual[k], v) {
			return false
		}
	}
	return true
}
func (r *Reconciler) set(c *api.ServiceClaim, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: c.Generation, LastTransitionTime: metav1.Now()})
}
func (r *Reconciler) report(ctx context.Context, c *api.ServiceClaim, reason, msg string) (ctrl.Result, error) {
	r.set(c, "Ready", metav1.ConditionFalse, reason, msg)
	return r.save(ctx, c)
}
func (r *Reconciler) reportError(ctx context.Context, c *api.ServiceClaim, reason string, e error) (ctrl.Result, error) {
	_, se := r.report(ctx, c, reason, e.Error())
	if se != nil {
		return ctrl.Result{}, se
	}
	return ctrl.Result{}, e
}
func (r *Reconciler) save(ctx context.Context, c *api.ServiceClaim) (ctrl.Result, error) {
	c.Status.ObservedGeneration = c.Generation
	current := &api.ServiceClaim{}
	if e := r.App.Get(ctx, client.ObjectKeyFromObject(c), current); e != nil {
		return ctrl.Result{}, e
	}
	if equality.Semantic.DeepEqual(current.Status, c.Status) {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, r.App.Status().Update(ctx, c)
}
func (r *Reconciler) finalize(ctx context.Context, c *api.ServiceClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(c, Finalizer) {
		return ctrl.Result{}, nil
	}
	kind, name := c.Spec.Kind, policy.Name(r.ClusterID, c.Namespace, c.Name, c.Spec.Resource.Name)
	if ref := c.Status.Application; ref != nil {
		if ref.Namespace != r.TenantNamespace {
			return r.report(ctx, c, "NameConflict", "Binding points outside the configured tenant")
		}
		kind, name = ref.Kind, ref.Name
	}
	m, ok := kinds.Table[kind]
	if !ok {
		return r.report(ctx, c, "ParameterNotAllowed", "Unknown bound kind")
	}
	apps := r.Dynamic.Resource(m.GVR()).Namespace(r.TenantNamespace)
	app, e := apps.Get(ctx, name, metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return ctrl.Result{}, e
	}
	if e == nil {
		if app.GetAnnotations()[Domain+"claim-uid"] != string(c.UID) {
			return r.report(ctx, c, "NameConflict", "Refusing to finalize another claim's application")
		}
		if app.GetAnnotations()[Domain+"paused"] == "true" {
			return r.report(ctx, c, "Paused", "Paused application blocks finalization")
		}
		if policy.Retain(r.Tier, r.TierDeletionPolicy, c.Spec.DeletionPolicy) {
			a := app.GetAnnotations()
			a[Domain+"orphaned"] = "true"
			app.SetAnnotations(a)
			if _, e = apps.Update(ctx, app, metav1.UpdateOptions{}); e != nil {
				return ctrl.Result{}, e
			}
		} else {
			uid, rv := app.GetUID(), app.GetResourceVersion()
			e = apps.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
			if e != nil && !errors.IsNotFound(e) {
				return ctrl.Result{}, e
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	base := c.DeepCopy()
	controllerutil.RemoveFinalizer(c, Finalizer)
	return ctrl.Result{}, r.App.Patch(ctx, c, client.MergeFrom(base))
}
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.TenantNamespace == "" {
		return fmt.Errorf("tenant namespace is required")
	}
	return ctrl.NewControllerManagedBy(mgr).For(&api.ServiceClaim{}).Complete(r)
}
