package operator

import (
	"reflect"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestBuildsValues(t *testing.T) {
	builds := api.ControlPlaneBuilds{RegistryHost: "harbor.example.com"}
	got, err := BuildsValues(builds, []string{"acme", "globex"}, "https://api.cp.example.com", "https://id.example.com/realms/aep/protocol/openid-connect/token")
	if err != nil {
		t.Fatal(err)
	}
	want := M{"plane": "control", "registry": M{"host": "harbor.example.com"},
		"organizations": []any{M{"namespace": "acme", "workflowPlane": "nonprod"}, M{"namespace": "globex", "workflowPlane": "nonprod"}},
		"openchoreo":    M{"apiUrl": "https://api.cp.example.com", "tokenUrl": "https://id.example.com/realms/aep/protocol/openid-connect/token"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
	for _, tc := range []struct {
		template, want, err string
		orgs                []string
	}{
		{template: "aep-{{.Organization}}-publisher", orgs: []string{"acme", "globex"}},
		{template: "shared-publisher", orgs: []string{"acme", "globex"}, want: "shared-publisher"},
		{template: "pub-{{.Organization}}", orgs: []string{"acme"}, want: "pub-acme"},
		{template: "pub-{{.Organization}}", orgs: []string{"acme", "globex"}, err: "renders 2 different client IDs"},
		{template: "{{.Tier}}", orgs: []string{"acme"}, err: "publisherClientIDTemplate"},
	} {
		builds.PublisherClientIDTemplate = tc.template
		got, err := BuildsValues(builds, tc.orgs, "https://api", "https://token")
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("%s: err %v", tc.template, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := got["openchoreo"].(M)["publisherClientId"].(string); id != tc.want {
			t.Fatalf("%s: publisherClientId %q, want %q", tc.template, id, tc.want)
		}
	}
	if got := ComponentTypesValues(nil); !reflect.DeepEqual(got, M{"buildOrganizations": []any{}}) {
		t.Fatal(got)
	}
	if got := oidcTokenURL(api.ControlPlaneOIDC{Issuer: "https://id/realms/aep"}); got != "https://id/realms/aep/protocol/openid-connect/token" {
		t.Fatal(got)
	}
}

func (f *cpFixture) updateSpec(mutate func(*api.ControlPlaneSpec)) {
	f.t.Helper()
	mutate(&f.cp.Spec)
	if err := f.client.Update(f.ctx, f.cp); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) values(release string) M {
	f.t.Helper()
	v, _, _ := unstructured.NestedMap(f.release(release).Object, "spec", "values")
	return v
}

func (f *cpFixture) resourceType(name string) (*unstructured.Unstructured, error) {
	return f.remote.get(f.ctx, ocAPI, "clusterresourcetypes", "", name)
}

func TestControlPlaneContent(t *testing.T) {
	vcs := &api.VCSSpec{Provider: "github", URL: "https://github.com", Org: "acme"}
	building, plain, pending, elsewhere := organization(), organization(), organization(), organization()
	building.Spec.VCS, building.Spec.ControlPlaneRef = vcs, "default"
	plain.Name, plain.Spec.ControlPlaneRef = "plain", "default"
	pending.Name, pending.Spec.VCS, pending.Spec.ControlPlaneRef = "pending", vcs, "default" // no namespace on aep-cp yet
	elsewhere.Name, elsewhere.Spec.VCS, elsewhere.Spec.ControlPlaneRef = "elsewhere", vcs, "cp2"
	f := newCPFixture(t, building, plain, pending, elsewhere)
	f.converge()
	for _, name := range []string{"cozystack-kafka", "cozystack-postgres", "cozystack-redis"} {
		u, err := f.resourceType(name)
		if err != nil || u.GetLabels()[ControlPlaneLabel] != "cp-uid" || u.GetKind() != "ClusterResourceType" {
			t.Fatal(name, err)
		}
	}
	if c := meta.FindStatusCondition(f.cp.Status.Conditions, "ContentReady"); c.Message != "resource types cozystack-kafka, cozystack-postgres, cozystack-redis; component types" {
		t.Fatal(c.Message)
	}
	// Without builds, no organization needs namespaced types.
	if got := f.values("aep-cp-aep-component-types"); !reflect.DeepEqual(got, M{"buildOrganizations": []any{}}) {
		t.Fatal(got)
	}
	hr := f.release("aep-cp-aep-component-types")
	if deps, _, _ := unstructured.NestedSlice(hr.Object, "spec", "dependsOn"); !reflect.DeepEqual(deps, []any{M{"name": "aep-cp-openchoreo-control-plane"}}) || hr.GetLabels()[ControlPlaneLabel] != "cp-uid" {
		t.Fatal(deps, hr.GetLabels())
	}
	source, err := f.mgmt.get(f.ctx, "source.toolkit.fluxcd.io/v1", "ocirepositories", cpNS, "aep-cp-aep-component-types")
	if url, _, _ := unstructured.NestedString(source.Object, "spec", "url"); err != nil || url != "oci://ghcr.io/tym83/aep/charts/aep-component-types" {
		t.Fatal(err, url)
	}
	if digest, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "digest"); !strings.HasPrefix(digest, "sha256:") {
		t.Fatal("content chart is not pinned by digest", source.Object["spec"])
	}

	// Builds: Organizations on this control plane with a VCS and a namespace on aep-cp.
	f.create(f.remoteDyn, "namespaces", obj("v1", "Namespace", "", "acme", nil))
	f.updateSpec(func(s *api.ControlPlaneSpec) {
		s.Content.Builds = &api.ControlPlaneBuilds{RegistryHost: "harbor.example.com"}
	})
	f.expect("ContentReady", false, "waiting for HelmRelease tenant-aepcp/aep-cp-aep-builds Ready")
	if got := f.values("aep-cp-aep-component-types"); !reflect.DeepEqual(got, M{"buildOrganizations": []any{"acme"}}) {
		t.Fatal(got)
	}
	want := M{"plane": "control", "registry": M{"host": "harbor.example.com"}, "organizations": []any{M{"namespace": "acme", "workflowPlane": "nonprod"}},
		"openchoreo": M{"apiUrl": "https://api.cp.example.com", "tokenUrl": "https://id.example.com/realms/aep/protocol/openid-connect/token"}}
	if got := f.values("aep-cp-aep-builds"); !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
	f.markReady(f.mgmtDyn, helmAPI, "helmreleases", cpNS, "aep-cp-aep-builds")
	f.expect("ContentReady", true, "builds for acme")
	f.expect("Ready", true, "")

	t.Run("idempotent", func(t *testing.T) {
		specs := map[string]any{}
		for _, name := range []string{"aep-cp-aep-component-types", "aep-cp-aep-builds"} {
			specs[name] = f.release(name).Object["spec"]
		}
		f.mgmtDyn.ClearActions()
		f.remoteDyn.ClearActions()
		if _, err := f.reconcile(); err != nil {
			t.Fatal(err)
		}
		for name, spec := range specs {
			if !reflect.DeepEqual(f.release(name).Object["spec"], spec) {
				t.Fatalf("%s changed on a second reconcile", name)
			}
		}
		for _, d := range []*dynamicfake.FakeDynamicClient{f.mgmtDyn, f.remoteDyn} {
			for _, a := range d.Actions() {
				if a.GetVerb() == "create" || a.GetVerb() == "delete" || a.GetVerb() == "update" {
					t.Fatalf("second reconcile changed an object: %v", a)
				}
			}
		}
	})

	t.Run("toggles", func(t *testing.T) {
		disabled := false
		f.updateSpec(func(s *api.ControlPlaneSpec) {
			s.Content.ResourceTypes = []string{"cozystack-redis"}
			s.Content.ComponentTypes = &disabled
			s.Content.Builds = nil
		})
		f.expect("ContentReady", false, "waiting for switched-off content to be removed: cozystack-kafka, cozystack-postgres, aep-component-types, aep-builds")
		f.expect("ContentReady", true, "resource types cozystack-redis")
		for _, name := range []string{"cozystack-kafka", "cozystack-postgres"} {
			if _, err := f.resourceType(name); err == nil {
				t.Fatal("deselected resource type kept:", name)
			}
		}
		for _, name := range []string{"aep-cp-aep-component-types", "aep-cp-aep-builds"} {
			if _, err := f.mgmt.get(f.ctx, helmAPI, "helmreleases", cpNS, name); err == nil {
				t.Fatal("disabled content kept:", name)
			}
			if _, err := f.mgmt.get(f.ctx, "source.toolkit.fluxcd.io/v1", "ocirepositories", cpNS, name); err == nil {
				t.Fatal("disabled content source kept:", name)
			}
		}
	})
}

func TestControlPlaneContentOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		adopt  bool
		want   string // "" when adopted
	}{
		{name: "unlabelled", want: "refusing to adopt ClusterResourceType /cozystack-redis"},
		{name: "unlabelled with adoption", adopt: true},
		{name: "organization label with adoption", labels: map[string]string{OwnerLabel: "org-uid"}, adopt: true, want: "refusing to adopt"},
		{name: "other control plane with adoption", labels: map[string]string{ControlPlaneLabel: "other-uid"}, adopt: true, want: "refusing to adopt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPFixture(t)
			existing := obj(ocAPI, "ClusterResourceType", "", "cozystack-redis", M{"hand": "built"})
			existing.SetLabels(tc.labels)
			f.create(f.remoteDyn, "clusterresourcetypes", existing)
			if tc.adopt {
				f.updateSpec(func(s *api.ControlPlaneSpec) { s.Adoption = &api.ControlPlaneAdoption{Confirm: cpNS + "/aepcp"} })
			}
			f.install()
			if _, err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			c := meta.FindStatusCondition(f.cp.Status.Conditions, "ContentReady")
			u, err := f.resourceType("cozystack-redis")
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				f.markReady(f.mgmtDyn, helmAPI, "helmreleases", cpNS, "aep-cp-aep-component-types")
				if _, err = f.reconcile(); err != nil {
					t.Fatal(err)
				}
				c = meta.FindStatusCondition(f.cp.Status.Conditions, "ContentReady")
				if c.Status != metav1.ConditionTrue || u.GetLabels()[ControlPlaneLabel] != "cp-uid" || u.Object["spec"].(map[string]any)["hand"] != nil {
					t.Fatalf("not adopted: %+v %v", c, u.Object)
				}
				return
			}
			if c.Status == metav1.ConditionTrue || !strings.Contains(c.Message, tc.want) || !reflect.DeepEqual(u.GetLabels(), tc.labels) {
				t.Fatalf("foreign object taken over: %+v %v", c, u.GetLabels())
			}
		})
	}
}
