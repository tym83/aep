package operator

import (
	"context"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func labelledTenant(name string, labels map[string]string) *unstructured.Unstructured {
	u := obj(appsAPI, "Tenant", "tenant-root", name, M{"etcd": false})
	u.SetLabels(labels)
	return u
}

func TestApplyOwnerKeys(t *testing.T) {
	ctx := context.Background()
	o, cp := organization(), controlPlane()
	c := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		labelledTenant("org-owned", map[string]string{OwnerLabel: string(o.UID)}),
		labelledTenant("cp-owned", map[string]string{ControlPlaneLabel: string(cp.UID)}),
		labelledTenant("both", map[string]string{OwnerLabel: string(o.UID), ControlPlaneLabel: string(cp.UID)}),
		labelledTenant("unowned", nil),
		labelledTenant("other-cp", map[string]string{ControlPlaneLabel: "other-uid"}),
	)}
	for _, tc := range []struct {
		name, object string
		labels       map[string]string
		asCP         bool
		want         string
	}{
		{name: "organization updates its own", object: "org-owned"},
		{name: "organization creates", object: "new-org"},
		{name: "organization refuses control plane object", object: "cp-owned", want: "without organization ownership"},
		{name: "organization refuses unowned", object: "unowned", want: "without organization ownership"},
		// Organization ownership is unchanged: a second label does not matter.
		{name: "organization keeps dual-labelled", object: "both"},
		{name: "control plane updates its own", object: "cp-owned", asCP: true},
		{name: "control plane creates", object: "new-cp", asCP: true},
		{name: "control plane refuses organization object", object: "org-owned", asCP: true, want: "without control plane ownership"},
		{name: "control plane refuses dual-labelled", object: "both", asCP: true, want: "without control plane ownership"},
		{name: "control plane refuses unowned", object: "unowned", asCP: true, want: "without control plane ownership"},
		{name: "control plane refuses another control plane", object: "other-cp", asCP: true, want: "without control plane ownership"},
		{name: "control plane refuses organization label", object: "new-cp-2", labels: map[string]string{OwnerLabel: "x"}, asCP: true, want: "labels of another owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := labelledTenant(tc.object, tc.labels)
			u.Object["spec"] = M{"etcd": true}
			var err error
			if tc.asCP {
				err = c.applyAs(ctx, controlPlaneOwner(cp), "tenants", u)
			} else {
				err = c.apply(ctx, o, "tenants", u)
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			live, err := c.get(ctx, appsAPI, "tenants", "tenant-root", tc.object)
			if err != nil {
				t.Fatal(err)
			}
			labels := live.GetLabels()
			if tc.asCP && (labels[ControlPlaneLabel] != string(cp.UID) || labels[OwnerLabel] != "") {
				t.Fatalf("control plane object labels %v", labels)
			}
			if !tc.asCP && labels[OwnerLabel] != string(o.UID) {
				t.Fatalf("organization object labels %v", labels)
			}
			if etcd, _, _ := unstructured.NestedBool(live.Object, "spec", "etcd"); !etcd {
				t.Fatal("spec not applied")
			}
		})
	}
	t.Run("control plane without UID", func(t *testing.T) {
		anonymous := &api.ControlPlane{}
		err := c.applyAs(ctx, controlPlaneOwner(anonymous), "tenants", labelledTenant("unowned", nil))
		if err == nil || !strings.Contains(err.Error(), "without a control plane UID") {
			t.Fatal(err)
		}
	})
}
