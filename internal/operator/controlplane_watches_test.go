package operator

import (
	"context"
	"reflect"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestControlPlanesForObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cp, other := controlPlane(), controlPlane()
	other.Name, other.UID, other.Spec.Tenant.Name = "cp2", "cp2-uid", "cp2"
	r := &ControlPlaneReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, other).Build()}
	for _, tc := range []struct {
		name, ns string
		labels   map[string]string
		want     []string
	}{
		{name: "owned Tenant in the parent namespace", ns: "tenant-root", labels: map[string]string{ControlPlaneLabel: "cp-uid"}, want: []string{"default"}},
		{name: "mirror Service in the tenant namespace", ns: "tenant-aepcp", want: []string{"default"}},
		{name: "other tenant namespace", ns: "tenant-cp2", want: []string{"cp2"}},
		{name: "label wins over namespace", ns: "tenant-aepcp", labels: map[string]string{ControlPlaneLabel: "cp2-uid"}, want: []string{"cp2"}},
		{name: "unrelated", ns: "tenant-root"},
		{name: "foreign control plane", ns: "tenant-root", labels: map[string]string{ControlPlaneLabel: "gone"}},
		{name: "organization object in a coinciding namespace", ns: "tenant-aepcp", labels: map[string]string{OwnerLabel: "org-uid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := obj("v1", "Service", tc.ns, "svc", nil)
			u.SetLabels(tc.labels)
			var got []string
			for _, req := range r.controlPlanesForObject(context.Background(), u) {
				got = append(got, req.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestControlPlanesForOrganization(t *testing.T) {
	r := &ControlPlaneReconciler{Options: Options{DefaultControlPlane: "default"}}
	o := organization()
	if got := r.controlPlanesForOrganization(context.Background(), o); !reflect.DeepEqual(got, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "default"}}}) {
		t.Fatal(got)
	}
	o.Spec.ControlPlaneRef = "cp2"
	if got := r.controlPlanesForOrganization(context.Background(), o); !reflect.DeepEqual(got, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "cp2"}}}) {
		t.Fatal(got)
	}
	if got := r.controlPlanesForOrganization(context.Background(), &corev1.Service{}); got != nil {
		t.Fatal(got)
	}
}

func TestObjectChangePredicate(t *testing.T) {
	p := objectChangePredicate()
	old := obj("v1", "Service", "ns", "svc", M{"clusterIP": "10.0.0.1"})
	same := old.DeepCopy()
	same.SetResourceVersion("2")
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: same}) {
		t.Fatal("metadata churn must not enqueue")
	}
	changed := old.DeepCopy()
	changed.Object["status"] = M{"loadBalancer": M{"ingress": []any{M{"ip": "192.0.2.1"}}}}
	relabelled := old.DeepCopy()
	relabelled.SetLabels(map[string]string{"cluster.x-k8s.io/tenant-service-name": "gateway-default"})
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) || !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: relabelled}) {
		t.Fatal("status and label changes enqueue")
	}
	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("creation and deletion enqueue")
	}
}
