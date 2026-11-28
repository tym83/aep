package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	corev1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// StrimziAPI is the Strimzi API version the bridge writes.
const StrimziAPI = "kafka.strimzi.io/v1"

// MaxKafkaName keeps every derived name short enough: broker pods are named
// <cluster>-<cluster>-pool-<id> and must stay a 63 character DNS label, and
// Strimzi resource names must not exceed 52 characters.
const MaxKafkaName = 26

const kafkaTopicsAnnotation = Domain + "topics"

// KafkaName derives the Strimzi cluster name from the stable claim name,
// shortening it deterministically with a hash suffix when needed.
func KafkaName(base string) string {
	return shorten(base, MaxKafkaName)
}
func shorten(s string, max int) string {
	if len(s) <= max {
		return s
	}
	h := sha256.Sum256([]byte(s))
	suffix := hex.EncodeToString(h[:])[:8]
	return strings.TrimRight(s[:max-len(suffix)-1], "-") + "-" + suffix
}

// KafkaTopicResource names a KafkaTopic object; the Kafka topic name itself is spec.topicName.
func KafkaTopicResource(cluster, topic string) string {
	return shorten(cluster+"-"+topic, 63)
}

// KafkaUserName is both the KafkaUser object and the SCRAM user name.
func KafkaUserName(cluster string) string { return cluster + "-app" }

// KafkaBootstrap is the in-cluster bootstrap Service name created by Strimzi.
func KafkaBootstrap(cluster string) string {
	return cluster + "-kafka-bootstrap." + kinds.KafkaNamespace + ".svc.cluster.local"
}

type KafkaTopic struct {
	Name       string
	Partitions int64
	Config     map[string]any
}
type KafkaSpec struct {
	Version, Size, Preset string
	Replicas              int64
	Topics                []KafkaTopic
}

// ParseKafka reads the spec returned by policy.Validate.
func ParseKafka(desired map[string]any) KafkaSpec {
	s := KafkaSpec{}
	s.Version, _ = desired["version"].(string)
	s.Size, _ = desired["size"].(string)
	s.Preset, _ = desired["resourcesPreset"].(string)
	s.Replicas, _ = desired["replicas"].(int64)
	topics, _ := desired["topics"].([]any)
	for _, raw := range topics {
		t, _ := raw.(map[string]any)
		topic := KafkaTopic{Config: map[string]any{}}
		topic.Name, _ = t["name"].(string)
		topic.Partitions, _ = t["partitions"].(int64)
		if c, ok := t["config"].(map[string]any); ok {
			topic.Config = c
		}
		s.Topics = append(s.Topics, topic)
	}
	return s
}
func (s KafkaSpec) topicNames() []string {
	out := []string{}
	for _, t := range s.Topics {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return out
}

// ReplicationFactor never exceeds the broker count.
func (s KafkaSpec) ReplicationFactor() int64 { return min(s.Replicas, 3) }

// MinInSync tolerates one broker loss once three replicas exist.
func (s KafkaSpec) MinInSync() int64 {
	if s.Replicas < 3 {
		return 1
	}
	return 2
}

// PresetResources maps a preset to equal requests and limits using the same
// relative scale as tier ceilings (policy.Preset): one CPU unit is 250m and
// one memory unit is 128Mi, with at least 1Gi so the broker JVM can start.
func PresetResources(preset string) (map[string]any, error) {
	cpu, memory, ok := policy.Preset(preset)
	if !ok {
		return nil, fmt.Errorf("unknown resourcesPreset %s", preset)
	}
	values := map[string]any{"cpu": strconv.Itoa(cpu*250) + "m", "memory": strconv.Itoa(max(memory*128, 1024)) + "Mi"}
	return map[string]any{"requests": values, "limits": copyMap(values)}, nil
}
func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strimziObject(kind, name string, labels, annotations map[string]string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": StrimziAPI, "kind": kind, "metadata": map[string]any{"name": name, "namespace": kinds.KafkaNamespace}, "spec": spec}}
	l := map[string]string{}
	for k, v := range labels {
		l[k] = v
	}
	a := map[string]string{}
	for k, v := range annotations {
		a[k] = v
	}
	u.SetLabels(l)
	u.SetAnnotations(a)
	return u
}

// KafkaObjects renders the Strimzi resources of one claim. Only the claim
// namespace may reach the SCRAM listener, and the user may only use the
// claim's topics and consumer groups prefixed with the cluster name.
func KafkaObjects(name, claimNamespace string, labels, annotations map[string]string, s KafkaSpec, retain bool) (kafka, pool *unstructured.Unstructured, topics []*unstructured.Unstructured, user *unstructured.Unstructured, err error) {
	resources, err := PresetResources(s.Preset)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	clusterLabels := map[string]string{"strimzi.io/cluster": name}
	for k, v := range labels {
		clusterLabels[k] = v
	}
	kafkaAnnotations := map[string]string{"strimzi.io/node-pools": "enabled", "strimzi.io/kraft": "enabled", kafkaTopicsAnnotation: strings.Join(s.topicNames(), ",")}
	for k, v := range annotations {
		kafkaAnnotations[k] = v
	}
	rf, isr := s.ReplicationFactor(), s.MinInSync()
	kafka = strimziObject("Kafka", name, labels, kafkaAnnotations, map[string]any{
		"kafka": map[string]any{
			"version": s.Version,
			"listeners": []any{map[string]any{"name": "tls", "port": int64(9093), "type": "internal", "tls": true,
				"authentication":     map[string]any{"type": "scram-sha-512"},
				"networkPolicyPeers": []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": claimNamespace}}}}}},
			"authorization": map[string]any{"type": "simple"},
			"config": map[string]any{"offsets.topic.replication.factor": rf, "transaction.state.log.replication.factor": rf, "transaction.state.log.min.isr": isr,
				"default.replication.factor": rf, "min.insync.replicas": isr},
		},
		"entityOperator": map[string]any{"topicOperator": map[string]any{}, "userOperator": map[string]any{}},
	})
	pool = strimziObject("KafkaNodePool", name+"-pool", clusterLabels, annotations, map[string]any{
		"replicas":  s.Replicas,
		"roles":     []any{"controller", "broker"},
		"storage":   map[string]any{"type": "persistent-claim", "size": s.Size, "deleteClaim": !retain},
		"resources": resources,
	})
	acls := []any{}
	for _, t := range s.Topics {
		config := map[string]any{}
		for k, v := range t.Config {
			config[k] = v
		}
		topics = append(topics, strimziObject("KafkaTopic", KafkaTopicResource(name, t.Name), clusterLabels, annotations, map[string]any{"topicName": t.Name, "partitions": t.Partitions, "replicas": rf, "config": config}))
		acls = append(acls, map[string]any{"resource": map[string]any{"type": "topic", "name": t.Name, "patternType": "literal"}, "operations": []any{"Read", "Write", "Describe", "DescribeConfigs"}})
	}
	acls = append(acls, map[string]any{"resource": map[string]any{"type": "group", "name": name + "-", "patternType": "prefix"}, "operations": []any{"Read", "Describe"}})
	user = strimziObject("KafkaUser", KafkaUserName(name), clusterLabels, annotations, map[string]any{
		"authentication": map[string]any{"type": "scram-sha-512"},
		"authorization":  map[string]any{"type": "simple", "acls": acls},
	})
	return kafka, pool, topics, user, nil
}

// owned fetches obj by name; it returns nil when absent and refuses objects
// that the bridge does not manage for this claim.
func (r *Reconciler) owned(ctx context.Context, c *api.ServiceClaim, kind, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(StrimziAPI)
	u.SetKind(kind)
	e := r.App.Get(ctx, types.NamespacedName{Namespace: kinds.KafkaNamespace, Name: name}, u)
	if errors.IsNotFound(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if u.GetLabels()[Domain+"managed-by"] != "bridge" || u.GetAnnotations()[Domain+"claim-uid"] != string(c.UID) {
		return u, fmt.Errorf("refusing to adopt %s %s/%s", kind, kinds.KafkaNamespace, name)
	}
	return u, nil
}

func (r *Reconciler) kafkaName(c *api.ServiceClaim) string {
	return KafkaName(policy.Name(r.ClusterID, c.Namespace, c.Name, c.Spec.Resource.Name))
}

func (r *Reconciler) reconcileKafka(ctx context.Context, c *api.ServiceClaim, desired map[string]any) (ctrl.Result, error) {
	name := r.kafkaName(c)
	current, e := r.owned(ctx, c, "Kafka", name)
	if e != nil {
		if current != nil {
			return r.report(ctx, c, "NameConflict", "Kafka belongs to a different claim UID")
		}
		return r.reportError(ctx, c, "ApplicationClusterUnavailable", e)
	}
	immutableHash := policy.Hash(map[string]any{"version": desired["version"]})
	if current != nil && current.GetAnnotations()[Domain+"immutable-spec-hash"] != "" && current.GetAnnotations()[Domain+"immutable-spec-hash"] != immutableHash {
		return r.report(ctx, c, "ParameterNotAllowed", "version is immutable; create a new resource or use an approved platform operation")
	}
	if current != nil && current.GetAnnotations()[Domain+"paused"] == "true" {
		return r.report(ctx, c, "Paused", "Kafka reconciliation is paused")
	}
	if !controllerutil.ContainsFinalizer(c, Finalizer) {
		base := c.DeepCopy()
		controllerutil.AddFinalizer(c, Finalizer)
		if e := r.App.Patch(ctx, c, client.MergeFrom(base)); e != nil {
			return ctrl.Result{}, e
		}
	}
	spec := ParseKafka(desired)
	retain := policy.Retain(r.Tier, r.TierDeletionPolicy, c.Spec.DeletionPolicy)
	labels := map[string]string{}
	for k, v := range map[string]string{"organization": r.Organization, "tier": r.Tier, "project": c.Spec.Resource.Project, "environment": c.Spec.Resource.Environment, "resource": c.Spec.Resource.Name, "managed-by": "bridge"} {
		labels[Domain+k] = v
	}
	hash := policy.Hash(desired)
	annotations := map[string]string{Domain + "claim-uid": string(c.UID), Domain + "cluster-id": r.ClusterID, Domain + "claim-ref": c.Namespace + "/" + c.Name}
	kafka, pool, topics, user, e := KafkaObjects(name, c.Namespace, labels, annotations, spec, retain)
	if e != nil {
		return r.report(ctx, c, "ParameterNotAllowed", e.Error())
	}
	ka := kafka.GetAnnotations()
	ka[Domain+"claim-spec-hash"] = hash
	ka[Domain+"immutable-spec-hash"] = immutableHash
	kafka.SetAnnotations(ka)
	// Remove topics dropped from the claim before recording the new topic set,
	// so a failed removal is retried from the persisted previous set.
	if current != nil {
		for _, topic := range strings.Split(current.GetAnnotations()[kafkaTopicsAnnotation], ",") {
			if topic == "" || slices.Contains(spec.topicNames(), topic) {
				continue
			}
			if e := r.releaseKafkaObject(ctx, c, "KafkaTopic", KafkaTopicResource(name, topic), retain); e != nil {
				return r.reportError(ctx, c, "ApplyFailed", e)
			}
		}
	}
	objects := append([]*unstructured.Unstructured{kafka, pool, user}, topics...)
	for _, obj := range objects[1:] {
		if old, e := r.owned(ctx, c, obj.GetKind(), obj.GetName()); e != nil {
			if old != nil {
				return r.report(ctx, c, "NameConflict", e.Error())
			}
			return r.reportError(ctx, c, "ApplicationClusterUnavailable", e)
		}
	}
	for _, obj := range objects {
		if e := r.App.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(Manager), client.ForceOwnership); e != nil {
			return r.reportError(ctx, c, "ApplyFailed", e)
		}
	}
	observed := []*unstructured.Unstructured{}
	for _, obj := range objects {
		u, e := r.owned(ctx, c, obj.GetKind(), obj.GetName())
		if e != nil {
			return r.reportError(ctx, c, "ApplicationClusterUnavailable", e)
		}
		observed = append(observed, u)
	}
	c.Status.Application = &api.ApplicationReference{Kind: c.Spec.Kind, Name: name, Namespace: kinds.KafkaNamespace, UID: string(observed[0].GetUID())}
	c.Status.AppliedSpecHash = hash
	for _, v := range KafkaReadiness(observed[0], observed[1], observed[2], observed[3:]) {
		v.ObservedGeneration = c.Generation
		meta.SetStatusCondition(&c.Status.Conditions, v)
		if v.Reason != "Pending" && v.Reason != "Healthy" {
			meta.FindStatusCondition(c.Status.Conditions, v.Type).LastTransitionTime = v.LastTransitionTime
		}
	}
	if r.Policy.BackupPolicy != "" {
		r.set(c, "BackupReady", metav1.ConditionFalse, "NotSupported", "Kafka backups are not provided; the tier backup policy does not cover Kafka")
	} else {
		meta.RemoveStatusCondition(&c.Status.Conditions, "BackupReady")
	}
	if meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") {
		if e := r.connectKafka(ctx, c, name, spec); e != nil {
			return r.reportError(ctx, c, "ConnectionUnavailable", e)
		}
	}
	return r.save(ctx, c)
}

// releaseKafkaObject deletes a bridge-owned Strimzi object, or marks it
// orphaned when the claim's data must be retained.
func (r *Reconciler) releaseKafkaObject(ctx context.Context, c *api.ServiceClaim, kind, name string, retain bool) error {
	u, e := r.owned(ctx, c, kind, name)
	if e != nil || u == nil {
		return e
	}
	if retain {
		if u.GetAnnotations()[Domain+"orphaned"] == "true" {
			return nil
		}
		base := u.DeepCopy()
		a := u.GetAnnotations()
		a[Domain+"orphaned"] = "true"
		u.SetAnnotations(a)
		return r.App.Patch(ctx, u, client.MergeFrom(base))
	}
	if !u.GetDeletionTimestamp().IsZero() {
		return nil
	}
	uid, rv := u.GetUID(), u.GetResourceVersion()
	return client.IgnoreNotFound(r.App.Delete(ctx, u, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}))
}

func (r *Reconciler) finalizeKafka(ctx context.Context, c *api.ServiceClaim, name string) (ctrl.Result, error) {
	kafka, e := r.owned(ctx, c, "Kafka", name)
	if e != nil {
		if kafka != nil {
			return r.report(ctx, c, "NameConflict", "Refusing to finalize another claim's Kafka")
		}
		return ctrl.Result{}, e
	}
	if kafka != nil && kafka.GetAnnotations()[Domain+"paused"] == "true" {
		return r.report(ctx, c, "Paused", "Paused Kafka blocks finalization")
	}
	// Topics come from the persisted set and the claim, never from a list.
	topics := []string{}
	if kafka != nil {
		topics = strings.Split(kafka.GetAnnotations()[kafkaTopicsAnnotation], ",")
	}
	if desired, e := policy.Validate(c, r.Policy, []string{c.Spec.Kind}); e == nil {
		topics = append(topics, ParseKafka(desired).topicNames()...)
	}
	slices.Sort(topics)
	dependents := [][2]string{{"KafkaUser", KafkaUserName(name)}}
	for _, t := range slices.Compact(topics) {
		if t != "" {
			dependents = append(dependents, [2]string{"KafkaTopic", KafkaTopicResource(name, t)})
		}
	}
	// The entity operator removes topic and user finalizers, so they go first.
	stages := [][][2]string{dependents, {{"Kafka", name}, {"KafkaNodePool", name + "-pool"}}}
	retain := policy.Retain(r.Tier, r.TierDeletionPolicy, c.Spec.DeletionPolicy)
	for _, stage := range stages {
		remaining := false
		for _, ref := range stage {
			u, e := r.owned(ctx, c, ref[0], ref[1])
			if e != nil {
				if u != nil {
					return r.report(ctx, c, "NameConflict", e.Error())
				}
				return ctrl.Result{}, e
			}
			if u == nil {
				continue
			}
			if e := r.releaseKafkaObject(ctx, c, ref[0], ref[1], retain); e != nil {
				return ctrl.Result{}, e
			}
			remaining = remaining || !retain
		}
		if remaining {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	base := c.DeepCopy()
	controllerutil.RemoveFinalizer(c, Finalizer)
	return ctrl.Result{}, r.App.Patch(ctx, c, client.MergeFrom(base))
}

// generationCondition is condition() that also requires the current generation.
func generationCondition(obj *unstructured.Unstructured, kind string) metav1.Condition {
	c := condition(obj, "Ready")
	if obj == nil {
		c.Message = kind + " has not been created"
		return c
	}
	observed, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if c.Status == metav1.ConditionTrue && (!found || observed != obj.GetGeneration()) {
		return metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Updating", Message: kind + " " + obj.GetName() + " has not observed its current generation", LastTransitionTime: metav1.Now()}
	}
	if c.Status != metav1.ConditionTrue && c.Message == "Ready condition has not been reported" {
		c.Message = kind + " " + obj.GetName() + " has not reported readiness"
	}
	return c
}

// poolCondition reports a node pool ready when Strimzi observed its current
// generation and assigned the requested number of nodes.
func poolCondition(pool *unstructured.Unstructured) metav1.Condition {
	pending := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Updating", Message: "KafkaNodePool has not been reconciled", LastTransitionTime: metav1.Now()}
	if pool == nil {
		return pending
	}
	observed, found, _ := unstructured.NestedInt64(pool.Object, "status", "observedGeneration")
	want, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	got, _, _ := unstructured.NestedInt64(pool.Object, "status", "replicas")
	if !found || observed != pool.GetGeneration() || got != want {
		pending.Message = fmt.Sprintf("KafkaNodePool %s has %d of %d nodes at its current generation", pool.GetName(), got, want)
		return pending
	}
	return metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "KafkaNodePool is reconciled", LastTransitionTime: metav1.Now()}
}

// KafkaReadiness derives Provisioned, Ready and Degraded from the Strimzi objects.
func KafkaReadiness(kafka, pool, user *unstructured.Unstructured, topics []*unstructured.Unstructured) []metav1.Condition {
	provisioned := generationCondition(kafka, "Kafka")
	provisioned.Type = "Provisioned"
	checks := []metav1.Condition{generationCondition(kafka, "Kafka"), poolCondition(pool), generationCondition(user, "KafkaUser")}
	for _, t := range topics {
		checks = append(checks, generationCondition(t, "KafkaTopic"))
	}
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "Kafka, node pool, topics and user are ready", LastTransitionTime: metav1.Now()}
	for _, v := range checks {
		if v.Status != metav1.ConditionTrue {
			ready = v
			ready.Type = "Ready"
			if ready.Status == metav1.ConditionUnknown {
				ready.Status = metav1.ConditionFalse
			}
			break
		}
	}
	degraded := metav1.Condition{Type: "Degraded", Status: metav1.ConditionFalse, Reason: "Healthy", Message: "No degradation reported", LastTransitionTime: metav1.Now()}
	for _, obj := range append([]*unstructured.Unstructured{kafka, user}, topics...) {
		if v := condition(obj, "Ready"); v.Status == metav1.ConditionFalse {
			degraded = v
			degraded.Type = "Degraded"
			degraded.Status = metav1.ConditionTrue
			break
		}
	}
	return []metav1.Condition{provisioned, ready, degraded}
}

func (r *Reconciler) strimziSecret(ctx context.Context, name, cluster, kind string, keys ...string) (map[string][]byte, error) {
	s := &corev1.Secret{}
	if e := r.App.Get(ctx, types.NamespacedName{Namespace: kinds.KafkaNamespace, Name: name}, s); e != nil {
		return nil, e
	}
	if s.Labels["strimzi.io/cluster"] != cluster || s.Labels["strimzi.io/kind"] != kind {
		return nil, fmt.Errorf("secret %s/%s is not managed by Strimzi for %s", kinds.KafkaNamespace, name, cluster)
	}
	for _, k := range keys {
		if len(s.Data[k]) == 0 {
			return nil, fmt.Errorf("secret %s/%s is missing key %s", kinds.KafkaNamespace, name, k)
		}
	}
	return s.Data, nil
}

// KafkaConnection renders the claim connection Secret data.
func KafkaConnection(cluster string, topics []string, password, jaas, ca []byte) map[string][]byte {
	host := KafkaBootstrap(cluster)
	port := strconv.Itoa(int(kinds.Table["Kafka"].Port))
	return map[string][]byte{"host": []byte(host), "port": []byte(port), "username": []byte(KafkaUserName(cluster)), "password": password,
		"sasl.jaas.config": jaas, "security.protocol": []byte("SASL_SSL"), "sasl.mechanism": []byte("SCRAM-SHA-512"),
		"bootstrap.servers": []byte(host + ":" + port), "topics": []byte(strings.Join(topics, ",")), "ca.crt": ca, "uri": []byte("kafka://" + host + ":" + port)}
}

func (r *Reconciler) connectKafka(ctx context.Context, c *api.ServiceClaim, name string, spec KafkaSpec) error {
	user, e := r.strimziSecret(ctx, KafkaUserName(name), name, "KafkaUser", "password", "sasl.jaas.config")
	if e != nil {
		return e
	}
	ca, e := r.strimziSecret(ctx, name+"-cluster-ca-cert", name, "Kafka", "ca.crt")
	if e != nil {
		return e
	}
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: outputMeta(c, c.Name+"-conn"), Type: corev1.SecretTypeOpaque,
		Data: KafkaConnection(name, spec.topicNames(), user["password"], user["sasl.jaas.config"], ca["ca.crt"])}
	if _, e := r.applyOutputs(ctx, c, secret); e != nil {
		return e
	}
	c.Status.Connection = &api.Connection{Host: KafkaBootstrap(name), Port: kinds.Table["Kafka"].Port, Username: KafkaUserName(name), SecretName: secret.Name}
	return nil
}
