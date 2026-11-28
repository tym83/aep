package operator

import (
	"context"
	"encoding/json"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"reflect"
	"strings"
)

const OwnerLabel = "aep.aenix.io/organization"

// ControlPlaneLabel marks objects owned by a ControlPlane with its UID.
const ControlPlaneLabel = "aep.aenix.io/control-plane"
const finalizer = "aep.aenix.io/organization-cleanup"

type M = map[string]any
type Cluster struct {
	Dynamic dynamic.Interface
	Kube    kubernetes.Interface
	Config  *rest.Config
}

func NewCluster(c *rest.Config) (*Cluster, error) {
	c = rest.CopyConfig(c)
	c.Timeout = 30 * 1000000000
	d, e := dynamic.NewForConfig(c)
	if e != nil {
		return nil, e
	}
	k, e := kubernetes.NewForConfig(c)
	return &Cluster{d, k, c}, e
}
func gvr(apiVersion, resource string) schema.GroupVersionResource {
	g, v, _ := strings.Cut(apiVersion, "/")
	if v == "" {
		v = g
		g = ""
	}
	return schema.GroupVersionResource{Group: g, Version: v, Resource: resource}
}
func (c *Cluster) resource(av, r, ns string) dynamic.ResourceInterface {
	return c.Dynamic.Resource(gvr(av, r)).Namespace(ns)
}
func obj(av, kind, ns, name string, spec M) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: M{"apiVersion": av, "kind": kind, "metadata": M{"name": name}}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

// ownerKey is the label that marks a management or remote object as owned by
// one AEP resource. Organizations and ControlPlanes use different labels, so
// neither can adopt the other's objects and watches never confuse them.
type ownerKey struct {
	label, value, kind string
}

func organizationOwner(o *api.Organization) ownerKey {
	return ownerKey{label: OwnerLabel, value: string(o.UID), kind: "organization"}
}

func controlPlaneOwner(cp *api.ControlPlane) ownerKey {
	return ownerKey{label: ControlPlaneLabel, value: string(cp.UID), kind: "control plane"}
}

// foreign reports a label of another owner kind on the labels of an object
// owned by k. Organization ownership predates ControlPlanes and stays as it was.
func (k ownerKey) foreign(labels map[string]string) bool {
	return k.label == ControlPlaneLabel && labels[OwnerLabel] != ""
}

func (c *Cluster) apply(ctx context.Context, o *api.Organization, r string, u *unstructured.Unstructured) error {
	return c.applyAs(ctx, organizationOwner(o), r, u)
}

// applyAs creates or updates u for owner and refuses objects owned by anyone else.
func (c *Cluster) applyAs(ctx context.Context, owner ownerKey, r string, u *unstructured.Unstructured) error {
	if owner.label == ControlPlaneLabel && owner.value == "" {
		return fmt.Errorf("refusing to apply %s %s/%s without a control plane UID", u.GetKind(), u.GetNamespace(), u.GetName())
	}
	if owner.foreign(u.GetLabels()) {
		return fmt.Errorf("refusing to apply %s %s/%s with labels of another owner", u.GetKind(), u.GetNamespace(), u.GetName())
	}
	ri := c.resource(u.GetAPIVersion(), r, u.GetNamespace())
	old, e := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return e
	}
	if e == nil && (old.GetLabels()[owner.label] != owner.value || owner.foreign(old.GetLabels())) {
		return fmt.Errorf("refusing to adopt %s %s/%s without %s ownership", u.GetKind(), u.GetNamespace(), u.GetName(), owner.kind)
	}
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[owner.label] = owner.value
	u.SetLabels(labels)
	// Create with normal API semantics to avoid adopting a racing unowned object through SSA.
	if errors.IsNotFound(e) {
		// Name the manager so later server-side applies own every field.
		_, e = ri.Create(ctx, u, metav1.CreateOptions{FieldManager: "aep-operator"})
		return e
	}
	// Cozystack serves an aggregated API; preserve defaults and use resourceVersion updates.
	if u.GetAPIVersion() == appsAPI {
		updated := old.DeepCopy()
		mergeObject(updated.Object, u.Object)
		replaceOwnedSpec(updated.Object, u.Object)
		if reflect.DeepEqual(old.Object, updated.Object) {
			return nil
		}
		_, e = ri.Update(ctx, updated, metav1.UpdateOptions{})
		return e
	}
	u.SetResourceVersion(old.GetResourceVersion())
	// Do not attach ownerReferences: garbage collection must not bypass ordered finalization.
	data, e := json.Marshal(u.Object)
	if e != nil {
		return e
	}
	force := true
	_, e = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "aep-operator", Force: &force})
	return e
}

// ensureNamespace creates a namespace that may already exist without the
// organization's ownership, e.g. created by a chart with createNamespace. The
// operator only places objects inside it and never adopts or labels it.
func (c *Cluster) ensureNamespace(ctx context.Context, name string) error {
	ri := c.resource("v1", "namespaces", "")
	if _, e := ri.Get(ctx, name, metav1.GetOptions{}); e == nil || !errors.IsNotFound(e) {
		return e
	}
	_, e := ri.Create(ctx, obj("v1", "Namespace", "", name, nil), metav1.CreateOptions{})
	if errors.IsAlreadyExists(e) {
		return nil
	}
	return e
}

// switchChartSource moves an existing HelmRelease between spec.chart and
// spec.chartRef in one JSON patch. Flux accepts exactly one of them, and a
// server-side apply cannot drop a field another manager still owns (objects
// created before the operator named its manager).
func (c *Cluster) switchChartSource(ctx context.Context, hr *unstructured.Unstructured) error {
	want, stale := "chartRef", "chart"
	if _, ok := hr.Object["spec"].(M)["chart"]; ok {
		want, stale = "chart", "chartRef"
	}
	ri := c.resource(hr.GetAPIVersion(), "helmreleases", hr.GetNamespace())
	old, e := ri.Get(ctx, hr.GetName(), metav1.GetOptions{})
	if errors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if _, has, _ := unstructured.NestedFieldNoCopy(old.Object, "spec", stale); !has {
		return nil
	}
	patch, e := json.Marshal([]M{{"op": "remove", "path": "/spec/" + stale}, {"op": "add", "path": "/spec/" + want, "value": hr.Object["spec"].(M)[want]}})
	if e != nil {
		return e
	}
	_, e = ri.Patch(ctx, hr.GetName(), types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "aep-operator"})
	return e
}
func (c *Cluster) get(ctx context.Context, av, r, ns, name string) (*unstructured.Unstructured, error) {
	return c.resource(av, r, ns).Get(ctx, name, metav1.GetOptions{})
}
func (c *Cluster) list(ctx context.Context, av, r, ns string) ([]unstructured.Unstructured, error) {
	l, e := c.resource(av, r, ns).List(ctx, metav1.ListOptions{})
	if e != nil {
		return nil, e
	}
	return l.Items, nil
}
func FromSecret(ctx context.Context, c *Cluster, ref string) (*Cluster, error) {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" {
		return nil, fmt.Errorf("secret reference must be namespace/name")
	}
	s, e := c.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return nil, e
	}
	cfg, e := clientcmd.RESTConfigFromKubeConfig(s.Data["super-admin.svc"])
	if e != nil {
		return nil, e
	}
	return NewCluster(cfg)
}
func ready(u *unstructured.Unstructured) bool {
	cs, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, v := range cs {
		c, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "Ready" && c["status"] == "True" {
			gen, ok := c["observedGeneration"].(int64)
			if !ok {
				gen, _, _ = unstructured.NestedInt64(u.Object, "status", "observedGeneration")
			}
			return gen >= u.GetGeneration()
		}
	}
	return false
}

type pendingError struct{ message string }

func (e *pendingError) Error() string { return e.message }

func pending(format string, a ...any) error {
	return &pendingError{message: fmt.Sprintf(format, a...)}
}

const appsAPI = "apps.cozystack.io/v1alpha1"
const helmAPI = "helm.toolkit.fluxcd.io/v2"
const ocAPI = "openchoreo.dev/v1alpha1"

func mergeObject(dst, src map[string]any) {
	for k, v := range src {
		m, ok := v.(map[string]any)
		if ok {
			existing, ok := dst[k].(map[string]any)
			if !ok {
				existing = map[string]any{}
				dst[k] = existing
			}
			mergeObject(existing, m)
		} else {
			dst[k] = v
		}
	}
}

// replaceOwnedSpec replaces every top-level spec field the operator sets
// instead of merging it, so keys removed from the desired state (for example
// a quota) disappear from the object. Fields the operator does not set keep
// the values Cozystack defaulted.
func replaceOwnedSpec(dst, desired map[string]any) {
	want, ok := desired["spec"].(map[string]any)
	if !ok {
		return
	}
	got, ok := dst["spec"].(map[string]any)
	if !ok {
		dst["spec"] = want
		return
	}
	for k, v := range want {
		got[k] = v
	}
}
