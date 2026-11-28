package operator

import (
	"context"
	stderrors "errors"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"time"
)

func (r *Reconciler) finalize(ctx context.Context, o *api.Organization) (ctrl.Result, error) {
	if !slices.Contains(o.Finalizers, finalizer) {
		return ctrl.Result{}, nil
	}
	s := &session{r: r, o: o, tiers: map[string]*Cluster{}}
	err := s.delete(ctx)
	if err != nil {
		o.Status.Phase = "DeletionBlocked"
		setCondition(o, "TenantsReady", false, "DeletionBlocked", err.Error())
		if e := r.Status().Update(ctx, o); e != nil {
			return ctrl.Result{}, e
		}
		var waiting *pendingError
		if stderrors.As(err, &waiting) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	base := o.DeepCopy()
	o.Finalizers = slices.DeleteFunc(o.Finalizers, func(v string) bool { return v == finalizer })
	return ctrl.Result{}, r.Patch(ctx, o, client.MergeFrom(base))
}
func (s *session) delete(ctx context.Context) error {
	if s.o.Annotations["aep.aenix.io/confirm-delete"] != s.o.Name {
		return pending("set aep.aenix.io/confirm-delete=%s after removing all ServiceClaims and Components", s.o.Name)
	}
	if e := s.controlPlane(ctx); e != nil {
		return e
	}
	if !s.o.Status.DeletionStarted {
		components, e := s.cp.list(ctx, ocAPI, "components", s.o.Name)
		if e != nil && !errors.IsNotFound(e) {
			return e
		}
		if len(components) > 0 {
			return pending("Component %s/%s blocks deletion", s.o.Name, components[0].GetName())
		}
		for _, t := range Tiers(s.o) {
			ns := TenantNamespace(s.o.Name, t)
			_, e := s.r.Management.get(ctx, appsAPI, "kuberneteses", ns, "apps")
			if errors.IsNotFound(e) {
				continue
			}
			if e != nil {
				return e
			}
			sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, "operator-access-"+s.o.Name+"-"+t, metav1.GetOptions{})
			if e != nil {
				return fmt.Errorf("cannot verify claims in %s: %w", ns, e)
			}
			cfg, e := clientcmd.RESTConfigFromKubeConfig(sec.Data["kubeconfig"])
			if e != nil {
				return e
			}
			c, e := NewCluster(cfg)
			if e != nil {
				return e
			}
			claims, e := c.list(ctx, "aep.aenix.io/v1alpha1", "serviceclaims", "")
			if e != nil {
				return e
			}
			if len(claims) > 0 {
				return pending("ServiceClaim %s/%s in %s blocks deletion", claims[0].GetNamespace(), claims[0].GetName(), t)
			}
			// Retained managed applications must never disappear with their tenant.
			for _, kind := range []string{"postgreses", "redises"} {
				items, e := s.r.Management.list(ctx, appsAPI, kind, ns)
				if e != nil {
					return e
				}
				if len(items) > 0 {
					return pending("managed service %s/%s blocks tenant deletion", ns, items[0].GetName())
				}
			}
		}
		s.o.Status.DeletionStarted = true
		if e := s.r.Status().Update(ctx, s.o); e != nil {
			return e
		}
	}
	// Organizations created by older releases carry admission rules that block finalizer release.
	if _, e := s.cp.get(ctx, "v1", "namespaces", "", s.o.Name); e == nil {
		if e = s.admission(ctx); e != nil {
			return e
		}
	} else if !errors.IsNotFound(e) {
		return e
	}
	var external api.ExternalClusterList
	if err := s.r.List(ctx, &external, client.InNamespace("tenant-"+s.o.Name)); err != nil {
		return err
	}
	for i := range external.Items {
		ec := &external.Items[i]
		if ec.Spec.Organization != s.o.Name {
			continue
		}
		if err := s.r.Delete(ctx, ec); client.IgnoreNotFound(err) != nil {
			return err
		}
		return pending("waiting for ExternalCluster %s cleanup", ec.Name)
	}
	var e error
	// Delete one dependency layer at a time; wait for finalizers before advancing.
	for _, r := range []string{"secretreferences", "authzrolebindings", "authzroles", "deploymentpipelines", "environments", "dataplanes"} {
		if e = s.deleteOwned(ctx, s.cp, ocAPI, r, s.o.Name); e != nil {
			return e
		}
	}
	for i := len(Tiers(s.o)) - 1; i >= 0; i-- {
		t := Tiers(s.o)[i]
		ns := TenantNamespace(s.o.Name, t)
		for _, r := range []string{"helmreleases", "ocirepositories", "helmrepositories"} {
			av := helmAPI
			if r != "helmreleases" {
				av = "source.toolkit.fluxcd.io/v1"
			}
			if e = s.deleteOwned(ctx, s.r.Management, av, r, ns); e != nil {
				return e
			}
		}
	}
	if e = s.deleteOwned(ctx, s.r.Management, "cilium.io/v2", "ciliumclusterwidenetworkpolicies", ""); e != nil {
		return e
	}
	for _, r := range []string{"deployments", "configmaps", "serviceaccounts"} {
		av := "v1"
		if r == "deployments" {
			av = "apps/v1"
		}
		if e = s.deleteOwned(ctx, s.r.Management, av, r, s.r.Options.Namespace); e != nil {
			return e
		}
	}
	for i := len(Tiers(s.o)) - 1; i >= 0; i-- {
		ns := TenantNamespace(s.o.Name, Tiers(s.o)[i])
		for _, r := range []string{"rolebindings", "roles", "kubernetesnodeses", "kuberneteses"} {
			av := appsAPI
			if r == "roles" || r == "rolebindings" {
				av = "rbac.authorization.k8s.io/v1"
			}
			if e = s.deleteOwned(ctx, s.r.Management, av, r, ns); e != nil {
				return e
			}
		}
	}
	if e = s.deleteOwned(ctx, s.r.Management, appsAPI, "tenants", TenantNamespace(s.o.Name, "")); e != nil {
		return e
	}
	if e = s.deleteNamed(ctx, s.r.Management, appsAPI, "tenants", "tenant-root", s.o.Name); e != nil {
		return e
	}
	if e = s.deleteNamed(ctx, s.cp, "v1", "namespaces", "", s.o.Name); e != nil {
		return e
	}
	for _, r := range []string{"validatingadmissionpolicybindings", "validatingadmissionpolicies"} {
		if e = s.deleteOwned(ctx, s.cp, "admissionregistration.k8s.io/v1", r, ""); e != nil {
			return e
		}
	}
	return s.deleteOwned(ctx, s.r.Management, "v1", "secrets", s.r.Options.Namespace)
}
func (s *session) deleteOwned(ctx context.Context, c *Cluster, av, r, ns string) error {
	items, e := c.resource(av, r, ns).List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + string(s.o.UID)})
	if e != nil {
		return e
	}
	for _, u := range items.Items {
		if e = s.deleteObject(ctx, c, r, &u); e != nil {
			return e
		}
	}
	if len(items.Items) > 0 {
		return pending("waiting for owned %s in %s to be deleted", r, ns)
	}
	return nil
}
func (s *session) deleteNamed(ctx context.Context, c *Cluster, av, r, ns, name string) error {
	u, e := c.get(ctx, av, r, ns, name)
	if errors.IsNotFound(e) {
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
func (s *session) deleteObject(ctx context.Context, c *Cluster, r string, u *unstructured.Unstructured) error {
	if u.GetLabels()[OwnerLabel] != string(s.o.UID) {
		return fmt.Errorf("refusing to delete unowned %s/%s", r, u.GetName())
	}
	uid := u.GetUID()
	policy := metav1.DeletePropagationForeground
	return client.IgnoreNotFound(c.resource(u.GetAPIVersion(), r, u.GetNamespace()).Delete(ctx, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy}))
}
