package resourcetypes

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestEmbeddedClusterResourceTypes(t *testing.T) {
	onDisk, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := fs.Glob(Files, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) == 0 || strings.Join(embedded, ",") != strings.Join(onDisk, ",") {
		t.Fatalf("embedded %v, directory %v", embedded, onDisk)
	}
	for _, name := range embedded {
		b, err := Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var u struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec map[string]any `json:"spec"`
		}
		// One document per file: the operator applies each file as one object.
		if strings.Contains(string(b), "\n---") {
			t.Fatalf("%s holds several documents", name)
		}
		if err := yaml.Unmarshal(b, &u); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if u.APIVersion != "openchoreo.dev/v1alpha1" || u.Kind != "ClusterResourceType" || u.Metadata.Namespace != "" || len(u.Spec) == 0 {
			t.Fatalf("%s is not a cluster-scoped ClusterResourceType: %+v", name, u)
		}
		if u.Metadata.Name+".yaml" != name {
			t.Fatalf("%s names %s", name, u.Metadata.Name)
		}
	}
}
