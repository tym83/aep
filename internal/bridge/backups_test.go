package bridge

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	fake "k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestBackupPlanSchemaAndOwnership(t *testing.T) {
	ctx := context.Background()
	d := fake.NewSimpleDynamicClient(runtime.NewScheme())
	r := &Reconciler{Dynamic: d, TenantNamespace: "tenant-a-prod", Policy: policy.Config{BackupPolicy: "daily", Backup: &policy.BackupConfig{ClassName: "aep-a-prod", Schedule: "0 2 * * *", RetentionPolicy: "30d"}}}
	c := &api.ServiceClaim{ObjectMeta: metav1.ObjectMeta{UID: "claim"}, Spec: api.ServiceClaimSpec{Kind: "Postgres"}}
	app := &unstructured.Unstructured{}
	app.SetName("db")
	for i := 0; i < 2; i++ {
		if e := r.backupPlan(ctx, c, app); e != nil {
			t.Fatal(e)
		}
	}
	p, e := d.Resource(Plans).Namespace(r.TenantNamespace).Get(ctx, "db", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if s, _, _ := unstructured.NestedString(p.Object, "spec", "schedule", "cron"); s != "0 2 * * *" {
		t.Fatal(p)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(p.Object, "spec", "retention"); ok {
		t.Fatal("Plan has no retention field")
	}
	c.UID = "other"
	if e = r.backupPlan(ctx, c, app); e == nil {
		t.Fatal("adopted foreign plan")
	}
}
