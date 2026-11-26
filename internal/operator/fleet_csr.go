package operator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sort"
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

	var candidates []*certificatesv1.CertificateSigningRequest
	for i := range list.Items {
		c := &list.Items[i]
		if validFleetCSR(c, id, ec.Spec.Organization) {
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.Name < b.Name
		}
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	})

	message := fmt.Sprintf("%d valid bootstrap CSRs", len(candidates))
	previous := meta.FindStatusCondition(ec.Status.Conditions, "DuplicateBootstrapCSR")
	if len(candidates) > 1 && f.Recorder != nil && (previous == nil || previous.Message != message || previous.Status != metav1.ConditionTrue) {
		f.Recorder.Event(ec, "Warning", "DuplicateBootstrapCSR", message)
	}
	fleetCondition(ec, "DuplicateBootstrapCSR", len(candidates) > 1, "Observed", message)
	active := candidates[:0]
	var rejected string
	for _, c := range candidates {
		terminal := false
		for _, condition := range c.Status.Conditions {
			if condition.Status == corev1.ConditionTrue && (condition.Type == certificatesv1.CertificateDenied || condition.Type == certificatesv1.CertificateFailed) {
				terminal = true
			}
		}
		if terminal {
			if rejected == "" {
				rejected = c.Name
			}
			continue
		}
		active = append(active, c)
	}
	fleetCondition(ec, "BootstrapRejected", rejected != "", "Observed", "Rejected bootstrap CSR: "+rejected)
	candidates = active
	for _, c := range candidates {
		for _, condition := range c.Status.Conditions {
			if condition.Type == certificatesv1.CertificateApproved && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Never approve a second request on retries, even if the first is already signed.
	c := candidates[0]
	if len(c.Status.Conditions) > 0 {
		return nil
	}
	c.Status.Conditions = append(c.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue, Reason: "AEPApprovedAssignment", Message: "Oldest CSR matches approved assignment", LastUpdateTime: metav1.Now()})
	_, e = f.Hub.Kube.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, c.Name, c, metav1.UpdateOptions{})
	return e
}
