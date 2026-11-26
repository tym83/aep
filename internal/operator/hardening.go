package operator

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// This installation-wide check intentionally runs independently of tenant readiness.
func (s *session) hardenControlPlane(ctx context.Context) {
	if err := s.controlPlane(ctx); err != nil {
		setCondition(s.o, "ControlPlaneHardened", false, "InspectionFailed", err.Error())
		return
	}
	ri := s.cp.Dynamic.Resource(gvr(ocAPI, "clusterauthzrolebindings"))
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		setCondition(s.o, "ControlPlaneHardened", false, "InspectionFailed", err.Error())
		return
	}
	var failures []string
	for i := range list.Items {
		u := &list.Items[i]
		if !bootstrapBindingOffends(u, s.r.Options.BootstrapBindingSystemClients) || !s.r.Options.PruneBootstrapBindings || !chartManagedBinding(u) {
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
		} else if s.r.Recorder != nil {
			s.r.Recorder.Eventf(s.o, "Normal", "BootstrapBindingPruned", "Deleted ClusterAuthzRoleBinding %s from the control-plane installation", u.GetName())
		}
	}
	// Re-list so terminating objects and concurrent chart recreation cannot yield a false success.
	list, err = ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		setCondition(s.o, "ControlPlaneHardened", false, "InspectionFailed", err.Error())
		return
	}
	var offenders []string
	for i := range list.Items {
		if bootstrapBindingOffends(&list.Items[i], s.r.Options.BootstrapBindingSystemClients) {
			offenders = append(offenders, list.Items[i].GetName())
		}
	}
	slices.Sort(offenders)
	if len(offenders) > 0 || len(failures) > 0 {
		message := "Offending ClusterAuthzRoleBindings: " + strings.Join(offenders, ", ")
		if len(failures) > 0 {
			message += "; deletion errors: " + strings.Join(failures, "; ")
		}
		setCondition(s.o, "ControlPlaneHardened", false, "BootstrapBindingsPresent", message)
		return
	}
	setCondition(s.o, "ControlPlaneHardened", true, "Verified", "No offending ClusterAuthzRoleBindings")
}
