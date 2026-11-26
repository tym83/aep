package cpbackup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestRestoreSafety(t *testing.T) {
	for _, kind := range []string{"Secret", "ReleaseBinding", "ComponentRelease", "Project"} {
		t.Run(kind, func(t *testing.T) {
			g := schema.GroupVersionResource{Group: "openchoreo.dev", Version: "v1alpha1", Resource: strings.ToLower(kind) + "s"}
			if kind == "Secret" {
				g = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
			}
			u := obj(g.GroupVersion().String(), kind, "acme", "x")
			a := New([]string{"acme"}, []Entry{{Resource: g, Object: u}})
			u.Object["spec"] = map[string]any{"value": "live"}
			c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{g: kind + "List"}, u)
			resources := []Resource{{g, kind, true, false}}
			opts := RestoreOptions{Merge: true}
			if Restore(context.Background(), c, resources, a, false, io.Discard, opts) == nil {
				t.Fatal("default overwrite allowed")
			}
			opts.AllowOverwrite = map[string]bool{kind: true}
			u.SetManagedFields([]metav1.ManagedFieldsEntry{{Time: &metav1.Time{Time: time.Now().Add(time.Hour)}}})
			c.Resource(g).Namespace("acme").Update(context.Background(), u, metav1.UpdateOptions{})
			if Restore(context.Background(), c, resources, a, false, io.Discard, opts) == nil {
				t.Fatal("newer live object overwritten")
			}
			for _, act := range c.Actions() {
				if act.GetVerb() == "create" {
					t.Fatal("policy failure wrote object")
				}
			}
		})
	}
}
func TestMergeRequired(t *testing.T) {
	g := schema.GroupVersionResource{Group: "openchoreo.dev", Version: "v1alpha1", Resource: "projects"}
	u := obj(g.GroupVersion().String(), "Project", "acme", "existing")
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{g: "ProjectList"}, u)
	a := New([]string{"acme"}, nil)
	if Restore(context.Background(), c, []Resource{{g, "Project", true, false}}, a, false, io.Discard) == nil {
		t.Fatal("occupied namespace accepted")
	}
}

type partialDiscovery struct {
	*discoveryfake.FakeDiscovery
	failure error
}

func (d partialDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return []*metav1.APIResourceList{{GroupVersion: "openchoreo.dev/v1alpha1", APIResources: []metav1.APIResource{{Name: "projects", Kind: "Project", Verbs: metav1.Verbs{"list"}}}}}, d.failure
}
func TestPartialDiscovery(t *testing.T) {
	for _, group := range []string{"metrics.k8s.io", "openchoreo.dev", "nested.openchoreo.dev"} {
		d := partialDiscovery{&discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}}, &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{{Group: group, Version: "v1"}: errors.New("unavailable")}}}
		_, err := Discover(d)
		if (err != nil) != IsOpenChoreo(group) {
			t.Fatalf("%s: %v", group, err)
		}
	}
}
func TestExportControllerOwnedAndRuns(t *testing.T) {
	g := schema.GroupVersionResource{Group: "openchoreo.dev", Version: "v1alpha1", Resource: "projects"}
	n := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	s := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	u := obj(g.GroupVersion().String(), "Project", "acme", "owned")
	yes := true
	u.SetOwnerReferences([]metav1.OwnerReference{{Controller: &yes, Name: "parent", UID: "id"}})
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{g: "ProjectList", s: "SecretList", n: "NamespaceList"}, obj("v1", "Namespace", "", "acme"), u)
	a, err := Export(context.Background(), c, []Resource{{g, "Project", true, false}}, []string{"acme"})
	if err != nil || len(a.Payload.Entries) != 1 || len(a.Payload.Skipped) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	for _, kind := range []string{"WorkflowRun", "ComponentWorkflowRun", "FutureRun", "RenderedRelease", "Resource"} {
		if !excludedKind(kind) {
			t.Fatal(kind)
		}
	}
	if rank("WorkflowRelease") <= rank("Component") || rank("WorkflowBinding") <= rank("WorkflowRelease") || rank("FutureWorkflowThing") == rank("Workflow") {
		t.Fatal("workflow substring affects ordering")
	}
}

type inventoryDiscovery struct{ *discoveryfake.FakeDiscovery }

func (d inventoryDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return []*metav1.APIResourceList{{GroupVersion: "openchoreo.dev/v1alpha1", APIResources: []metav1.APIResource{
		{Name: "workflowruns", Kind: "WorkflowRun", Namespaced: true, Verbs: metav1.Verbs{"list"}},
		{Name: "renderedreleases", Kind: "RenderedRelease", Namespaced: true, Verbs: metav1.Verbs{"list"}},
		{Name: "projects", Kind: "Project", Namespaced: true, Verbs: metav1.Verbs{"list"}},
	}}}, nil
}
func TestDiscoveryInventoriesExcludedObjectsWithoutExportingBodies(t *testing.T) {
	resources, err := Discover(inventoryDiscovery{&discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[schema.GroupVersionResource]string{{Version: "v1", Resource: "secrets"}: "SecretList"}
	objects := []runtime.Object{obj("v1", "Namespace", "", "acme")}
	for _, r := range resources {
		kinds[r.GVR] = r.Kind + "List"
		objects = append(objects, obj(r.GVR.GroupVersion().String(), r.Kind, "acme", "example"))
		if (r.Kind != "Project") != r.InventoryOnly {
			t.Fatal("discovery exclusion missing", r.Kind)
		}
	}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
	a, err := Export(context.Background(), c, resources, []string{"acme"})
	if err != nil || len(a.Payload.Entries) != 2 || len(a.Payload.Skipped) != 2 {
		t.Fatalf("entries=%d skipped=%d err=%v", len(a.Payload.Entries), len(a.Payload.Skipped), err)
	}
	for _, s := range a.Payload.Skipped {
		if s.Name != "example" || s.Reason != "run or controller-rendered kind" {
			t.Fatal(s)
		}
	}
}
