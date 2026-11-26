package operator

import (
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sort"
	"strings"
)

// Resolve custom kinds from the same CRDs being delivered, never from a spoke.
// Built-ins are explicit so a new kind cannot silently receive wildcard rights.
func fleetResources(manifests []any) map[string]string {
	resources := map[string]string{
		"v1/Namespace": "namespaces", "v1/Secret": "secrets", "v1/ConfigMap": "configmaps", "v1/ServiceAccount": "serviceaccounts", "v1/Service": "services", "v1/Pod": "pods", "v1/PersistentVolumeClaim": "persistentvolumeclaims",
		"apps/v1/Deployment": "deployments", "apps/v1/StatefulSet": "statefulsets", "apps/v1/DaemonSet": "daemonsets",
		"batch/v1/Job": "jobs", "batch/v1/CronJob": "cronjobs",
		"rbac.authorization.k8s.io/v1/ClusterRole": "clusterroles", "rbac.authorization.k8s.io/v1/ClusterRoleBinding": "clusterrolebindings", "rbac.authorization.k8s.io/v1/Role": "roles", "rbac.authorization.k8s.io/v1/RoleBinding": "rolebindings",
		"apiextensions.k8s.io/v1/CustomResourceDefinition":          "customresourcedefinitions",
		"admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy": "validatingadmissionpolicies", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding": "validatingadmissionpolicybindings",
		"admissionregistration.k8s.io/v1/ValidatingWebhookConfiguration": "validatingwebhookconfigurations", "admissionregistration.k8s.io/v1/MutatingWebhookConfiguration": "mutatingwebhookconfigurations",
		"networking.k8s.io/v1/NetworkPolicy": "networkpolicies", "networking.k8s.io/v1/Ingress": "ingresses", "policy/v1/PodDisruptionBudget": "poddisruptionbudgets",
		"cert-manager.io/v1/Certificate": "certificates", "cert-manager.io/v1/Issuer": "issuers",
		"gateway.networking.k8s.io/v1/Gateway": "gateways", "gateway.networking.k8s.io/v1/GatewayClass": "gatewayclasses", "gateway.networking.k8s.io/v1/HTTPRoute": "httproutes",
		"scheduling.k8s.io/v1/PriorityClass": "priorityclasses",
	}
	for _, raw := range manifests {
		m := raw.(map[string]any)
		if m["kind"] != "CustomResourceDefinition" {
			continue
		}
		group, _, _ := unstructured.NestedString(m, "spec", "group")
		kind, _, _ := unstructured.NestedString(m, "spec", "names", "kind")
		plural, _, _ := unstructured.NestedString(m, "spec", "names", "plural")
		versions, _, _ := unstructured.NestedSlice(m, "spec", "versions")
		for _, v := range versions {
			version, _ := v.(map[string]any)["name"].(string)
			resources[group+"/"+version+"/"+kind] = plural
		}
	}
	return resources
}
func fleetWorkPermissions(manifests []any) (*unstructured.Unstructured, error) {
	resources := fleetResources(manifests)
	groups := map[string]map[string]bool{"rbac.authorization.k8s.io": {"clusterroles": true}}
	for _, raw := range manifests {
		m := raw.(map[string]any)
		av, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		resource := resources[av+"/"+kind]
		if resource == "" {
			return nil, fmt.Errorf("no work permission mapping for %s/%s", av, kind)
		}
		group := ""
		if strings.Contains(av, "/") {
			group = strings.SplitN(av, "/", 2)[0]
		}
		if groups[group] == nil {
			groups[group] = map[string]bool{}
		}
		groups[group][resource] = true
	}
	keys := []string{}
	for g := range groups {
		keys = append(keys, g)
	}
	sort.Strings(keys)
	rules := []any{}
	for _, g := range keys {
		rs := []string{}
		for r := range groups[g] {
			rs = append(rs, r)
		}
		sort.Strings(rs)
		rules = append(rules, rule(g, rs, []string{"get", "list", "watch", "create", "update", "patch", "delete"}))
	}
	u := obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "aep-fleet-work", nil)
	u.SetLabels(map[string]string{"open-cluster-management.io/aggregate-to-work": "true"})
	u.Object["rules"] = rules
	return u, nil
}

func fleetFeedback(batch []any) []any {
	configs := []any{}
	for _, raw := range batch {
		m := raw.(map[string]any)
		av, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		var resource string
		var feedback M
		switch {
		case av == "apps/v1" && kind == "Deployment":
			resource = "deployments"
			feedback = M{"type": "WellKnownStatus"}
		case strings.HasPrefix(av, "kyverno.io/") && kind == "ClusterPolicy":
			resource = "clusterpolicies"
			feedback = M{"type": "JSONPaths", "jsonPaths": []any{M{"name": "Ready", "path": ".status.conditions[?(@.type==\"Ready\")].status"}}}
		default:
			continue
		}
		group := strings.SplitN(av, "/", 2)[0]
		name, _, _ := unstructured.NestedString(m, "metadata", "name")
		ns, _, _ := unstructured.NestedString(m, "metadata", "namespace")
		rules := []any{feedback}
		if resource == "deployments" {
			rules = append(rules, M{"type": "JSONPaths", "jsonPaths": []any{M{"name": "Generation", "path": ".metadata.generation"}, M{"name": "ObservedGeneration", "path": ".status.observedGeneration"}, M{"name": "DesiredReplicas", "path": ".spec.replicas"}}})
		}
		configs = append(configs, M{"resourceIdentifier": M{"group": group, "resource": resource, "name": name, "namespace": ns}, "feedbackRules": rules})
	}
	return configs
}
func fleetFeedbackReady(work *unstructured.Unstructured, configs []any) bool {
	statuses, _, _ := unstructured.NestedSlice(work.Object, "status", "resourceStatus", "manifests")
	for _, raw := range configs {
		cfg := raw.(M)
		id := cfg["resourceIdentifier"].(M)
		found := false
		for _, raw := range statuses {
			status, _ := raw.(map[string]any)
			rid, _ := status["resourceMeta"].(map[string]any)
			if rid["group"] != id["group"] || rid["resource"] != id["resource"] || rid["name"] != id["name"] {
				continue
			}
			ns, _ := rid["namespace"].(string)
			if ns != id["namespace"] {
				continue
			}
			values, _, _ := unstructured.NestedSlice(status, "statusFeedback", "values")

			nums := map[string]int64{}
			ready := false
			for _, v := range values {
				value := v.(map[string]any)
				name, _ := value["name"].(string)
				field, _ := value["fieldValue"].(map[string]any)
				if name == "Ready" && field["string"] == "True" {
					ready = true
				}
				if n, ok := field["integer"].(int64); ok {
					nums[name] = n
				}
			}
			if id["resource"] == "clusterpolicies" {
				found = ready
			} else {
				found = nums["Generation"] > 0 && nums["ObservedGeneration"] >= nums["Generation"] && nums["DesiredReplicas"] > 0 && nums["AvailableReplicas"] >= nums["DesiredReplicas"] && nums["ReadyReplicas"] >= nums["DesiredReplicas"]
			}

		}
		if !found {
			return false
		}
	}
	return true
}
