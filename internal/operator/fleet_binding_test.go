package operator

import (
	"context"
	"encoding/json"
	"github.com/google/cel-go/cel"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"os"
	"testing"
)

func TestExternalEnvironmentAdmission(t *testing.T) {
	b, err := os.ReadFile("../../config/crd/aep.aenix.io_organizations.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd M
	if err = json.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	versions := crd["spec"].(M)["versions"].([]any)
	spec := versions[0].(M)["schema"].(M)["openAPIV3Schema"].(M)["properties"].(M)["spec"].(M)
	rules := spec["x-kubernetes-validations"].([]any)
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	base := M{"name": "edge", "tier": "nonprod", "dataPlane": M{"external": "west"}}
	for _, tc := range []struct {
		name string
		next []any
		ok   bool
	}{
		{"same", []any{base}, true}, {"remove", []any{}, false},
		{"tier", []any{M{"name": "edge", "tier": "prod", "dataPlane": M{"external": "west"}}}, false},
		{"cluster", []any{M{"name": "edge", "tier": "nonprod", "dataPlane": M{"external": "east"}}}, false},
		{"local", []any{M{"name": "edge", "tier": "nonprod"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepted := true
			for _, r := range rules {
				ast, issues := env.Compile(r.(M)["rule"].(string))
				if issues.Err() != nil {
					t.Fatal(issues.Err())
				}
				prg, err := env.Program(ast)
				if err != nil {
					t.Fatal(err)
				}
				out, _, err := prg.Eval(map[string]any{"oldSelf": M{"environments": []any{base}}, "self": M{"environments": tc.next}})
				if err != nil {
					t.Fatal(err)
				}
				accepted = accepted && out.Value() == true
			}
			if accepted != tc.ok {
				t.Fatalf("accepted=%v", accepted)
			}
		})
	}
}

// No ExternalCluster reader is configured: pending, invalid or unavailable
// assignments cannot block this Organization step or cause a local binding.
func TestOrganizationReservesExternalEnvironmentBeforeAttach(t *testing.T) {
	ctx := context.Background()
	o := organization()
	o.Spec.Environments[0].DataPlane.External = "edge"
	cp := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
	mgmt := &Cluster{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), Kube: kubefake.NewClientset()}
	s := &session{r: &Reconciler{Management: mgmt, Options: Options{Namespace: "aep-system"}}, o: o, cp: cp}
	if err := s.openChoreo(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.get(ctx, ocAPI, "environments", o.Name, "dev"); !errors.IsNotFound(err) {
		t.Fatal("external environment created locally", err)
	}
	prod, err := cp.get(ctx, ocAPI, "environments", o.Name, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if prod.GetAnnotations()["aep.aenix.io/change-model"] != "required" {
		t.Fatal("local environment lost change model")
	}
	if isProd, _, _ := unstructured.NestedBool(prod.Object, "spec", "isProduction"); !isProd {
		t.Fatal("prod-tier environment not marked as production")
	}
}
