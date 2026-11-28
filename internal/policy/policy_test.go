package policy

import (
	"encoding/json"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		n := Name("cluster", "dp-test", fmt.Sprint(i), "Orders-DB_"+strings.Repeat("x", 100))
		if len(n) > 26 || !regexp.MustCompile(`^aep[a-z2-7]{10}-[a-z0-9]{1,12}$`).MatchString(n) {
			t.Fatal(n)
		}
		if seen[n] {
			t.Fatalf("collision: %s", n)
		}
		seen[n] = true
		if n != Name("cluster", "dp-test", fmt.Sprint(i), "Orders-DB_"+strings.Repeat("x", 100)) {
			t.Fatal("not deterministic")
		}
	}
	for _, parts := range [][3]string{{"a", "b", "c"}, {"b", "b", "c"}, {"a", "c", "c"}, {"a", "b", "d"}} {
		n := Name(parts[0], parts[1], parts[2], "!!!")
		if seen[n] {
			t.Fatal("identity collision")
		}
		seen[n] = true
	}
}
func Claim() *api.ServiceClaim {
	return &api.ServiceClaim{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "dp-test"}, Spec: api.ServiceClaimSpec{Kind: "Postgres", Resource: api.ResourceIdentity{Project: "payments", Name: "orders", Environment: "dev"}, Parameters: map[string]ext.JSON{"version": {Raw: []byte(`"v17"`)}, "users": {Raw: []byte(`{"app":{}}`)}, "databases": {Raw: []byte(`{"appdb":{"roles":{"admin":["app"]}}}`)}}, EnvironmentParameters: map[string]ext.JSON{"resourcesPreset": {Raw: []byte(`"t1.small"`)}, "size": {Raw: []byte(`"2Gi"`)}, "replicas": {Raw: []byte(`1`)}}}}
}
func TestParameters(t *testing.T) {
	tests := []struct {
		name, key, value string
		valid            bool
	}{
		{"valid", "size", `"2Gi"`, true}, {"privilege", "users", `{"app":{"createRole":true}}`, false}, {"false privilege still forbidden", "users", `{"app":{"replication":false}}`, false}, {"password", "users", `{"app":{"password":"secret"}}`, false}, {"external", "external", `true`, false}, {"bootstrap", "bootstrap", `{}`, false}, {"extensions", "databases", `{"db":{"extensions":["dblink"]}}`, false}, {"unknown role", "databases", `{"db":{"roles":{"superuser":["app"]}}}`, false}, {"unknown member", "databases", `{"db":{"roles":{"admin":["other"]}}}`, false}, {"oversize", "size", `"101Gi"`, false}, {"negative", "size", `"-1Gi"`, false}, {"fractional replicas", "replicas", `1.2`, false}, {"too many replicas", "replicas", `4`, false}, {"preset memory", "resourcesPreset", `"m1.large"`, false}, {"preset cpu", "resourcesPreset", `"t1.xlarge"`, false}, {"unknown version", "version", `"latest"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Claim()
			if _, ok := c.Spec.EnvironmentParameters[tt.key]; ok {
				c.Spec.EnvironmentParameters[tt.key] = ext.JSON{Raw: []byte(tt.value)}
			} else {
				c.Spec.Parameters[tt.key] = ext.JSON{Raw: []byte(tt.value)}
			}
			_, err := Validate(c, Default(), []string{"Postgres"})
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
		})
	}
	c := Claim()
	if _, e := Validate(c, Default(), []string{"Redis"}); e == nil {
		t.Fatal("kind bypass")
	}
	c.Spec.EnvironmentParameters["version"] = c.Spec.Parameters["version"]
	if _, e := Validate(c, Default(), []string{"Postgres"}); e == nil {
		t.Fatal("duplicate bypass")
	}
	cfg := Default()
	l := cfg.Kinds["Postgres"]
	l.AllowedParameters = []string{"size"}
	cfg.Kinds["Postgres"] = l
	if _, e := Validate(Claim(), cfg, []string{"Postgres"}); e == nil {
		t.Fatal("configured allowlist ignored")
	}
}
func TestHashCanonical(t *testing.T) {
	a := map[string]any{"replicas": int64(1)}
	b := map[string]any{}
	_ = json.Unmarshal([]byte(`{"replicas":1}`), &b)
	if Hash(a) != Hash(b) {
		t.Fatal("numeric representation changes hash")
	}
}
func TestDeletion(t *testing.T) {
	for _, tier := range []string{"prod", "nonprod"} {
		for _, policy := range []string{"Delete", "Retain"} {
			for _, claim := range []string{"", "Delete", "Retain"} {
				want := tier == "prod" || policy == "Retain" || claim == "Retain"
				if Retain(tier, policy, claim) != want {
					t.Fatalf("%s %s %s", tier, policy, claim)
				}
			}
		}
	}
}

func kafkaClaim() *api.ServiceClaim {
	return &api.ServiceClaim{ObjectMeta: metav1.ObjectMeta{Name: "events", Namespace: "dp-test"}, Spec: api.ServiceClaimSpec{Kind: "Kafka", Resource: api.ResourceIdentity{Project: "payments", Name: "events", Environment: "dev"}, Parameters: map[string]ext.JSON{"topics": {Raw: []byte(`[{"name":"orders"}]`)}}, EnvironmentParameters: map[string]ext.JSON{"resourcesPreset": {Raw: []byte(`"t1.small"`)}, "size": {Raw: []byte(`"10Gi"`)}, "replicas": {Raw: []byte(`1`)}}}}
}
func TestKafkaParameters(t *testing.T) {
	out, err := Validate(kafkaClaim(), Default(), []string{"Kafka"})
	if err != nil {
		t.Fatal(err)
	}
	topic := out["topics"].([]any)[0].(map[string]any)
	if out["version"] != "4.1.1" || topic["partitions"] != int64(3) || topic["name"] != "orders" || len(topic["config"].(map[string]any)) != 0 {
		t.Fatal("defaults not applied", out)
	}
	for _, tt := range []struct {
		name, key, value string
		valid            bool
	}{
		{"five replicas", "replicas", `5`, true},
		{"six replicas", "replicas", `6`, false},
		{"version", "version", `"4.1.1"`, true},
		{"unknown version", "version", `"3.9.0"`, false},
		{"redis version", "version", `"v8"`, false},
		{"config", "topics", `[{"name":"orders","partitions":100,"config":{"retention.ms":"60000","cleanup.policy":"compact"}}]`, true},
		{"empty topics", "topics", `[]`, true},
		{"long topic", "topics", `[{"name":"` + strings.Repeat("a", 100) + `"}]`, true},
		{"too long topic", "topics", `[{"name":"` + strings.Repeat("a", 101) + `"}]`, false},
		{"uppercase topic", "topics", `[{"name":"Orders"}]`, false},
		{"dotted topic", "topics", `[{"name":"orders.v1"}]`, false},
		{"duplicate topic", "topics", `[{"name":"orders"},{"name":"orders"}]`, false},
		{"zero partitions", "topics", `[{"name":"orders","partitions":0}]`, false},
		{"too many partitions", "topics", `[{"name":"orders","partitions":101}]`, false},
		{"fractional partitions", "topics", `[{"name":"orders","partitions":1.5}]`, false},
		{"numeric config", "topics", `[{"name":"orders","config":{"retention.ms":60000}}]`, false},
		{"bad config key", "topics", `[{"name":"orders","config":{"Retention":"1"}}]`, false},
		{"unknown topic field", "topics", `[{"name":"orders","replicas":3}]`, false},
		{"topics object", "topics", `{"orders":{}}`, false},
		{"postgres users", "users", `{"app":{}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := kafkaClaim()
			if _, ok := c.Spec.EnvironmentParameters[tt.key]; ok {
				c.Spec.EnvironmentParameters[tt.key] = ext.JSON{Raw: []byte(tt.value)}
			} else {
				c.Spec.Parameters[tt.key] = ext.JSON{Raw: []byte(tt.value)}
			}
			_, err := Validate(c, Default(), []string{"Kafka"})
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
		})
	}
	cfg := Default()
	l := cfg.Kinds["Kafka"]
	l.MaxReplicas = 6
	cfg.Kinds["Kafka"] = l
	dir := t.TempDir()
	b, _ := json.Marshal(cfg)
	path := dir + "/policy.yaml"
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Kafka replica cap must not be raised by configuration")
	}
}
