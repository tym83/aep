package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math/big"
	"time"
)

func issueAgentLeaf(id string, ca map[string][]byte, now time.Time) (map[string][]byte, error) {
	cb, _ := pem.Decode(ca["tls.crt"])
	kb, _ := pem.Decode(ca["tls.key"])
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	cert, e := x509.ParseCertificate(cb.Bytes)
	if e != nil {
		return nil, e
	}
	key, e := x509.ParseECPrivateKey(kb.Bytes)
	if e != nil {
		return nil, e
	}
	leaf, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, e
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		return nil, e
	}
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: id, Organization: []string{"OpenChoreo"}}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if t.NotAfter.After(cert.NotAfter) {
		t.NotAfter = cert.NotAfter
	}
	der, e := x509.CreateCertificate(rand.Reader, t, cert, &leaf.PublicKey, key)
	if e != nil {
		return nil, e
	}
	pk, e := x509.MarshalECPrivateKey(leaf)
	if e != nil {
		return nil, e
	}
	return map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk})}, nil
}
func (f *FleetReconciler) agentLeaf(ctx context.Context, o *api.Organization, id string, ca map[string][]byte) (map[string][]byte, error) {
	name := "agent-leaf-" + id
	sec, e := f.Management.Kube.CoreV1().Secrets(f.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return nil, e
	}
	if e == nil {
		if sec.Labels[OwnerLabel] != string(o.UID) {
			return nil, fmt.Errorf("leaf ownership mismatch")
		}
		b, _ := pem.Decode(sec.Data["tls.crt"])
		if b != nil {
			c, err := x509.ParseCertificate(b.Bytes)
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(ca["tls.crt"])
			valid := false
			if err == nil {
				_, verifyErr := c.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
				valid = verifyErr == nil && c.Subject.CommonName == id
			}
			if valid && time.Until(c.NotAfter) > 30*24*time.Hour {
				return sec.Data, nil
			}
		}
	}
	data, e := issueAgentLeaf(id, ca, time.Now())
	if e != nil {
		return nil, e
	}
	if e = f.Management.apply(ctx, o, "secrets", secretObject(f.Options.Namespace, name, data)); e != nil {
		return nil, e
	}
	return data, nil
}
