package bridge

import (
	"context"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"reflect"
)

var Plans = schema.GroupVersionResource{Group: "backups.cozystack.io", Version: "v1alpha1", Resource: "plans"}

func (r *Reconciler) backupPlan(ctx context.Context, c *api.ServiceClaim, app *unstructured.Unstructured) error {
	if r.Policy.BackupPolicy == "" {
		return nil
	} // Never delete existing plans on policy removal.
	p := r.Policy.Backup
	if p == nil || p.ClassName == "" || p.Schedule == "" {
		return fmt.Errorf("backup policy is unresolved")
	}
	ri := r.Dynamic.Resource(Plans).Namespace(r.TenantNamespace)
	name := app.GetName()
	old, e := ri.Get(ctx, name, metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return e
	}
	spec := map[string]any{"applicationRef": map[string]any{"apiGroup": "apps.cozystack.io", "kind": c.Spec.Kind, "name": name}, "backupClassName": p.ClassName, "schedule": map[string]any{"type": "cron", "cron": p.Schedule}}
	if e == nil {
		if old.GetAnnotations()[Domain+"claim-uid"] != string(c.UID) {
			return fmt.Errorf("backup Plan name belongs to another claim")
		}
		if reflect.DeepEqual(old.Object["spec"], spec) {
			return nil
		}
		old.Object["spec"] = spec
		_, e = ri.Update(ctx, old, metav1.UpdateOptions{})
		return e
	}
	plan := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "backups.cozystack.io/v1alpha1", "kind": "Plan", "metadata": map[string]any{"name": name, "namespace": r.TenantNamespace}, "spec": spec}}
	plan.SetLabels(app.GetLabels())
	plan.SetAnnotations(map[string]string{Domain + "claim-uid": string(c.UID), Domain + "backup-policy": r.Policy.BackupPolicy})
	_, e = ri.Create(ctx, plan, metav1.CreateOptions{})
	return e
}
