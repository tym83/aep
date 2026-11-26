//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func runClaimRef(ref, run string) bool {
	namespace, name, ok := strings.Cut(ref, "/")
	if !ok || name == "" || strings.Contains(name, "/") || run == "" {
		return false
	}
	if strings.Contains(name, "-"+run+"-") {
		return true
	}
	// Some data-plane namespace names use only the first eight run characters.
	return len(run) >= 8 && strings.HasPrefix(namespace, "dp-") && strings.HasSuffix(namespace, "-"+run[:8])
}

// Only identities are returned: application specs, credentials and API errors
// must never enter the acceptance report. This function performs no writes.
func (s *suite) applicationLeftovers(ctx context.Context, client dynamic.Interface, resources []schema.GroupVersionResource) ([]string, []string) {
	var leftovers, failures []string
	for _, org := range s.c.Organizations {
		namespace := "tenant-" + org.Name + "-nonprod"
		for _, resource := range resources {
			list, err := client.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s list failed (details redacted)", namespace, resource.Resource))
				continue
			}
			for _, app := range list.Items {
				if runClaimRef(app.GetAnnotations()["aep.aenix.io/claim-ref"], s.run) {
					leftovers = append(leftovers, fmt.Sprintf("%s/%s/%s (details redacted)", namespace, resource.Resource, app.GetName()))
				}
			}
		}
	}
	sort.Strings(leftovers)
	sort.Strings(failures)
	return leftovers, failures
}

func (s *suite) verifyApplicationCleanup() (int, int) {
	if s.c.ManagementKubeconfig == "" {
		s.t.Log("application cleanup verification skipped: management kubeconfig not configured")
		return 0, 0
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", s.c.ManagementKubeconfig)
	if err != nil {
		s.t.Error("application cleanup: management kubeconfig cannot be loaded (details redacted)")
		return 0, 1
	}
	cfg.Timeout = 15 * time.Second
	d, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		s.t.Error("application cleanup: discovery client cannot be configured (details redacted)")
		return 0, 1
	}
	list, err := d.ServerResourcesForGroupVersion("apps.cozystack.io/v1alpha1")
	if err != nil {
		s.t.Error("application cleanup: discovery failed (details redacted)")
		return 0, 1
	}
	var resources []schema.GroupVersionResource
	for _, resource := range list.APIResources {
		if resource.Namespaced && !strings.Contains(resource.Name, "/") && contains(resource.Verbs, "list") {
			resources = append(resources, schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: resource.Name})
		}
	}
	if len(resources) == 0 {
		s.t.Error("application cleanup: no listable application resources discovered")
		return 0, 1
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		s.t.Error("application cleanup: management client cannot be configured (details redacted)")
		return 0, 1
	}
	var leftovers, failures []string
	err = wait.PollUntilContextTimeout(context.Background(), 2*time.Second, time.Duration(s.c.TimeoutSeconds)*time.Second, true, func(ctx context.Context) (bool, error) {
		next, errs := s.applicationLeftovers(ctx, client, resources)
		if len(errs) == 0 {
			leftovers = next
		} else {
			// Keep the last observed identities if the deadline interrupts a list.
			for _, item := range next {
				if !contains(leftovers, item) {
					leftovers = append(leftovers, item)
				}
			}
		}
		failures = errs
		return len(leftovers) == 0 && len(failures) == 0, nil
	})
	for _, item := range leftovers {
		s.t.Errorf("run %s leftover Cozystack application: %s", s.run, item)
	}
	for _, failure := range failures {
		s.t.Errorf("application cleanup verification: %s", failure)
	}
	if err != nil && len(leftovers) == 0 && len(failures) == 0 {
		s.t.Error("application cleanup verification timed out (details redacted)")
		return 0, 1
	}
	return len(leftovers), len(failures)
}
