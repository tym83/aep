package operator

import (
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"regexp"
	"slices"
)

var roles = []string{"developer", "platform", "approver", "admin", "auditor"}

func Tiers(o *api.Organization) []string {
	if o.Spec.Profile == "small" {
		return []string{"nonprod"}
	}
	return []string{"nonprod", "prod"}
}
func TenantNamespace(org, tier string) string {
	if tier == "" {
		return "tenant-" + org
	}
	return "tenant-" + org + "-" + tier
}
func Validate(o *api.Organization) error {
	if o.Spec.Policy.Mode != "" && o.Spec.Policy.Mode != "audit" && o.Spec.Policy.Mode != "enforce" {
		return fmt.Errorf("policy mode must be audit or enforce")
	}
	for _, registry := range o.Spec.Policy.Registries {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*(:[0-9]+)?$`).MatchString(registry) {
			return fmt.Errorf("registries must be exact registry hosts with optional ports")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]{1,32}$`).MatchString(o.Name) || slices.Contains([]string{"aep", "root", "cozy", "system"}, o.Name) {
		return fmt.Errorf("organization name must be 1-32 lowercase alphanumeric characters and not reserved")
	}
	if o.Spec.Profile == "regulated" {
		return fmt.Errorf("regulated profile is not implemented")
	}
	if o.Spec.Profile != "standard" && o.Spec.Profile != "small" {
		return fmt.Errorf("unknown profile")
	}
	if len(o.Spec.Environments) == 0 || o.Spec.Environments[0].Tier != "nonprod" {
		return fmt.Errorf("pipeline root must be a nonprod environment")
	}
	seen := map[string]bool{}
	for _, e := range o.Spec.Environments {
		if len(validation.IsDNS1123Label(e.Name)) > 0 || seen[e.Name] {
			return fmt.Errorf("invalid or duplicate environment %q", e.Name)
		}
		seen[e.Name] = true
		if !slices.Contains([]string{"nonprod", "prod"}, e.Tier) {
			return fmt.Errorf("environment %s references unavailable tier", e.Name)
		}
		if e.ChangeModel != "none" && e.ChangeModel != "required" {
			return fmt.Errorf("invalid changeModel")
		}
		if e.Tier == "prod" && e.ChangeModel != "required" {
			return fmt.Errorf("prod requires changeModel required")
		}
	}
	for _, t := range Tiers(o) {
		s, ok := o.Spec.Tiers[t]
		if !ok {
			return fmt.Errorf("missing tier %s", t)
		}
		n, ok := s.Cluster.NodeGroups["md0"]
		if !ok || len(s.Cluster.NodeGroups) != 1 || n.MinReplicas < 1 || n.MaxReplicas < n.MinReplicas || n.InstanceType == "" || s.Cluster.Version == "" {
			return fmt.Errorf("invalid cluster configuration for %s", t)
		}
		if s.Services.BackupPolicy != "" && len(validation.IsDNS1123Subdomain(s.Services.BackupPolicy)) > 0 {
			return fmt.Errorf("invalid backupPolicy reference")
		}
		if len(s.Services.AllowedKinds) == 0 {
			return fmt.Errorf("tier %s requires at least one allowed service kind", t)
		}
		if s.Services.MaxPreset != "" && !regexp.MustCompile(`^(t1|c1|s1|u1|m1)\.(nano|micro|small|medium|large|xlarge|2xlarge|4xlarge)$`).MatchString(s.Services.MaxPreset) {
			return fmt.Errorf("invalid maxPreset for %s", t)
		}
		for _, k := range s.Services.AllowedKinds {
			if _, ok := kinds.Table[k]; !ok {
				return fmt.Errorf("bridge does not support kind %s", k)
			}
		}
	}
	for k := range o.Spec.Access {
		if !slices.Contains(roles, k) {
			return fmt.Errorf("unknown access role %s", k)
		}
	}
	_, _, err := QuotaAllocation(o.Spec.Quotas, len(Tiers(o)))
	return err
}

// Quotas are the total user budget. Remainders go to the first tier, preserving the total.
func QuotaAllocation(q api.Quotas, count int) ([]api.Quotas, api.Quotas, error) {
	if count < 1 {
		return nil, nil, fmt.Errorf("positive tier count required")
	}
	out := make([]api.Quotas, count)
	for i := range out {
		out[i] = api.Quotas{}
	}
	parent := api.Quotas{}
	for _, key := range []string{"cpu", "memory"} {
		if q[key] == "" {
			return nil, nil, fmt.Errorf("quota %s required", key)
		}
	}
	for k, v := range q {
		if !slices.Contains([]string{"cpu", "memory", "storage", "gpu"}, k) {
			return nil, nil, fmt.Errorf("unknown quota %s", k)
		}
		n, err := resource.ParseQuantity(v)
		if err != nil || n.Sign() < 0 {
			return nil, nil, fmt.Errorf("invalid quota %s", k)
		}
		scale := int64(1)
		if k == "cpu" {
			scale = 1000
		}
		total := n.Value()
		if scale == 1000 {
			total = n.MilliValue()
		}
		sum := resource.NewQuantity(0, resource.DecimalSI)
		for i := range out {
			value := total / int64(count)
			if i == 0 {
				value += total % int64(count)
			}
			part := resource.NewQuantity(value, n.Format)
			if scale == 1000 {
				part = resource.NewMilliQuantity(value, n.Format)
			}
			if k == "cpu" {
				part.Add(resource.MustParse("20"))
			}
			if k == "memory" {
				part.Add(resource.MustParse("8Gi"))
			}
			out[i][k] = part.String()
			sum.Add(*part)
		}
		parent[k] = sum.String()
	}
	return out, parent, nil
}

func PhysicalTier(o *api.Organization, tier string) string {
	if o.Spec.Profile == "small" {
		return "nonprod"
	}
	return tier
}
