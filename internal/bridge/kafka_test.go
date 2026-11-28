package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func kafkaClaim(topics string) *api.ServiceClaim {
	return &api.ServiceClaim{ObjectMeta: metav1.ObjectMeta{Name: "events", Namespace: "dp-test", UID: "claim-uid", Generation: 1}, Spec: api.ServiceClaimSpec{Kind: "Kafka", Resource: api.ResourceIdentity{Project: "project", Name: "events", Environment: "dev"},
		Parameters:            map[string]ext.JSON{"version": {Raw: []byte(`"4.1.1"`)}, "topics": {Raw: []byte(topics)}},
		EnvironmentParameters: map[string]ext.JSON{"resourcesPreset": {Raw: []byte(`"t1.small"`)}, "size": {Raw: []byte(`"10Gi"`)}, "replicas": {Raw: []byte(`1`)}}}}
}

func strimzi(t *testing.T, r *Reconciler, kind, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(StrimziAPI)
	u.SetKind(kind)
	if e := r.App.Get(context.Background(), types.NamespacedName{Namespace: kinds.KafkaNamespace, Name: name}, u); e != nil {
		if errors.IsNotFound(e) {
			return nil
		}
		t.Fatal(e)
	}
	return u
}

// markReady simulates Strimzi reporting every object as reconciled.
func markReady(t *testing.T, r *Reconciler, objects ...*unstructured.Unstructured) {
	t.Helper()
	for _, u := range objects {
		status := map[string]any{"observedGeneration": u.GetGeneration(), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Ready", "lastTransitionTime": "2026-11-25T00:00:00Z"}}}
		if u.GetKind() == "KafkaNodePool" {
			replicas, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
			status = map[string]any{"observedGeneration": u.GetGeneration(), "replicas": replicas}
		}
		u.Object["status"] = status
		if e := r.App.Update(context.Background(), u); e != nil {
			t.Fatal(e)
		}
	}
}

func TestKafkaName(t *testing.T) {
	base := policy.Name("cluster", "dp-test", "events", "events")
	if KafkaName(base) != base || len(base) > MaxKafkaName {
		t.Fatal("policy names must pass unchanged", base)
	}
	long := strings.Repeat("a", 70)
	short := KafkaName(long)
	if len(short) > MaxKafkaName || short != KafkaName(long) || short == KafkaName(long+"b") {
		t.Fatal(short)
	}
	for _, n := range []string{short + "-pool", KafkaUserName(short), short + "-kafka-bootstrap"} {
		if len(n) > 52 {
			t.Fatal("Strimzi name too long", n)
		}
	}
	if pod := short + "-" + short + "-pool-9"; len(pod) > 63 {
		t.Fatal("broker pod name too long", pod)
	}
	if n := KafkaTopicResource(short, strings.Repeat("t", policy.MaxKafkaTopicName)); len(n) > 63 {
		t.Fatal(n)
	}
}

func TestKafkaObjects(t *testing.T) {
	for _, tt := range []struct{ replicas, rf, isr int64 }{{1, 1, 1}, {2, 2, 1}, {3, 3, 2}, {5, 3, 2}} {
		t.Run(fmt.Sprint(tt.replicas), func(t *testing.T) {
			spec := KafkaSpec{Version: "4.1.1", Size: "10Gi", Preset: "u1.small", Replicas: tt.replicas, Topics: []KafkaTopic{{Name: "orders", Partitions: 3, Config: map[string]any{"retention.ms": "60000"}}, {Name: "audit", Partitions: 1}}}
			kafka, pool, topics, user, err := KafkaObjects("aepx-events", "dp-team", map[string]string{Domain + "managed-by": "bridge"}, map[string]string{Domain + "claim-uid": "uid"}, spec, false)
			if err != nil {
				t.Fatal(err)
			}
			listeners, _, _ := unstructured.NestedSlice(kafka.Object, "spec", "kafka", "listeners")
			if len(listeners) != 1 {
				t.Fatal("exactly one listener expected", listeners)
			}
			l := listeners[0].(map[string]any)
			if l["port"] != int64(9093) || l["tls"] != true || l["type"] != "internal" || l["authentication"].(map[string]any)["type"] != "scram-sha-512" {
				t.Fatal("listener must require SCRAM over TLS", l)
			}
			peers := l["networkPolicyPeers"].([]any)
			ns, _, _ := unstructured.NestedString(peers[0].(map[string]any), "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name")
			if len(peers) != 1 || ns != "dp-team" {
				t.Fatal("network access must be limited to the claim namespace", peers)
			}
			if authz, _, _ := unstructured.NestedString(kafka.Object, "spec", "kafka", "authorization", "type"); authz != "simple" {
				t.Fatal("authorization disabled")
			}
			config, _, _ := unstructured.NestedMap(kafka.Object, "spec", "kafka", "config")
			if config["offsets.topic.replication.factor"] != tt.rf || config["transaction.state.log.replication.factor"] != tt.rf || config["min.insync.replicas"] != tt.isr || config["transaction.state.log.min.isr"] != tt.isr {
				t.Fatal(config)
			}
			a := kafka.GetAnnotations()
			if a["strimzi.io/node-pools"] != "enabled" || a["strimzi.io/kraft"] != "enabled" || a[Domain+"claim-uid"] != "uid" || kafka.GetNamespace() != kinds.KafkaNamespace {
				t.Fatal(a)
			}
			if _, ok, _ := unstructured.NestedMap(kafka.Object, "spec", "entityOperator", "userOperator"); !ok {
				t.Fatal("user operator missing")
			}
			roles, _, _ := unstructured.NestedStringSlice(pool.Object, "spec", "roles")
			deleteClaim, _, _ := unstructured.NestedBool(pool.Object, "spec", "storage", "deleteClaim")
			if pool.GetName() != "aepx-events-pool" || pool.GetLabels()["strimzi.io/cluster"] != "aepx-events" || strings.Join(roles, ",") != "controller,broker" || !deleteClaim {
				t.Fatal(pool.Object)
			}
			if cpu, _, _ := unstructured.NestedString(pool.Object, "spec", "resources", "limits", "cpu"); cpu != "1000m" {
				t.Fatal(cpu)
			}
			if len(topics) != 2 {
				t.Fatal(topics)
			}
			for _, topic := range topics {
				name, _, _ := unstructured.NestedString(topic.Object, "spec", "topicName")
				replicas, _, _ := unstructured.NestedInt64(topic.Object, "spec", "replicas")
				if topic.GetName() != "aepx-events-"+name || replicas != tt.rf || topic.GetLabels()["strimzi.io/cluster"] != "aepx-events" {
					t.Fatal(topic.Object)
				}
			}
			acls, _, _ := unstructured.NestedSlice(user.Object, "spec", "authorization", "acls")
			allowed := map[string]bool{"topic/literal/orders": true, "topic/literal/audit": true, "group/prefix/aepx-events-": true}
			if len(acls) != len(allowed) {
				t.Fatal(acls)
			}
			for _, raw := range acls {
				acl := raw.(map[string]any)
				res := acl["resource"].(map[string]any)
				key := fmt.Sprint(res["type"], "/", res["patternType"], "/", res["name"])
				if !allowed[key] {
					t.Fatal("ACL outside the claim scope", key)
				}
				ops := fmt.Sprint(acl["operations"])
				if res["type"] == "topic" && ops != "[Read Write Describe DescribeConfigs]" || res["type"] == "group" && ops != "[Read Describe]" {
					t.Fatal(key, ops)
				}
			}
			if auth, _, _ := unstructured.NestedString(user.Object, "spec", "authentication", "type"); auth != "scram-sha-512" || user.GetName() != "aepx-events-app" {
				t.Fatal(user.Object)
			}
		})
	}
	_, pool, _, _, _ := KafkaObjects("c", "dp-a", nil, nil, KafkaSpec{Version: "4.1.1", Size: "1Gi", Preset: "t1.nano", Replicas: 1}, true)
	if deleteClaim, _, _ := unstructured.NestedBool(pool.Object, "spec", "storage", "deleteClaim"); deleteClaim {
		t.Fatal("retained storage must keep its claims")
	}
	if memory, _, _ := unstructured.NestedString(pool.Object, "spec", "resources", "requests", "memory"); memory != "1024Mi" {
		t.Fatal("JVM floor not applied", memory)
	}
}

func TestKafkaReconcileAndConnection(t *testing.T) {
	ctx := context.Background()
	c := kafkaClaim(`[{"name":"orders","partitions":6},{"name":"audit"}]`)
	r, d := harness(t, c)
	c = reconcile(t, r, c)
	name := KafkaName(policy.Name(r.ClusterID, c.Namespace, c.Name, c.Spec.Resource.Name))
	if c.Status.Application == nil || c.Status.Application.Namespace != kinds.KafkaNamespace || c.Status.Application.Name != name {
		t.Fatal(c.Status.Application)
	}
	if meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") {
		t.Fatal("ready before Strimzi reported")
	}
	for _, a := range d.Actions() {
		if a.GetVerb() == "create" {
			t.Fatal("Kafka must not create management applications", a)
		}
	}
	objects := []*unstructured.Unstructured{strimzi(t, r, "Kafka", name), strimzi(t, r, "KafkaNodePool", name+"-pool"), strimzi(t, r, "KafkaUser", name+"-app"), strimzi(t, r, "KafkaTopic", name+"-orders"), strimzi(t, r, "KafkaTopic", name+"-audit")}
	for _, u := range objects {
		if u == nil {
			t.Fatal("missing Strimzi object")
		}
		if u.GetLabels()[Domain+"managed-by"] != "bridge" || u.GetAnnotations()[Domain+"claim-uid"] != "claim-uid" {
			t.Fatal("ownership metadata missing", u.GetName())
		}
	}
	markReady(t, r, objects...)
	labels := map[string]string{"strimzi.io/cluster": name}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: name + "-app", Namespace: kinds.KafkaNamespace, Labels: map[string]string{"strimzi.io/cluster": name, "strimzi.io/kind": "KafkaUser"}}, Data: map[string][]byte{"password": []byte("s3cret"), "sasl.jaas.config": []byte("jaas s3cret")}},
		{ObjectMeta: metav1.ObjectMeta{Name: name + "-cluster-ca-cert", Namespace: kinds.KafkaNamespace, Labels: map[string]string{"strimzi.io/cluster": labels["strimzi.io/cluster"], "strimzi.io/kind": "Kafka"}}, Data: map[string][]byte{"ca.crt": []byte("test-ca")}},
	} {
		if e := r.App.Create(ctx, s); e != nil {
			t.Fatal(e)
		}
	}
	c = reconcile(t, r, c)
	if !meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") {
		t.Fatal(c.Status.Conditions)
	}
	conn := &corev1.Secret{}
	if e := r.App.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name + "-conn"}, conn); e != nil {
		t.Fatal(e)
	}
	host := name + "-kafka-bootstrap.aep-kafka.svc.cluster.local"
	want := map[string]string{"host": host, "port": "9093", "username": name + "-app", "password": "s3cret", "sasl.jaas.config": "jaas s3cret", "security.protocol": "SASL_SSL", "sasl.mechanism": "SCRAM-SHA-512", "bootstrap.servers": host + ":9093", "topics": "audit,orders", "ca.crt": "test-ca", "uri": "kafka://" + host + ":9093"}
	for k, v := range want {
		if string(conn.Data[k]) != v {
			t.Fatalf("%s=%q, want %q", k, conn.Data[k], v)
		}
	}
	if conn.Labels[Domain+"managed-by"] != "bridge" || len(conn.OwnerReferences) != 1 {
		t.Fatal("connection Secret ownership", conn.ObjectMeta)
	}
	if got := c.Status.Connection; got == nil || got.Host != host || got.Port != 9093 || got.Username != name+"-app" || got.Database != "" || got.SecretName != c.Name+"-conn" {
		t.Fatal(got)
	}
	if serialized, _ := json.Marshal(c.Status); containsPassword(serialized, "s3cret") {
		t.Fatal("password in status")
	}
	if e := r.App.Get(ctx, client.ObjectKeyFromObject(c), &corev1.Service{}); !errors.IsNotFound(e) {
		t.Fatal("Kafka needs no local Service", e)
	}
	if e := r.App.Get(ctx, client.ObjectKeyFromObject(c), &discoveryv1.EndpointSlice{}); !errors.IsNotFound(e) {
		t.Fatal("Kafka needs no EndpointSlice", e)
	}
	// A stale generation of any topic prevents readiness.
	topic := strimzi(t, r, "KafkaTopic", name+"-orders")
	_ = unstructured.SetNestedField(topic.Object, topic.GetGeneration()-1, "status", "observedGeneration")
	if e := r.App.Update(ctx, topic); e != nil {
		t.Fatal(e)
	}
	c = reconcile(t, r, c)
	if meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") {
		t.Fatal("stale topic reported ready")
	}
	// Degraded mirrors the first False condition.
	user := strimzi(t, r, "KafkaUser", name+"-app")
	_ = unstructured.SetNestedSlice(user.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "InvalidResource", "message": "ACL rejected", "lastTransitionTime": "2026-11-25T00:00:00Z"}}, "status", "conditions")
	_ = r.App.Update(ctx, user)
	c = reconcile(t, r, c)
	if got := meta.FindStatusCondition(c.Status.Conditions, "Degraded"); got.Status != metav1.ConditionTrue || got.Message != "ACL rejected" {
		t.Fatal(got)
	}
}

func TestKafkaTopicRemoval(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprint("retain=", retain), func(t *testing.T) {
			ctx := context.Background()
			c := kafkaClaim(`[{"name":"orders"},{"name":"audit"}]`)
			if retain {
				c.Spec.DeletionPolicy = "Retain"
			}
			r, _ := harness(t, c)
			c = reconcile(t, r, c)
			name := c.Status.Application.Name
			c.Spec.Parameters["topics"] = ext.JSON{Raw: []byte(`[{"name":"orders"}]`)}
			if e := r.App.Update(ctx, c); e != nil {
				t.Fatal(e)
			}
			reconcile(t, r, c)
			audit := strimzi(t, r, "KafkaTopic", name+"-audit")
			if retain && (audit == nil || audit.GetAnnotations()[Domain+"orphaned"] != "true") {
				t.Fatal("retained topic must remain, marked orphaned")
			}
			if !retain && audit != nil {
				t.Fatal("removed topic not deleted")
			}
			if strimzi(t, r, "KafkaTopic", name+"-orders") == nil {
				t.Fatal("kept topic deleted")
			}
			acls, _, _ := unstructured.NestedSlice(strimzi(t, r, "KafkaUser", name+"-app").Object, "spec", "authorization", "acls")
			if strings.Contains(fmt.Sprint(acls), "audit") {
				t.Fatal("removed topic still granted", acls)
			}
		})
	}
}

func TestKafkaFinalization(t *testing.T) {
	for _, tt := range []struct {
		tier, claimPolicy string
		retain            bool
	}{{"nonprod", "Delete", false}, {"nonprod", "Retain", true}, {"prod", "Delete", true}} {
		t.Run(fmt.Sprint(tt), func(t *testing.T) {
			ctx := context.Background()
			c := kafkaClaim(`[{"name":"orders"}]`)
			c.Spec.DeletionPolicy = tt.claimPolicy
			r, _ := harness(t, c)
			r.Tier = tt.tier
			c = reconcile(t, r, c)
			name := c.Status.Application.Name
			if e := r.App.Delete(ctx, c); e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 4; i++ {
				if _, e := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); e != nil {
					t.Fatal(e)
				}
			}
			if e := r.App.Get(ctx, client.ObjectKeyFromObject(c), &api.ServiceClaim{}); !errors.IsNotFound(e) {
				t.Fatal("finalizer not released", e)
			}
			for _, ref := range [][2]string{{"KafkaUser", name + "-app"}, {"KafkaTopic", name + "-orders"}, {"Kafka", name}, {"KafkaNodePool", name + "-pool"}} {
				u := strimzi(t, r, ref[0], ref[1])
				if tt.retain && (u == nil || u.GetAnnotations()[Domain+"orphaned"] != "true") {
					t.Fatal("retained object missing or not orphaned", ref)
				}
				if !tt.retain && u != nil {
					t.Fatal("object not deleted", ref)
				}
			}
		})
	}
}

func TestKafkaConflictAndBackup(t *testing.T) {
	ctx := context.Background()
	c := kafkaClaim(`[]`)
	r, d := harness(t, c)
	r.Policy.BackupPolicy = "prod-default"
	r.Policy.Backup = &policy.BackupConfig{ClassName: "aep-acme-prod", Schedule: "0 2 * * *", RetentionPolicy: "30d"}
	c = reconcile(t, r, c)
	if got := meta.FindStatusCondition(c.Status.Conditions, "BackupReady"); got == nil || got.Reason != "NotSupported" {
		t.Fatal(c.Status.Conditions)
	}
	for _, a := range d.Actions() {
		if a.GetResource().Resource == "plans" {
			t.Fatal("Kafka must not create a backup Plan")
		}
	}
	kafka := strimzi(t, r, "Kafka", c.Status.Application.Name)
	a := kafka.GetAnnotations()
	a[Domain+"claim-uid"] = "other"
	kafka.SetAnnotations(a)
	_ = r.App.Update(ctx, kafka)
	c = reconcile(t, r, c)
	if meta.FindStatusCondition(c.Status.Conditions, "Ready").Reason != "NameConflict" {
		t.Fatal(c.Status.Conditions)
	}
}
