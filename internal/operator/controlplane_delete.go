package operator

import (
	"context"
	stderrors "errors"
	"fmt"
	"slices"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *ControlPlaneReconciler) finalize(ctx context.Context, cp *api.ControlPlane) (ctrl.Result, error) {
	if !slices.Contains(cp.Finalizers, ControlPlaneFinalizer) {
		return ctrl.Result{}, nil
	}
	before := cp.Status.DeepCopy()
	s := r.session(cp)
	if err := s.delete(ctx); err != nil {
		cp.Status.Phase = "DeletionBlocked"
		setControlPlaneCondition(cp, "Ready", false, "DeletionBlocked", err.Error())
		if e := r.writeStatus(ctx, cp, before); e != nil {
			return ctrl.Result{}, e
		}
		var waiting *pendingError
		if stderrors.As(err, &waiting) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	base := cp.DeepCopy()
	cp.Finalizers = slices.DeleteFunc(cp.Finalizers, func(v string) bool { return v == ControlPlaneFinalizer })
	if err := r.Patch(ctx, cp, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	r.mu.Lock()
	delete(r.state, cp.UID)
	r.mu.Unlock()
	return ctrl.Result{}, nil
}

// usesControlPlane reports whether Organization o is served by the
// ControlPlane named name: by reference, or as the default when no legacy
// aep-cp kubeconfig is configured.
func (o Options) usesControlPlane(org *api.Organization, name string) bool {
	if org.Spec.ControlPlaneRef != "" || org.Status.ControlPlaneRef != "" {
		return org.Spec.ControlPlaneRef == name || org.Status.ControlPlaneRef == name
	}
	return o.DefaultControlPlane == name && o.CPSecret == "" && o.CPFile == ""
}

// delete tears the control plane down in dependency order, one layer at a
// time: the content charts, the control plane chart, kgateway, the ingress and network policy,
// the cluster, the portal database (only on request), the tenant and the
// operator credential. Objects inside aep-cp go with its cluster.
func (s *controlPlaneSession) delete(ctx context.Context) error {
	if s.cp.Spec.Suspend {
		return pending("reconciliation is suspended; clear spec.suspend to delete")
	}
	if s.cp.Annotations["aep.aenix.io/confirm-delete"] != s.cp.Name {
		return pending("set aep.aenix.io/confirm-delete=%s to delete the control plane", s.cp.Name)
	}
	var organizations api.OrganizationList
	if err := s.r.List(ctx, &organizations); err != nil {
		return err
	}
	for i := range organizations.Items {
		if s.r.Options.usesControlPlane(&organizations.Items[i], s.cp.Name) {
			return pending("Organization %s uses this control plane", organizations.Items[i].Name)
		}
	}
	mgmt := s.r.Management
	for _, chart := range []string{BuildsChart, ComponentTypesChart, ControlPlaneChart, "kgateway", "kgateway-crds"} {
		if e := s.deleteNamed(ctx, mgmt, helmAPI, "helmreleases", s.ns, releaseName(chart)); e != nil {
			return e
		}
	}
	layers := []struct{ av, resource, ns string }{
		{"source.toolkit.fluxcd.io/v1", "ocirepositories", s.ns},
		{"networking.k8s.io/v1", "ingresses", s.ns},
		{"cilium.io/v2", "ciliumclusterwidenetworkpolicies", ""},
		{appsAPI, "kubernetesnodeses", s.ns},
		{appsAPI, "kuberneteses", s.ns},
	}
	for _, l := range layers {
		if e := s.deleteOwned(ctx, mgmt, l.av, l.resource, l.ns); e != nil {
			return e
		}
	}
	if s.cp.Annotations[DeletePortalDatabaseAnnotation] == "true" {
		if e := s.deleteNamed(ctx, mgmt, appsAPI, "postgreses", s.ns, portalDatabase); e != nil {
			return e
		}
	} else if _, e := mgmt.get(ctx, appsAPI, "postgreses", s.ns, portalDatabase); e == nil {
		// Deleting the tenant would delete the database with its namespace.
		return pending("portal database Postgres %s/%s is kept: set %s=true or delete it to remove tenant %s", s.ns, portalDatabase, DeletePortalDatabaseAnnotation, s.ns)
	} else if !apierrors.IsNotFound(e) {
		return e
	}
	if e := s.deleteOwned(ctx, mgmt, "v1", "secrets", s.ns); e != nil {
		return e
	}
	if e := s.deleteNamed(ctx, mgmt, appsAPI, "tenants", s.cp.Spec.Tenant.Parent, s.cp.Spec.Tenant.Name); e != nil {
		return e
	}
	return s.deleteNamed(ctx, mgmt, "v1", "secrets", s.r.Options.Namespace, s.credentialName())
}

func (s *controlPlaneSession) deleteOwned(ctx context.Context, c *Cluster, av, r, ns string) error {
	items, e := c.resource(av, r, ns).List(ctx, metav1.ListOptions{LabelSelector: ControlPlaneLabel + "=" + string(s.cp.UID)})
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	for i := range items.Items {
		if e = s.deleteObject(ctx, c, r, &items.Items[i]); e != nil {
			return e
		}
	}
	if len(items.Items) > 0 {
		return pending("waiting for owned %s in %s to be deleted", r, ns)
	}
	return nil
}

func (s *controlPlaneSession) deleteNamed(ctx context.Context, c *Cluster, av, r, ns, name string) error {
	u, e := c.get(ctx, av, r, ns, name)
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = s.deleteObject(ctx, c, r, u); e != nil {
		return e
	}
	return pending("waiting for %s %s deletion", r, name)
}

// deleteObject deletes an object owned by this control plane in the background.
func (s *controlPlaneSession) deleteObject(ctx context.Context, c *Cluster, r string, u *unstructured.Unstructured) error {
	if u.GetLabels()[ControlPlaneLabel] != string(s.cp.UID) {
		return fmt.Errorf("refusing to delete unowned %s %s/%s", r, u.GetNamespace(), u.GetName())
	}
	if !u.GetDeletionTimestamp().IsZero() {
		return nil
	}
	uid := u.GetUID()
	policy := metav1.DeletePropagationBackground
	return client.IgnoreNotFound(c.resource(u.GetAPIVersion(), r, u.GetNamespace()).Delete(ctx, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy}))
}
