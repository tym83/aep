package bridge

import (
	"os"
	"path/filepath"
	"testing"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Set AEP_UPSTREAM_CHARTS to the unpacked pinned charts (as CI does) to check
// rendered objects against the delivered Strimzi CRDs: valid, nothing pruned.
func TestKafkaObjectsMatchStrimziCRDs(t *testing.T) {
	dir := os.Getenv("AEP_UPSTREAM_CHARTS")
	if dir == "" {
		t.Skip("set AEP_UPSTREAM_CHARTS for Strimzi CRD validation")
	}
	files, err := filepath.Glob(filepath.Join(dir, "strimzi-kafka-operator", "crds", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("Strimzi CRDs missing", err)
	}
	type schema struct {
		validator  validation.SchemaValidator
		structural *structuralschema.Structural
	}
	schemas := map[string]schema{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		crd := &ext.CustomResourceDefinition{}
		if err := yaml.Unmarshal(b, crd); err != nil {
			t.Fatal(err)
		}
		for _, v := range crd.Spec.Versions {
			if StrimziAPI != crd.Spec.Group+"/"+v.Name {
				continue
			}
			internal := &apiext.CustomResourceValidation{}
			if err := ext.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(v.Schema, internal, nil); err != nil {
				t.Fatal(err)
			}
			validator, _, err := validation.NewSchemaValidator(internal.OpenAPIV3Schema)
			if err != nil {
				t.Fatal(err)
			}
			structural, err := structuralschema.NewStructural(internal.OpenAPIV3Schema)
			if err != nil {
				t.Fatal(err)
			}
			schemas[crd.Spec.Names.Kind] = schema{validator, structural}
		}
	}
	spec := KafkaSpec{Version: "4.1.1", Size: "10Gi", Preset: "u1.small", Replicas: 3, Topics: []KafkaTopic{{Name: "orders", Partitions: 3, Config: map[string]any{"retention.ms": "60000"}}}}
	kafka, pool, topics, user, err := KafkaObjects("aepx-events", "dp-team", nil, nil, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range append([]*unstructured.Unstructured{kafka, pool, user}, topics...) {
		s, ok := schemas[obj.GetKind()]
		if !ok {
			t.Fatalf("%s is not served as %s", obj.GetKind(), StrimziAPI)
		}
		for _, e := range s.validator.Validate(obj.Object).Errors {
			t.Errorf("%s: %v", obj.GetKind(), e)
		}
		body := obj.DeepCopy().Object
		delete(body, "metadata")
		if pruned := pruning.PruneWithOptions(body, s.structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}); len(pruned) > 0 {
			t.Errorf("%s fields unknown to Strimzi: %v", obj.GetKind(), pruned)
		}
	}
}
