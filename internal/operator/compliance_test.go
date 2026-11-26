package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/tym83/aep/internal/compliance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestExceptionExpiry(t *testing.T) {
	now := time.Now().UTC()
	objects := []runtime.Object{}
	for _, av := range []string{"kyverno.io/v2", "policies.kyverno.io/v1beta1"} {
		for name, expiry := range map[string]string{"expired": now.Add(-time.Minute).Format(time.RFC3339), "active": now.Add(time.Hour).Format(time.RFC3339)} {
			u := obj(av, "PolicyException", "aep-system", name, nil)
			u.SetAnnotations(map[string]string{"aep.aenix.io/expires": expiry})
			u.SetUID("test-uid")
			u.SetResourceVersion("123")
			objects = append(objects, u)
		}
	}
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	d.PrependReactor("delete", "policyexceptions", func(a ktesting.Action) (bool, runtime.Object, error) {
		opts := a.(ktesting.DeleteAction).GetDeleteOptions()
		if opts.Preconditions == nil || *opts.Preconditions.UID != "test-uid" || *opts.Preconditions.ResourceVersion != "123" {
			t.Fatal("unprotected delete")
		}
		return false, nil, nil
	})
	c := &Cluster{Dynamic: d}
	if err := cleanExceptions(context.Background(), c, metav1.NewTime(now)); err != nil {
		t.Fatal(err)
	}
	for _, av := range []string{"kyverno.io/v2", "policies.kyverno.io/v1beta1"} {
		items, err := c.list(context.Background(), av, "policyexceptions", "aep-system")
		if err != nil || len(items) != 1 || items[0].GetName() != "active" {
			t.Fatal(items, err)
		}
	}
}
func TestCollectComplianceIntervalAndFailure(t *testing.T) {
	now := time.Now().UTC()
	o := organization()
	old := compliance.Aggregate("prod", nil, now.Add(-time.Minute))
	raw, _ := json.Marshal(old)
	cm := obj("v1", "ConfigMap", "aep-system", "compliance-acme-prod", nil)
	cm.SetLabels(map[string]string{OwnerLabel: string(o.UID)})
	cm.Object["data"] = M{"snapshot.json": string(raw)}
	mgmt := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), cm)}
	report := obj("wgpolicyk8s.io/v1alpha2", "PolicyReport", "apps", "test", nil)
	report.Object["results"] = []any{M{"policy": "baseline", "result": "fail"}}
	tier := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr("wgpolicyk8s.io/v1alpha2", "policyreports"): "PolicyReportList", gvr("wgpolicyk8s.io/v1alpha2", "clusterpolicyreports"): "ClusterPolicyReportList"}, report)}
	s := &session{r: &Reconciler{Management: mgmt, Options: Options{Namespace: "aep-system"}}, o: o}
	if err := s.collectCompliance(context.Background(), tier, "prod", now); err != nil {
		t.Fatal(err)
	}
	if len(tier.Dynamic.(*dynamicfake.FakeDynamicClient).Actions()) != 0 {
		t.Fatal("collected before 5 minutes")
	}
	// SSA is not implemented by the fake, so capture the replacement and inspect it.
	mgmt.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("patch", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
		var u unstructured.Unstructured
		if err := json.Unmarshal(a.(ktesting.PatchAction).GetPatch(), &u.Object); err != nil {
			t.Fatal(err)
		}
		data, _, _ := unstructured.NestedString(u.Object, "data", "snapshot.json")
		var got compliance.Snapshot
		_ = json.Unmarshal([]byte(data), &got)
		if got.Policies["baseline"].Fail != 1 || !got.CollectedAt.Equal(now.Add(5*time.Minute)) {
			t.Fatal(got)
		}
		return true, &u, nil
	})
	if err := s.collectCompliance(context.Background(), tier, "prod", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	tier.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "clusterpolicyreports", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("tier unavailable")
	})
	before := len(mgmt.Dynamic.(*dynamicfake.FakeDynamicClient).Actions())
	if err := s.collectCompliance(context.Background(), tier, "prod", now.Add(10*time.Minute)); err == nil {
		t.Fatal("partial collection published")
	}
	for _, action := range mgmt.Dynamic.(*dynamicfake.FakeDynamicClient).Actions()[before:] {
		if action.GetVerb() == "patch" || action.GetVerb() == "create" {
			t.Fatal("overwrote last good snapshot")
		}
	}

}
