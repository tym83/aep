package operator

import (
	"context"
	"encoding/json"
	"fmt"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"slices"
)

// Once an object is being deleted, the garbage collector and OpenChoreo controllers
// must still release their finalizers; an update that leaves spec untouched can do nothing else.
const finalizerReleaseExemption = "!(request.operation == 'UPDATE' && has(oldObject.metadata.deletionTimestamp) && has(object.spec) == has(oldObject.spec) && (!has(object.spec) || object.spec == oldObject.spec))"

// Delegated role administration is safe only when Kubernetes admission also constrains
// the roles and bindings written by the OpenChoreo API service account.
func (s *session) admission(ctx context.Context) error {
	review, e := s.cp.Kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if e != nil {
		return e
	}
	identity := review.Status.UserInfo.Username
	if identity == "" {
		return fmt.Errorf("cannot establish aep-cp operator identity")
	}
	encoded, _ := json.Marshal(identity)
	restricted := actions([]string{"releasebinding", "resourcereleasebinding", "projectreleasebinding", "endpointreleasebinding"}, []string{"create", "update", "delete"})
	restricted = append(restricted, "component:exec", "logs:view")
	envs := []string{}
	for _, env := range s.o.Spec.Environments {
		if env.Tier == "nonprod" {
			envs = append(envs, s.o.Name+"/"+env.Name)
		}
	}
	envJSON, _ := json.Marshal(envs)
	expression, _ := json.Marshal("resource.environment in " + string(envJSON))
	restrictedJSON, _ := json.Marshal(restricted)
	allowed := []string{}
	for _, u := range RenderAuthz(s.o) {
		if u.GetKind() != "AuthzRole" || u.GetName() == "aep-platform-protected" {
			continue
		}
		list, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "actions")
		allowed = append(allowed, list...)
	}
	slices.Sort(allowed)
	allowed = slices.Compact(allowed)
	allowedJSON, _ := json.Marshal(allowed)
	validations := []any{
		M{"expression": "!(request.resource.resource in ['environments','dataplanes','deploymentpipelines','observabilityplanes','componenttypes','traits','resourcetypes','workflows','workflowplanes'])", "message": "Only the organization operator may modify platform objects."},
		M{"expression": "!(request.resource.resource in ['authzroles','authzrolebindings']) || !(request.name.startsWith('aep-') || (object != null && object.metadata.name.startsWith('aep-')))", "message": "Operator roles and bindings are reserved."},
		M{"expression": "request.operation == 'DELETE' || request.resource.resource != 'authzroles' || object.spec.actions.all(a, a in " + string(allowedJSON) + ")", "message": "Delegated roles may only grant approved organization actions."},
		M{"expression": "request.operation == 'DELETE' || request.resource.resource != 'authzrolebindings' || object.spec.roleMappings.all(m, m.roleRef.kind == 'AuthzRole' && m.roleRef.name != 'aep-platform-protected' && has(m.conditions) && m.conditions.all(c, c.expression == " + string(expression) + " && c.actions.all(a, a in " + string(restrictedJSON) + ")) && " + string(restrictedJSON) + ".all(a, m.conditions.exists(c, a in c.actions)))", "message": "Delegated bindings require the exact nonprod restrictions and namespaced roles."},
		// Until decommission admission can inspect bindings, fail closed on cascading deletion.
		M{"expression": "request.operation != 'DELETE' || !(request.resource.resource in ['projects','components','resources'])", "message": "Cascading deletion requires an operator-reviewed decommission."},
	}

	groups := []struct {
		suffix    string
		resources []string
		checks    []any
	}{
		{"platform", []string{"environments", "dataplanes", "deploymentpipelines", "observabilityplanes", "componenttypes", "traits", "resourcetypes", "workflows", "workflowplanes"}, []any{validations[0]}},
		{"roles", []string{"authzroles"}, []any{validations[1], validations[2]}},
		{"bindings", []string{"authzrolebindings"}, []any{validations[1], validations[3]}},
		{"vcs-secrets", []string{"secretreferences"}, []any{M{"expression": "!(request.name.startsWith('aep-') || (object != null && object.metadata.name.startsWith('aep-')))", "message": "System SecretReferences are managed by the organization operator."}, M{"expression": "request.operation == 'DELETE' || !has(object.spec.data) || object.spec.data.all(d, !has(d.remoteRef) || !d.remoteRef.key.startsWith('vcs-'))", "message": "VCS remote keys are reserved for the organization operator."}}},
		{"decommission", []string{"projects", "components", "resources"}, []any{validations[4]}},
	}
	for _, group := range groups {
		name := "aep-" + s.o.Name + "-" + group.suffix
		policy := obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", "", name, M{"failurePolicy": "Fail", "matchConstraints": M{"resourceRules": []any{M{"apiGroups": []any{"openchoreo.dev"}, "apiVersions": []any{"v1alpha1"}, "operations": []any{"CREATE", "UPDATE", "DELETE"}, "resources": stringsAny(group.resources)}}}, "matchConditions": []any{M{"name": "operator-exemption", "expression": "request.userInfo.username != " + string(encoded)}, M{"name": "finalizer-release", "expression": finalizerReleaseExemption}}, "validations": group.checks})
		if e = s.cp.apply(ctx, s.o, "validatingadmissionpolicies", policy); e != nil {
			return e
		}
		got, e := s.cp.get(ctx, "admissionregistration.k8s.io/v1", "validatingadmissionpolicies", "", name)
		if e != nil {
			return e
		}
		observed, _, _ := unstructured.NestedInt64(got.Object, "status", "observedGeneration")
		if observed < got.GetGeneration() {
			return pending("waiting for authorization admission type checking: %s", name)
		}
		warnings, _, _ := unstructured.NestedSlice(got.Object, "status", "typeChecking", "expressionWarnings")
		if len(warnings) > 0 {
			return fmt.Errorf("authorization admission type checking failed: %v", warnings)
		}
		binding := obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", "", name, M{"policyName": name, "validationActions": []any{"Deny"}, "matchResources": M{"namespaceSelector": M{"matchLabels": M{"kubernetes.io/metadata.name": s.o.Name}}}})
		if e = s.cp.apply(ctx, s.o, "validatingadmissionpolicybindings", binding); e != nil {
			return e
		}
	}
	return nil
}
