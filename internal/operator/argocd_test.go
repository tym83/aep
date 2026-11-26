package operator

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func argoOrg() *api.Organization {
	return &api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: api.OrganizationSpec{VCS: &api.VCSSpec{Provider: "gitlab", URL: "https://git.example", GroupPath: "acme", CredentialsSecretRef: api.SecretNameReference{Name: "git"}, ReadCredentialsSecretRef: api.SecretNameReference{Name: "git-read"}}, Tiers: map[string]api.TierSpec{"nonprod": {ArgoCD: &api.ArgoCDSpec{Enabled: true}}}}}
}
func TestArgoIsolation(t *testing.T) {
	o := argoOrg()
	v := argoValues(o, "argo.example", "https://id.example/realms/acme", []string{"default", "payments"})
	if v["createClusterRoles"] != false {
		t.Fatal("cluster privileges enabled")
	}
	for _, item := range v["extraObjects"].([]any) {
		u := item.(M)
		if u["kind"] == "AppProject" {
			spec := u["spec"].(M)
			for _, dest := range spec["destinations"].([]any) {
				ns := dest.(M)["namespace"].(string)
				if !strings.HasPrefix(ns, "gitops-") {
					t.Fatal(ns)
				}
			}
		}
		if u["kind"] == "ClusterRole" || u["kind"] == "ClusterRoleBinding" {
			t.Fatal("cluster RBAC")
		}
	}
	p := v["configs"].(M)["rbac"].(M)["policy.csv"].(string)
	if strings.Contains(p, "applications, *, */*") || !strings.Contains(p, "aep-payments/*") {
		t.Fatal(p)
	}
	if path := os.Getenv("ARGO_VALUES_OUTPUT"); path != "" {
		data, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
func TestIntegrationValidation(t *testing.T) {
	o := argoOrg()
	if e := validateIntegrations(o); e != nil {
		t.Fatal(e)
	}
	o.Spec.Tiers["prod"] = api.TierSpec{ArgoCD: &api.ArgoCDSpec{Enabled: true}}
	if e := validateIntegrations(o); e == nil || !strings.Contains(e.Error(), "only in nonprod") {
		t.Fatal(e)
	}
	delete(o.Spec.Tiers, "prod")
	for _, url := range []string{"https://git.example.evil/a", "https://user:token@git.example", "https://git.example/*", "http://git.example"} {
		o.Spec.VCS.URL = url
		if validateIntegrations(o) == nil {
			t.Fatal(url)
		}
	}
}
func TestArgoHost(t *testing.T) {
	if h, e := argoHost("argocd.{{.Organization}}.example", "acme"); e != nil || h != "argocd.acme.example" {
		t.Fatal(h, e)
	}
	for _, p := range []string{"", "{{.Unknown}}", "https://bad.example"} {
		if _, e := argoHost(p, "acme"); e == nil {
			t.Fatal(p)
		}
	}
}

func TestRenderedArgoSecurity(t *testing.T) {
	path := os.Getenv("ARGO_RENDERED_INPUT")
	if path == "" {
		chart := "../../release/.cache/argo-cd"
		if _, err := os.Stat(filepath.Join(chart, "Chart.yaml")); err != nil {
			t.Skip("cache pinned chart with tools/check-argocd.sh for offline rendering")
		}
		if _, err := exec.LookPath("helm"); err != nil {
			t.Skip("Helm required")
		}
		values, _ := json.Marshal(argoValues(argoOrg(), "argo.example", "https://id.example", []string{"payments"}))
		file := filepath.Join(t.TempDir(), "values.json")
		if err := os.WriteFile(file, values, 0600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command("helm", "template", "argocd", chart, "--namespace", "argocd", "-f", file).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, output)
		}
		path = filepath.Join(t.TempDir(), "rendered.yaml")
		if err := os.WriteFile(path, output, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	pods := 0
	schemesChecked := false
	for {
		var u unstructured.Unstructured
		e = decoder.Decode(&u.Object)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if u.GetKind() == "ConfigMap" && u.GetName() == "argocd-cm" {
			schemes, found, _ := unstructured.NestedString(u.Object, "data", "helm.valuesFileSchemes")
			if !found || strings.TrimSpace(schemes) != "" {
				t.Fatal("remote values schemes must be explicitly empty", schemes)
			}
			schemesChecked = true
		}
		if u.GetKind() == "ClusterRole" || u.GetKind() == "ClusterRoleBinding" {
			t.Fatalf("unexpected cluster privileges: %s", u.GetName())
		}
		if u.GetKind() != "Deployment" && u.GetKind() != "StatefulSet" && u.GetKind() != "Job" {
			continue
		}
		pods++
		pod, _, _ := unstructured.NestedMap(u.Object, "spec", "template", "spec")
		nonroot, _, _ := unstructured.NestedBool(pod, "securityContext", "runAsNonRoot")
		seccomp, _, _ := unstructured.NestedString(pod, "securityContext", "seccompProfile", "type")
		for _, field := range []string{"containers", "initContainers"} {
			cs, _, _ := unstructured.NestedSlice(pod, field)
			for _, raw := range cs {
				c := raw.(map[string]any)
				sc, _, _ := unstructured.NestedMap(c, "securityContext")
				if !nonroot && sc["runAsNonRoot"] != true {
					t.Errorf("%s: root container", u.GetName())
				}
				typ, _, _ := unstructured.NestedString(sc, "seccompProfile", "type")
				if typ != "RuntimeDefault" && seccomp != "RuntimeDefault" {
					t.Errorf("%s: seccomp missing", u.GetName())
				}
				if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
					t.Errorf("%s: insecure container %s", u.GetName(), c["name"])
				}
				drop, _, _ := unstructured.NestedStringSlice(sc, "capabilities", "drop")
				if len(drop) != 1 || drop[0] != "ALL" {
					t.Errorf("%s: capabilities not dropped", u.GetName())
				}
				for _, field := range []string{"requests", "limits"} {
					for _, resource := range []string{"cpu", "memory"} {
						value, _, _ := unstructured.NestedString(c, "resources", field, resource)
						if value == "" {
							t.Errorf("%s: missing %s %s", u.GetName(), field, resource)
						}
					}
				}
				image, _ := c["image"].(string)
				if image != "quay.io/argoproj/argocd:v3.4.4" && image != "ecr-public.aws.com/docker/library/redis:8.2.3-alpine" {
					t.Errorf("untracked image %s", image)
				}
			}
		}
	}
	if !schemesChecked {
		t.Fatal("missing Argo settings ConfigMap")
	}
	if pods < 4 {
		t.Fatal("missing Argo CD workloads")
	}
}

func TestArgoDeliveryContract(t *testing.T) {
	o := argoOrg()
	o.UID = "org-uid"
	management := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cp := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr(ocAPI, "projects"): "ProjectList"}, obj(ocAPI, "Project", "acme", "payments", M{}))
	s := &session{o: o, vcsVerified: true, r: &Reconciler{ResolveHost: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, nil
	}, Management: &Cluster{Dynamic: management, Kube: kubefake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "argocd-acme-oidc", Namespace: "aep-system"}, Data: map[string][]byte{"rootCA": []byte("CUSTOM\nCA")}})}, Options: Options{Namespace: "aep-system", ArgoCDHostTemplate: "argo.{{.Organization}}.example", ArgoCDChartURL: "https://mirror.example/charts", KeycloakURL: "https://id.example", KeycloakRealm: "acme"}}, cp: &Cluster{Dynamic: cp}, tiers: map[string]*Cluster{"nonprod": {}}}
	if e := s.argocd(context.Background()); e == nil {
		t.Fatal("reported Ready without OIDC credentials")
	}
	hr, e := management.Resource(gvr(helmAPI, "helmreleases")).Namespace("tenant-acme-nonprod").Get(context.Background(), "aep-argocd", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	for _, check := range []struct {
		path []string
		want string
	}{
		{[]string{"spec", "kubeConfig", "secretRef", "name"}, "kubernetes-apps-admin-kubeconfig"},
		{[]string{"spec", "kubeConfig", "secretRef", "key"}, "super-admin.svc"},
		{[]string{"spec", "targetNamespace"}, "argocd"},
		{[]string{"spec", "chart", "spec", "version"}, ArgoCDChartVersion},
	} {
		got, _, _ := unstructured.NestedString(hr.Object, check.path...)
		if got != check.want {
			t.Fatalf("%v: %s", check.path, got)
		}
	}
	source, e := management.Resource(gvr("source.toolkit.fluxcd.io/v1", "helmrepositories")).Namespace("tenant-acme-nonprod").Get(context.Background(), "aep-argocd", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	chartURL, _, _ := unstructured.NestedString(source.Object, "spec", "url")
	if chartURL != "https://mirror.example/charts" {
		t.Fatal(chartURL)
	}
	extra, _, _ := unstructured.NestedSlice(hr.Object, "spec", "values", "extraObjects")
	networkFound := false
	for _, raw := range extra {
		u := raw.(map[string]any)
		if u["kind"] == "NetworkPolicy" {
			networkFound = true
		}
	}
	if !networkFound {
		t.Fatal("missing repo-server egress policy")
	}
	oidc, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "configs", "cm", "oidc.config")
	if !strings.Contains(oidc, "rootCA: |\n  CUSTOM\n  CA\n") {
		t.Fatal("Keycloak root CA missing", oidc)
	}
	if hr.GetLabels()["cozystack.io/target-cluster-name"] != "kubernetes-apps" || hr.GetLabels()["sharding.fluxcd.io/key"] != "tenants" {
		t.Fatal(hr.GetLabels())
	}
	for _, a := range management.Actions() {
		if a.GetNamespace() == "tenant-acme-prod" {
			t.Fatal("touched prod")
		}
	}
}
func TestRepositorySchemaPaths(t *testing.T) {
	schema := M{"properties": M{"source": M{"properties": M{"cloneURL": M{"type": "string", "x-openchoreo-component-parameter-repository-url": true}}}}}
	paths := repositoryPaths(schema, nil)
	if len(paths) != 1 || strings.Join(paths[0], ".") != "source.cloneURL" {
		t.Fatal(paths)
	}
}

func TestOptionalIntegrationFieldsAndDeepCopy(t *testing.T) {
	o := argoOrg()
	data, e := json.Marshal(o)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), "caBundleSecretRef") {
		t.Fatal("absent optional Secret reference was serialized")
	}
	copy := o.DeepCopy()
	copy.Spec.Tiers["nonprod"].ArgoCD.Enabled = false
	if !argoEnabled(o) {
		t.Fatal("Argo CD spec aliases its deep copy")
	}
	o.Spec.VCS.CABundleSecretRef = &api.SecretNameReference{Name: "ca"}
	copy = o.DeepCopy()
	copy.Spec.VCS.CABundleSecretRef.Name = "other"
	if o.Spec.VCS.CABundleSecretRef.Name != "ca" {
		t.Fatal("CA reference aliases its deep copy")
	}
	o.Spec.Tiers["prod"] = api.TierSpec{ArgoCD: &api.ArgoCDSpec{Enabled: false}}
	if validateIntegrations(o) == nil {
		t.Fatal("accepted Argo CD field on prod")
	}
}

func TestArgoCannotPublishIngress(t *testing.T) {
	o := argoOrg()
	o.UID = "org-uid"
	v := argoValues(o, "argo.example", "https://id.example", []string{"payments"})
	for _, raw := range v["extraObjects"].([]any) {
		u := raw.(M)
		if u["kind"] == "Role" {
			b, _ := json.Marshal(u)
			if strings.Contains(string(b), `"ingresses"`) || strings.Contains(string(b), `"tlsroutes"`) || strings.Contains(string(b), `"grpcroutes"`) {
				t.Fatal(string(b))
			}
		}
		if u["kind"] == "Namespace" {
			n := &unstructured.Unstructured{Object: u}
			if n.GetLabels()["aep.aenix.io/argocd"] != "allowed" || n.GetLabels()[OwnerLabel] != "org-uid" {
				t.Fatal(n)
			}
		}
	}
}

func TestArgoProjectRBAC(t *testing.T) {
	v := argoValues(argoOrg(), "argo.example", "https://id.example", []string{"payments", "shop"})
	policy := v["configs"].(M)["rbac"].(M)["policy.csv"].(string)
	for _, want := range []string{"g, aep-acme-payments-developer, role:aep-payments", "g, /aep-acme-shop-developer, role:aep-shop", "g, aep-acme-developer, role:readonly", "g, /aep-acme-platform, role:admin"} {
		if !strings.Contains(policy, want+"\n") {
			t.Fatal(want, policy)
		}
	}
	if strings.Contains(policy, "g, aep-acme-developer, role:aep-") {
		t.Fatal(policy)
	}
	if argoChartURL(Options{ArgoCDChartURL: "https://mirror.example"}) != "https://mirror.example" {
		t.Fatal("mirror ignored")
	}
	if strings.TrimSpace(v["configs"].(M)["cm"].(M)["helm.valuesFileSchemes"].(string)) != "" {
		t.Fatal("remote values enabled")
	}
}
func TestArgoEgress(t *testing.T) {
	calls := []string{}
	resolve := func(_ context.Context, host string) ([]net.IPAddr, error) {
		calls = append(calls, host)
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.4")}, {IP: net.ParseIP("2001:db8::4")}}, nil
	}
	p, err := argoEgress(context.Background(), "https://git.example:8443", "https://id.example", resolve)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	for _, want := range []string{"192.0.2.4/32", "2001:db8::4/128", "8443", "kube-dns", "argocd-redis"} {
		if !strings.Contains(string(b), want) {
			t.Fatal(want, string(b))
		}
	}
	if strings.Join(calls, ",") != "git.example,id.example" || strings.Contains(string(b), "0.0.0.0/0") {
		t.Fatal(string(b), calls)
	}
	if _, err = argoEgress(context.Background(), "https://git.example", "https://id.example", func(context.Context, string) ([]net.IPAddr, error) { return nil, nil }); err == nil {
		t.Fatal("DNS failed open")
	}
}
