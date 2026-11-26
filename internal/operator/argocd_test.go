package operator

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"os"
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
		t.Skip("run tools/check-argocd.sh to test the pinned upstream chart")
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	pods := 0
	for {
		var u unstructured.Unstructured
		e = decoder.Decode(&u.Object)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
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
				image, _ := c["image"].(string)
				if image != "quay.io/argoproj/argocd:v3.4.4" && image != "ecr-public.aws.com/docker/library/redis:8.2.3-alpine" {
					t.Errorf("untracked image %s", image)
				}
			}
		}
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
	s := &session{o: o, vcsVerified: true, r: &Reconciler{Management: &Cluster{Dynamic: management, Kube: kubefake.NewSimpleClientset()}, Options: Options{Namespace: "aep-system", ArgoCDHostTemplate: "argo.{{.Organization}}.example", KeycloakURL: "https://id.example", KeycloakRealm: "acme"}}, cp: &Cluster{Dynamic: cp}, tiers: map[string]*Cluster{"nonprod": {}}}
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
