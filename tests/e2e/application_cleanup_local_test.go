//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestRunClaimRef(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"dp-globex-acceptance-d5-dev-3af31469/r-db-d5bf2dd37afc-f6d6ed64-dev-2387659b", true},
		{"dp-globex-dev-d5bf2dd3/db", true},
		{"other-d5bf2dd3/db", false},
		{"dp-globex-dev-d5bf2dd30/db", false},
		{"dp-globex-dev-12345678/r-db-other-f6d6ed64", false},
		{"dp-x/r-db-d5bf2dd37afc0-suffix", false},
		{"", false}, {"dp-x/", false}, {"dp-x/db/extra", false},
	} {
		if got := runClaimRef(tc.ref, "d5bf2dd37afc"); got != tc.want {
			t.Errorf("runClaimRef(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestApplicationLeftovers(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: "postgreses"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr: "PostgresList"})
	for _, tc := range []struct{ name, tier, ref string }{
		{"leftover", "nonprod", "dp-x/db-d5bf2dd37afc-test"},
		{"unrelated", "nonprod", "dp-x/db-other-test"},
		{"retained", "prod", "dp-x/db-d5bf2dd37afc-test"},
	} {
		app := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps.cozystack.io/v1alpha1", "kind": "Postgres"}}
		app.SetName(tc.name)
		app.SetNamespace("tenant-acme-" + tc.tier)
		app.SetAnnotations(map[string]string{"aep.aenix.io/claim-ref": tc.ref, "secret": "must-not-leak"})
		if _, err := client.Resource(gvr).Namespace(app.GetNamespace()).Create(context.Background(), app, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	client.ClearActions()
	s := &suite{run: "d5bf2dd37afc", c: config{Organizations: []organization{{Name: "acme"}}}}
	leftovers, failures := s.applicationLeftovers(context.Background(), client, []schema.GroupVersionResource{gvr})
	if len(failures) != 0 || len(leftovers) != 1 || leftovers[0] != "tenant-acme-nonprod/postgreses/leftover (details redacted)" {
		t.Fatalf("leftovers=%v failures=%v", leftovers, failures)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "list" || action.GetNamespace() != "tenant-acme-nonprod" {
			t.Fatalf("unexpected action: %v", action)
		}
	}
	client.PrependReactor("list", "postgreses", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("must-not-leak")
	})
	_, failures = s.applicationLeftovers(context.Background(), client, []schema.GroupVersionResource{gvr})
	if len(failures) != 1 || strings.Contains(failures[0], "must-not-leak") {
		t.Fatalf("failures=%v", failures)
	}
}
