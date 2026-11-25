package change

import (
	"context"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const leaseDuration = 60 * time.Second

// leaseName serializes applies per target binding: one Lease per target key
// in the organization namespace.
func leaseName(cr *api.ChangeRequest) string {
	key := cr.Status.TargetKey
	if key == "" {
		key = TargetKey(cr.Spec)
	}
	if len(key) > 24 {
		key = key[:24]
	}
	return "aep-change-" + key
}

// acquireLease takes or renews the per-binding lease. It returns false when
// another live holder owns it.
func (r *Reconciler) acquireLease(ctx context.Context, cr *api.ChangeRequest) (bool, error) {
	now := r.now()
	key := types.NamespacedName{Namespace: cr.Namespace, Name: leaseName(cr)}
	lease := &coordinationv1.Lease{}
	e := r.Client.Get(ctx, key, lease)
	if errors.IsNotFound(e) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &r.InstanceID,
				LeaseDurationSeconds: ptr(int32(leaseDuration / time.Second)),
				RenewTime:            &metav1.MicroTime{Time: now},
			},
		}
		if e = r.Client.Create(ctx, lease); errors.IsAlreadyExists(e) {
			return false, nil
		}
		return e == nil, e
	}
	if e != nil {
		return false, e
	}
	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	expired := true
	if lease.Spec.RenewTime != nil {
		d := leaseDuration
		if lease.Spec.LeaseDurationSeconds != nil {
			d = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
		}
		expired = lease.Spec.RenewTime.Add(d).Before(now)
	}
	if holder != "" && holder != r.InstanceID && !expired {
		return false, nil
	}
	lease.Spec.HolderIdentity = &r.InstanceID
	lease.Spec.LeaseDurationSeconds = ptr(int32(leaseDuration / time.Second))
	lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
	if e = r.Client.Update(ctx, lease); e != nil {
		if errors.IsConflict(e) {
			return false, nil
		}
		return false, e
	}
	return true, nil
}

// releaseLease drops the lease when this instance holds it; failures are
// harmless because the lease expires on its own.
func (r *Reconciler) releaseLease(ctx context.Context, cr *api.ChangeRequest) {
	key := types.NamespacedName{Namespace: cr.Namespace, Name: leaseName(cr)}
	lease := &coordinationv1.Lease{}
	if e := r.Client.Get(ctx, key, lease); e != nil {
		return
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == r.InstanceID {
		_ = r.Client.Delete(ctx, lease)
	}
}

func ptr[T any](v T) *T { return &v }
