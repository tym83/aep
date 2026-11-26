//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCleanupProductionRetriesRead(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	calls := 0
	client.PrependReactor("get", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, fmt.Errorf("transport secret")
		}
		return false, nil, nil // tracker returns NotFound
	})
	s := &suite{cleanupClient: client, c: config{TimeoutSeconds: 2, Organizations: []organization{{Name: "acme"}}}}
	if err := s.cleanupProduction(0, "resourcereleasebindings", "test-prod"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("GET count %d", calls)
	}
}

func TestCleanupReadTimeoutRedacts(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("get", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("secret object contents")
	})
	s := &suite{cleanupClient: client, c: config{Organizations: []organization{{Name: "acme"}}}}
	_, gone, err := s.cleanupGet(0, "projects", "p", time.Now().Add(30*time.Millisecond))
	if gone || err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "errorString") {
		t.Fatalf("unsafe result: %v", err)
	}
}

func TestCleanupWaitsForProjectFinalizer(t *testing.T) {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "openchoreo.dev", Version: "v1alpha1", Resource: "releasebindings"}:         "ReleaseBindingList",
		{Group: "openchoreo.dev", Version: "v1alpha1", Resource: "resourcereleasebindings"}: "ResourceReleaseBindingList",
		{Group: "openchoreo.dev", Version: "v1alpha1", Resource: "projectreleasebindings"}:  "ProjectReleaseBindingList",
	})
	gets := 0
	client.PrependReactor("delete", "projects", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	client.PrependReactor("get", "projects", func(ktesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets < 3 {
			return true, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "openchoreo.dev/v1alpha1", "kind": "Project", "metadata": map[string]any{"name": "p-local-x", "finalizers": []any{"openchoreo.dev/project-cleanup"}}}}, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "openchoreo.dev", Resource: "projects"}, "p-local-x")
	})
	s := &suite{cleanupClient: client, run: "local", c: config{TimeoutSeconds: 2, Organizations: []organization{{Name: "acme"}}}}
	if err := s.cleanupNow(0, "projects", "p-local-x"); err != nil {
		t.Fatal(err)
	}
	if gets < 3 {
		t.Fatal("did not wait for finalizer")
	}
}
