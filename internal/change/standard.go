package change

import (
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/policy"
)

// StandardEligible reports whether a standard-model change matches a
// pre-approved template and may run immediately with a record: rollback to a
// previously applied release, credential rotation, or scaling within policy.
// The second return value explains the decision for the audit trail.
func StandardEligible(spec api.ChangeRequestSpec, history []api.ChangeRequest, cfg policy.Config) (bool, string) {
	switch spec.Operation {
	case api.OperationCredentialRotate:
		return true, "credential rotation is a pre-approved standard change"
	case api.OperationRollback:
		release, e := DesiredRelease(spec)
		if e != nil {
			return false, e.Error()
		}
		key := TargetKey(spec)
		for i := range history {
			c := &history[i]
			if c.Status.Phase != api.PhaseApplied || c.Status.TargetKey != key {
				continue
			}
			if r, e := DesiredRelease(c.Spec); e == nil && r == release {
				return true, "rollback to release " + release + " applied by " + c.Name
			}
		}
		return false, "release " + release + " was never applied to this target; a rollback to it is not a standard change"
	case api.OperationResourceUpdate:
		d, e := DesiredMap(spec)
		if e != nil {
			return false, e.Error()
		}
		configs, ok := d["resourceTypeEnvironmentConfigs"].(map[string]any)
		if !ok || len(d) != 1 {
			return false, "only environment config scaling is a standard change"
		}
		if e := policy.ScaleWithinLimits(cfg.ScaleCeiling(), configs); e != nil {
			return false, e.Error()
		}
		return true, "scaling within policy limits"
	}
	return false, "operation " + string(spec.Operation) + " is never a standard change"
}
