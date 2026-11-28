package operator

import (
	"context"
	"reflect"

	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func (r *Reconciler) SetupWithManager(m ctrl.Manager) error {
	r.Options.Normalize()
	if err := m.Add(&policyMaintenance{r: r}); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(m).
		For(&api.Organization{}).
		WithOptions(controller.Options{RateLimiter: retryLimiter()})
	for _, resource := range []struct{ api, kind string }{
		{helmAPI, "HelmRelease"},
		{appsAPI, "Tenant"},
		{appsAPI, "Kubernetes"},
		{appsAPI, "KubernetesNodes"},
		{"kamaji.clastix.io/v1alpha1", "TenantControlPlane"},
		{"kubevirt.io/v1", "VirtualMachineInstance"},
	} {
		// Metadata-only watches omit status, which is needed to distinguish
		// readiness transitions from spec/metadata churn.
		b = b.Watches(obj(resource.api, resource.kind, "", "", nil),
			handler.EnqueueRequestsFromMapFunc(r.organizationsForObject),
			builder.WithPredicates(dependencyStatusPredicate()))
	}
	// Organizations follow the readiness, credential and gateway of their
	// ControlPlane, and are rejected while one shares their tenant namespace.
	b = b.Watches(&api.ControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.organizationsForControlPlane), builder.WithPredicates(controlPlaneChangePredicate()))
	return b.Complete(r)
}

// organizationsForControlPlane maps a ControlPlane to the Organizations it
// serves or whose tenant namespaces it collides with.
func (r *Reconciler) organizationsForControlPlane(ctx context.Context, object client.Object) []ctrl.Request {
	cp, ok := object.(*api.ControlPlane)
	if !ok {
		return nil
	}
	var organizations api.OrganizationList
	if err := r.List(ctx, &organizations); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map ControlPlane to Organizations")
		return nil // The periodic requeue remains a fallback.
	}
	var requests []ctrl.Request
	for i := range organizations.Items {
		o := &organizations.Items[i]
		if r.Options.usesControlPlane(o, cp.Name) || namespaceCollision(cp, o) != "" {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: o.Name}})
		}
	}
	return requests
}

// controlPlaneChangePredicate passes ControlPlane creation, deletion and
// changes of spec or status.
func controlPlaneChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok := e.ObjectOld.(*api.ControlPlane)
			updated, ok2 := e.ObjectNew.(*api.ControlPlane)
			return !ok || !ok2 || old.Generation != updated.Generation || !reflect.DeepEqual(old.Status, updated.Status)
		},
	}
}

// Match actual Organizations instead of splitting namespace names, so tenant-root
// and unrelated namespaces cannot accidentally map to an Organization. The label
// is a UID, not a name, and also covers root Tenants in tenant-root.
func (r *Reconciler) organizationsForObject(ctx context.Context, object client.Object) []ctrl.Request {
	// A ControlPlane tenant namespace can equal an Organization one (tenant
	// cp under tenant-root is tenant-cp, as for an Organization named cp);
	// ControlPlane-owned objects never map to Organizations.
	if object.GetLabels()[ControlPlaneLabel] != "" {
		return nil
	}
	var organizations api.OrganizationList
	if err := r.List(ctx, &organizations); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map dependency to Organizations")
		return nil // The periodic requeue remains a fallback.
	}
	var requests []ctrl.Request
	for _, o := range organizations.Items {
		owned := object.GetLabels()[OwnerLabel] != "" && object.GetLabels()[OwnerLabel] == string(o.UID)
		matches := object.GetNamespace() == TenantNamespace(o.Name, "")
		for _, tier := range Tiers(&o) {
			matches = matches || object.GetNamespace() == TenantNamespace(o.Name, tier)
		}
		if owned || matches {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: o.Name}})
		}
	}
	return requests
}

func dependencyStatusPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return hasReadinessStatus(e.Object) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !reflect.DeepEqual(readinessStatus(e.ObjectOld), readinessStatus(e.ObjectNew))
		},
	}
}

func hasReadinessStatus(o client.Object) bool {
	for _, value := range readinessStatus(o) {
		if value != nil {
			return true
		}
	}
	return false
}

func readinessStatus(o client.Object) []any {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	conditions, _, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "conditions")
	// Kamaji reports readiness outside status.conditions.
	version, _, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "kubernetesResources", "version", "status")
	return []any{conditions, version}
}
