// +kubebuilder:object:generate=true
// +groupName=aep.aenix.io
package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "aep.aenix.io", Version: "v1alpha1"}
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &ServiceClaim{}, &ServiceClaimList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

type ResourceIdentity struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Project string `json:"project"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Environment string `json:"environment"`
}
type ServiceClaimSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kind is immutable"
	Kind                  string                          `json:"kind"`
	Parameters            map[string]apiextensionsv1.JSON `json:"parameters,omitempty"`
	EnvironmentParameters map[string]apiextensionsv1.JSON `json:"environmentParameters,omitempty"`
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="resource identity is immutable"
	Resource ResourceIdentity `json:"resource"`
}
type ApplicationReference struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid"`
}
type Connection struct {
	Host       string `json:"host"`
	Port       int32  `json:"port"`
	Database   string `json:"database"`
	Username   string `json:"username"`
	SecretName string `json:"secretName"`
}
type ServiceClaimStatus struct {
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	AppliedSpecHash    string                `json:"appliedSpecHash,omitempty"`
	Application        *ApplicationReference `json:"application,omitempty"`
	Connection         *Connection           `json:"connection,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Facts      map[string]string  `json:"facts,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type ServiceClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServiceClaimSpec   `json:"spec"`
	Status            ServiceClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ServiceClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceClaim `json:"items"`
}
