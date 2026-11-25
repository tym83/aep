package policy

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

type Limits struct {
	AllowedParameters []string `json:"allowedParameters"`
	MaxPreset         string   `json:"maxPreset"`
	MaxSize           string   `json:"maxSize"`
	MaxReplicas       int64    `json:"maxReplicas"`
}
type Config struct {
	// BackupPolicy is a reference for backup-plan integration, not an execution guarantee.
	BackupPolicy string            `json:"backupPolicy,omitempty"`
	Kinds        map[string]Limits `json:"kinds"`
}

func Default() Config {
	c := Config{Kinds: map[string]Limits{}}
	for k, m := range kinds.Table {
		c.Kinds[k] = Limits{AllowedParameters: m.Parameters, MaxPreset: "u1.large", MaxSize: "100Gi", MaxReplicas: 3}
	}
	return c
}
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return c, e
		}
		c = Config{}
		if e = yaml.UnmarshalStrict(b, &c); e != nil {
			return c, e
		}
	}
	if c.BackupPolicy != "" && len(validation.IsDNS1123Subdomain(c.BackupPolicy)) > 0 {
		return c, fmt.Errorf("invalid backupPolicy reference")
	}
	for k, l := range c.Kinds {
		m, ok := kinds.Table[k]
		if !ok {
			return c, fmt.Errorf("unsupported kind %s", k)
		}
		for _, p := range l.AllowedParameters {
			if !slices.Contains(m.Parameters, p) {
				return c, fmt.Errorf("unsafe configured parameter %s.%s", k, p)
			}
		}
		if _, _, ok := preset(l.MaxPreset); !ok {
			return c, fmt.Errorf("invalid maxPreset for %s", k)
		}
		q, e := resource.ParseQuantity(l.MaxSize)
		if e != nil || q.Sign() <= 0 || l.MaxReplicas < 1 {
			return c, fmt.Errorf("positive maxSize and maxReplicas required for %s", k)
		}
	}
	if len(c.Kinds) == 0 {
		return c, fmt.Errorf("at least one kind policy required")
	}
	return c, nil
}
func Name(cluster, namespace, name, suffix string) string {
	h := sha256.Sum256([]byte(cluster + "/" + namespace + "/" + name))
	s := regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(suffix), "")
	if len(s) > 12 {
		s = s[:12]
	}
	if s == "" {
		s = "resource"
	}
	return "aep" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h[:])[:10]) + "-" + s
}
func Hash(spec map[string]any) string {
	b, _ := json.Marshal(spec)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Retain(tier, tierPolicy, claimPolicy string) bool {
	return tier == "prod" || tierPolicy == "Retain" || claimPolicy == "Retain"
}
func preset(s string) (int, int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	series := slices.Index([]string{"t1", "c1", "s1", "u1", "m1"}, parts[0])
	size := slices.Index([]string{"nano", "micro", "small", "medium", "large", "xlarge", "2xlarge", "4xlarge"}, parts[1])
	if series < 0 || size < 0 {
		return 0, 0, false
	}
	cpu := 1 << size
	return cpu, cpu * (1 << series), true
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ScaleKeys are the environment config keys a standard change may touch.
var ScaleKeys = []string{"resourcesPreset", "size", "replicas"}

// ScaleCeiling returns the most restrictive limits across all configured
// kinds; the change service applies it to scale-only standard changes because
// a ResourceReleaseBinding does not reveal its backend kind.
func (c Config) ScaleCeiling() Limits {
	out := Limits{MaxReplicas: -1}
	for _, l := range c.Kinds {
		if out.MaxReplicas < 0 || l.MaxReplicas < out.MaxReplicas {
			out.MaxReplicas = l.MaxReplicas
		}
		if out.MaxPreset == "" || presetExceeds(out.MaxPreset, l.MaxPreset) {
			out.MaxPreset = l.MaxPreset
		}
		if out.MaxSize == "" || quantityExceeds(out.MaxSize, l.MaxSize) {
			out.MaxSize = l.MaxSize
		}
	}
	return out
}

func presetExceeds(a, b string) bool {
	ac, am, aok := preset(a)
	bc, bm, bok := preset(b)
	if !aok || !bok {
		return false
	}
	return bc < ac || bm < am
}

func quantityExceeds(a, b string) bool {
	qa, ea := resource.ParseQuantity(a)
	qb, eb := resource.ParseQuantity(b)
	if ea != nil || eb != nil {
		return false
	}
	return qb.Cmp(qa) < 0
}

// ScaleWithinLimits verifies that environment configs only scale a service
// and stay inside the given limits.
func ScaleWithinLimits(l Limits, configs map[string]any) error {
	for k := range configs {
		if !slices.Contains(ScaleKeys, k) {
			return fmt.Errorf("config %s is not a scaling parameter", k)
		}
	}
	if v, ok := configs["resourcesPreset"]; ok {
		p, ok := v.(string)
		cpu, mem, valid := preset(p)
		mc, mm, _ := preset(l.MaxPreset)
		if !ok || !valid || cpu > mc || mem > mm {
			return fmt.Errorf("resourcesPreset exceeds %s or is unknown", l.MaxPreset)
		}
	}
	if v, ok := configs["size"]; ok {
		s, ok := v.(string)
		q, e := resource.ParseQuantity(s)
		max, _ := resource.ParseQuantity(l.MaxSize)
		if !ok || e != nil || q.Sign() <= 0 || q.Cmp(max) > 0 {
			return fmt.Errorf("size must be positive and at most %s", l.MaxSize)
		}
	}
	if v, ok := configs["replicas"]; ok {
		n, ok := v.(float64)
		if !ok || n < 1 || n > float64(l.MaxReplicas) || n != float64(int64(n)) {
			return fmt.Errorf("replicas must be an integer between 1 and %d", l.MaxReplicas)
		}
	}
	return nil
}

func Validate(c *api.ServiceClaim, cfg Config, allowed []string) (map[string]any, error) {
	l, ok := cfg.Kinds[c.Spec.Kind]
	if !ok || !slices.Contains(allowed, c.Spec.Kind) {
		return nil, fmt.Errorf("kind %q is not allowed", c.Spec.Kind)
	}
	if !strings.HasPrefix(c.Namespace, "dp-") {
		return nil, fmt.Errorf("claim namespace must start with dp-")
	}
	if len(c.Name) > 63 || len(validation.IsDNS1035Label(c.Name)) > 0 {
		return nil, fmt.Errorf("claim name must be a DNS-1035 label of at most 63 characters")
	}
	for _, s := range []string{c.Spec.Resource.Project, c.Spec.Resource.Name, c.Spec.Resource.Environment} {
		if s == "" || len(validation.IsValidLabelValue(s)) > 0 {
			return nil, fmt.Errorf("resource identity must contain valid nonempty label values")
		}
	}
	if c.Spec.DeletionPolicy != "" && c.Spec.DeletionPolicy != "Delete" && c.Spec.DeletionPolicy != "Retain" {
		return nil, fmt.Errorf("invalid deletionPolicy")
	}
	out := map[string]any{}
	for _, params := range []map[string]json.RawMessage{raw(c.Spec.Parameters), raw(c.Spec.EnvironmentParameters)} {
		for k, v := range params {
			if !slices.Contains(l.AllowedParameters, k) {
				return nil, fmt.Errorf("parameter %s is not allowed for %s", k, c.Spec.Kind)
			}
			if _, exists := out[k]; exists {
				return nil, fmt.Errorf("duplicate parameter %s", k)
			}
			var value any
			if e := json.Unmarshal(v, &value); e != nil {
				return nil, fmt.Errorf("invalid parameter %s", k)
			}
			out[k] = value
		}
	}
	for _, k := range []string{"resourcesPreset", "size", "replicas"} {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("parameter %s is required to enforce tier limits", k)
		}
	}
	p, ok := out["resourcesPreset"].(string)
	cpu, mem, valid := preset(p)
	mc, mm, _ := preset(l.MaxPreset)
	if !ok || !valid || cpu > mc || mem > mm {
		return nil, fmt.Errorf("resourcesPreset exceeds %s or is unknown", l.MaxPreset)
	}
	size, ok := out["size"].(string)
	q, e := resource.ParseQuantity(size)
	max, _ := resource.ParseQuantity(l.MaxSize)
	if !ok || e != nil || q.Sign() <= 0 || q.Cmp(max) > 0 {
		return nil, fmt.Errorf("size must be positive and at most %s", l.MaxSize)
	}
	n, ok := out["replicas"].(float64)
	if !ok || n < 1 || n > float64(l.MaxReplicas) || n != float64(int64(n)) {
		return nil, fmt.Errorf("replicas must be an integer between 1 and %d", l.MaxReplicas)
	}
	out["replicas"] = int64(n)
	if v, exists := out["version"]; exists {
		s, ok := v.(string)
		versions := []string{"v7", "v8"}
		if c.Spec.Kind == "Postgres" {
			versions = []string{"v13", "v14", "v15", "v16", "v17", "v18"}
		}
		if !ok || !slices.Contains(versions, s) {
			return nil, fmt.Errorf("unsupported version")
		}
	}
	if c.Spec.Kind == "Postgres" {
		if e := validatePostgres(out); e != nil {
			return nil, e
		}
	}
	return out, nil
}
func validatePostgres(out map[string]any) error {
	users, ok := out["users"].(map[string]any)
	if !ok || len(users) == 0 {
		return fmt.Errorf("users must be a nonempty map")
	}
	for name, v := range users {
		obj, ok := v.(map[string]any)
		if !identifier.MatchString(name) || len(name) > 63 || !ok || len(obj) != 0 {
			return fmt.Errorf("users.%s must be an empty object; privileges and passwords are not allowed", name)
		}
	}
	dbs, ok := out["databases"].(map[string]any)
	if !ok || len(dbs) == 0 {
		return fmt.Errorf("databases must be a nonempty map")
	}
	for name, v := range dbs {
		if !identifier.MatchString(name) || len(name) > 54 || slices.Contains([]string{"postgres", "template0", "template1"}, name) {
			return fmt.Errorf("invalid database name %s", name)
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("database %s must be an object", name)
		}
		for key, value := range obj {
			if key != "roles" {
				return fmt.Errorf("databases.%s.%s is not allowed", name, key)
			}
			roles, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("roles must be an object")
			}
			for role, list := range roles {
				if role != "admin" && role != "readonly" {
					return fmt.Errorf("database role %s is not allowed", role)
				}
				names, ok := list.([]any)
				if !ok {
					return fmt.Errorf("role members must be an array")
				}
				for _, member := range names {
					s, ok := member.(string)
					if !ok {
						return fmt.Errorf("role member must be a string")
					}
					if _, ok := users[s]; !ok {
						return fmt.Errorf("role member %s is not a declared user", s)
					}
				}
			}
		}
	}
	return nil
}

func raw(m map[string]ext.JSON) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		out[k] = v.Raw
	}
	return out
}
