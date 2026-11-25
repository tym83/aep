package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *Organization) DeepCopyInto(out *Organization) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Quotas = make(Quotas, len(in.Spec.Quotas))
	for k, v := range in.Spec.Quotas {
		out.Spec.Quotas[k] = v
	}
	out.Spec.Environments = append([]Environment(nil), in.Spec.Environments...)
	out.Spec.Tiers = make(map[string]TierSpec, len(in.Spec.Tiers))
	for k, v := range in.Spec.Tiers {
		n := v
		n.Cluster.NodeGroups = make(map[string]NodeGroup, len(v.Cluster.NodeGroups))
		for a, b := range v.Cluster.NodeGroups {
			n.Cluster.NodeGroups[a] = b
		}
		n.Services.AllowedKinds = append([]string(nil), v.Services.AllowedKinds...)
		out.Spec.Tiers[k] = n
	}
	out.Spec.Access = make(map[string][]string, len(in.Spec.Access))
	for k, v := range in.Spec.Access {
		out.Spec.Access[k] = append([]string(nil), v...)
	}
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	out.Status.Remediations = append([]Remediation(nil), in.Status.Remediations...)
	out.Status.ControlPlaneReadySince = make(map[string]metav1.Time, len(in.Status.ControlPlaneReadySince))
	for k, v := range in.Status.ControlPlaneReadySince {
		out.Status.ControlPlaneReadySince[k] = v
	}
}
func (in *Organization) DeepCopy() *Organization {
	if in == nil {
		return nil
	}
	out := new(Organization)
	in.DeepCopyInto(out)
	return out
}
func (in *Organization) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *OrganizationList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(OrganizationList)
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]Organization, len(in.Items))
	for i := range in.Items {
		in.Items[i].DeepCopyInto(&out.Items[i])
	}
	return out
}
