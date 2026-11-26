package alertrouter

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("01234567890123456789012345678901")

const payload = `{"namespace":"acme","project":"p","component":"c","environment":"dev","release_line":"main","alert":{"id":"id1","value":3,"timestamp":"2026-01-01T00:00:00Z","rule":{"name":"errors"}}}`

func router(t *testing.T, target string) *Router {
	t.Helper()
	c := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "acme", "labels": map[string]any{"openchoreo.dev/control-plane": "true", "aep.aenix.io/organization": "acme"}}}})
	r, err := New(c, target+"/{org}", testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func signed(body string) *http.Request {
	r := httptest.NewRequest("POST", Path, strings.NewReader(body))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r.Header.Set(TimestampHeader, ts)
	r.Header.Set(SignatureHeader, Signature(testSecret, ts, []byte(body)))
	return r
}
func TestForwardAndReject(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/acme"+Path {
			t.Error(r.URL.Path)
		}
		if r.Header.Get(SignatureHeader) != "" {
			t.Error("forwarded credentials")
		}
		w.WriteHeader(202)
		w.Write([]byte(`{"report_id":"report"}`))
	}))
	defer up.Close()
	s := router(t, up.URL)
	for _, tc := range []struct {
		body   string
		status int
	}{{payload, 202}, {payload, 401}, {strings.Replace(payload, "acme", "other", 1), 403}, {`{}`, 400}} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, signed(tc.body))
		if w.Code != tc.status {
			t.Fatalf("got %d: %s", w.Code, w.Body)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestHMACFailures(t *testing.T) {
	s := router(t, "http://agent")
	for _, mode := range []string{"missing", "tampered", "expired", "future"} {
		r := signed(payload)
		switch mode {
		case "missing":
			r.Header.Del(SignatureHeader)
		case "tampered":
			r.Body = http.NoBody
		case "expired", "future":
			offset := -time.Hour
			if mode == "future" {
				offset = time.Hour
			}
			ts := strconv.FormatInt(time.Now().Add(offset).Unix(), 10)
			r.Header.Set(TimestampHeader, ts)
			r.Header.Set(SignatureHeader, Signature(testSecret, ts, []byte(payload)))
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(mode, w.Code)
		}
	}
}
func TestMTLS(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "observer-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	s := router(t, "http://agent")
	s.secret = nil
	s.ca = pool
	r := httptest.NewRequest("POST", Path, nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if !s.authenticated(r, nil) {
		t.Fatal("valid cert rejected")
	}
	r.TLS = nil
	if s.authenticated(r, nil) {
		t.Fatal("missing cert accepted")
	}
	s.ca = x509.NewCertPool()
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if s.authenticated(r, nil) {
		t.Fatal("untrusted cert accepted")
	}
}
func TestRedirectRejected(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://untrusted.invalid", 302) }))
	defer up.Close()
	s := router(t, up.URL)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signed(payload))
	if w.Code != 502 {
		t.Fatal(w.Code)
	}
}
