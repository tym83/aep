package operator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	api "github.com/tym83/aep/api/v1alpha1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
)

func validFleetCSR(c *certificatesv1.CertificateSigningRequest, id, org string) bool {
	if c.Spec.Username != "system:serviceaccount:aep-"+org+":bootstrap-"+id || c.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName || c.Labels["open-cluster-management.io/cluster-name"] != id {
		return false
	}
	if len(c.Spec.Usages) == 0 {
		return false
	}
	for _, u := range c.Spec.Usages {
		if u != certificatesv1.UsageClientAuth && u != certificatesv1.UsageDigitalSignature && u != certificatesv1.UsageKeyEncipherment {
			return false
		}
	}
	b, _ := pem.Decode(c.Spec.Request)
	if b == nil {
		return false
	}
	r, e := x509.ParseCertificateRequest(b.Bytes)
	if e != nil || r.CheckSignature() != nil {
		return false
	}
	prefix := "system:open-cluster-management:" + id + ":"
	agent := strings.TrimPrefix(r.Subject.CommonName, prefix)
	if agent == r.Subject.CommonName || agent == "" || strings.Contains(agent, ":") || len(r.DNSNames)+len(r.IPAddresses)+len(r.EmailAddresses)+len(r.URIs) > 0 {
		return false
	}
	hasOrg := false
	for _, o := range r.Subject.Organization {
		if o == "system:open-cluster-management:"+id {
			hasOrg = true
		} else if o != "system:open-cluster-management:managed-clusters" {
			return false
		}
	}
	return hasOrg
}
func (f *FleetReconciler) approveFleetCSR(ctx context.Context, ec *api.ExternalCluster, id string) error {
	mc, e := f.Hub.get(ctx, "cluster.open-cluster-management.io/v1", "managedclusters", "", id)
	if e != nil {
		return e
	}
	if ocmCondition(mc, "ManagedClusterJoined") {
		return nil
	}
	list, e := f.Hub.Kube.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{LabelSelector: "open-cluster-management.io/cluster-name=" + id})
	if e != nil {
		return e
	}
	for i := range list.Items {
		c := &list.Items[i]
		if len(c.Status.Conditions) > 0 || !validFleetCSR(c, id, ec.Spec.Organization) {
			continue
		}
		c.Status.Conditions = append(c.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue, Reason: "AEPApprovedAssignment", Message: "Cluster identity matches approved AEP assignment", LastUpdateTime: metav1.Now()})
		if _, e = f.Hub.Kube.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, c.Name, c, metav1.UpdateOptions{}); e != nil {
			return e
		}
	}
	return nil
}
