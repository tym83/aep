package operator

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func Pipeline(o *api.Organization) M {
	paths := []any{}
	for i, e := range o.Spec.Environments {
		targets := []any{}
		if i+1 < len(o.Spec.Environments) {
			targets = append(targets, M{"kind": "Environment", "name": o.Spec.Environments[i+1].Name})
		}
		paths = append(paths, M{"sourceEnvironmentRef": M{"kind": "Environment", "name": e.Name}, "targetEnvironmentRefs": targets})
	}
	return M{"promotionPaths": paths}
}
func (s *session) openChoreo(ctx context.Context) error {
	ns := obj("v1", "Namespace", "", s.o.Name, nil)
	ns.SetLabels(map[string]string{"openchoreo.dev/control-plane": "true"})
	if e := s.cp.apply(ctx, s.o, "namespaces", ns); e != nil {
		return e
	}
	for _, t := range Tiers(s.o) {
		data, e := s.ca(ctx, t)
		if e != nil {
			return e
		}
		dp := obj(ocAPI, "DataPlane", s.o.Name, t, M{"planeID": s.o.Name + "-" + t, "clusterAgent": M{"clientCA": M{"value": string(data["tls.crt"])}}, "secretStoreRef": M{"name": "default"}, "gateway": M{"ingress": M{"external": M{"name": "gateway-default", "namespace": "openchoreo-data-plane", "http": M{"host": s.o.Name + "-" + t + ".apps.local", "port": int64(80)}}}}})
		if e = s.cp.apply(ctx, s.o, "dataplanes", dp); e != nil {
			return e
		}
	}
	for _, e := range s.o.Spec.Environments {
		u := obj(ocAPI, "Environment", s.o.Name, e.Name, M{"dataPlaneRef": M{"kind": "DataPlane", "name": PhysicalTier(s.o, e.Tier)}})
		u.SetAnnotations(map[string]string{"aep.aenix.io/change-model": e.ChangeModel})
		if err := s.cp.apply(ctx, s.o, "environments", u); err != nil {
			return err
		}
	}
	return s.cp.apply(ctx, s.o, "deploymentpipelines", obj(ocAPI, "DeploymentPipeline", s.o.Name, "default", Pipeline(s.o)))
}
func actions(resources, verbs []string) []string {
	out := []string{}
	for _, r := range resources {
		for _, v := range verbs {
			out = append(out, r+":"+v)
		}
	}
	return out
}

// Restrictions belong on RoleMapping, not AuthzRole (OpenChoreo's schema).
func RenderAuthz(o *api.Organization) []*unstructured.Unstructured {
	reads := actions([]string{"project", "component", "resource", "releasebinding", "resourcereleasebinding", "projectreleasebinding", "endpointreleasebinding", "environment", "dataplane", "deploymentpipeline", "observabilityplane", "componenttype", "trait", "resourcetype", "workflow", "workflowrun", "authzrole", "authzrolebinding"}, []string{"view"})
	writes := actions([]string{"releasebinding", "resourcereleasebinding", "projectreleasebinding", "endpointreleasebinding"}, []string{"create", "update", "delete"})
	platformWrites := actions([]string{"environment", "dataplane", "deploymentpipeline", "observabilityplane", "componenttype", "trait", "resourcetype", "workflow"}, []string{"create", "update", "delete"})
	envs := []string{}
	for _, e := range o.Spec.Environments {
		if e.Tier == "nonprod" {
			envs = append(envs, o.Name+"/"+e.Name)
		}
	}
	encoded, _ := json.Marshal(envs)
	expr := "resource.environment in " + string(encoded)
	out := []*unstructured.Unstructured{obj(ocAPI, "AuthzRole", o.Name, "aep-platform-protected", M{"actions": stringsAny(platformWrites)})}
	for _, role := range roles {
		grants := append([]string(nil), reads...)
		conditions := []any{}
		if role == "developer" || role == "platform" {
			grants = append(grants, actions([]string{"project", "component", "resource"}, []string{"create", "update"})...)
			grants = append(grants, writes...)
			runtime := []string{"component:exec", "logs:view"}
			grants = append(grants, runtime...)
			restricted := append(append([]string(nil), writes...), runtime...)
			conditions = append(conditions, M{"actions": stringsAny(restricted), "expression": expr})
		}
		if role == "platform" {
			grants = append(grants, actions([]string{"secretreference", "workflowrun"}, []string{"create", "update", "delete"})...)
		}
		if role == "admin" {
			grants = append(grants, actions([]string{"authzrole", "authzrolebinding"}, []string{"create", "update", "delete"})...)
		}
		out = append(out, obj(ocAPI, "AuthzRole", o.Name, "aep-"+role, M{"actions": stringsAny(grants)}))
		mapping := M{"roleRef": M{"kind": "AuthzRole", "name": "aep-" + role}}
		if len(conditions) > 0 {
			mapping["conditions"] = conditions
		}
		entitlement := M{"claim": "groups", "value": "aep-" + o.Name + "-" + role}
		out = append(out, obj(ocAPI, "AuthzRoleBinding", o.Name, "aep-"+role, M{"entitlement": entitlement, "effect": "allow", "roleMappings": []any{mapping}}), obj(ocAPI, "AuthzRoleBinding", o.Name, "aep-"+role+"-deny-platform", M{"entitlement": entitlement, "effect": "deny", "roleMappings": []any{M{"roleRef": M{"kind": "AuthzRole", "name": "aep-platform-protected"}}}}))
	}
	out = append(out, obj(ocAPI, "AuthzRoleBinding", o.Name, "aep-workload-publisher", M{
		"entitlement": M{"claim": "client_id", "value": "aep-" + o.Name + "-publisher"},
		"effect":      "allow", "roleMappings": []any{M{"roleRef": M{"kind": "ClusterAuthzRole", "name": "workload-publisher"}}},
	}))
	return out
}
