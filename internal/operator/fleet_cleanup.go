package operator

import (
	"context"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"time"
)

const fleetFinalizer = "aep.aenix.io/fleet-cleanup"

func (f *FleetReconciler) profileVersion() (string, error) {
	for _, v := range []string{"multicluster.x-k8s.io/v1alpha2", "multicluster.x-k8s.io/v1alpha1"} {
		resources, err := f.Hub.Kube.Discovery().ServerResourcesForGroupVersion(v)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		for _, r := range resources.APIResources {
			if r.Name == "clusterprofiles" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("hub does not serve ClusterProfile v1alpha1 or v1alpha2")
}

// Quarantine revokes hub acceptance before touching control-plane credentials.
// The Environment remains as a tombstone; its immutable reference cannot migrate.
func (f *FleetReconciler) quarantine(ctx context.Context, ec *api.ExternalCluster, reason string) error {
	fleetCondition(ec, "PoliciesReady", false, "Quarantined", reason)
	fleetCondition(ec, "DataPlaneConnected", false, "Quarantined", reason)
	if err := f.revokeHub(ctx, ec); err != nil {
		return err
	}
	id := FleetID(ec)
	var err error
	o := &api.Organization{}
	if err = f.Get(ctx, client.ObjectKey{Name: ec.Spec.Organization}, o); err != nil {
		return err
	}
	s := &session{r: f.Reconciler, o: o}
	if err = s.controlPlane(ctx); err != nil {
		return err
	}
	// Removing the DataPlane removes the accepted client CA, even while a spoke is offline.
	return client.IgnoreNotFound(s.cp.resource(ocAPI, "dataplanes", o.Name).Delete(ctx, id, metav1.DeleteOptions{}))
}

func (f *FleetReconciler) cleanupBootstrap(ctx context.Context, ec *api.ExternalCluster) error {
	name := "bootstrap-" + FleetID(ec)
	ns := "aep-" + ec.Spec.Organization
	for _, fn := range []func() error{
		func() error { return f.Hub.Kube.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}) },
		func() error {
			return f.Hub.Kube.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
		},
		func() error {
			return f.Hub.Kube.RbacV1().RoleBindings("kube-public").Delete(ctx, name, metav1.DeleteOptions{})
		},
		func() error {
			return f.Hub.Kube.RbacV1().Roles("kube-public").Delete(ctx, name, metav1.DeleteOptions{})
		},
		func() error { return f.Hub.Kube.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{}) },
		func() error { return f.Hub.Kube.CoreV1().ServiceAccounts(ns).Delete(ctx, name, metav1.DeleteOptions{}) },
	} {
		if err := client.IgnoreNotFound(fn()); err != nil {
			return err
		}
	}
	return nil
}

func (f *FleetReconciler) finalizeFleet(ctx context.Context, ec *api.ExternalCluster) (ctrl.Result, error) {
	if !slices.Contains(ec.Finalizers, fleetFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := f.revokeHub(ctx, ec); err != nil {
		return ctrl.Result{}, err
	}
	o := &api.Organization{}
	if err := f.Get(ctx, client.ObjectKey{Name: ec.Spec.Organization}, o); err != nil {
		return ctrl.Result{}, err
	}
	s := &session{r: f.Reconciler, o: o}
	return f.cleanupFleetObjects(ctx, ec, s)
}

func (f *FleetReconciler) cleanupFleetObjects(ctx context.Context, ec *api.ExternalCluster, s *session) (ctrl.Result, error) {
	o := s.o
	id := FleetID(ec)
	// Orphan preserves customer workloads; no workload deletion is implied by detach.
	works, err := f.Hub.resource("work.open-cluster-management.io/v1", "manifestworks", id).List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + string(o.UID)})
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, w := range works.Items {
		if _, err = f.Hub.resource(w.GetAPIVersion(), "manifestworks", id).Patch(ctx, w.GetName(), types.MergePatchType, []byte(`{"spec":{"deleteOption":{"propagationPolicy":"Orphan"}}}`), metav1.PatchOptions{}); err != nil {
			return ctrl.Result{}, err
		}
		if err = s.deleteObject(ctx, f.Hub, "manifestworks", &w); err != nil {
			return ctrl.Result{}, err
		}
	}
	if len(works.Items) > 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err = s.controlPlane(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err = s.deleteNamed(ctx, s.cp, ocAPI, "dataplanes", o.Name, id); err != nil {
		return fleetCleanupResult(err)
	}
	for _, name := range []string{shortName("agent-ca-" + o.Name + "-" + id), "agent-leaf-" + id} {
		if err = s.deleteNamed(ctx, f.Management, "v1", "secrets", f.Options.Namespace, name); err != nil {
			return fleetCleanupResult(err)
		}
	}
	if err = f.cleanupBootstrap(ctx, ec); err != nil {
		return ctrl.Result{}, err
	}
	for _, v := range []string{"multicluster.x-k8s.io/v1alpha2", "multicluster.x-k8s.io/v1alpha1"} {
		if err = s.deleteNamed(ctx, f.Hub, v, "clusterprofiles", "aep-"+o.Name, ec.Spec.ClusterProfileRef.Name); err != nil && !errors.IsNotFound(err) {
			return fleetCleanupResult(err)
		}
	}

	mc, e := f.Hub.get(ctx, "cluster.open-cluster-management.io/v1", "managedclusters", "", id)
	if e != nil && !errors.IsNotFound(e) {
		return ctrl.Result{}, e
	}
	if e == nil {
		uid := mc.GetUID()
		if err = f.Hub.resource(mc.GetAPIVersion(), "managedclusters", "").Delete(ctx, id, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	base := ec.DeepCopy()
	ec.Finalizers = slices.DeleteFunc(ec.Finalizers, func(v string) bool { return v == fleetFinalizer })
	return ctrl.Result{}, f.Patch(ctx, ec, client.MergeFrom(base))
}
func fleetCleanupResult(err error) (ctrl.Result, error) {
	if _, ok := err.(*pendingError); ok {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, err
}

func (f *FleetReconciler) revokeHub(ctx context.Context, ec *api.ExternalCluster) error {
	id := FleetID(ec)
	mc, err := f.Hub.get(ctx, "cluster.open-cluster-management.io/v1", "managedclusters", "", id)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Hub.resource(mc.GetAPIVersion(), "managedclusters", "").Patch(ctx, id, types.MergePatchType, []byte(`{"spec":{"hubAcceptsClient":false}}`), metav1.PatchOptions{})
	if err != nil {
		return err
	}
	return nil
}

// ManagedCluster metadata is writable by the spoke. Its name, derived from the
// management assignment UID, is the identity; its labels confer no authority.
func (f *FleetReconciler) applyManagedCluster(ctx context.Context, o *api.Organization, want *unstructured.Unstructured) error {
	ri := f.Hub.resource(want.GetAPIVersion(), "managedclusters", "")
	old, err := ri.Get(ctx, want.GetName(), metav1.GetOptions{})
	labels := want.GetLabels()
	labels[OwnerLabel] = string(o.UID)
	want.SetLabels(labels)
	if errors.IsNotFound(err) {
		_, err = ri.Create(ctx, want, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	next := old.DeepCopy()
	next.SetLabels(labels)
	next.Object["spec"] = want.Object["spec"]
	if reflect.DeepEqual(old.Object, next.Object) {
		return nil
	}
	_, err = ri.Update(ctx, next, metav1.UpdateOptions{})
	return err
}
