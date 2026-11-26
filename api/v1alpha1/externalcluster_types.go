package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ExternalCluster is namespaced with its organization's ChangeRequests. Its
// immutable spec is an assignment, not a discovery record or an approval flag.
type ExternalClusterSpec struct {
	Organization      string                  `json:"organization"`
	Tier              string                  `json:"tier"`
	Environment       string                  `json:"environment"`
	TrustLevel        string                  `json:"trustLevel"`
	Provider          string                  `json:"provider"`
	Distro            string                  `json:"distro"`
	ChangeRequestRef  string                  `json:"changeRequestRef"`
	ClusterProfileRef ClusterProfileReference `json:"clusterProfileRef"`
}
type ClusterProfileReference struct {
	APIVersion string `json:"apiVersion"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}
type ExternalClusterStatus struct {
	PlaneID string `json:"planeID,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ExternalCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExternalClusterSpec   `json:"spec"`
	Status            ExternalClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ExternalClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalCluster `json:"items"`
}

func (in *ExternalCluster) DeepCopy() *ExternalCluster {
	if in == nil {
		return nil
	}
	out := new(ExternalCluster)
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	return out
}
func (in *ExternalCluster) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *ExternalClusterList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(ExternalClusterList)
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ExternalCluster, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return out
}
func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ExternalCluster{}, &ExternalClusterList{})
		return nil
	})
}
