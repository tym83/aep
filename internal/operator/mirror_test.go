package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/version"
	inventory "github.com/tym83/aep/release"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
		if err := o.chartSource(u); err != nil {
			t.Fatal(err)
		}
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
			for _, doc := range strings.Split(string(out), "\n---") {
				// CRD schemas declare image fields (Strimzi: "image: {}"); they are not workloads.
				if strings.Contains(doc, "kind: CustomResourceDefinition") {
					continue
				}
				for _, image := range regexp.MustCompile(`(?m)^\s+image: *"?([^"\s]+)`).FindAllStringSubmatch(doc, -1) {
					checkMirroredImage(t, image[1], "mirror.local/cache/")
				}
			}
			// Scan all rendered text, including hooks and controller environment defaults.
			for _, host := range []string{"ghcr.io/", "cr.kgateway.dev/", "cr.openchoreo.dev/", "reg.kyverno.io/", "quay.io/", "registry.k8s.io/", "docker.io/", "ecr-public.aws.com/"} {
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

// checkMirroredImage requires exactly one mirror prefix in front of an
// inventory repository, so a chart cannot re-host an image under another
// registry path. Upstream images also carry their digest; own images are
// pinned to the operator build at runtime.
func checkMirroredImage(t *testing.T, ref, prefix string) {
	t.Helper()
	upstream, ok := strings.CutPrefix(ref, prefix)
	if !ok || strings.Contains(upstream, prefix) {
		t.Fatalf("image not mirrored exactly once: %s", ref)
	}
	repo, _, _ := strings.Cut(upstream, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, a := range inventory.Artifacts() {
		if a.Kind == "image" && a.Ref == repo {
			if !a.Own && !strings.Contains(upstream, "@sha256:") {
				t.Fatalf("unpinned delivered image: %s", ref)
			}
			return
		}
	}
	t.Fatalf("mirrored image is no inventory repository: %s", ref)
}

func TestChartLayerSelector(t *testing.T) {
	for _, c := range DeliveryCharts() {
		u := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "tenant", "chart", M{"url": c.URL, "ref": M{"tag": c.Version}})
		if err := (Options{ChartRegistryMirror: "mirror.local/cache"}).chartSource(u); err != nil {
			t.Fatal(err)
		}
		selector, found, _ := unstructured.NestedMap(u.Object, "spec", "layerSelector")
		if c.Name != ExternalSecretsChart {
			if found {
				t.Fatal(c.Name, selector)
			}
			continue
		}
		if selector["mediaType"] != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" || selector["operation"] != "copy" {
			t.Fatal(selector)
		}
	}
}

func TestDeliveryTargets(t *testing.T) {
	var tier, cp []string
	for _, c := range DeliveryCharts() {
		switch c.Target {
		case TargetTier:
			tier = append(tier, c.Name)
		case TargetControlPlane:
			cp = append(cp, c.Name)
		}
		if c.Namespace == "" || !strings.HasPrefix(c.URL, "oci://") || !strings.HasSuffix(c.URL, "/"+c.Name) {
			t.Fatal(c)
		}
	}
	if strings.Join(tier, ",") != "kgateway-crds,kgateway,openchoreo-data-plane,"+StrimziChart+",kyverno,argo-cd" {
		t.Fatal(tier)
	}
	if strings.Join(cp, ",") != ControlPlaneChart+","+ExternalSecretsChart {
		t.Fatal(cp)
	}
	names := []string{}
	for _, c := range tierCharts {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "kgateway-crds,kgateway,openchoreo-data-plane,"+StrimziChart {
		t.Fatal("tier delivery changed", names)
	}
}

func TestNormalizeMirrorOptions(t *testing.T) {
	o := Options{ImageRegistryMirror: "oci://mirror.local/cache///", ChartRegistryMirror: "oci://mirror.local/cache///", RegistryInsecure: true}
	o.Normalize()
	if o.ImageRegistryMirror != "mirror.local/cache" || o.ChartRegistryMirror != o.ImageRegistryMirror {
		t.Fatal(o)
	}
	u := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "tenant", "chart", M{"url": DeliveryCharts()[0].URL})
	if err := o.chartSource(u); err != nil {
		t.Fatal(err)
	}
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

func TestArgoMirroredDelivery(t *testing.T) {
	for _, override := range []string{"", "https://charts.corp.example/argo", ArgoCDChartOCI, "oci://mirror.local/cache/argoproj.github.io/argo-helm/argo-cd"} {
		t.Run(override, func(t *testing.T) {
			o := Options{ArgoCDChartURL: override, ChartRegistryMirror: "mirror.local/cache", ImageRegistryMirror: "mirror.local/cache", RegistrySecret: "auth", RegistryCASecret: "ca", RegistryInsecure: true}
			values := MirrorValues("argo-cd", o.ImageRegistryMirror, argoValues(argoOrg(), "argo.example", "https://id.example", []string{"payments"}))
			source, hr, err := argoDelivery(argoOrg(), "tenant-acme-nonprod", o, values)
			if err != nil {
				t.Fatal(err)
			}
			spec := source.Object["spec"].(M)
			if source.GetKind() != "OCIRepository" || spec["url"] != "oci://mirror.local/cache/argoproj.github.io/argo-helm/argo-cd" || spec["insecure"] != true || spec["secretRef"].(M)["name"] != "auth" || spec["certSecretRef"].(M)["name"] != "ca" {
				t.Fatal(source)
			}
			if !strings.HasPrefix(spec["ref"].(M)["digest"].(string), "sha256:") {
				t.Fatal(spec)
			}
			hs := hr.Object["spec"].(M)
			if hs["chart"] != nil || hs["chartRef"].(M)["kind"] != "OCIRepository" {
				t.Fatal(hs)
			}
			global := values["global"].(M)
			if global["domain"] != "argo.example" || global["securityContext"] == nil {
				t.Fatal("mirror overwrote existing values", global)
			}
			for _, key := range []string{"global", "redis"} {
				im := values[key].(M)["image"].(M)
				if !strings.HasPrefix(im["repository"].(string), "mirror.local/cache/") || !strings.Contains(im["tag"].(string), "@sha256:") {
					t.Fatal(im)
				}
			}
		})
	}
}

func TestMirroredBridgePin(t *testing.T) {
	old := version.Version
	version.Version = "sha-1234567"
	defer func() { version.Version = old }()
	o := Options{ImageRegistryMirror: "mirror.local/cache"}
	ref := "ghcr.io/tym83/aep-bridge:sha-1234567"
	a, err := o.pinnedImage(ref)
	if err != nil || a != o.ImageRegistryMirror+"/"+ref {
		t.Fatal(a, err)
	}
	b, err := o.pinnedImage(a)
	if err != nil || a != b {
		t.Fatal(a, b, err)
	}
	for _, ref := range []string{"ghcr.io/tym83/aep-bridge:sha-7654321", "ghcr.io/tym83/aep-bridge@sha256:" + strings.Repeat("0", 64), "ghcr.io/tym83/aep-unlisted:sha-1234567", "example.org/upstream:v1"} {
		if _, err := o.pinnedImage(ref); err == nil {
			t.Fatal("accepted", ref)
		}
	}
	explicit := "ghcr.io/tym83/aep-bridge:v1.2.3@sha256:" + strings.Repeat("a", 64)
	if got, err := o.pinnedImage(explicit); err != nil || got != o.ImageRegistryMirror+"/"+explicit {
		t.Fatal(got, err)
	}
}
