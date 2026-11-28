package kinds

import "k8s.io/apimachinery/pkg/runtime/schema"

type Mapping struct {
	Resource, Prefix, ServicePrefix, ServiceSuffix, SecretSuffix, PasswordKey, CASuffix, Scheme string
	Port                                                                                        int32
	Parameters                                                                                  []string
	// AppCluster kinds are not Cozystack applications: the bridge renders them
	// for an operator that runs inside the tier application cluster.
	AppCluster bool
	// MaxReplicas is a hard per-kind replica cap; zero means the tier limit only.
	MaxReplicas int64
	// Versions lists the accepted version parameter values.
	Versions []string
	// DefaultVersion is applied when a claim omits version; empty leaves the backend default.
	DefaultVersion string
}

// KafkaNamespace holds the Strimzi operator and every Kafka cluster of a tier
// application cluster.
const KafkaNamespace = "aep-kafka"

var Table = map[string]Mapping{
	"Postgres": {Resource: "postgreses", Prefix: "postgres-", ServiceSuffix: "-rw", SecretSuffix: "-credentials", CASuffix: ".tenant-ca", Scheme: "postgresql", Port: 5432, Parameters: []string{"version", "databases", "users", "resourcesPreset", "size", "replicas"}, Versions: []string{"v13", "v14", "v15", "v16", "v17", "v18"}},
	"Redis":    {Resource: "redises", Prefix: "redis-", ServicePrefix: "rfrm-", SecretSuffix: "-auth", PasswordKey: "password", Scheme: "redis", Port: 6379, Parameters: []string{"version", "resourcesPreset", "size", "replicas"}, Versions: []string{"v7", "v8"}},
	"Kafka":    {Scheme: "kafka", Port: 9093, Parameters: []string{"version", "topics", "resourcesPreset", "size", "replicas"}, AppCluster: true, MaxReplicas: 5, Versions: []string{"4.1.1"}, DefaultVersion: "4.1.1"},
}

func (m Mapping) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: m.Resource}
}
func (m Mapping) Release(name string) string { return m.Prefix + name }
func (m Mapping) Service(name string) string {
	return m.ServicePrefix + m.Release(name) + m.ServiceSuffix
}
func (m Mapping) Secret(name string) string { return m.Release(name) + m.SecretSuffix }
