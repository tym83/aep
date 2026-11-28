package operator

import (
	"context"
	"fmt"
	"github.com/robfig/cron/v3"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"regexp"
	"slices"
)

const backupsAPI = "backups.cozystack.io/v1alpha1"
const strategyAPI = "strategy.backups.cozystack.io/v1alpha1"

// Named policies are administrator-owned ConfigMaps in the operator namespace.
// Per-tier classes isolate strategy/retention changes from other tenants.
func (s *session) backupPolicy(ctx context.Context, tier string) (*policy.BackupConfig, error) {
	name := s.o.Spec.Tiers[tier].Services.BackupPolicy
	if name == "" {
		return nil, nil
	}
	cm, e := s.r.Management.Kube.CoreV1().ConfigMaps(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return nil, e
	}
	schedule, source, retention := cm.Data["schedule"], cm.Data["backupClassName"], cm.Data["retentionPolicy"]
	_, scheduleErr := cron.ParseStandard(schedule)
	if scheduleErr != nil || source == "" || !regexp.MustCompile(`^[1-9][0-9]*d$`).MatchString(retention) {
		return nil, fmt.Errorf("backup policy %s requires five-field schedule, backupClassName and retentionPolicy (Nd)", name)
	}
	class, e := s.r.Management.get(ctx, backupsAPI, "backupclasses", "", source)
	if e != nil {
		return nil, e
	}
	spec, _, _ := unstructured.NestedMap(class.Object, "spec")
	strategies, _, _ := unstructured.NestedSlice(spec, "strategies")
	seen := map[string]bool{}
	className := "aep-" + s.o.Name + "-" + tier
	filtered := []any{}
	for _, v := range strategies {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid BackupClass strategy")
		}
		kind, _, _ := unstructured.NestedString(m, "application", "kind")
		if !slices.Contains(s.o.Spec.Tiers[tier].Services.AllowedKinds, kind) {
			continue
		}
		seen[kind] = true
		if kind == "Postgres" {
			ref, _, _ := unstructured.NestedString(m, "strategyRef", "name")
			driver, _, _ := unstructured.NestedString(m, "strategyRef", "kind")
			if driver != "CNPG" {
				return nil, fmt.Errorf("Postgres requires the CNPG backup driver")
			}
			original, e := s.r.Management.get(ctx, strategyAPI, "cnpgs", "", ref)
			if e != nil {
				return nil, e
			}
			driverSpec, _, _ := unstructured.NestedMap(original.Object, "spec")
			if e = unstructured.SetNestedField(driverSpec, retention, "template", "barmanObjectStore", "retentionPolicy"); e != nil {
				return nil, e
			}
			u := obj(strategyAPI, "CNPG", "", className, driverSpec)
			u.SetAnnotations(original.GetAnnotations())
			if e = s.r.Management.apply(ctx, s.o, "cnpgs", u); e != nil {
				return nil, e
			}
			if e = unstructured.SetNestedField(m, className, "strategyRef", "name"); e != nil {
				return nil, e
			}
		} else if kind == "Redis" && cm.Data["objectStorageRetentionVerified"] != retention {
			return nil, fmt.Errorf("Redis policy requires objectStorageRetentionVerified=%s after configuring bucket lifecycle/Object Lock", retention)
		}
		filtered = append(filtered, m)
	}
	for _, kind := range s.o.Spec.Tiers[tier].Services.AllowedKinds {
		// Kafka runs in the tier cluster; Cozystack backup classes do not cover it.
		if !seen[kind] && !kinds.Table[kind].AppCluster {
			return nil, fmt.Errorf("backup class %s has no strategy for %s", source, kind)
		}
	}
	spec["strategies"] = filtered
	u := obj(backupsAPI, "BackupClass", "", className, spec)
	u.SetAnnotations(map[string]string{"aep.aenix.io/backup-policy": name, "aep.aenix.io/retention-policy": retention})
	if e = s.r.Management.apply(ctx, s.o, "backupclasses", u); e != nil {
		return nil, e
	}
	return &policy.BackupConfig{Schedule: schedule, ClassName: className, RetentionPolicy: retention}, nil
}
