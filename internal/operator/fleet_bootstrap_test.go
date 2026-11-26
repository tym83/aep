package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	api "github.com/tym83/aep/api/v1alpha1"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"testing"
	"time"
)

func TestFleetOnlyOldestCSRAndBootstrapCleanup(t *testing.T) {
	ctx := context.Background()
	ec := &api.ExternalCluster{ObjectMeta: metav1.ObjectMeta{Name: "west", Namespace: "tenant-acme", UID: "one"}, Spec: api.ExternalClusterSpec{Organization: "acme"}}
	id := FleetID(ec)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "system:open-cluster-management:" + id + ":agent", Organization: []string{"system:open-cluster-management:" + id}}}, key)
	old := &certv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)), Labels: map[string]string{"open-cluster-management.io/cluster-name": id}}, Spec: certv1.CertificateSigningRequestSpec{Username: "system:serviceaccount:aep-acme:bootstrap-" + id, SignerName: certv1.KubeAPIServerClientSignerName, Request: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), Usages: []certv1.KeyUsage{certv1.UsageClientAuth}}}
	newer := old.DeepCopy()
	newer.Name = "new"
	newer.CreationTimestamp = metav1.Now()
	k := kubefake.NewClientset(newer, old)
	mc := obj("cluster.open-cluster-management.io/v1", "ManagedCluster", "", id, M{"hubAcceptsClient": true})
	recorder := record.NewFakeRecorder(10)
	f := &FleetReconciler{Reconciler: &Reconciler{Recorder: recorder}, Hub: &Cluster{Kube: k, Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), mc)}}
	for range 2 {
		if err := f.approveFleetCSR(ctx, ec, id); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := k.CertificatesV1().CertificateSigningRequests().Get(ctx, "old", metav1.GetOptions{})
	other, _ := k.CertificatesV1().CertificateSigningRequests().Get(ctx, "new", metav1.GetOptions{})
	if len(got.Status.Conditions) != 1 || len(other.Status.Conditions) != 0 {
		t.Fatal("did not approve exactly oldest CSR")
	}
	select {
	case <-recorder.Events:
	default:
		t.Fatal("duplicates not alerted")
	}
	if err := f.cleanupBootstrap(ctx, ec); err != nil {
		t.Fatal(err)
	}
	deleted := map[string]bool{}
	for _, a := range k.Actions() {
		if a.GetVerb() == "delete" {
			deleted[a.GetResource().Resource] = true
		}
	}
	for _, resource := range []string{"secrets", "serviceaccounts", "roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
		if !deleted[resource] {
			t.Fatal("bootstrap not revoked", resource)
		}
	}
}
