// Package rcaeval runs reversible stand experiments and scores RCA reports.
package rcaeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tym83/aep/internal/operator"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

type Target struct{ Organization, Tier, Quota, App, WorkloadImage string }

func (t Target) Namespace() string { return operator.TenantNamespace(t.Organization, t.Tier) }

type Journal struct {
	Resource   schema.GroupVersionResource `json:"resource"`
	Namespace  string                      `json:"namespace"`
	Name       string                      `json:"name"`
	UID        types.UID                   `json:"uid"`
	Path       string                      `json:"path"`
	Before     any                         `json:"before"`
	Injected   any                         `json:"injected"`
	Deployment string                      `json:"deployment,omitempty"`
}
type Undo func(context.Context) error
type Injector interface {
	Inject(context.Context, dynamic.Interface, Target, string) (Undo, error)
}
type QuotaInjector struct{}
type SimulatedDiskInjector struct{}
type StubInjector struct{}

var quotaResource = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}
var deploymentResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
var appResource = schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: "postgreses"}

func (StubInjector) Inject(context.Context, dynamic.Interface, Target, string) (Undo, error) {
	return nil, fmt.Errorf("TODO: implement and review stand injector")
}
func save(path string, j Journal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}
func apply(ctx context.Context, c dynamic.Interface, j Journal) error {
	ops := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": j.UID}, {"op": "test", "path": j.Path, "value": j.Before}, {"op": "replace", "path": j.Path, "value": j.Injected}}
	b, _ := json.Marshal(ops)
	_, err := c.Resource(j.Resource).Namespace(j.Namespace).Patch(ctx, j.Name, types.JSONPatchType, b, metav1.PatchOptions{})
	return err
}

// Restore refuses to overwrite concurrent edits or a replacement object. The
// journal remains on disk until restoration succeeds, including after a crash.
func Restore(ctx context.Context, c dynamic.Interface, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var j Journal
	if err = json.Unmarshal(b, &j); err != nil {
		return err
	}
	// Journals are local operator input, but limit them to the supported resources.
	if (j.Resource != quotaResource || j.Path != "/spec/hard") && (j.Resource != appResource || j.Path != "/metadata/annotations") {
		return fmt.Errorf("unsupported recovery journal")
	}
	current, err := c.Resource(j.Resource).Namespace(j.Namespace).Get(ctx, j.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.GetUID() != j.UID {
		return fmt.Errorf("object was replaced; manual recovery required")
	}
	var actual any
	if j.Path == "/spec/hard" {
		actual, _, _ = unstructured.NestedFieldCopy(current.Object, "spec", "hard")
	} else {
		actual, _, _ = unstructured.NestedFieldCopy(current.Object, "metadata", "annotations")
	}
	same := func(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return string(x) == string(y) }
	// Remove the probe and its dependents before reopening pod admission.
	if j.Deployment != "" {
		deployments := c.Resource(deploymentResource).Namespace(j.Namespace)
		probe, getErr := deployments.Get(ctx, j.Deployment, metav1.GetOptions{})
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return getErr
		}
		if getErr == nil {
			if probe.GetLabels()["aep.aenix.io/eval-probe"] != j.Deployment {
				return fmt.Errorf("probe identity changed; manual recovery required")
			}
			uid := probe.GetUID()
			propagation := metav1.DeletePropagationForeground
			if err = deployments.Delete(ctx, j.Deployment, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			for {
				_, err = deployments.Get(ctx, j.Deployment, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					break
				}
				if err != nil {
					return err
				}
				if err = wait(ctx, 100*time.Millisecond); err != nil {
					return err
				}
			}
		}
	}
	if !same(actual, j.Before) {
		if !same(actual, j.Injected) {
			return fmt.Errorf("concurrent edit; manual recovery required using %s", path)
		}
		j.Before, j.Injected = j.Injected, j.Before
		if err = apply(ctx, c, j); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
func (QuotaInjector) Inject(ctx context.Context, c dynamic.Interface, t Target, path string) (Undo, error) {
	o, err := c.Resource(quotaResource).Namespace(t.Namespace()).Get(ctx, t.Quota, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if scopes, _, _ := unstructured.NestedSlice(o.Object, "spec", "scopes"); len(scopes) > 0 {
		return nil, fmt.Errorf("quota must be unscoped")
	}
	if selector, _, _ := unstructured.NestedMap(o.Object, "spec", "scopeSelector"); len(selector) > 0 {
		return nil, fmt.Errorf("quota must be unscoped")
	}
	before, found, err := unstructured.NestedMap(o.Object, "spec", "hard")
	if err != nil || !found {
		return nil, fmt.Errorf("quota hard is missing")
	}
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	after["pods"] = "0"
	name := fmt.Sprintf("aep-eval-%d", time.Now().UnixNano())
	j := Journal{Resource: quotaResource, Namespace: t.Namespace(), Name: t.Quota, UID: o.GetUID(), Path: "/spec/hard", Before: before, Injected: after, Deployment: name}
	if err = save(path, j); err != nil {
		return nil, err
	}
	undo := func(ctx context.Context) error { return Restore(ctx, c, path) }
	if err = apply(ctx, c, j); err != nil {
		return injectionFailure(path, undo, err)
	}
	// Admission observes status.hard, which is asynchronously reconciled.
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		current, getErr := c.Resource(quotaResource).Namespace(t.Namespace()).Get(readyCtx, t.Quota, metav1.GetOptions{})
		if getErr != nil {
			return undo, getErr
		}
		hard, _, _ := unstructured.NestedString(current.Object, "status", "hard", "pods")
		if hard == "0" {
			break
		}
		if err = wait(readyCtx, 100*time.Millisecond); err != nil {
			return undo, fmt.Errorf("quota admission did not converge: %w", err)
		}
	}
	d := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name, "labels": map[string]any{"aep.aenix.io/eval-probe": name}}, "spec": map[string]any{"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"aep-eval": name}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"aep-eval": name}}, "spec": map[string]any{"automountServiceAccountToken": false, "securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532), "seccompProfile": map[string]any{"type": "RuntimeDefault"}}, "containers": []any{map[string]any{"name": "probe", "image": t.WorkloadImage, "securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}, "resources": map[string]any{"requests": map[string]any{"cpu": "1m", "memory": "8Mi"}, "limits": map[string]any{"cpu": "10m", "memory": "16Mi"}}}}}}}}}
	_, err = c.Resource(deploymentResource).Namespace(t.Namespace()).Create(ctx, d, metav1.CreateOptions{})
	return undo, err
}
func (SimulatedDiskInjector) Inject(ctx context.Context, c dynamic.Interface, t Target, path string) (Undo, error) {
	o, err := c.Resource(appResource).Namespace(t.Namespace()).Get(ctx, t.App, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	before, found, _ := unstructured.NestedMap(o.Object, "metadata", "annotations")
	if !found {
		return nil, fmt.Errorf("simulation requires an existing annotations map")
	}
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	after["aep.aenix.io/eval-simulated-failure"] = "postgres-disk-full (simulated; no disk writes)"
	j := Journal{Resource: appResource, Namespace: t.Namespace(), Name: t.App, UID: o.GetUID(), Path: "/metadata/annotations", Before: before, Injected: after}
	if err = save(path, j); err != nil {
		return nil, err
	}
	undo := func(ctx context.Context) error { return Restore(ctx, c, path) }
	if err = apply(ctx, c, j); err != nil {
		return injectionFailure(path, undo, err)
	}
	return undo, nil
}

// An invalid JSON patch (including a failed test) is rejected atomically. No
// injected state exists to restore. Ambiguous transport errors retain recovery.
func injectionFailure(path string, undo Undo, err error) (Undo, error) {
	if apierrors.IsInvalid(err) {
		if removeErr := os.Remove(path); removeErr != nil {
			return undo, errors.Join(err, removeErr)
		}
		return nil, err
	}
	return undo, err
}
