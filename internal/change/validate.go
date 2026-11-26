package change

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// DesiredMap decodes the operation payload; a missing payload is an empty map.
func DesiredMap(spec api.ChangeRequestSpec) (map[string]any, error) {
	if spec.Desired == nil {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if e := json.Unmarshal(spec.Desired.Raw, &out); e != nil {
		return nil, fmt.Errorf("desired must be a JSON object: %w", e)
	}
	return out, nil
}

// DesiredRelease returns the release a promote or rollback pins.
func DesiredRelease(spec api.ChangeRequestSpec) (string, error) {
	d, e := DesiredMap(spec)
	if e != nil {
		return "", e
	}
	s, _ := d["releaseName"].(string)
	if s == "" {
		return "", fmt.Errorf("desired.releaseName is required")
	}
	return s, nil
}

type opRule struct {
	kinds         []string
	project       bool // project required
	environment   bool // environment required
	desired       bool // desired payload required
	self          bool // target must be the organization itself
	validateExtra func(spec api.ChangeRequestSpec, d map[string]any, now time.Time) error
}

var opRules = map[api.ChangeOperation]opRule{
	api.OperationPromote:             {kinds: []string{"Component", "Resource", "Project"}, project: true, environment: true, desired: true, validateExtra: requireRelease},
	api.OperationRollback:            {kinds: []string{"Component", "Resource", "Project"}, project: true, environment: true, desired: true, validateExtra: requireRelease},
	api.OperationResourceUpdate:      {kinds: []string{"Resource"}, project: true, environment: true, desired: true, validateExtra: requireResourceUpdate},
	api.OperationDecommission:        {kinds: []string{"Component", "Resource"}, environment: true, validateExtra: validateDecommission},
	api.OperationResourceRestore:     {kinds: []string{"Resource"}, project: true, environment: true, desired: true},
	api.OperationCredentialRotate:    {kinds: []string{"Resource"}, project: true, environment: true},
	api.OperationBreakglass:          {kinds: []string{"Organization"}, self: true, validateExtra: validateBreakglass},
	api.OperationOrganizationChange:  {kinds: []string{"Organization"}, self: true, desired: true, validateExtra: requireOrgChange},
	api.OperationPolicyException:     {kinds: []string{"Organization"}, self: true, desired: true, validateExtra: requirePolicyException},
	api.OperationClusterDetach:       {kinds: []string{"Cluster"}, environment: true, desired: true, validateExtra: requireClusterDetach},
	api.OperationClusterAttach:       {kinds: []string{"Cluster"}, environment: true, desired: true, validateExtra: requireClusterAttach},
	api.OperationMachineApprove:      {kinds: []string{"Machine"}},
	api.OperationMachineDecommission: {kinds: []string{"Machine"}},
}

func validateDecommission(spec api.ChangeRequestSpec, d map[string]any, _ time.Time) error {
	for k, v := range d {
		if k != "deleteData" {
			return fmt.Errorf("desired.%s is not allowed for decommission", k)
		}
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("desired.deleteData must be a boolean")
		}
		if spec.Target.Kind != "Resource" {
			return fmt.Errorf("desired.deleteData is only supported for Resource targets")
		}
	}
	if d["deleteData"] == true && spec.Model != api.ModelNormal {
		return fmt.Errorf("decommission with deleteData requires the normal model and human approval")
	}
	return nil
}

func requireRelease(_ api.ChangeRequestSpec, d map[string]any, _ time.Time) error {
	s, _ := d["releaseName"].(string)
	if s == "" {
		return fmt.Errorf("desired.releaseName is required")
	}
	for k := range d {
		if k != "releaseName" {
			return fmt.Errorf("desired.%s is not allowed for promote/rollback", k)
		}
	}
	return nil
}

func requireResourceUpdate(_ api.ChangeRequestSpec, d map[string]any, _ time.Time) error {
	allowed := []string{"resourceRelease", "resourceTypeEnvironmentConfigs", "retainPolicy"}
	for k := range d {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("desired.%s is not allowed for resource-update", k)
		}
	}
	if len(d) == 0 {
		return fmt.Errorf("desired must change at least one of %v", allowed)
	}
	if v, ok := d["resourceRelease"]; ok {
		if s, ok := v.(string); !ok || s == "" {
			return fmt.Errorf("desired.resourceRelease must be a nonempty string")
		}
	}
	if v, ok := d["resourceTypeEnvironmentConfigs"]; ok {
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("desired.resourceTypeEnvironmentConfigs must be an object")
		}
	}
	if v, ok := d["retainPolicy"]; ok {
		if s, ok := v.(string); !ok || (s != "Retain" && s != "Delete") {
			return fmt.Errorf("desired.retainPolicy must be Retain or Delete")
		}
	}
	return nil
}

func validateBreakglass(spec api.ChangeRequestSpec, d map[string]any, _ time.Time) error {
	for k := range d {
		if k != "duration" && k != "scope" {
			return fmt.Errorf("desired.%s is not allowed for breakglass", k)
		}
	}
	if v, ok := d["duration"]; ok {
		s, ok := v.(string)
		dur, e := time.ParseDuration(s)
		if !ok || e != nil || dur <= 0 || dur > 4*time.Hour {
			return fmt.Errorf("desired.duration must be a positive duration of at most 4h")
		}
	}
	return nil
}

var orgChangeKeys = []string{"access", "quotas", "profile", "displayName"}

func requireOrgChange(_ api.ChangeRequestSpec, d map[string]any, _ time.Time) error {
	if len(d) == 0 {
		return fmt.Errorf("desired must change at least one of %v", orgChangeKeys)
	}
	for k := range d {
		if !slices.Contains(orgChangeKeys, k) {
			return fmt.Errorf("desired.%s is not an organization setting", k)
		}
	}
	return nil
}

func requirePolicyException(_ api.ChangeRequestSpec, d map[string]any, now time.Time) error {
	if s, _ := d["policy"].(string); s == "" {
		return fmt.Errorf("desired.policy is required")
	}
	if s, _ := d["owner"].(string); s == "" {
		return fmt.Errorf("desired.owner is required: every exception has an owner")
	}
	expires, _ := d["expires"].(string)
	t, e := time.Parse(time.RFC3339, expires)
	if e != nil || !t.After(now) {
		return fmt.Errorf("desired.expires must be a future RFC 3339 timestamp: exceptions always have a deadline")
	}
	return nil
}

// ValidateSpec checks a ChangeRequest spec beyond what the CRD schema can
// express: operation-specific target shape and payload. The HTTP API uses it
// for early feedback and the controller re-checks every reconciled object.
func ValidateSpec(spec api.ChangeRequestSpec, now time.Time) error {
	if len(validation.IsDNS1123Label(spec.Organization)) > 0 {
		return fmt.Errorf("organization must be a DNS-1123 label")
	}
	rule, ok := opRules[spec.Operation]
	if !ok {
		return fmt.Errorf("unsupported operation %q", spec.Operation)
	}
	switch spec.Model {
	case api.ModelStandard, api.ModelNormal, api.ModelEmergency:
	default:
		return fmt.Errorf("unsupported model %q", spec.Model)
	}
	if spec.Requester == "" {
		return fmt.Errorf("requester is required; the API fills it from the caller token")
	}
	if spec.Justification == "" {
		return fmt.Errorf("justification is required")
	}
	if spec.Window != nil && !spec.Window.Start.Before(&spec.Window.End) {
		return fmt.Errorf("window start must be before window end")
	}
	t := spec.Target
	if !slices.Contains(rule.kinds, t.Kind) {
		return fmt.Errorf("operation %s does not accept target kind %s", spec.Operation, t.Kind)
	}
	if rule.self && t.Name != spec.Organization {
		return fmt.Errorf("target must be the organization %q itself", spec.Organization)
	}
	if rule.project && t.Kind != "Project" && t.Project == "" {
		return fmt.Errorf("target.project is required for %s targets", t.Kind)
	}
	if t.Kind == "Project" && t.Project != "" && t.Project != t.Name {
		return fmt.Errorf("target.project must be empty or equal to the project name")
	}
	if rule.environment && t.Environment == "" {
		return fmt.Errorf("target.environment is required for %s", spec.Operation)
	}
	for _, s := range []string{t.Name, t.Project, t.Environment} {
		if s != "" && len(validation.IsDNS1123Label(s)) > 0 {
			return fmt.Errorf("target names must be DNS-1123 labels, got %q", s)
		}
	}
	if IsBindingOp(spec.Operation) {
		if name := BindingName(t); len(name) > 63 || len(validation.IsDNS1123Subdomain(name)) > 0 {
			return fmt.Errorf("binding name %q must be a DNS-1123 subdomain of at most 63 characters", name)
		}
	}
	d, e := DesiredMap(spec)
	if e != nil {
		return e
	}
	if rule.desired && len(d) == 0 {
		return fmt.Errorf("desired payload is required for %s", spec.Operation)
	}
	if rule.validateExtra != nil {
		if e := rule.validateExtra(spec, d, now); e != nil {
			return e
		}
	}
	return nil
}
