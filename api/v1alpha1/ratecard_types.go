package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"maps"
)

type SharedCost struct {
	Monthly       float64 `json:"monthly"`
	AllocationKey string  `json:"allocationKey"`
}
type RateCardSpec struct {
	EffectiveFrom   metav1.Time           `json:"effectiveFrom"`
	Currency        string                `json:"currency"`
	VCPUHour        float64               `json:"vcpuHour"`
	MemoryGiBHour   float64               `json:"memoryGiBHour"`
	StorageGiBMonth map[string]float64    `json:"storageGiBMonth"`
	GPUHour         map[string]float64    `json:"gpuHour,omitempty"`
	ExternalIPMonth float64               `json:"externalIPMonth"`
	BucketGiBMonth  float64               `json:"bucketGiBMonth"`
	Shared          map[string]SharedCost `json:"shared,omitempty"`
}

// RateCard is an immutable price version. Create a new card for a new effective hour.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type RateCard struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RateCardSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type RateCardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RateCard `json:"items"`
}

func (in *RateCard) DeepCopy() *RateCard {
	if in == nil {
		return nil
	}
	out := new(RateCard)
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.StorageGiBMonth = maps.Clone(in.Spec.StorageGiBMonth)
	out.Spec.GPUHour = maps.Clone(in.Spec.GPUHour)
	out.Spec.Shared = maps.Clone(in.Spec.Shared)
	return out
}
func (in *RateCard) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *RateCardList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(RateCardList)
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]RateCard, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return out
}
func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error { s.AddKnownTypes(GroupVersion, &RateCard{}, &RateCardList{}); return nil })
}
