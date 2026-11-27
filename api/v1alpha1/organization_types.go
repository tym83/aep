package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type Quotas map[string]string
type EnvironmentDataPlane struct {
	External string `json:"external"`
}
type Environment struct {
	DataPlane   EnvironmentDataPlane `json:"dataPlane,omitzero"`
	Name        string               `json:"name"`
	Tier        string               `json:"tier"`
	ChangeModel string               `json:"changeModel"`
}
type NodeGroup struct {
	MinReplicas  int32  `json:"minReplicas"`
	MaxReplicas  int32  `json:"maxReplicas"`
	InstanceType string `json:"instanceType"`
}
type ClusterSpec struct {
	Version    string               `json:"version"`
	NodeGroups map[string]NodeGroup `json:"nodeGroups"`
}
type ServicesSpec struct {
	AllowedKinds []string `json:"allowedKinds"`
	MaxPreset    string   `json:"maxPreset,omitempty"`
	BackupPolicy string   `json:"backupPolicy,omitempty"`
}
type ArgoCDSpec struct {
	Enabled bool   `json:"enabled"`
	Version string `json:"version,omitempty"`
}
type SecretNameReference struct {
	Name string `json:"name"`
}
type VCSSpec struct {
	Provider                 string               `json:"provider"`
	URL                      string               `json:"url"`
	GroupPath                string               `json:"groupPath,omitempty"`
	Org                      string               `json:"org,omitempty"`
	EnsureGroup              bool                 `json:"ensureGroup,omitempty"`
	ReadCredentialsSecretRef SecretNameReference  `json:"readCredentialsSecretRef"`
	CredentialsSecretRef     SecretNameReference  `json:"credentialsSecretRef"`
	CABundleSecretRef        *SecretNameReference `json:"caBundleSecretRef,omitempty"`
}
type TierSpec struct {
	ArgoCD   *ArgoCDSpec  `json:"argocd,omitempty"`
	Cluster  ClusterSpec  `json:"cluster"`
	Services ServicesSpec `json:"services"`
}

// PlatformImageTrust separates vendor image trust from organization signing keys.
type PlatformImageTrust struct {
	Mode             string   `json:"mode,omitempty"`
	VendorPublicKeys []string `json:"vendorPublicKeys,omitempty"`
	AllowedImages    []string `json:"allowedImages,omitempty"`
}

// PolicySpec controls baseline reporting and the platform image trust boundary.
type PolicySpec struct {
	// PlatformNamespaces adds exact addon namespace names; dp-* cells cannot be excluded.
	PlatformNamespaces []string           `json:"platformNamespaces,omitempty"`
	PlatformImageTrust PlatformImageTrust `json:"platformImageTrust,omitempty"`
	Registries         []string           `json:"registries,omitempty"`
	// RegistryPullSecretRef names a kubernetes.io/dockerconfigjson Secret in the
	// operator namespace (the organization's registry pull robot). The operator
	// delivers it to every OpenChoreo environment namespace of the organization
	// as aep-registry-pull, which the AEP component types reference.
	RegistryPullSecretRef *SecretNameReference `json:"registryPullSecretRef,omitempty"`
	// CosignPublicKey is an inline PEM public key, never a private signing key.
	CosignPublicKey string `json:"cosignPublicKey,omitempty"`
	Mode            string `json:"mode,omitempty"`
}

type OrganizationSpec struct {
	VCS          *VCSSpec            `json:"vcs,omitempty"`
	Policy       PolicySpec          `json:"policy,omitempty"`
	DisplayName  string              `json:"displayName"`
	Profile      string              `json:"profile"`
	CostCenter   string              `json:"costCenter,omitempty"`
	Quotas       Quotas              `json:"quotas"`
	Environments []Environment       `json:"environments"`
	Tiers        map[string]TierSpec `json:"tiers"`
	Access       map[string][]string `json:"access,omitempty"`
}
type Remediation struct {
	Time      metav1.Time `json:"time"`
	Namespace string      `json:"namespace"`
	Release   string      `json:"release"`
	Reason    string      `json:"reason"`
}
type OrganizationStatus struct {
	ArgoCDURL              string                 `json:"argocdURL,omitempty"`
	DeletionStarted        bool                   `json:"deletionStarted,omitempty"`
	Phase                  string                 `json:"phase,omitempty"`
	Conditions             []metav1.Condition     `json:"conditions,omitempty"`
	ControlPlaneReadySince map[string]metav1.Time `json:"controlPlaneReadySince,omitempty"`
	Remediations           []Remediation          `json:"remediations,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
type Organization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OrganizationSpec   `json:"spec"`
	Status            OrganizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Organization `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Organization{}, &OrganizationList{})
		return nil
	})
}
