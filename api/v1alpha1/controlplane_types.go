package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ControlPlaneSpec declares an aep-cp (OpenChoreo control plane) that the
// operator installs in a Cozystack tenant. Secret references name Secrets in
// the operator namespace unless stated otherwise.
type ControlPlaneSpec struct {
	// Suspend stops reconciliation of this control plane without removing
	// anything, for example during a disaster-recovery drill.
	Suspend bool `json:"suspend,omitempty"`
	// +optional
	// +kubebuilder:default={}
	Tenant ControlPlaneTenant `json:"tenant,omitzero"`
	// +optional
	// +kubebuilder:default={}
	Cluster ControlPlaneCluster `json:"cluster,omitzero"`
	// Domain is the public DNS suffix: the portal is served at
	// console.<domain> and the OpenChoreo API at api.<domain>.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=245
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Domain  string              `json:"domain"`
	Ingress ControlPlaneIngress `json:"ingress"`
	OIDC    ControlPlaneOIDC    `json:"oidc"`
	// +optional
	// +kubebuilder:default={}
	Portal ControlPlanePortal `json:"portal,omitzero"`
	// +optional
	ClusterGateway ControlPlaneClusterGateway `json:"clusterGateway,omitzero"`
	// +optional
	// +kubebuilder:default={}
	Content ControlPlaneContent `json:"content,omitzero"`
	// Audit is optional; nil disables audit forwarding.
	Audit *ControlPlaneAudit `json:"audit,omitempty"`
	// Backup is optional; nil disables backup reader provisioning.
	Backup *ControlPlaneBackup `json:"backup,omitempty"`
	// Adoption takes over an existing, hand-built control plane.
	Adoption *ControlPlaneAdoption `json:"adoption,omitempty"`
}

// ControlPlaneTenant places the control plane in a Cozystack tenant. Its
// namespace is tenant-<name> under tenant-root, otherwise <parent>-<name>.
type ControlPlaneTenant struct {
	// Parent is the namespace of the parent tenant.
	// +kubebuilder:default=tenant-root
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^tenant-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tenant parent is immutable"
	Parent string `json:"parent,omitempty"`
	// Name is alphanumeric: Cozystack joins tenant names with dashes.
	// +kubebuilder:default=aepcp
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tenant name is immutable"
	Name string `json:"name,omitempty"`
	// Quotas are Cozystack tenant resourceQuotas, for example cpu or memory.
	Quotas Quotas `json:"quotas,omitempty"`
}

// ControlPlaneCluster is the Cozystack Kubernetes application hosting aep-cp.
type ControlPlaneCluster struct {
	// +kubebuilder:default=aepcp
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cluster name is immutable"
	Name string `json:"name,omitempty"`
	// +kubebuilder:default="v1.35"
	// +kubebuilder:validation:Pattern=`^v1\.[0-9]+$`
	Version string `json:"version,omitempty"`
	// +kubebuilder:default=replicated
	// +kubebuilder:validation:MinLength=1
	StorageClass string `json:"storageClass,omitempty"`
	// +optional
	// +kubebuilder:default={}
	NodeGroup ControlPlaneNodeGroup `json:"nodeGroup,omitzero"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || !has(self.maxReplicas) || self.maxReplicas >= self.minReplicas",message="maxReplicas must not be lower than minReplicas"
type ControlPlaneNodeGroup struct {
	// +kubebuilder:default="u1.large"
	// +kubebuilder:validation:MinLength=1
	InstanceType string `json:"instanceType,omitempty"`
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// +kubebuilder:default="20Gi"
	// +kubebuilder:validation:Pattern=`^[0-9]+(Mi|Gi|Ti)$`
	DiskSize string `json:"diskSize,omitempty"`
}

// ControlPlaneIngress publishes console.<domain> and api.<domain> through the
// management ingress. Exactly one certificate source is required.
// +kubebuilder:validation:XValidation:rule="has(self.clusterIssuer) != has(self.tlsSecretRef)",message="exactly one of clusterIssuer or tlsSecretRef is required"
type ControlPlaneIngress struct {
	// +kubebuilder:default=tenant-root
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className,omitempty"`
	// ClusterIssuer is a cert-manager ClusterIssuer that issues the host certificate.
	// +kubebuilder:validation:MinLength=1
	ClusterIssuer string `json:"clusterIssuer,omitempty"`
	// TLSSecretRef names an existing kubernetes.io/tls Secret in the Ingress namespace.
	TLSSecretRef *SecretNameReference `json:"tlsSecretRef,omitempty"`
}

// ControlPlaneOIDC configures portal and API sign-in. Endpoints default to the
// Keycloak layout below the issuer.
type ControlPlaneOIDC struct {
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	JWKSURL string `json:"jwksURL,omitempty"`
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	AuthorizationURL string `json:"authorizationURL,omitempty"`
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	TokenURL string `json:"tokenURL,omitempty"`
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	WellKnownURL string `json:"wellKnownURL,omitempty"`
	// +kubebuilder:default=preferred_username
	// +kubebuilder:validation:MinLength=1
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// CABundleSecretRef holds a PEM bundle for a privately issued IdP certificate.
	CABundleSecretRef *SecretKeyReference `json:"caBundleSecretRef,omitempty"`
	PortalClient      ControlPlaneClient  `json:"portalClient"`
	// +kubebuilder:default=openchoreo-cli
	// +kubebuilder:validation:MinLength=1
	CLIClientID string `json:"cliClientID,omitempty"`
}

// ControlPlaneClient is a confidential OIDC client.
type ControlPlaneClient struct {
	// +kubebuilder:default=openchoreo-backstage-client
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID,omitempty"`
	// SecretRef holds the client secret.
	SecretRef SecretKeyReference `json:"secretRef"`
}

// SecretKeyReference selects one key of a Secret in the operator namespace.
type SecretKeyReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Key defaults per field: client-secret for clients, ca.crt for CA bundles.
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`
}

type ControlPlanePortal struct {
	// Image overrides the portal image; empty uses the aep-portal image of the
	// operator release.
	Image string `json:"image,omitempty"`
	// +optional
	// +kubebuilder:default={}
	Database ControlPlaneDatabase `json:"database,omitzero"`
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	ReadAPIURL string `json:"readAPIURL,omitempty"`
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	ChangeAPIURL string `json:"changeAPIURL,omitempty"`
}

// ControlPlaneDatabase is the Cozystack Postgres of the portal.
type ControlPlaneDatabase struct {
	// +kubebuilder:default="5Gi"
	// +kubebuilder:validation:Pattern=`^[0-9]+(Mi|Gi|Ti)$`
	Size string `json:"size,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	Replicas int32 `json:"replicas,omitempty"`
	// +kubebuilder:default=small
	// +kubebuilder:validation:Enum=nano;micro;small;medium;large;xlarge;"2xlarge"
	ResourcesPreset string `json:"resourcesPreset,omitempty"`
}

type ControlPlaneClusterGateway struct {
	// Host is a fixed cluster-gateway hostname placed in its certificate,
	// required for air-gapped data planes; empty uses --gateway-host-template.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Host string `json:"host,omitempty"`
}

type ControlPlaneContent struct {
	// ResourceTypes lists the embedded ClusterResourceTypes to apply; empty applies all.
	// +listType=set
	ResourceTypes []string `json:"resourceTypes,omitempty"`
	// +kubebuilder:default=true
	ComponentTypes *bool               `json:"componentTypes,omitempty"`
	Builds         *ControlPlaneBuilds `json:"builds,omitempty"`
}

type ControlPlaneBuilds struct {
	// +kubebuilder:validation:MinLength=1
	RegistryHost string `json:"registryHost"`
	// PublisherClientIDTemplate is a Go template with Organization.
	PublisherClientIDTemplate string `json:"publisherClientIDTemplate,omitempty"`
}

type ControlPlaneAudit struct {
	// ForwarderService is the namespace/name of the management audit forwarder Service.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ForwarderService string               `json:"forwarderService"`
	TokenSecretRef   *SecretNameReference `json:"tokenSecretRef,omitempty"`
	// OpenChoreoAPI also enables OpenChoreo API audit events.
	OpenChoreoAPI bool `json:"openChoreoAPI,omitempty"`
}

type ControlPlaneBackup struct {
	// +kubebuilder:validation:MinLength=1
	ReaderKubeconfigSecret string `json:"readerKubeconfigSecret"`
}

type ControlPlaneAdoption struct {
	// Confirm is "<tenant namespace>/<cluster>" of the existing control plane.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Confirm string `json:"confirm"`
}

// ControlPlaneOutputs are what consumers of the control plane read.
type ControlPlaneOutputs struct {
	// KubeconfigSecretRef is the operator-scoped aep-cp credential in the operator namespace.
	KubeconfigSecretRef *SecretNameReference `json:"kubeconfigSecretRef,omitempty"`
	TenantNamespace     string               `json:"tenantNamespace,omitempty"`
	ClusterName         string               `json:"clusterName,omitempty"`
}

type ControlPlaneEndpoints struct {
	Console     string `json:"console,omitempty"`
	API         string `json:"api,omitempty"`
	InternalAPI string `json:"internalAPI,omitempty"`
	APIHost     string `json:"apiHost,omitempty"`
}

type ControlPlaneGatewayStatus struct {
	Host             string `json:"host,omitempty"`
	URL              string `json:"url,omitempty"`
	MirrorService    string `json:"mirrorService,omitempty"`
	TargetPort       int32  `json:"targetPort,omitempty"`
	ServedCertSerial string `json:"servedCertSerial,omitempty"`
}

type ControlPlaneStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Phase              string `json:"phase,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Outputs ControlPlaneOutputs `json:"outputs,omitzero"`
	// +optional
	Endpoints ControlPlaneEndpoints `json:"endpoints,omitzero"`
	// +optional
	Gateway  ControlPlaneGatewayStatus `json:"gateway,omitzero"`
	Versions map[string]string         `json:"versions,omitempty"`
}

// ControlPlane is an operator-managed aep-cp. Usually one named default exists.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=aepcp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ControlPlaneSpec   `json:"spec"`
	Status            ControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ControlPlane{}, &ControlPlaneList{})
		return nil
	})
}
