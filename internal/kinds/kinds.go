package kinds

import "k8s.io/apimachinery/pkg/runtime/schema"

type Mapping struct {
	Resource, Prefix, ServicePrefix, ServiceSuffix, SecretSuffix, PasswordKey, CASuffix, Scheme string
	Port                                                                                        int32
	Parameters                                                                                  []string
}

var Table = map[string]Mapping{
	"Postgres": {Resource: "postgreses", Prefix: "postgres-", ServiceSuffix: "-rw", SecretSuffix: "-credentials", CASuffix: ".tenant-ca", Scheme: "postgresql", Port: 5432, Parameters: []string{"version", "databases", "users", "resourcesPreset", "size", "replicas"}},
	"Redis":    {Resource: "redises", Prefix: "redis-", ServicePrefix: "rfrm-", SecretSuffix: "-auth", PasswordKey: "password", Scheme: "redis", Port: 6379, Parameters: []string{"version", "resourcesPreset", "size", "replicas"}},
}

func (m Mapping) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: m.Resource}
}
func (m Mapping) Release(name string) string { return m.Prefix + name }
func (m Mapping) Service(name string) string {
	return m.ServicePrefix + m.Release(name) + m.ServiceSuffix
}
func (m Mapping) Secret(name string) string { return m.Release(name) + m.SecretSuffix }
