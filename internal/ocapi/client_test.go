package ocapi

import (
	"context"
	"testing"

	"github.com/tym83/aep/internal/ocapi/octest"
)

func clientFor(s *octest.Server) *Client {
	return &Client{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test-token", nil }}
}

func TestBindingCRUD(t *testing.T) {
	s := octest.New(t)
	s.Token = "test-token"
	c := clientFor(s)
	ctx := context.Background()

	b, e := c.Get(ctx, "acme", KindReleaseBinding, "api-prod")
	if e != nil || b != nil {
		t.Fatalf("expected missing binding, got %v %v", b, e)
	}
	nb := NewBinding(KindReleaseBinding, "api-prod", "prod", "api-1.0.0", map[string]any{"projectName": "shop", "componentName": "api"})
	if _, e = c.Create(ctx, "acme", KindReleaseBinding, nb); e != nil {
		t.Fatal(e)
	}
	b, e = c.Get(ctx, "acme", KindReleaseBinding, "api-prod")
	if e != nil || b == nil {
		t.Fatalf("expected binding, got %v %v", b, e)
	}
	if b.Spec["releaseName"] != "api-1.0.0" || b.Spec["environment"] != "prod" {
		t.Fatalf("unexpected spec %v", b.Spec)
	}

	// Update replaces the whole spec: fields absent from the request vanish.
	b.Spec["workloadOverrides"] = map[string]any{"replicas": 2}
	b.Spec["state"] = "Active"
	if _, e = c.Update(ctx, "acme", KindReleaseBinding, "api-prod", b); e != nil {
		t.Fatal(e)
	}
	b.Spec = map[string]any{"owner": b.Spec["owner"], "environment": "prod", "releaseName": "api-1.1.0"}
	if _, e = c.Update(ctx, "acme", KindReleaseBinding, "api-prod", b); e != nil {
		t.Fatal(e)
	}
	got := s.Binding("acme", KindReleaseBinding, "api-prod")
	if _, ok := got["workloadOverrides"]; ok {
		t.Fatalf("update must replace the whole spec, got %v", got)
	}
	if got["releaseName"] != "api-1.1.0" {
		t.Fatalf("unexpected spec %v", got)
	}

	if e = c.Delete(ctx, "acme", KindReleaseBinding, "api-prod"); e != nil {
		t.Fatal(e)
	}
	if e = c.Delete(ctx, "acme", KindReleaseBinding, "api-prod"); e != nil {
		t.Fatal("delete of a missing binding must stay idempotent:", e)
	}
	if b, _ = c.Get(ctx, "acme", KindReleaseBinding, "api-prod"); b != nil {
		t.Fatal("expected binding to be gone")
	}
}

func TestAuthAndErrorClassification(t *testing.T) {
	s := octest.New(t)
	s.Token = "expected"
	c := clientFor(s)
	c.Token = func(context.Context) (string, error) { return "wrong", nil }
	_, e := c.Get(context.Background(), "acme", KindReleaseBinding, "x")
	if e == nil || !Permanent(e) || IsNotFound(e) {
		t.Fatalf("expected permanent 401, got %v", e)
	}
	s.Token = ""
	s.FailWrites = 1
	_, e = c.Create(context.Background(), "acme", KindReleaseBinding, NewBinding(KindReleaseBinding, "x", "dev", "r1", map[string]any{"projectName": "p", "componentName": "c"}))
	if e == nil || Permanent(e) {
		t.Fatalf("expected transient 500, got %v", e)
	}
	if _, e = c.Create(context.Background(), "acme", KindReleaseBinding, NewBinding(KindReleaseBinding, "x", "dev", "r1", map[string]any{"projectName": "p", "componentName": "c"})); e != nil {
		t.Fatal("retry after injected failure must succeed:", e)
	}
}

func TestUpdateMissingBindingIsNotFound(t *testing.T) {
	s := octest.New(t)
	c := clientFor(s)
	_, e := c.Update(context.Background(), "acme", KindResourceReleaseBinding, "db-prod", NewBinding(KindResourceReleaseBinding, "db-prod", "prod", "db-1", map[string]any{"projectName": "p", "resourceName": "db"}))
	if !IsNotFound(e) {
		t.Fatalf("expected not found, got %v", e)
	}
}

func TestFactoryClientCredentials(t *testing.T) {
	ts := octest.NewTokenServer(t, "org-token")
	api := octest.New(t)
	api.Token = "org-token"
	calls := 0
	f := &Factory{BaseURL: api.URL, Credentials: func(context.Context, string) (Credentials, error) {
		calls++
		return Credentials{ClientID: "aep-change-acme", ClientSecret: "s", TokenURL: ts.URL}, nil
	}}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		c, e := f.For(ctx, "acme")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.Get(ctx, "acme", KindReleaseBinding, "none"); e != nil {
			t.Fatal(e)
		}
	}
	if ts.Count() != 1 {
		t.Fatalf("token must be cached per credential set, got %d token requests", ts.Count())
	}
	// Rotated secret must rebuild the token source.
	rotated := 0
	f.Credentials = func(context.Context, string) (Credentials, error) {
		rotated++
		return Credentials{ClientID: "aep-change-acme", ClientSecret: "rotated", TokenURL: ts.URL}, nil
	}
	c, e := f.For(ctx, "acme")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Get(ctx, "acme", KindReleaseBinding, "none"); e != nil {
		t.Fatal(e)
	}
	if ts.Count() != 2 {
		t.Fatalf("expected a new token after rotation, got %d requests", ts.Count())
	}
	if rotated != 1 {
		t.Fatalf("credentials must be re-resolved on every For, got %d", rotated)
	}
}

func TestUnsupportedKind(t *testing.T) {
	c := &Client{BaseURL: "http://invalid"}
	if _, e := c.Get(context.Background(), "acme", "EndpointReleaseBinding", "x"); e == nil {
		t.Fatal("expected an error for an unsupported binding kind")
	}
}
