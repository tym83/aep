// Package change implements the AEP change service: validation, separation
// of duties, the ChangeRequest state machine and the operation handlers that
// converge OpenChoreo bindings after approval.
package change

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/ocapi"
)

// BindingKind maps a target kind to the OpenChoreo binding kind that carries
// the release pin for it.
func BindingKind(targetKind string) (string, error) {
	switch targetKind {
	case "Component":
		return ocapi.KindReleaseBinding, nil
	case "Resource":
		return ocapi.KindResourceReleaseBinding, nil
	case "Project":
		return ocapi.KindProjectReleaseBinding, nil
	}
	return "", fmt.Errorf("target kind %q has no release binding", targetKind)
}

// BindingName is the deterministic name of the binding of one target in one
// environment; the change service is the only writer of protected bindings,
// so the name is derived, never discovered.
func BindingName(t api.ChangeTarget) string {
	return t.Name + "-" + t.Environment
}

// Owner returns the spec.owner fields identifying the target's binding.
func Owner(t api.ChangeTarget) map[string]any {
	switch t.Kind {
	case "Component":
		return map[string]any{"projectName": t.Project, "componentName": t.Name}
	case "Resource":
		return map[string]any{"projectName": t.Project, "resourceName": t.Name}
	case "Project":
		return map[string]any{"projectName": t.Name}
	}
	return nil
}

// IsBindingOp reports whether the operation converges a release binding.
func IsBindingOp(op api.ChangeOperation) bool {
	switch op {
	case api.OperationPromote, api.OperationRollback, api.OperationResourceUpdate, api.OperationDecommission:
		return true
	}
	return false
}

// TargetKey is the deterministic serialization key: one active ChangeRequest
// per target binding, a newer one supersedes an older one.
func TargetKey(spec api.ChangeRequestSpec) string {
	id := spec.Organization + "|" + spec.Target.Kind + "|" + spec.Target.Name + "|" + spec.Target.Environment
	if spec.Operation == api.OperationClusterAttach || spec.Operation == api.OperationClusterDetach {
		id = spec.Organization + "|fleet-environment|" + spec.Target.Environment
	}
	if IsBindingOp(spec.Operation) {
		kind, e := BindingKind(spec.Target.Kind)
		if e == nil {
			id = spec.Organization + "|" + kind + "|" + BindingName(spec.Target)
		}
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:32]
}
