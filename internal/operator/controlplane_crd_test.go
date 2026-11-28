package operator

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// crdAdmission applies a checked-in CRD the way the API server does on
// create and update: structural defaulting, OpenAPI validation and CEL rules.
type crdAdmission struct {
	structural *structuralschema.Structural
	schema     apiservervalidation.SchemaValidator
	cel        *cel.Validator
}

func loadCRD(t *testing.T, path string) *crdAdmission {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	var internal apiextensions.CustomResourceValidation
	if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(crd.Spec.Versions[0].Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(internal.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(internal.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	return &crdAdmission{structural: structural, schema: validator, cel: cel.NewValidator(structural, true, celconfig.PerCallLimit)}
}

// admit defaults and validates obj (and old on update), returning the
// defaulted object and every error.
func (a *crdAdmission) admit(t *testing.T, obj, old M) (M, field.ErrorList) {
	t.Helper()
	obj = jsonObject(t, obj)
	defaulting.Default(obj, a.structural)
	var oldObj any
	if old != nil {
		old = jsonObject(t, old)
		defaulting.Default(old, a.structural)
		oldObj = old
	}
	errs := apiservervalidation.ValidateCustomResource(nil, obj, a.schema)
	celErrs, _ := a.cel.Validate(t.Context(), nil, a.structural, obj, oldObj, celconfig.RuntimeCELCostBudget)
	return obj, append(errs, celErrs...)
}

// jsonObject round-trips through JSON as the API server decodes requests,
// with integers as int64.
func jsonObject(t *testing.T, in M) M {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out M
	if err := utiljson.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func controlPlaneObject() M {
	return M{"apiVersion": "aep.aenix.io/v1alpha1", "kind": "ControlPlane", "metadata": M{"name": "default"}, "spec": M{
		"domain":  "cp.example.com",
		"ingress": M{"clusterIssuer": "letsencrypt-prod"},
		"oidc":    M{"issuer": "https://id.example.com/realms/aep", "portalClient": M{"secretRef": M{"name": "aep-portal-client"}}},
	}}
}

func edit(base M, change func(spec M)) M {
	b, _ := json.Marshal(base)
	var out M
	_ = json.Unmarshal(b, &out)
	change(out["spec"].(M))
	return out
}

func TestControlPlaneCRDDefaults(t *testing.T) {
	got, errs := loadCRD(t, "../../config/crd/aep.aenix.io_controlplanes.yaml").admit(t, controlPlaneObject(), nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	b, _ := json.Marshal(got["spec"])
	for _, want := range []string{`"parent":"tenant-root"`, `"name":"aepcp"`, `"name":"aepcp"`, `"version":"v1.35"`, `"storageClass":"replicated"`,
		`"instanceType":"u1.large"`, `"minReplicas":3`, `"maxReplicas":4`, `"diskSize":"20Gi"`, `"className":"tenant-root"`,
		`"usernameClaim":"preferred_username"`, `"clientID":"openchoreo-backstage-client"`, `"cliClientID":"openchoreo-cli"`,
		`"size":"5Gi"`, `"replicas":1`, `"resourcesPreset":"small"`, `"componentTypes":true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("defaulted spec lacks %s: %s", want, b)
		}
	}
	// The defaulted object is also valid for the operator.
	var cp struct {
		Spec json.RawMessage `json:"spec"`
	}
	b, _ = json.Marshal(got)
	if err := json.Unmarshal(b, &cp); err != nil {
		t.Fatal(err)
	}
	o := controlPlane()
	o.Spec = defaultedSpec(t, cp.Spec)
	if err := ValidateControlPlane(o); err != nil {
		t.Fatal(err)
	}
}

func TestControlPlaneCRDValidation(t *testing.T) {
	a := loadCRD(t, "../../config/crd/aep.aenix.io_controlplanes.yaml")
	base := controlPlaneObject()
	for _, tc := range []struct {
		name   string
		change func(spec M)
		want   string
	}{
		{"both certificate sources", func(s M) { s["ingress"].(M)["tlsSecretRef"] = M{"name": "tls"} }, "exactly one of clusterIssuer or tlsSecretRef"},
		{"no certificate source", func(s M) { delete(s["ingress"].(M), "clusterIssuer") }, "exactly one of clusterIssuer or tlsSecretRef"},
		{"empty issuer name", func(s M) { s["ingress"].(M)["clusterIssuer"] = "" }, "clusterIssuer"},
		{"domain with scheme", func(s M) { s["domain"] = "https://cp.example.com" }, "spec.domain"},
		{"single label domain", func(s M) { s["domain"] = "localhost" }, "spec.domain"},
		{"upper case domain", func(s M) { s["domain"] = "CP.example.com" }, "spec.domain"},
		{"missing domain", func(s M) { delete(s, "domain") }, "spec.domain"},
		{"http issuer", func(s M) { s["oidc"].(M)["issuer"] = "http://id.example.com" }, "spec.oidc.issuer"},
		{"missing client secret", func(s M) { delete(s["oidc"].(M)["portalClient"].(M), "secretRef") }, "secretRef"},
		{"replicas inverted", func(s M) { s["cluster"] = M{"nodeGroup": M{"minReplicas": 5, "maxReplicas": 4}} }, "maxReplicas must not be lower"},
		{"bad preset", func(s M) { s["portal"] = M{"database": M{"resourcesPreset": "t1.small"}} }, "resourcesPreset"},
		{"bad adoption", func(s M) { s["adoption"] = M{"confirm": "aepcp"} }, "spec.adoption.confirm"},
		{"bad parent", func(s M) { s["tenant"] = M{"parent": "aepdev"} }, "spec.tenant.parent"},
		{"dashed tenant name", func(s M) { s["tenant"] = M{"name": "aep-cp"} }, "spec.tenant.name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := a.admit(t, edit(base, tc.change), nil)
			if !strings.Contains(errs.ToAggregate().Error(), tc.want) {
				t.Fatalf("errors %v, want %q", errs, tc.want)
			}
		})
	}
	t.Run("tls secret alone", func(t *testing.T) {
		_, errs := a.admit(t, edit(base, func(s M) { s["ingress"] = M{"tlsSecretRef": M{"name": "tls"}} }), nil)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
	})
}

func TestControlPlaneCRDImmutability(t *testing.T) {
	a := loadCRD(t, "../../config/crd/aep.aenix.io_controlplanes.yaml")
	old := edit(controlPlaneObject(), func(s M) { s["tenant"] = M{"parent": "tenant-aepdev", "name": "cp2"} })
	for _, tc := range []struct {
		name   string
		change func(spec M)
		want   string
	}{
		{"tenant name", func(s M) { s["tenant"].(M)["name"] = "cp3" }, "tenant name is immutable"},
		{"tenant parent", func(s M) { s["tenant"].(M)["parent"] = "tenant-root" }, "tenant parent is immutable"},
		{"tenant defaulted away", func(s M) { delete(s, "tenant") }, "tenant name is immutable"},
		{"cluster name", func(s M) { s["cluster"] = M{"name": "other"} }, "cluster name is immutable"},
		{"quota change", func(s M) { s["tenant"].(M)["quotas"] = M{"cpu": "64"} }, ""},
		{"domain change", func(s M) { s["domain"] = "cp3.example.com" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := a.admit(t, edit(old, tc.change), old)
			if tc.want == "" {
				if len(errs) > 0 {
					t.Fatal(errs)
				}
				return
			}
			if !strings.Contains(errs.ToAggregate().Error(), tc.want) {
				t.Fatalf("errors %v, want %q", errs, tc.want)
			}
		})
	}
}

func organizationObject(ref string) M {
	spec := M{"displayName": "ACME", "profile": "small", "quotas": M{"cpu": "8", "memory": "16Gi"},
		"environments": []any{M{"name": "dev", "tier": "nonprod", "changeModel": "none"}},
		"tiers":        M{"nonprod": M{"cluster": M{"version": "v1.35", "nodeGroups": M{"md0": M{"minReplicas": 1, "maxReplicas": 2, "instanceType": "u1.large"}}}, "services": M{"allowedKinds": []any{"Postgres"}}}}}
	if ref != "" {
		spec["controlPlaneRef"] = ref
	}
	return M{"apiVersion": "aep.aenix.io/v1alpha1", "kind": "Organization", "metadata": M{"name": "acme"}, "spec": spec}
}

func TestOrganizationControlPlaneRefCRD(t *testing.T) {
	for _, path := range []string{"../../config/crd/aep.aenix.io_organizations.yaml", "../../charts/aep-operator/crds/aep.aenix.io_organizations.yaml"} {
		a := loadCRD(t, path)
		for _, tc := range []struct {
			name     string
			old, new string
			create   bool
			want     string
		}{
			{name: "legacy create", create: true},
			{name: "create with reference", new: "cp2", create: true},
			{name: "invalid reference", new: "Not_Valid", create: true, want: "controlPlaneRef"},
			{name: "legacy update"},
			// The API server cannot read the adoption annotation; the
			// operator enforces it (validateControlPlaneRef).
			{name: "set once from empty", new: "cp2"},
			{name: "unchanged", old: "cp2", new: "cp2"},
			{name: "changed", old: "cp2", new: "cp3", want: "controlPlaneRef is immutable once set"},
			{name: "removed", old: "cp2", want: "controlPlaneRef is immutable once set"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var old M
				if !tc.create {
					old = organizationObject(tc.old)
				}
				_, errs := a.admit(t, organizationObject(tc.new), old)
				if tc.want == "" {
					if len(errs) > 0 {
						t.Fatal(errs)
					}
					return
				}
				if !strings.Contains(errs.ToAggregate().Error(), tc.want) {
					t.Fatalf("errors %v, want %q", errs, tc.want)
				}
			})
		}
	}
}

func TestOperatorCRDCopiesIdentical(t *testing.T) {
	for _, name := range []string{"aep.aenix.io_controlplanes.yaml", "aep.aenix.io_organizations.yaml"} {
		source, err := os.ReadFile("../../config/crd/" + name)
		if err != nil {
			t.Fatal(err)
		}
		packaged, err := os.ReadFile("../../charts/aep-operator/crds/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if string(source) != string(packaged) {
			t.Fatalf("%s: operator chart CRD drift", name)
		}
	}
}
