// Package alertrouter authenticates observer webhooks and resolves their tenant.
package alertrouter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

const Path = "/api/v1alpha1/rca-agent/analyze"
const SignatureHeader = "X-AEP-Signature"
const TimestampHeader = "X-AEP-Timestamp"

type AnalyzeRequest struct {
	Namespace   string         `json:"namespace"`
	Project     string         `json:"project"`
	Component   string         `json:"component"`
	Environment string         `json:"environment"`
	ReleaseLine string         `json:"release_line,omitempty"`
	Alert       Alert          `json:"alert"`
	Meta        map[string]any `json:"meta,omitempty"`
}
type Alert struct {
	ID        string         `json:"id"`
	Value     any            `json:"value"`
	Timestamp string         `json:"timestamp"`
	Rule      map[string]any `json:"rule"`
}
type Router struct {
	client   dynamic.Interface
	template string
	secret   []byte
	ca       *x509.CertPool
	http     *http.Client
	mu       sync.Mutex
	seen     map[string]time.Time
}

func New(c dynamic.Interface, template string, secret []byte, ca *x509.CertPool) (*Router, error) {
	if len(secret) == 0 && ca == nil {
		return nil, fmt.Errorf("client CA or HMAC secret required")
	}
	if len(secret) > 0 && len(secret) < 32 {
		return nil, fmt.Errorf("HMAC secret must contain at least 32 bytes")
	}
	if !strings.Contains(template, "{org}") {
		return nil, fmt.Errorf("agent URL template requires {org}")
	}
	u, err := url.Parse(strings.ReplaceAll(template, "{org}", "example"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid agent URL template")
	}
	return &Router{client: c, template: template, secret: secret, ca: ca, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, seen: map[string]time.Time{}}, nil
}
func Signature(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
func (s *Router) authenticated(r *http.Request, body []byte) bool {
	if s.ca != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		intermediates := x509.NewCertPool()
		for _, cert := range r.TLS.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		_, err := r.TLS.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: s.ca, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err == nil {
			return true
		}
	}
	if len(s.secret) == 0 {
		return false
	}
	timestamp := r.Header.Get(TimestampHeader)
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	t := time.Unix(unix, 0)
	if time.Since(t) > 5*time.Minute || time.Until(t) > 5*time.Minute {
		return false
	}
	got, err := hex.DecodeString(r.Header.Get(SignatureHeader))
	if err != nil {
		return false
	}
	expected, _ := hex.DecodeString(Signature(s.secret, timestamp, body))
	if !hmac.Equal(got, expected) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.seen {
		if time.Since(v) > 10*time.Minute {
			delete(s.seen, k)
		}
	}
	key := hex.EncodeToString(got)
	if _, ok := s.seen[key]; ok {
		return false
	}
	if len(s.seen) >= 10000 {
		return false
	}
	s.seen[key] = time.Now()
	return true
}

// TLSConfig requests a certificate when HMAC is also enabled; otherwise the
// handshake itself requires a client certificate from the observer-only CA.
func (s *Router) TLSConfig() *tls.Config {
	mode := tls.NoClientCert
	if s.ca != nil {
		mode = tls.RequireAndVerifyClientCert
		if len(s.secret) > 0 {
			mode = tls.VerifyClientCertIfGiven
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientCAs: s.ca, ClientAuth: mode}
}
func (s *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path != Path {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", 413)
		return
	}
	if !s.authenticated(r, body) {
		http.Error(w, "observer authentication required", 401)
		return
	}
	var payload AnalyzeRequest
	if json.Unmarshal(body, &payload) != nil || len(validation.IsDNS1123Label(payload.Namespace)) != 0 || payload.Namespace == "" || payload.Project == "" || payload.Component == "" || payload.Environment == "" || payload.Alert.ID == "" || payload.Alert.Timestamp == "" || payload.Alert.Rule["name"] == nil || payload.Alert.Value == nil {
		http.Error(w, "invalid analyze payload", 400)
		return
	}
	org, err := s.client.Resource(schema.GroupVersionResource{Group: "aep.aenix.io", Version: "v1alpha1", Resource: "organizations"}).Get(r.Context(), payload.Namespace, metav1.GetOptions{})
	if err != nil || org.GetUID() == "" {
		http.Error(w, "organization unavailable", 403)
		return
	}
	ns, err := s.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(r.Context(), payload.Namespace, metav1.GetOptions{})
	if err != nil || ns.GetLabels()["openchoreo.dev/control-plane"] != "true" || ns.GetLabels()["aep.aenix.io/organization"] != string(org.GetUID()) {
		http.Error(w, "namespace is not an AEP organization", 403)
		return
	}
	endpoint := strings.TrimSuffix(strings.ReplaceAll(s.template, "{org}", payload.Namespace), "/") + Path
	req, err := http.NewRequestWithContext(r.Context(), "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "invalid destination", 500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		http.Error(w, "agent unavailable", 502)
		return
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(result) > 1<<20 {
		http.Error(w, "invalid agent response", 502)
		return
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		http.Error(w, "agent redirects are forbidden", 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(result)
}
