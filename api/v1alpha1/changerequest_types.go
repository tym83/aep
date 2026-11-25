package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ChangeOperation is the typed operation a ChangeRequest asks for. Every
// operation has its own desired-payload schema and handler.
// +kubebuilder:validation:Enum=promote;rollback;resource-update;resource-restore;credential-rotate;policy-exception;breakglass;organization-change;cluster-attach;machine-approve;machine-decommission;decommission
type ChangeOperation string

const (
	OperationPromote             ChangeOperation = "promote"
	OperationRollback            ChangeOperation = "rollback"
	OperationResourceUpdate      ChangeOperation = "resource-update"
	OperationResourceRestore     ChangeOperation = "resource-restore"
	OperationCredentialRotate    ChangeOperation = "credential-rotate"
	OperationPolicyException     ChangeOperation = "policy-exception"
	OperationBreakglass          ChangeOperation = "breakglass"
	OperationOrganizationChange  ChangeOperation = "organization-change"
	OperationClusterAttach       ChangeOperation = "cluster-attach"
	OperationMachineApprove      ChangeOperation = "machine-approve"
	OperationMachineDecommission ChangeOperation = "machine-decommission"
	OperationDecommission        ChangeOperation = "decommission"
)

// ChangeModel selects the approval path. Standard changes are pre-approved
// templates executed immediately with a record; normal changes need an
// eligible human approver; emergency changes need a second person from the
// security on-call group.
// +kubebuilder:validation:Enum=standard;normal;emergency
type ChangeModel string

const (
	ModelStandard  ChangeModel = "standard"
	ModelNormal    ChangeModel = "normal"
	ModelEmergency ChangeModel = "emergency"
)

// ChangePhase is the state machine position of a ChangeRequest.
type ChangePhase string

const (
	PhaseDraft      ChangePhase = "Draft"
	PhasePending    ChangePhase = "Pending"
	PhaseApproved   ChangePhase = "Approved"
	PhaseRejected   ChangePhase = "Rejected"
	PhaseScheduled  ChangePhase = "Scheduled"
	PhaseApplying   ChangePhase = "Applying"
	PhaseApplied    ChangePhase = "Applied"
	PhaseFailed     ChangePhase = "Failed"
	PhaseExpired    ChangePhase = "Expired"
	PhaseSuperseded ChangePhase = "Superseded"
)

func (p ChangePhase) Terminal() bool {
	switch p {
	case PhaseRejected, PhaseApplied, PhaseFailed, PhaseExpired, PhaseSuperseded:
		return true
	}
	return false
}

// ChangeTarget is a typed reference to what the change acts on. For binding
// operations (promote, rollback, resource-update, decommission) the binding
// kind is derived from Kind: Component -> ReleaseBinding, Resource ->
// ResourceReleaseBinding, Project -> ProjectReleaseBinding.
type ChangeTarget struct {
	// +kubebuilder:validation:Enum=Component;Resource;Project;Organization;Cluster;Machine
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Project is required for Component and Resource targets and must be empty
	// for Project targets (the name already carries the project).
	// +kubebuilder:validation:MaxLength=63
	Project string `json:"project,omitempty"`
	// +kubebuilder:validation:MaxLength=63
	Environment string `json:"environment,omitempty"`
}

// ChangeWindow is the planned execution window. An approval is valid until
// the window end; requests without a window expire after a fixed TTL.
type ChangeWindow struct {
	Start metav1.Time `json:"start"`
	End   metav1.Time `json:"end"`
}

type ChangeRequestSpec struct {
	// Organization owning the change; must match the tenant namespace of the
	// object. The OpenChoreo namespace of the organization is derived from it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Organization string          `json:"organization"`
	Operation    ChangeOperation `json:"operation"`
	Model        ChangeModel     `json:"model"`
	Target       ChangeTarget    `json:"target"`
	// Desired is the operation-specific payload, e.g. the releaseName to
	// promote. The schema is validated per operation by the change service.
	Desired *apiextensionsv1.JSON `json:"desired,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Justification string        `json:"justification"`
	Window        *ChangeWindow `json:"window,omitempty"`
	// Requester is the authenticated identity that created the change. It is
	// filled by the API from the caller's token, never from the request body.
	// +kubebuilder:validation:MinLength=1
	Requester string `json:"requester"`
	// RequesterGroups records the token groups at creation time so the
	// controller can enforce separation of duties without trusting the API.
	RequesterGroups []string `json:"requesterGroups,omitempty"`
	// ExternalRef is the ITSM identifier mirroring this change.
	ExternalRef string `json:"externalRef,omitempty"`
}

// ChangeApproval is one human (or system, for standard changes) decision.
// Decisions are only accepted from the approver's own token; adapters cannot
// approve on behalf of a person.
type ChangeApproval struct {
	User   string   `json:"user"`
	Groups []string `json:"groups,omitempty"`
	// Role the decision counts under: approver, platform, security,
	// security-oncall or standard.
	Role     string      `json:"role"`
	Decision string      `json:"decision"`
	Time     metav1.Time `json:"time"`
	// +kubebuilder:validation:Enum=portal;itsm;system
	Source string `json:"source"`
	// Evidence links the decision to an external record, e.g. a ServiceNow
	// approval sys_id.
	Evidence string `json:"evidence,omitempty"`
}

// ChangeNote is a free-form comment recorded on the change.
type ChangeNote struct {
	User   string      `json:"user"`
	Time   metav1.Time `json:"time"`
	Text   string      `json:"text"`
	Source string      `json:"source"`
}

// BindingSnapshot is the "before" state of one OpenChoreo binding. The
// controller compares the live binding against it before every apply attempt
// and fails the change as Stale when they differ.
type BindingSnapshot struct {
	BindingKind string                `json:"bindingKind"`
	Name        string                `json:"name"`
	Exists      bool                  `json:"exists"`
	Spec        *apiextensionsv1.JSON `json:"spec,omitempty"`
	Hash        string                `json:"hash,omitempty"`
	CapturedAt  metav1.Time           `json:"capturedAt"`
}

// RollbackPlan tells how to undo the change once applied.
type RollbackPlan struct {
	// +kubebuilder:validation:Enum=restore-release;restore-spec;recreate-binding;undeploy;revoke-grant;none
	Action      string `json:"action"`
	ReleaseName string `json:"releaseName,omitempty"`
	Description string `json:"description,omitempty"`
}

type ChangeRequestStatus struct {
	Phase ChangePhase `json:"phase,omitempty"`
	// TargetKey is the deterministic key serializing changes: one active
	// ChangeRequest per target binding, newer requests supersede older ones.
	TargetKey    string            `json:"targetKey,omitempty"`
	Approvals    []ChangeApproval  `json:"approvals,omitempty"`
	Notes        []ChangeNote      `json:"notes,omitempty"`
	Before       []BindingSnapshot `json:"before,omitempty"`
	RollbackPlan *RollbackPlan     `json:"rollbackPlan,omitempty"`
	// Attempts counts apply attempts; the controller backs off between them.
	Attempts int32 `json:"attempts,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.spec.operation`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Requester",type=string,JSONPath=`.spec.requester`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ChangeRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   ChangeRequestSpec   `json:"spec"`
	Status ChangeRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ChangeRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ChangeRequest `json:"items"`
}
