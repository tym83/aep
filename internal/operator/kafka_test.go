package operator

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tym83/aep/internal/kinds"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestRetainedKafkaBlocksDeletion(t *testing.T) {
	lists := map[schema.GroupVersionResource]string{{Group: "kafka.strimzi.io", Version: "v1", Resource: "kafkas"}: "KafkaList"}
	empty := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)}
	if err := retainedKafka(context.Background(), empty, "prod"); err != nil {
		t.Fatal(err)
	}
	kafka := obj("kafka.strimzi.io/v1", "Kafka", kinds.KafkaNamespace, "aepabc-events", nil)
	kafka.SetAnnotations(map[string]string{"aep.aenix.io/orphaned": "true"})
	retained := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, kafka)}
	err := retainedKafka(context.Background(), retained, "prod")
	if _, ok := err.(*pendingError); !ok || !strings.Contains(err.Error(), "aep-kafka/aepabc-events") {
		t.Fatal("retained Kafka must block deletion", err)
	}
}

func strimziIndex(t *testing.T) int {
	t.Helper()
	for i, c := range tierCharts {
		if c.Name == StrimziChart {
			return i
		}
	}
	t.Fatal("Strimzi chart is not delivered")
	return -1
}

func TestStrimziDelivery(t *testing.T) {
	i := strimziIndex(t)
	if c := tierCharts[i]; c.Version != "0.50.0" || c.URL != "oci://quay.io/strimzi-helm/strimzi-kafka-operator" {
		t.Fatal(c)
	}
	found := false
	for _, c := range DeliveryCharts() {
		found = found || (c.Name == StrimziChart && c.URL == "oci://quay.io/strimzi-helm/strimzi-kafka-operator" && c.Version == "0.50.0")
	}
	if !found {
		t.Fatal("Strimzi missing from release inventory charts")
	}
	hr := deliveryRelease("tenant-acme-nonprod", tierCharts[i], tierCharts[i-1].Name, M{})
	spec := hr.Object["spec"].(M)
	if spec["releaseName"] != StrimziChart || spec["targetNamespace"] != kinds.KafkaNamespace || spec["storageNamespace"] != kinds.KafkaNamespace {
		t.Fatal(spec)
	}
	if spec["install"].(M)["crds"] != "CreateReplace" || spec["upgrade"].(M)["crds"] != "CreateReplace" {
		t.Fatal("Strimzi CRDs must follow chart upgrades", spec)
	}
	for _, c := range tierCharts {
		if c.Name != StrimziChart && deliveryRelease("ns", c, "", M{}).Object["spec"].(M)["targetNamespace"] != "openchoreo-data-plane" {
			t.Fatal("data plane charts moved", c.Name)
		}
	}
}

func TestStrimziValues(t *testing.T) {
	plain := MirrorValues(StrimziChart, "", ReleaseValues(StrimziChart))
	if _, ok := plain["defaultImageRegistry"]; ok {
		t.Fatal("no mirror must keep the chart registry", plain)
	}
	if w, _, _ := unstructured.NestedSlice(plain, "watchNamespaces"); len(w) != 0 {
		t.Fatal("Strimzi must watch only its own namespace")
	}
	if env := plain["extraEnvs"].([]any)[0].(M); env["name"] != "STRIMZI_POD_SECURITY_PROVIDER_CLASS" || env["value"] != "restricted" {
		t.Fatal(env)
	}
	for _, path := range [][]string{{"image", "tag"}, {"topicOperator", "image", "tag"}, {"userOperator", "image", "tag"}, {"kafkaInit", "image", "tag"}} {
		if tag, _, _ := unstructured.NestedString(plain, path...); !strings.HasPrefix(tag, "0.50.0@sha256:") {
			t.Fatal(path, tag)
		}
	}
	if tag, _, _ := unstructured.NestedString(plain, "kafka", "image", "tag"); !strings.HasPrefix(tag, "0.50.0-kafka-4.1.1@sha256:") {
		t.Fatal(tag)
	}
	mirrored := PullSecretValues(StrimziChart, kinds.KafkaNamespace, MirrorValues(StrimziChart, "mirror.example/aep", StrimziValues()))
	if mirrored["defaultImageRegistry"] != "mirror.example/aep/quay.io" || mirrored["defaultImageRepository"] != nil {
		t.Fatal(mirrored)
	}
	secrets, _, _ := unstructured.NestedSlice(mirrored, "image", "imagePullSecrets")
	if len(secrets) != 1 || secrets[0].(M)["name"] != MirrorPullSecret {
		t.Fatal(secrets)
	}
}

func TestKafkaTierContract(t *testing.T) {
	rules := roleScopes("acme")[kinds.KafkaNamespace]
	if len(rules) != 1 || fmt.Sprint(rules[0]) != "map[apiGroups:[kafka.strimzi.io] resources:[kafkas] verbs:[list]]" {
		t.Fatal(rules)
	}
	found := false
	for _, ns := range mirrorPullNamespaces {
		found = found || ns == kinds.KafkaNamespace
	}
	if !found {
		t.Fatal("Kafka operands need the mirror pull secret")
	}
	if !slices.Contains(stringsOf(policyValues(organization(), "nonprod", "")["app-cluster-baseline"]["platformNamespaces"]), kinds.KafkaNamespace) {
		t.Fatal("aep-kafka must be a platform namespace")
	}
}
func stringsOf(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

func TestBackupPolicyIgnoresKafka(t *testing.T) {
	ctx := context.Background()
	o := organization()
	tier := o.Spec.Tiers["prod"]
	tier.Services.BackupPolicy = "daily"
	tier.Services.AllowedKinds = []string{"Postgres", "Kafka"}
	o.Spec.Tiers["prod"] = tier
	source := obj(backupsAPI, "BackupClass", "", "cozy-default", M{"strategies": []any{M{"application": M{"kind": "Postgres"}, "strategyRef": M{"apiGroup": "strategy.backups.cozystack.io", "kind": "CNPG", "name": "default"}}}})
	strategy := obj(strategyAPI, "CNPG", "", "default", M{"template": M{"barmanObjectStore": M{"retentionPolicy": "7d"}}})
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), source, strategy)
	k := kubefake.NewClientset(&core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "aep-system"}, Data: map[string]string{"schedule": "0 2 * * *", "backupClassName": "cozy-default", "retentionPolicy": "30d"}})
	s := &session{o: o, r: &Reconciler{Management: &Cluster{Dynamic: d, Kube: k}, Options: Options{Namespace: "aep-system"}}}
	if _, e := s.backupPolicy(ctx, "prod"); e != nil {
		t.Fatal("Kafka has no Cozystack backup strategy and must not block the policy", e)
	}
}
