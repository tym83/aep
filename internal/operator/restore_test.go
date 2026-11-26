package operator

import (
	"context"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"testing"
	"time"
)

func TestRestoreVerificationCompletionAndOwnership(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	ns := "tenant-acme-prod"
	name := "aep-test-restore"
	report := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "aep-system", UID: "report-uid"}, Data: map[string]string{"organization": "acme", "namespace": ns, "source": "live", "sourceUID": "live-uid", "kind": "Postgres", "temporary": name, "phase": "Pending", "backup": "backup", "started": now.Format(time.RFC3339)}}
	target := obj(appsAPI, "Postgres", ns, name, M{})
	target.SetUID("temp-uid")
	target.SetLabels(map[string]string{verificationLabel: "report-uid"})
	target.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True", "observedGeneration": int64(0)}}}
	job := obj(backupsAPI, "RestoreJob", ns, name, M{"backupRef": M{"name": "backup"}, "targetApplicationRef": M{"apiGroup": "apps.cozystack.io", "kind": "Postgres", "name": name}})
	job.SetLabels(map[string]string{verificationLabel: "report-uid"})
	job.Object["status"] = M{"phase": "Succeeded"}
	live := obj(appsAPI, "Postgres", ns, "live", M{})
	live.SetUID("live-uid")
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), target, job, live)
	k := kubefake.NewClientset(report, &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "postgres-" + name + "-rw", Namespace: ns}, Spec: core.ServiceSpec{ClusterIP: "192.0.2.2"}})
	v := &RestoreVerifier{Reconciler: &Reconciler{Management: &Cluster{Dynamic: d, Kube: k}}, Check: func(_ context.Context, h string, p int32) error {
		if h != "192.0.2.2" || p != 5432 {
			t.Fatal(h, p)
		}
		return nil
	}}
	for i := 0; i < 4; i++ {
		if e := v.advance(ctx, report, now); e != nil {
			t.Fatal(e)
		}
	}
	if report.Data["result"] != "Passed" || report.Data["phase"] != "Complete" {
		t.Fatal(report.Data)
	}
	if _, e := d.Resource(gvr(appsAPI, "postgreses")).Namespace(ns).Get(ctx, "live", metav1.GetOptions{}); e != nil {
		t.Fatal("live application was removed")
	}
	// Even a validly-shaped report must not clean up a colliding unowned app.
	report.Data["phase"] = "Cleanup"
	foreign := target.DeepCopy()
	foreign.SetLabels(nil)
	_, _ = d.Resource(gvr(appsAPI, "postgreses")).Namespace(ns).Create(ctx, foreign, metav1.CreateOptions{})
	if e := v.advance(ctx, report, now); e == nil {
		t.Fatal("deleted foreign application")
	}
	report.Data["namespace"] = "tenant-acme-nonprod"
	if e := v.advance(ctx, report, now); e == nil {
		t.Fatal("accepted cross-tier restore")
	}
}
func TestBackupPolicyRetention(t *testing.T) {
	ctx := context.Background()
	o := organization()
	tier := o.Spec.Tiers["prod"]
	tier.Services.BackupPolicy = "daily"
	tier.Services.AllowedKinds = []string{"Postgres"}
	o.Spec.Tiers["prod"] = tier
	source := obj(backupsAPI, "BackupClass", "", "cozy-default", M{"strategies": []any{M{"application": M{"kind": "Postgres"}, "strategyRef": M{"apiGroup": "strategy.backups.cozystack.io", "kind": "CNPG", "name": "default"}}}})
	strategy := obj(strategyAPI, "CNPG", "", "default", M{"template": M{"barmanObjectStore": M{"retentionPolicy": "7d"}}})
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), source, strategy)
	k := kubefake.NewClientset(&core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "aep-system"}, Data: map[string]string{"schedule": "0 2 * * *", "backupClassName": "cozy-default", "retentionPolicy": "30d"}})
	s := &session{o: o, r: &Reconciler{Management: &Cluster{Dynamic: d, Kube: k}, Options: Options{Namespace: "aep-system"}}}
	p, e := s.backupPolicy(ctx, "prod")
	if e != nil || p.ClassName != "aep-acme-prod" {
		t.Fatal(p, e)
	}
	clone, e := d.Resource(gvr(strategyAPI, "cnpgs")).Get(ctx, "aep-acme-prod", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	ret, _, _ := unstructured.NestedString(clone.Object, "spec", "template", "barmanObjectStore", "retentionPolicy")
	if ret != "30d" {
		t.Fatal(ret)
	}
}
