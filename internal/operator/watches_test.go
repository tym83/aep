package operator

import (
	"context"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestOrganizationsForObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	o := organization()
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(o).Build()}
	for _, tc := range []struct {
		name, namespace, uid string
		want                 bool
	}{
		{"root namespace", "tenant-acme", "", true},
		{"nonprod", "tenant-acme-nonprod", "", true},
		{"prod", "tenant-acme-prod", "", true},
		{"root Tenant label", "tenant-root", string(o.UID), true},
		{"label elsewhere", "other", string(o.UID), true},
		{"deduplicate", "tenant-acme-prod", string(o.UID), true},
		{"UID is not name", "tenant-root", o.Name, false},
		{"unrelated", "tenant-other-prod", "", false},
		{"prefix collision", "tenant-acme2", "", false},
		{"unknown tier", "tenant-acme-extra", "", false},
		{"root unowned", "tenant-root", "", false},
		{"no namespace", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := obj(appsAPI, "Tenant", tc.namespace, "test", nil)
			if tc.uid != "" {
				u.SetLabels(map[string]string{OwnerLabel: tc.uid})
			}
			got := r.organizationsForObject(context.Background(), u)
			if tc.want {
				if len(got) != 1 || got[0].Name != o.Name {
					t.Fatalf("requests=%v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("requests=%v", got)
			}
		})
	}
}

func TestDependencyStatusPredicate(t *testing.T) {
	p := dependencyStatusPredicate()
	for _, kind := range []string{"HelmRelease", "Tenant", "Kubernetes", "KubernetesNodes", "VirtualMachineInstance", "TenantControlPlane"} {
		t.Run(kind, func(t *testing.T) {
			old := obj(appsAPI, kind, "tenant-acme-prod", "test", nil)
			current := old.DeepCopy()
			current.SetResourceVersion("2")
			current.SetLabels(map[string]string{"changed": "true"})
			current.Object["spec"] = M{"changed": true}
			if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
				t.Fatal("metadata/spec update passed")
			}
			current.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True"}}}
			if kind == "TenantControlPlane" {
				current.Object["status"] = M{"kubernetesResources": M{"version": M{"status": "Ready"}}}
			}
			if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
				t.Fatal("readiness change ignored")
			}
			if p.Update(event.UpdateEvent{ObjectOld: current, ObjectNew: current.DeepCopy()}) {
				t.Fatal("unchanged status passed")
			}
			if !p.Create(event.CreateEvent{Object: current}) {
				t.Fatal("initial ready object ignored")
			}
			if p.Create(event.CreateEvent{Object: old}) || p.Delete(event.DeleteEvent{Object: current}) || p.Generic(event.GenericEvent{Object: current}) {
				t.Fatal("non-status event passed")
			}
		})
	}
}

// ControlPlane-owned objects never map to Organizations, even in a namespace
// or with a label that would otherwise match one.
func TestOrganizationsForObjectIgnoresControlPlaneObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	o := organization()
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(o).Build()}
	for _, labels := range []map[string]string{
		{ControlPlaneLabel: "cp-uid"},
		{ControlPlaneLabel: "cp-uid", OwnerLabel: string(o.UID)},
	} {
		for _, ns := range []string{"tenant-acme", "tenant-acme-prod", "tenant-root"} {
			u := obj(helmAPI, "HelmRelease", ns, "aep-cp-openchoreo-control-plane", nil)
			u.SetLabels(labels)
			if got := r.organizationsForObject(context.Background(), u); len(got) != 0 {
				t.Fatalf("%s %v mapped to %v", ns, labels, got)
			}
		}
	}
}
