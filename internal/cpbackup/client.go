package cpbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

func Discover(d discovery.DiscoveryInterface) ([]Resource, error) {
	lists, e := d.ServerPreferredResources()
	if e != nil {
		return nil, fmt.Errorf("incomplete discovery: %w", e)
	}
	var out []Resource
	for _, l := range lists {
		gv, e := schema.ParseGroupVersion(l.GroupVersion)
		if e != nil {
			return nil, e
		}
		if !IsOpenChoreo(gv.Group) {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			list := false
			for _, v := range r.Verbs {
				if v == "list" {
					list = true
				}
			}
			if !list {
				return nil, fmt.Errorf("OpenChoreo resource %s cannot be listed", r.Name)
			}
			out = append(out, Resource{gv.WithResource(r.Name), r.Kind, r.Namespaced})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no OpenChoreo APIs discovered")
	}
	return out, nil
}
func Organizations(ctx context.Context, c dynamic.Interface) ([]string, error) {
	l, e := c.Resource(schema.GroupVersionResource{Group: "aep.aenix.io", Version: "v1alpha1", Resource: "organizations"}).List(ctx, metav1.ListOptions{})
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, u := range l.Items {
		out = append(out, u.GetName())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no AEP organizations found")
	}
	return out, nil
}
func Export(ctx context.Context, c dynamic.Interface, resources []Resource, orgs []string) (Archive, error) {
	if len(orgs) == 0 {
		return Archive{}, fmt.Errorf("explicit organization scope required")
	}
	entries := []Entry{}
	add := func(r schema.GroupVersionResource, u *unstructured.Unstructured) {
		entries = append(entries, Entry{Resource: r, Object: u})
	}
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	list := func(r schema.GroupVersionResource, ns string) error {
		token := ""
		for {
			l, e := c.Resource(r).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 500, Continue: token})
			if e != nil {
				return fmt.Errorf("list %s namespace %s: %w", r, ns, e)
			}
			for i := range l.Items {
				u := &l.Items[i]
				if r == secretGVR {
					t, _, _ := unstructured.NestedString(u.Object, "type")
					if t == "kubernetes.io/service-account-token" {
						continue
					}
				}
				add(r, u)
			}
			token = l.GetContinue()
			if token == "" {
				return nil
			}
		}
	}
	for _, ns := range orgs {
		u, e := c.Resource(nsGVR).Get(ctx, ns, metav1.GetOptions{})
		if e != nil {
			return Archive{}, e
		}
		add(nsGVR, u)
		if e = list(secretGVR, ns); e != nil {
			return Archive{}, e
		}
	}
	for _, r := range resources {
		if !IsOpenChoreo(r.GVR.Group) {
			return Archive{}, fmt.Errorf("invalid discovery scope")
		}
		if r.Namespaced {
			for _, ns := range orgs {
				if e := list(r.GVR, ns); e != nil {
					return Archive{}, e
				}
			}
		} else {
			if e := list(r.GVR, ""); e != nil {
				return Archive{}, e
			}
		}
	}
	a := New(orgs, entries)
	return a, a.Validate()
}

// Restore changes only archived fields, preserves target status and server
// metadata, and uses resourceVersion preconditions. It never deletes objects.
// Dry-run/diff is a client-side plan; it performs no API writes.
func Restore(ctx context.Context, c dynamic.Interface, resources []Resource, a Archive, dryRun bool, out io.Writer) error {
	if e := a.Validate(); e != nil {
		return e
	}
	available := map[schema.GroupVersionResource]Resource{}
	for _, r := range resources {
		available[r.GVR] = r
	}
	// Preflight all kinds before creating anything.
	for _, e := range a.Payload.Entries {
		if e.Resource.Group == "" {
			continue
		}
		r, ok := available[e.Resource]
		if !ok || r.Kind != e.Object.GetKind() || r.Namespaced != (e.Object.GetNamespace() != "") {
			return fmt.Errorf("target API missing or incompatible: %s", e.Resource)
		}
	}
	for _, e := range Ordered(a) {
		u := e.Object.DeepCopy()
		ri := c.Resource(e.Resource).Namespace(u.GetNamespace())
		old, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		action := "create"
		var updated *unstructured.Unstructured
		if err == nil {
			updated = old.DeepCopy()
			for k, v := range u.Object {
				if k != "metadata" {
					updated.Object[k] = v
				}
			}
			// Preserve unrelated target metadata; archived labels/annotations win.
			labels := updated.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			for k, v := range u.GetLabels() {
				labels[k] = v
			}
			if len(labels) > 0 {
				updated.SetLabels(labels)
			}
			annotations := updated.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			for k, v := range u.GetAnnotations() {
				annotations[k] = v
			}
			if len(annotations) > 0 {
				updated.SetAnnotations(annotations)
			}
			if reflect.DeepEqual(Clean(old).Object, Clean(updated).Object) {
				action = "unchanged"
			} else {
				action = "update"
			}
		}
		// Never print bodies: Secrets and confidential CR fields may be present.
		before := "absent"
		if old != nil {
			before = hash(Clean(old))
		}
		after := hash(u)
		if updated != nil {
			after = hash(Clean(updated))
		}
		if _, err = fmt.Fprintf(out, "%s %s before=%s after=%s\n", action, identity(e), before, after); err != nil {
			return err
		}
		if dryRun || action == "unchanged" {
			continue
		}
		if action == "create" {
			_, err = ri.Create(ctx, u, metav1.CreateOptions{})
		} else {
			_, err = ri.Update(ctx, updated, metav1.UpdateOptions{})
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", action, identity(e), err)
		}
	}
	return nil
}
func Encode(a Archive) ([]byte, error) {
	if e := a.Validate(); e != nil {
		return nil, e
	}
	b, err := json.Marshal(a)
	if len(b) > 256<<20 {
		return nil, fmt.Errorf("archive exceeds 256 MiB")
	}
	return b, err
}
