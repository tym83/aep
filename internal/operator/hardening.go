package operator

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

const DefaultBootstrapBindingSystemClients = "openchoreo-backstage-client,openchoreo-observer-resource-reader-client,openchoreo-rca-agent,openchoreo-finops-agent"

func bootstrapBindingOffends(u *unstructured.Unstructured, allowed string) bool {
	claim, _, _ := unstructured.NestedString(u.Object, "spec", "entitlement", "claim")
	value, _, _ := unstructured.NestedString(u.Object, "spec", "entitlement", "value")
	if claim == "client_id" {
		for _, client := range strings.Split(allowed, ",") {
			if strings.TrimSpace(client) != "" && value == strings.TrimSpace(client) {
				return false
			}
		}
	}
	return claim == "groups" || (claim == "client_id" && value == "service_mcp_client") || u.GetName() == "workload-publisher-binding"
}

func chartManagedBinding(u *unstructured.Unstructured) bool {
	labels := u.GetLabels()
	for key := range labels {
		if strings.HasPrefix(key, "aep.aenix.io/") {
			return false
		}
	}
	return labels["app.kubernetes.io/managed-by"] == "Helm" &&
		(labels["app.kubernetes.io/instance"] == "openchoreo-control-plane" || labels["release"] == "openchoreo-control-plane")
}

// This installation-wide check intentionally runs independently of tenant
// readiness. A managed ControlPlane hardens its installation itself, so its
// Organizations report its Hardened condition.
func (s *session) hardenControlPlane(ctx context.Context) {
	if t, err := s.target(ctx); err == nil && t.managed() {
		c := meta.FindStatusCondition(t.cp.Status.Conditions, "Hardened")
		if c == nil {
			setCondition(s.o, "ControlPlaneHardened", false, "Pending", "ControlPlane "+t.cp.Name+" has not reported Hardened yet")
			return
		}
		setCondition(s.o, "ControlPlaneHardened", c.Status == metav1.ConditionTrue, c.Reason, "ControlPlane "+t.cp.Name+": "+c.Message)
		return
	}
	if err := s.controlPlane(ctx); err != nil {
		setCondition(s.o, "ControlPlaneHardened", false, "InspectionFailed", err.Error())
		return
	}
	ok, reason, message := hardenBootstrapBindings(ctx, s.cp, s.r.Options.PruneBootstrapBindings, s.r.Options.BootstrapBindingSystemClients, s.r.Recorder, s.o)
	setCondition(s.o, "ControlPlaneHardened", ok, reason, message)
}

// hardenBootstrapBindings prunes chart-managed ClusterAuthzRoleBindings that
// grant more than the allowed system clients when prune is set, and reports
// whether none remain. Events about deletions are recorded on subject.
func hardenBootstrapBindings(ctx context.Context, c *Cluster, prune bool, allowed string, recorder record.EventRecorder, subject runtime.Object) (bool, string, string) {
	ri := c.Dynamic.Resource(gvr(ocAPI, "clusterauthzrolebindings"))
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "InspectionFailed", err.Error()
	}
	var failures []string
	for i := range list.Items {
		u := &list.Items[i]
		if !bootstrapBindingOffends(u, allowed) || !prune || !chartManagedBinding(u) {
			continue
		}
		// Both preconditions prevent deleting a replacement or an object whose ownership changed.
		uid, version := u.GetUID(), u.GetResourceVersion()
		err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", u.GetName(), err))
		} else if recorder != nil {
			recorder.Eventf(subject, "Normal", "BootstrapBindingPruned", "Deleted ClusterAuthzRoleBinding %s from the control-plane installation", u.GetName())
		}
	}
	// Re-list so terminating objects and concurrent chart recreation cannot yield a false success.
	list, err = ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "InspectionFailed", err.Error()
	}
	var offenders []string
	for i := range list.Items {
		if bootstrapBindingOffends(&list.Items[i], allowed) {
			offenders = append(offenders, list.Items[i].GetName())
		}
	}
	slices.Sort(offenders)
	if len(offenders) > 0 || len(failures) > 0 {
		message := "Offending ClusterAuthzRoleBindings: " + strings.Join(offenders, ", ")
		if len(failures) > 0 {
			message += "; deletion errors: " + strings.Join(failures, "; ")
		}
		return false, "BootstrapBindingsPresent", message
	}
	return true, "Verified", "No offending ClusterAuthzRoleBindings"
}
