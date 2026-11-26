package audit

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type capturedStore struct {
	bodies [][]byte
	keys   []string
}

func (s *capturedStore) Put(_ context.Context, k string, b []byte, _ string) error {
	s.keys = append(s.keys, k)
	s.bodies = append(s.bodies, append([]byte(nil), b...))
	return nil
}
func TestSourceIsolationAndPartialEventList(t *testing.T) {
	s := openTest(t)
	store := &capturedStore{}
	credentials := []Credential{{strings.Repeat("a", 32), "aep-cp", "/kubernetes"}, {strings.Repeat("b", 32), "gateway", "/openchoreo"}}
	if err := ValidateCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	r := Receiver{Spool: s, Credentials: credentials, DeadLetters: store}
	body := `{"apiVersion":"audit.k8s.io/v1","kind":"EventList","items":[{},{"auditID":"bad","stage":"ResponseComplete","stageTimestamp":"invalid"},{"auditID":"ok","stage":"ResponseComplete","source":"forged","objectRef":{"apiGroup":"openchoreo.dev","resource":"components"}},{"auditID":"subgroup","stage":"ResponseComplete","objectRef":{"apiGroup":"authz.openchoreo.dev"}}]}`
	send := func(path, token string) int {
		q := httptest.NewRequest("POST", path, strings.NewReader(body))
		q.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		return w.Code
	}
	if send("/openchoreo", credentials[0].Token) != 401 || send("/kubernetes", credentials[1].Token) != 401 {
		t.Fatal("cross-source access")
	}
	before := InvalidWebhookCount.Load()
	if send("/kubernetes", credentials[0].Token) != 204 {
		t.Fatal("partial batch rejected")
	}
	if len(s.Pending()) != 2 || len(store.bodies) != 2 || InvalidWebhookCount.Load()-before != 2 {
		t.Fatal("invalid items not isolated")
	}
	for _, e := range s.Pending() {
		if e.Source != "aep-cp" {
			t.Fatal("forged source accepted")
		}
	}
	if !bytes.Contains(store.bodies[0], []byte("invalid_audit_event")) {
		t.Fatal("missing dead-letter evidence")
	}
	policy, err := os.ReadFile("../../config/audit/policy.yaml")
	if err != nil || !bytes.Contains(policy, []byte("group: openchoreo.dev")) || !bytes.Contains(policy, []byte("level: Metadata")) {
		t.Fatal("missing aep-cp audit policy")
	}
}
