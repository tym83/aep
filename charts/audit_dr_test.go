package charts

import (
	"strings"
	"testing"
)

func TestAuditDRRestrictedPods(t *testing.T) {
	for _, chart := range []string{"aep-audit-forwarder", "aep-cp-backup"} {
		t.Run(chart, func(t *testing.T) {
			out, err := render(t, chart, "global.aepVersion=sha-1234567")
			if err != nil {
				t.Fatal(out, err)
			}
			docs := resources(t, out)
			var pod map[string]any
			if chart == "aep-audit-forwarder" {
				spec := docs["Deployment"]["spec"].(map[string]any)
				if spec["replicas"] != float64(1) || spec["strategy"].(map[string]any)["type"] != "Recreate" {
					t.Fatal("spool permits multiple writers")
				}
				pod = spec["template"].(map[string]any)["spec"].(map[string]any)
			} else {
				spec := docs["CronJob"]["spec"].(map[string]any)
				if spec["concurrencyPolicy"] != "Forbid" {
					t.Fatal("overlapping backups")
				}
				pod = spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			}
			security := pod["securityContext"].(map[string]any)
			if security["runAsNonRoot"] != true || security["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" {
				t.Fatal(security)
			}
			for _, raw := range pod["containers"].([]any) {
				c := raw.(map[string]any)
				sc := c["securityContext"].(map[string]any)
				if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false || sc["capabilities"].(map[string]any)["drop"].([]any)[0] != "ALL" {
					t.Fatal(sc)
				}
				if c["image"] != "ghcr.io/tym83/"+chart+":sha-1234567" {
					t.Fatal("mutable image")
				}
			}
			for _, raw := range docs["ClusterRole"]["rules"].([]any) {
				r := raw.(map[string]any)
				for _, v := range r["verbs"].([]any) {
					if v != "get" && v != "list" && v != "watch" {
						t.Fatal("excessive RBAC")
					}
				}
			}
		})
	}
}

func TestEtcdBackupCronJob(t *testing.T) {
	out, err := render(t, "aep-cp-backup", "ageRecipient=age1test", "s3Endpoint=https://s3.example", "s3Bucket=dr", "etcd.enabled=true", "etcd.namespace=tenant-aep-cp", "etcd.cluster=kubernetes-aepcp")
	if err != nil {
		t.Fatal(out, err)
	}
	for _, want := range []string{"name: test-etcd", "namespace: tenant-aep-cp", `policy.cozystack.io/allow-to-etcd: "true"`, "secretName: kubernetes-aepcp-datastore-certificate", "name: kubernetes-aepcp-datastore-config, key: DB_SCHEMA", "--etcd-prefix=/$(DB_SCHEMA)", "- etcd-backup", "automountServiceAccountToken: false"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if out, err = render(t, "aep-cp-backup", "ageRecipient=age1test", "etcd.enabled=true", "etcd.namespace=ns"); err == nil {
		t.Fatal("etcd.cluster must be required", out)
	}
}
