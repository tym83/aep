package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math/big"
	"time"
)

func GenerateCA(name string, now time.Time) (map[string][]byte, error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, e
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		return nil, e
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(3, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		return nil, e
	}
	pk, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return nil, e
	}
	return map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk})}, nil
}
func (s *session) ca(ctx context.Context, tier string) (map[string][]byte, error) {
	name := shortName("agent-ca-" + s.o.Name + "-" + tier)
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
	var data map[string][]byte
	if errors.IsNotFound(e) {
		data, e = GenerateCA(s.o.Name+"-"+tier, time.Now())
		if e != nil {
			return nil, e
		}
		u := secretObject(s.r.Options.Namespace, name, data)
		u.Object["type"] = "kubernetes.io/tls"
		if e = s.r.Management.apply(ctx, s.o, "secrets", u); e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	} else {
		if sec.Labels[OwnerLabel] != string(s.o.UID) {
			return nil, fmt.Errorf("CA Secret ownership mismatch")
		}
		data = map[string][]byte{"tls.crt": sec.Data["tls.crt"], "tls.key": sec.Data["tls.key"]}
	}
	if _, e := tls.X509KeyPair(data["tls.crt"], data["tls.key"]); e != nil {
		return nil, fmt.Errorf("invalid agent CA key pair: %w", e)
	}
	block, _ := pem.Decode(data["tls.crt"])
	if block == nil {
		return nil, fmt.Errorf("invalid stored agent CA")
	}
	cert, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return nil, e
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("agent certificate is not a CA")
	}
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		return nil, fmt.Errorf("agent CA expires within 30 days; coordinated rotation required")
	}
	return data, nil
}
