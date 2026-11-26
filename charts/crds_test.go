package charts

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Validate the checked-in manifests on the API server's create path, including
// structural schemas, CEL compilation, and per-rule and total estimated costs.
func TestCustomResourceDefinitions(t *testing.T) {
	roots, err := filepath.Glob("*/crds")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("no chart CRD directories found")
	}
	roots = append(roots, "../config/crd")
	for _, root := range roots {
		count := 0
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return nil
			}
			t.Run(path, func(t *testing.T) {
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
				for document := 1; ; document++ {
					var raw json.RawMessage
					if err := decoder.Decode(&raw); err == io.EOF {
						break
					} else if err != nil {
						t.Fatalf("document %d: %v", document, err)
					}
					if len(raw) == 0 || string(raw) == "null" {
						continue
					}
					t.Run(fmt.Sprintf("document-%d", document), func(t *testing.T) {
						var crd apiextensionsv1.CustomResourceDefinition
						if err := json.Unmarshal(raw, &crd); err != nil {
							t.Fatal(err)
						}
						if crd.Kind != "CustomResourceDefinition" {
							return
						}
						count++
						if crd.APIVersion != apiextensionsv1.SchemeGroupVersion.String() {
							t.Fatalf("unsupported CRD apiVersion %q", crd.APIVersion)
						}
						// Creation discards submitted status. Defaulting also initializes
						// storedVersions from the storage version, as PrepareForCreate does.
						crd.Status = apiextensionsv1.CustomResourceDefinitionStatus{}
						apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(&crd)
						var internal apiextensions.CustomResourceDefinition
						if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internal, nil); err != nil {
							t.Fatal(err)
						}
						for _, err := range validation.ValidateCustomResourceDefinition(t.Context(), &internal) {
							t.Errorf("%s: %v", crd.Name, err)
						}
					})
				}
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Errorf("no CRDs found under %s", root)
		}
	}
}
