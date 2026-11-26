package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestMirrorSources(t *testing.T) {
	o := Options{ChartRegistryMirror: "mirror.local/cache", RegistrySecret: "registry-auth", RegistryCASecret: "registry-ca"}
	for _, c := range DeliveryCharts() {
		u := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "tenant", "chart", M{"url": c.URL})
		o.chartSource(u)
		spec := u.Object["spec"].(map[string]any)
		if !strings.HasPrefix(spec["url"].(string), "oci://mirror.local/cache/") {
			t.Fatal(spec)
		}
		if spec["secretRef"].(map[string]any)["name"] != "registry-auth" || spec["certSecretRef"].(map[string]any)["name"] != "registry-ca" {
			t.Fatal(spec)
		}
	}
}

// Set AEP_UPSTREAM_CHARTS to a directory containing the pinned, unpacked charts.
// CI supplies these; developers can run the rest of the suite without registries.
func TestUpstreamMirrorRendering(t *testing.T) {
	dir := os.Getenv("AEP_UPSTREAM_CHARTS")
	if dir == "" {
		t.Skip("set AEP_UPSTREAM_CHARTS for upstream Helm rendering")
	}
	for _, chart := range DeliveryCharts() {
		t.Run(chart.Name, func(t *testing.T) {
			values := MirrorValues(chart.Name, "mirror.local/cache", ReleaseValues(chart.Name))
			b, e := json.Marshal(values)
			if e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command("helm", "template", "aep", filepath.Join(dir, chart.Name), "--values", "-", "--include-crds")
			cmd.Stdin = strings.NewReader(string(b))
			out, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("%v: %s", e, out)
			}
			for _, image := range regexp.MustCompile(`(?m)^\s+image: *"?([^"\s]+)`).FindAllStringSubmatch(string(out), -1) {
				if !strings.Contains(image[1], "@sha256:") {
					t.Fatalf("unpinned delivered image: %s", image[1])
				}
			}
			// Scan all rendered text, including hooks and controller environment defaults.
			for _, host := range []string{"ghcr.io/", "cr.kgateway.dev/", "cr.openchoreo.dev/", "reg.kyverno.io/", "quay.io/", "registry.k8s.io/", "docker.io/"} {
				stripped := strings.ReplaceAll(string(out), "mirror.local/cache/"+host, "")
				// Descriptions and CRD examples can contain unrelated public URLs; only
				// manifests with workloads are scanned below, CRDs have no executable images.
				for _, doc := range strings.Split(stripped, "\n---") {
					if strings.Contains(doc, "kind: CustomResourceDefinition") {
						continue
					}
					if strings.Contains(doc, host) {
						t.Fatalf("public reference %s in %s", host, doc)
					}
				}
			}
		})
	}
}

func TestNormalizeMirrorOptions(t *testing.T) {
	o := Options{ImageRegistryMirror: "oci://mirror.local/cache///", ChartRegistryMirror: "oci://mirror.local/cache///", RegistryInsecure: true}
	o.Normalize()
	if o.ImageRegistryMirror != "mirror.local/cache" || o.ChartRegistryMirror != o.ImageRegistryMirror {
		t.Fatal(o)
	}
	u := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "tenant", "chart", M{"url": DeliveryCharts()[0].URL})
	o.chartSource(u)
	spec := u.Object["spec"].(map[string]any)
	if spec["insecure"] != true || !strings.HasPrefix(spec["url"].(string), "oci://mirror.local/cache/") {
		t.Fatal(spec)
	}
	values := MirrorValues("kgateway", o.ImageRegistryMirror, M{})
	if values["image"].(map[string]any)["registry"] != "mirror.local/cache/cr.kgateway.dev/kgateway-dev" {
		t.Fatal(values)
	}
}

func TestMirrorSecrets(t *testing.T) {
	ctx := context.Background()
	source := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mirror-auth", Namespace: "aep-system"}, Type: core.SecretTypeDockerConfigJson, Data: map[string][]byte{".dockerconfigjson": []byte("credentials")}}
	ca := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mirror-ca", Namespace: "aep-system"}, Data: map[string][]byte{"ca.crt": []byte("certificate")}}
	d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := &Reconciler{Management: &Cluster{Kube: kubefake.NewClientset(source, ca), Dynamic: d}, Options: Options{RegistrySecret: "mirror-auth", RegistryCASecret: "mirror-ca"}}
	s := &session{r: r, o: &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "owner"}}}
	if err := s.mirrorSecrets(ctx, "tenant-acme-prod"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mirror-auth", "mirror-ca"} {
		got, err := r.Management.get(ctx, "v1", "secrets", "tenant-acme-prod", name)
		if err != nil || got.GetLabels()[OwnerLabel] != "owner" || got.Object["data"] == nil {
			t.Fatal(got, err)
		}
	}
	s.o.UID = "other-owner"
	if err := s.mirrorSecrets(ctx, "tenant-acme-prod"); err == nil {
		t.Fatal("adopted foreign credentials")
	}
	r.Options.RegistrySecret = "missing"
	if err := s.mirrorSecrets(ctx, "tenant-acme-test"); err == nil {
		t.Fatal("missing source credentials accepted")
	}
}

func TestSourceUsesInventoryDigest(t *testing.T) {
	o := Options{}
	for _, chart := range DeliveryCharts() {
		u := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "tenant", "chart", M{"url": chart.URL, "ref": M{"tag": chart.Version}})
		if err := o.chartSource(u); err != nil {
			t.Fatal(err)
		}
		ref := u.Object["spec"].(map[string]any)["ref"].(map[string]any)
		if ref["tag"] != nil || !strings.HasPrefix(ref["digest"].(string), "sha256:") {
			t.Fatal(ref)
		}
	}
}
