package cpbackup

import (
	"bytes"
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func obj(av, kind, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": av, "kind": kind, "metadata": map[string]any{"name": name}, "spec": map[string]any{"value": "old"}}}
	u.SetNamespace(ns)
	return u
}
func TestArchiveTamperingAndScope(t *testing.T) {
	g := schema.GroupVersionResource{Group: "openchoreo.dev", Version: "v1alpha1", Resource: "projects"}
	u := obj(g.GroupVersion().String(), "Project", "acme", "p")
	u.SetResourceVersion("123")
	u.SetUID("old-uid")
	u.Object["status"] = map[string]any{"ready": true}
	a := New([]string{"acme"}, []Entry{{Resource: g, Object: u}})
	if e := a.Validate(); e != nil {
		t.Fatal(e)
	}
	if a.Payload.Entries[0].Object.GetUID() != "" || a.Payload.Entries[0].Object.Object["status"] != nil {
		t.Fatal("server fields retained")
	}
	b, e := Encode(a)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Decode(b); e != nil {
		t.Fatal(e)
	}
	if _, e = Decode(bytes.Replace(b, []byte("old"), []byte("new"), 1)); e == nil {
		t.Fatal("tamper accepted")
	}
	u.SetNamespace("other")
	a = New([]string{"acme"}, []Entry{{Resource: g, Object: u}})
	if a.Validate() == nil {
		t.Fatal("cross-organization object accepted")
	}
}
func TestRestoreOrderDryRunIdempotent(t *testing.T) {
	ns := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	g := schema.GroupVersion{Group: "openchoreo.dev", Version: "v1alpha1"}
	entries := []Entry{{Resource: g.WithResource("releasebindings"), Object: obj(g.String(), "ReleaseBinding", "acme", "b")}, {Resource: g.WithResource("components"), Object: obj(g.String(), "Component", "acme", "c")}, {Resource: ns, Object: obj("v1", "Namespace", "", "acme")}, {Resource: g.WithResource("environments"), Object: obj(g.String(), "Environment", "acme", "dev")}}
	a := New([]string{"acme"}, entries)
	resources := []Resource{{g.WithResource("releasebindings"), "ReleaseBinding", true, false}, {g.WithResource("components"), "Component", true, false}, {g.WithResource("environments"), "Environment", true, false}}
	kinds := map[schema.GroupVersionResource]string{}
	for _, r := range resources {
		kinds[r.GVR] = r.Kind + "List"
	}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	var b bytes.Buffer
	if e := Restore(context.Background(), c, resources, a, true, &b); e != nil {
		t.Fatal(e)
	}
	for _, act := range c.Actions() {
		if act.GetVerb() != "get" && act.GetVerb() != "list" {
			t.Fatal("dry-run mutated API")
		}
	}
	c.ClearActions()
	if e := Restore(context.Background(), c, resources, a, false, &b, RestoreOptions{Merge: true, AllowOverwrite: map[string]bool{"Component": true}}); e != nil {
		t.Fatal(e)
	}
	var created []string
	for _, act := range c.Actions() {
		if act.GetVerb() == "create" {
			created = append(created, act.GetResource().Resource)
		}
	}
	if strings.Join(created, ",") != "namespaces,environments,components,releasebindings" {
		t.Fatal(created)
	}
	c.ClearActions()
	b.Reset()
	if e := Restore(context.Background(), c, resources, a, false, &b, RestoreOptions{Merge: true, AllowOverwrite: map[string]bool{"Component": true}}); e != nil {
		t.Fatal(e)
	}
	for _, act := range c.Actions() {
		if act.GetVerb() != "get" && act.GetVerb() != "list" {
			t.Fatal("second restore wrote identical object")
		}
	}
	entry := a.Payload.Entries[0]
	for _, candidate := range a.Payload.Entries {
		if candidate.Object.GetKind() == "Component" {
			entry = candidate
		}
	}
	changed, err := c.Resource(entry.Resource).Namespace("acme").Get(context.Background(), entry.Object.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	changed.Object["spec"] = map[string]any{"value": "drift"}
	changed.Object["status"] = map[string]any{"ready": true}
	if _, err = c.Resource(entry.Resource).Namespace("acme").Update(context.Background(), changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err = Restore(context.Background(), c, resources, a, false, &b, RestoreOptions{Merge: true, AllowOverwrite: map[string]bool{"Component": true}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "update ") {
		t.Fatal("drift not detected")
	}
	got, err := c.Resource(entry.Resource).Namespace("acme").Get(context.Background(), entry.Object.GetName(), metav1.GetOptions{})
	if err != nil || got.Object["status"] == nil {
		t.Fatal("status lost")
	}
	if e := Restore(context.Background(), c, nil, a, false, &b); e == nil {
		t.Fatal("missing APIs accepted")
	}
}
func TestExportOrganizationSecretsAndClusterScope(t *testing.T) {
	g := schema.GroupVersion{Group: "openchoreo.dev", Version: "v1alpha1"}
	p := g.WithResource("projects")
	cp := g.WithResource("clusterdataplanes")
	n := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	s := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	secret := obj("v1", "Secret", "acme", "credentials")
	secret.Object["data"] = map[string]any{"password": "c2VjcmV0"}
	token := obj("v1", "Secret", "acme", "sa-token")
	token.Object["type"] = "kubernetes.io/service-account-token"
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{p: "ProjectList", cp: "ClusterDataPlaneList", n: "NamespaceList", s: "SecretList"}, obj("v1", "Namespace", "", "acme"), secret, token, obj(g.String(), "Project", "acme", "one"), obj(g.String(), "Project", "other", "excluded"), obj(g.String(), "ClusterDataPlane", "", "shared"))
	a, e := Export(context.Background(), c, []Resource{{p, "Project", true, false}, {cp, "ClusterDataPlane", false, false}}, []string{"acme"})
	if e != nil {
		t.Fatal(e)
	}
	if len(a.Payload.Entries) != 4 {
		t.Fatal(len(a.Payload.Entries))
	}
	// Missing organizations may not yet have been provisioned on aep-cp.
	if absent, e := Export(context.Background(), c, []Resource{{p, "Project", true, false}}, []string{"missing"}); e != nil || len(absent.Payload.Skipped) != 1 || absent.Payload.Skipped[0].Reason != "absent" {
		t.Fatal("missing namespace not recorded", e)
	}
	_, _ = c.Resource(s).Namespace("acme").Get(context.Background(), "credentials", metav1.GetOptions{})
}
