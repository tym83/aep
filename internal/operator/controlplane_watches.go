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

func (r *ControlPlaneReconciler) SetupWithManager(m ctrl.Manager) error {
	r.Options.Normalize()
	b := ctrl.NewControllerManagedBy(m).
		For(&api.ControlPlane{}).
		Named("controlplane").
		WithOptions(controller.Options{RateLimiter: retryLimiter()})
	for _, resource := range []struct{ api, kind string }{
		{helmAPI, "HelmRelease"},
		{appsAPI, "Tenant"},
		{appsAPI, "Kubernetes"},
		{appsAPI, "KubernetesNodes"},
		{appsAPI, "Postgres"},
		{"kamaji.clastix.io/v1alpha1", "TenantControlPlane"},
	} {
		b = b.Watches(obj(resource.api, resource.kind, "", "", nil),
			handler.EnqueueRequestsFromMapFunc(r.controlPlanesForObject),
			builder.WithPredicates(dependencyStatusPredicate()))
	}
	// Mirror Services and Ingresses report readiness outside conditions: the
	// gateway mirror appears, the ingress gets an address.
	for _, resource := range []struct{ api, kind string }{{"v1", "Service"}, {"networking.k8s.io/v1", "Ingress"}} {
		b = b.Watches(obj(resource.api, resource.kind, "", "", nil),
			handler.EnqueueRequestsFromMapFunc(r.controlPlanesForObject),
			builder.WithPredicates(objectChangePredicate()))
	}
	// Organizations block deletion while they use a control plane.
	b = b.Watches(&api.Organization{}, handler.EnqueueRequestsFromMapFunc(r.controlPlanesForOrganization))
	return b.Complete(r)
}

// controlPlanesForObject maps an object to the ControlPlanes that own it by
// label or whose tenant namespace holds it. Organization-owned objects never
// map to a ControlPlane, even when the namespaces coincide.
func (r *ControlPlaneReconciler) controlPlanesForObject(ctx context.Context, object client.Object) []ctrl.Request {
	if object.GetLabels()[OwnerLabel] != "" {
		return nil
	}
	var planes api.ControlPlaneList
	if err := r.List(ctx, &planes); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map dependency to ControlPlanes")
		return nil // The periodic requeue remains a fallback.
	}
	owner := object.GetLabels()[ControlPlaneLabel]
	var requests []ctrl.Request
	for i := range planes.Items {
		cp := &planes.Items[i]
		if (owner != "" && owner == string(cp.UID)) || (owner == "" && object.GetNamespace() == ControlPlaneNamespace(cp)) {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name}})
		}
	}
	return requests
}

func (r *ControlPlaneReconciler) controlPlanesForOrganization(ctx context.Context, object client.Object) []ctrl.Request {
	o, ok := object.(*api.Organization)
	if !ok {
		return nil
	}
	names := map[string]bool{}
	for _, name := range []string{o.Spec.ControlPlaneRef, o.Status.ControlPlaneRef, r.Options.DefaultControlPlane} {
		if name != "" && r.Options.usesControlPlane(o, name) {
			names[name] = true
		}
	}
	var requests []ctrl.Request
	for name := range names {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	}
	return requests
}

// objectChangePredicate passes creation, deletion and changes of labels,
// spec or status, ignoring metadata churn.
func objectChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !reflect.DeepEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) || !reflect.DeepEqual(specAndStatus(e.ObjectOld), specAndStatus(e.ObjectNew))
		},
	}
}

func specAndStatus(o client.Object) []any {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	return []any{u.Object["spec"], u.Object["status"]}
}
