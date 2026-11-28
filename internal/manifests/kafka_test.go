package manifests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tym83/aep/internal/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Resource types may only pass parameters the bridge accepts for their kind.
func TestResourceTypesMatchBridgeKinds(t *testing.T) {
	paths, err := filepath.Glob("../../resource-types/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatal(paths, err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		u := unstructured.Unstructured{Object: decode(t, b)[0]}
		resources, _, _ := unstructured.NestedSlice(u.Object, "spec", "resources")
		template := resources[0].(map[string]any)["template"].(map[string]any)
		kind, _, _ := unstructured.NestedString(template, "spec", "kind")
		m, ok := kinds.Table[kind]
		if !ok {
			t.Fatalf("%s: unknown kind %s", p, kind)
		}
		seen[kind] = true
		for _, section := range []string{"parameters", "environmentParameters"} {
			params, _, _ := unstructured.NestedMap(template, "spec", section)
			for key := range params {
				if !slices.Contains(m.Parameters, key) {
					t.Fatalf("%s: %s.%s is not a bridge parameter", p, section, key)
				}
			}
		}
		versions, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "parameters", "openAPIV3Schema", "properties", "version", "enum")
		for _, v := range versions {
			if !slices.Contains(m.Versions, v) {
				t.Fatalf("%s: version %s is rejected by the bridge", p, v)
			}
		}
		if m.MaxReplicas > 0 {
			max, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "environmentConfigs", "openAPIV3Schema", "properties", "replicas", "maximum")
			if fmt.Sprint(max) != fmt.Sprint(m.MaxReplicas) {
				t.Fatalf("%s: replicas maximum %v, bridge cap %d", p, max, m.MaxReplicas)
			}
		}
	}
	if !seen["Kafka"] {
		t.Fatal("Kafka resource type missing")
	}
}

func TestKafkaResourceTypeOutputs(t *testing.T) {
	b, err := os.ReadFile("../../resource-types/cozystack-kafka.yaml")
	if err != nil {
		t.Fatal(err)
	}
	u := unstructured.Unstructured{Object: decode(t, b)[0]}
	outputs, _, _ := unstructured.NestedSlice(u.Object, "spec", "outputs")
	got := map[string]string{}
	for _, o := range outputs {
		m := o.(map[string]any)
		if ref, ok := m["secretKeyRef"].(map[string]any); ok {
			got[m["name"].(string)] = "secret:" + fmt.Sprint(ref["key"])
		} else {
			got[m["name"].(string)] = fmt.Sprint(m["value"])
		}
	}
	for name, want := range map[string]string{"password": "secret:password", "ca.crt": "secret:ca.crt", "saslMechanism": "SCRAM-SHA-512"} {
		if got[name] != want {
			t.Fatalf("output %s=%q, want %q", name, got[name], want)
		}
	}
	for _, name := range []string{"host", "port", "username", "bootstrapServers"} {
		if !strings.Contains(got[name], "applied.claim.status.connection") {
			t.Fatalf("output %s must come from the claim connection status: %q", name, got[name])
		}
	}
}

// The bridge may manage Strimzi resources only through a namespaced Role in
// the Kafka namespace, in the operator bundle and in the app-cluster chart.
func TestKafkaBridgeRBAC(t *testing.T) {
	bundle := []byte{}
	for _, p := range []string{"../../config/app-cluster/rbac.yaml", "../../config/app-cluster/admission.yaml"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(append(bundle, b...), []byte("\n---\n")...)
	}
	sources := map[string][]byte{"bundle": bundle}
	if _, err := exec.LookPath("helm"); err == nil {
		b, err := exec.Command("helm", "template", "bridge", "../../charts/aep-bridge-appcluster").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		sources["chart"] = b
	}
	for source, raw := range sources {
		t.Run(source, func(t *testing.T) {
			roles, bindings, policies := 0, 0, 0
			for _, d := range decode(t, raw) {
				u := unstructured.Unstructured{Object: d}
				rules, _, _ := unstructured.NestedSlice(d, "rules")
				for _, r := range rules {
					groups := fmt.Sprint(r.(map[string]any)["apiGroups"])
					if strings.Contains(groups, "kafka.strimzi.io") && (u.GetKind() != "Role" || u.GetNamespace() != kinds.KafkaNamespace) {
						t.Fatalf("Strimzi access outside a %s Role: %s %s", kinds.KafkaNamespace, u.GetKind(), u.GetName())
					}
				}
				switch u.GetKind() {
				case "Role":
					if u.GetNamespace() != kinds.KafkaNamespace {
						continue
					}
					roles++
					want := "[map[apiGroups:[kafka.strimzi.io] resources:[kafkas kafkanodepools kafkatopics kafkausers] verbs:[get list watch create update patch delete]] map[apiGroups:[] resources:[secrets] verbs:[get]]]"
					if fmt.Sprint(rules) != want {
						t.Fatal(fmt.Sprint(rules))
					}
				case "RoleBinding":
					if u.GetNamespace() != kinds.KafkaNamespace {
						continue
					}
					bindings++
					subjects, _, _ := unstructured.NestedSlice(d, "subjects")
					if fmt.Sprint(subjects) != "[map[kind:ServiceAccount name:aep-bridge namespace:aep-system]]" {
						t.Fatal(subjects)
					}
				case "ValidatingAdmissionPolicy":
					if strings.Contains(fmt.Sprint(d["spec"]), "kafka.strimzi.io") {
						policies++
						if !strings.Contains(fmt.Sprint(d["spec"]), "request.namespace == '"+kinds.KafkaNamespace+"'") {
							t.Fatal("Kafka admission must pin the namespace")
						}
					}
				}
			}
			if roles != 1 || bindings != 1 || policies != 1 {
				t.Fatalf("roles=%d bindings=%d policies=%d", roles, bindings, policies)
			}
		})
	}
}
