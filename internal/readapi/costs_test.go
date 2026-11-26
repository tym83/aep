package readapi

import (
	"context"
	"github.com/tym83/aep/internal/cost"
	"testing"
	"time"
)

type costReader struct{ org string }

func (c *costReader) Query(_ context.Context, org string, _, _ time.Time, _ string) ([]cost.Summary, error) {
	c.org = org
	return []cost.Summary{{Group: "p", Cost: 5, Currency: "USD"}}, nil
}
func TestCostAuthorization(t *testing.T) {
	s, _, _, sign := fixture(t)
	reader := &costReader{}
	s.Costs = reader
	path := "/v1/organizations/acme/costs?from=2026-11-01&to=2026-12-01&groupBy=service"
	for _, role := range []string{"developer", "platform", "admin", "auditor", "finance"} {
		w := call(s, path, sign([]string{"aep-acme-" + role}, nil))
		if w.Code != 200 || reader.org != "acme" {
			t.Fatalf("%s: %d %s", role, w.Code, w.Body)
		}
	}
	for _, group := range []string{"aep-beta-finance", "aep-acme-approver"} {
		if w := call(s, path, sign([]string{group}, nil)); w.Code != 403 {
			t.Fatalf("%s: %d", group, w.Code)
		}
	}
	if w := call(s, "/v1/organizations/acme/costs?from=bad&to=bad", sign([]string{"aep-acme-finance"}, nil)); w.Code != 400 {
		t.Fatal(w.Code)
	}
	s.Costs = nil
	if w := call(s, path, sign([]string{"aep-acme-finance"}, nil)); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
