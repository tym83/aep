package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFleetLeafTrustAndLifetime(t *testing.T) {
	now := time.Now()
	ca, e := GenerateCA("external-a", now)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := issueAgentLeaf("plane-a", ca, now)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := pem.Decode(leaf["tls.crt"])
	cert, e := x509.ParseCertificate(b.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	if cert.IsCA || cert.Subject.CommonName != "plane-a" || cert.NotAfter.Sub(now) > 90*24*time.Hour {
		t.Fatal(cert)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca["tls.crt"])
	if _, e = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); e != nil {
		t.Fatal(e)
	}
	other, _ := GenerateCA("other", now)
	roots = x509.NewCertPool()
	roots.AppendCertsFromPEM(other["tls.crt"])
	if _, e = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); e == nil {
		t.Fatal("cross-cluster trust")
	}
	if string(ca["tls.key"]) == string(leaf["tls.key"]) {
		t.Fatal("CA key delivered")
	}
}
func TestFleetCSRIdentity(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	makeCSR := func(org string) *certv1.CertificateSigningRequest {
		der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "system:open-cluster-management:ext-a:agent", Organization: []string{org}}}, key)
		return &certv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"open-cluster-management.io/cluster-name": "ext-a"}}, Spec: certv1.CertificateSigningRequestSpec{Username: "system:serviceaccount:aep-acme:bootstrap-ext-a", SignerName: certv1.KubeAPIServerClientSignerName, Request: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), Usages: []certv1.KeyUsage{certv1.UsageClientAuth}}}
	}
	csr := makeCSR("system:open-cluster-management:ext-a")
	if !validFleetCSR(csr, "ext-a", "acme") {
		t.Fatal("valid CSR denied")
	}
	if validFleetCSR(csr, "ext-b", "acme") || validFleetCSR(csr, "ext-a", "other") {
		t.Fatal("foreign identity accepted")
	}
	if validFleetCSR(makeCSR("system:masters"), "ext-a", "acme") {
		t.Fatal("privileged group accepted")
	}
}
func TestFleetReadinessGeneration(t *testing.T) {
	u := obj("work.open-cluster-management.io/v1", "ManifestWork", "x", "y", nil)
	u.SetGeneration(2)
	u.Object["status"] = M{"conditions": []any{M{"type": "Applied", "status": "True", "observedGeneration": int64(1)}}}
	if observed(u, "Applied") {
		t.Fatal("stale work ready")
	}
	_ = unstructured.SetNestedSlice(u.Object, []any{M{"type": "Applied", "status": "True", "observedGeneration": int64(2)}}, "status", "conditions")
	if !observed(u, "Applied") {
		t.Fatal("fresh work not ready")
	}
}
func TestFleetPinnedChartRendering(t *testing.T) {
	dir := os.Getenv("AEP_FLEET_CHART_TEST_DIR")
	if dir == "" {
		t.Skip("set AEP_FLEET_CHART_TEST_DIR to staged upstream artifacts")
	}
	h := HelmFleetRenderer{Directory: dir, Mirror: "mirror.example"}
	o := organization()
	o.Spec.Policy.CosignPublicKey = "test-public-key"
	values := policyValues(o, "prod", "test-public-key")
	matrix := []struct {
		n, v, ns string
		values   M
	}{}
	for _, ch := range tierCharts {
		if ch.Name == StrimziChart {
			continue
		}
		v := M{}
		if ch.Name == "openchoreo-data-plane" {
			v = M{"gateway": M{"tls": M{"enabled": false}}, "clusterAgent": M{"planeID": "ext-test", "tls": M{"generateCerts": false}}}
		}
		matrix = append(matrix, struct {
			n, v, ns string
			values   M
		}{ch.Name, ch.Version, "openchoreo-data-plane", v})
	}
	for _, name := range []string{"kyverno", "app-cluster-baseline", "app-cluster-nonprod", "app-cluster-prod"} {
		v := PolicyBundleVersion
		if name == "kyverno" {
			v = KyvernoChartVersion
		}
		matrix = append(matrix, struct {
			n, v, ns string
			values   M
		}{name, v, "kyverno", values[name]})
	}
	matrix = append(matrix, struct {
		n, v, ns string
		values   M
	}{"aep-bridge-appcluster", "0.1.3", "aep-system", M{}})
	var all []any
	for _, tc := range matrix {
		t.Run(tc.n, func(t *testing.T) {
			out, e := h.Render(context.Background(), tc.n, tc.v, tc.ns, tc.values)
			if e != nil {
				t.Fatal(e)
			}
			all = append(all, out...)
			if tc.n == "openchoreo-data-plane" {
				for _, v := range out {
					m := v.(map[string]any)
					md := m["metadata"].(map[string]any)
					labels, _ := md["labels"].(map[string]any)
					if labels["app.kubernetes.io/component"] == "cluster-agent" && (m["kind"] == "Certificate" || m["kind"] == "Issuer") {
						t.Fatal("agent issuer would replace AEP leaf")
					}
				}
			}
			if _, e := workBatches(out); e != nil {
				t.Fatal(e)
			}

			if len(out) == 0 {
				t.Fatal("empty chart")
			}
		})
	}
	role, err := fleetWorkPermissions(all)
	if err != nil {
		t.Fatal(err)
	}
	if role.GetLabels()["open-cluster-management.io/aggregate-to-work"] != "true" {
		t.Fatal("missing aggregation")
	}
	mapping := fleetResources(all)
	for _, raw := range all {
		m := raw.(map[string]any)
		av := m["apiVersion"].(string)
		kind := m["kind"].(string)
		group := ""
		if strings.Contains(av, "/") {
			group = strings.SplitN(av, "/", 2)[0]
		}
		covered := false
		for _, raw := range role.Object["rules"].([]any) {
			r := raw.(M)
			if r["apiGroups"].([]any)[0] != group {
				continue
			}
			for _, v := range r["resources"].([]any) {
				if v == "*" {
					t.Fatal("wildcard resource")
				}
				covered = covered || v == mapping[av+"/"+kind]
			}
		}
		if !covered {
			t.Fatalf("uncovered %s %s", av, kind)
		}
	}
}
