package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

func TestBootstrapHardening(t *testing.T) {
	for _, tc := range []struct {
		name, claim, value, labelMode, allowed string
		prune, remains, offends, failDelete    bool
	}{
		{name: "arbitrary-group", claim: "groups", value: "custom", prune: true},
		{name: "mcp", claim: "client_id", value: "service_mcp_client", prune: true},
		{name: "workload-publisher-binding", claim: "client_id", value: "shared", prune: true},
		{name: "unrelated", claim: "client_id", value: "other", prune: true, remains: true},
		{name: "unmanaged", claim: "groups", labelMode: "none", prune: true, remains: true, offends: true},
		{name: "other-release", claim: "groups", labelMode: "foreign", prune: true, remains: true, offends: true},
		{name: "not-helm", claim: "groups", labelMode: "not-helm", prune: true, remains: true, offends: true},
		{name: "aep", claim: "groups", labelMode: "aep", prune: true, remains: true, offends: true},
		{name: "report-only", claim: "groups", remains: true, offends: true},
		{name: "allowed-mcp", claim: "client_id", value: "service_mcp_client", allowed: " service_mcp_client ", prune: true, remains: true},
		{name: "groups-not-clients", claim: "groups", value: "system", allowed: "system", prune: true},
		{name: "delete-error", claim: "groups", prune: true, remains: true, offends: true, failDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			u := obj(ocAPI, "ClusterAuthzRoleBinding", "", tc.name, M{"entitlement": M{"claim": tc.claim, "value": tc.value}})
			labels := map[string]string{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "openchoreo-control-plane"}
			switch tc.labelMode {
			case "none":
				labels = nil
			case "foreign":
				labels["app.kubernetes.io/instance"] = "other"
			case "not-helm":
				labels["app.kubernetes.io/managed-by"] = "other"
			case "aep":
				labels["aep.aenix.io/custom"] = ""
			}
			u.SetLabels(labels)
			u.SetUID("original")
			u.SetResourceVersion("7")
			resource := gvr(ocAPI, "clusterauthzrolebindings")
			d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{resource: "ClusterAuthzRoleBindingList"}, u)
			d.PrependReactor("delete", resource.Resource, func(a ktesting.Action) (bool, runtime.Object, error) {
				p := a.(ktesting.DeleteAction).GetDeleteOptions().Preconditions
				if p == nil || *p.UID != "original" || *p.ResourceVersion != "7" {
					t.Fatal("missing deletion preconditions")
				}
				if tc.failDelete {
					return true, nil, errors.New("denied")
				}
				return false, nil, nil
			})
			recorder := record.NewFakeRecorder(10)
			s := &session{o: organization(), cp: &Cluster{Dynamic: d}, r: &Reconciler{Recorder: recorder, Options: Options{PruneBootstrapBindings: tc.prune, BootstrapBindingSystemClients: tc.allowed}}}
			s.hardenControlPlane(ctx)
			condition := meta.FindStatusCondition(s.o.Status.Conditions, "ControlPlaneHardened")
			if condition == nil || (condition.Status == metav1.ConditionFalse) != tc.offends {
				t.Fatalf("condition: %+v", condition)
			}
			if tc.offends && !strings.Contains(condition.Message, tc.name) {
				t.Fatal(condition.Message)
			}
			_, err := d.Resource(resource).Get(ctx, tc.name, metav1.GetOptions{})
			if (err == nil) != tc.remains {
				t.Fatalf("remains=%v error=%v", tc.remains, err)
			}
			if !tc.remains {
				select {
				case <-recorder.Events:
				default:
					t.Fatal("missing deletion event")
				}
			}
			// A second pass is idempotent; chart recreation is removed on the next pass.
			s.hardenControlPlane(ctx)
			if !tc.remains {
				if _, err := d.Resource(resource).Create(ctx, u, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				s.hardenControlPlane(ctx)
				if _, err := d.Resource(resource).Get(ctx, tc.name, metav1.GetOptions{}); err == nil {
					t.Fatal("recreated binding survived")
				}
			}
		})
	}
	for _, value := range strings.Split(DefaultBootstrapBindingSystemClients, ",") {
		u := obj(ocAPI, "ClusterAuthzRoleBinding", "", "workload-publisher-binding", M{"entitlement": M{"claim": "client_id", "value": value}})
		if bootstrapBindingOffends(u, DefaultBootstrapBindingSystemClients) {
			t.Fatalf("default client %s not exempt", value)
		}
	}
}

func TestHardeningInspectionFailure(t *testing.T) {
	resource := gvr(ocAPI, "clusterauthzrolebindings")
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{resource: "ClusterAuthzRoleBindingList"})
	d.PrependReactor("list", resource.Resource, func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("unavailable") })
	s := &session{o: organization(), cp: &Cluster{Dynamic: d}, r: &Reconciler{}}
	s.hardenControlPlane(context.Background())
	c := meta.FindStatusCondition(s.o.Status.Conditions, "ControlPlaneHardened")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "InspectionFailed" {
		t.Fatalf("condition: %+v", c)
	}
}

func TestOrganizationPublisherBinding(t *testing.T) {
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	c := &Cluster{Dynamic: d}
	for _, name := range []string{"acme", "other"} {
		o := organization()
		o.Name = name
		found := false
		for _, u := range RenderAuthz(o) {
			if u.GetName() != "aep-workload-publisher" {
				continue
			}
			found = true
			if err := c.apply(context.Background(), o, "authzrolebindings", u); err != nil {
				t.Fatal(err)
			}
			got, err := c.get(context.Background(), ocAPI, "authzrolebindings", name, u.GetName())
			if err != nil {
				t.Fatal(err)
			}
			spec := got.Object["spec"].(map[string]any)
			entitlement := spec["entitlement"].(map[string]any)
			mapping := spec["roleMappings"].([]any)[0].(map[string]any)["roleRef"].(map[string]any)
			if got.GetLabels()[OwnerLabel] != string(o.UID) || entitlement["claim"] != "client_id" || entitlement["value"] != "aep-"+name+"-publisher" || mapping["kind"] != "ClusterAuthzRole" || mapping["name"] != "workload-publisher" {
				t.Fatalf("unexpected publisher binding: %+v", got)
			}
		}
		if !found {
			t.Fatal("publisher binding missing")
		}
	}
}
