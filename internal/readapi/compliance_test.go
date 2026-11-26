package readapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/compliance"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestComplianceAuthorizationAndFreshness(t *testing.T) {
	s, d, c, sign := fixture(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	o := &api.Organization{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "acme"}, o); err != nil {
		t.Fatal(err)
	}
	o.Spec.Profile = "standard"
	o.Spec.Access = map[string][]string{"auditor": {"external-audit"}, "approver": {"external-approver"}}
	if err := c.Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(compliance.Aggregate("nonprod", nil, now.Add(-10*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "compliance-acme-nonprod", Namespace: "aep-system", Labels: map[string]string{"aep.aenix.io/organization": "org-uid"}}, Data: map[string]string{"snapshot.json": string(raw)}}
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		group, org string
		code       int
	}{
		{"aep-other-admin", "acme", 403}, {"aep-acme-approver", "acme", 403},
		{"external-approver", "acme", 403}, {"aep-missing-admin", "missing", 403},
		{"aep-acme-developer", "acme", 200}, {"aep-acme-platform", "acme", 200},
		{"aep-acme-admin", "acme", 200}, {"/aep-acme-auditor", "acme", 200},
		{"external-audit", "acme", 200}, {"/teams/external-audit", "acme", 200},
		{"/teams/aep-acme-auditor", "acme", 403}, {"//aep-acme-admin", "acme", 403},
		{"aep-acme-admin/", "acme", 403},
	} {
		t.Run(tc.group+"/"+tc.org, func(t *testing.T) {
			w := call(s, "/v1/organizations/"+tc.org+"/compliance", sign([]string{tc.group}, nil))
			if w.Code != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w.Header())
			}
			if w.Code == 200 {
				var out struct {
					Organization string     `json:"organization"`
					Items        []TierView `json:"items"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if out.Organization != "acme" || len(out.Items) != 2 || out.Items[0].Tier != "nonprod" || !out.Items[0].Stale || out.Items[0].Snapshot == nil || out.Items[1].Tier != "prod" || !out.Items[1].Stale || out.Items[1].Snapshot != nil {
					t.Fatalf("%+v", out)
				}
			}
		})
	}
	if len(d.Actions()) != 0 {
		t.Fatal("compliance must only read collector snapshots", d.Actions())
	}
}

func TestComplianceSnapshotValidation(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, owner, tier      string
		age                    time.Duration
		invalid, zero, missing bool
		code                   int
		stale                  bool
	}{
		{name: "fresh", owner: "org-uid", tier: "nonprod", code: 200},
		{name: "boundary", owner: "org-uid", tier: "nonprod", age: 6 * time.Minute, code: 200},
		{name: "stale", owner: "org-uid", tier: "nonprod", age: 6*time.Minute + time.Second, code: 200, stale: true},
		{name: "missing", missing: true, code: 200, stale: true},
		{name: "old owner", owner: "old-uid", tier: "nonprod", code: 503},
		{name: "wrong tier", owner: "org-uid", tier: "prod", code: 503},
		{name: "invalid JSON", owner: "org-uid", invalid: true, code: 503},
		{name: "missing timestamp", owner: "org-uid", tier: "nonprod", zero: true, code: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, c, sign := fixture(t)
			s.now = func() time.Time { return now }
			WithComplianceNamespace("collector-system")(s)
			o := &api.Organization{}
			if err := c.Get(context.Background(), client.ObjectKey{Name: "acme"}, o); err != nil {
				t.Fatal(err)
			}
			o.Spec.Profile = "small"
			if err := c.Update(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			collected := now.Add(-tc.age)
			if tc.zero {
				collected = time.Time{}
			}
			raw, err := json.Marshal(compliance.Aggregate(tc.tier, nil, collected))
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalid {
				raw = []byte("sensitive invalid data")
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "compliance-acme-nonprod", Namespace: "collector-system", Labels: map[string]string{"aep.aenix.io/organization": tc.owner}}, Data: map[string]string{"snapshot.json": string(raw)}}
			if !tc.missing {
				if err := c.Create(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			}
			w := call(s, "/v1/organizations/acme/compliance", sign([]string{"aep-acme-admin"}, nil))
			if w.Code != tc.code || strings.Contains(w.Body.String(), "sensitive") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if w.Code == 200 {
				var out struct {
					Items []TierView `json:"items"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if len(out.Items) != 1 || out.Items[0].Stale != tc.stale || (out.Items[0].Snapshot == nil) != tc.missing {
					t.Fatalf("%+v", out)
				}
			}
		})
	}
}

type failingComplianceReader struct {
	client.Reader
	failOrganization bool
}

func (c failingComplianceReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	_, organization := obj.(*api.Organization)
	if organization == c.failOrganization {
		return fmt.Errorf("sensitive backend failure")
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}
func TestComplianceReadErrors(t *testing.T) {
	for _, failOrganization := range []bool{false, true} {
		s, _, c, sign := fixture(t)
		s.client = failingComplianceReader{Reader: c, failOrganization: failOrganization}
		w := call(s, "/v1/organizations/acme/compliance", sign([]string{"aep-acme-admin"}, nil))
		if w.Code != 503 || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}
