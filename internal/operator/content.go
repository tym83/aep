package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"text/template"

	api "github.com/tym83/aep/api/v1alpha1"
	resourcetypes "github.com/tym83/aep/resource-types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// Own content charts for aep-cp. Their Chart.yaml carries the
// aep.aenix.io/fixed-version annotation, so builds keep these versions and the
// embedded inventory pins their digests.
const (
	OwnChartRepository         = "oci://ghcr.io/tym83/aep/charts"
	ComponentTypesChart        = "aep-component-types"
	ComponentTypesChartVersion = "0.3.0"
	BuildsChart                = "aep-builds"
	BuildsChartVersion         = "0.3.0"
)

// BuildsWorkflowPlane is the namespaced WorkflowPlane of an organization
// that runs its builds (docs/vcs.md).
const BuildsWorkflowPlane = "nonprod"

// contentChart is an own content chart released into the control plane
// chart namespace of aep-cp.
func contentChart(name, version string) DeliveryChart {
	return DeliveryChart{Name: name, Version: version, URL: OwnChartRepository + "/" + name, Namespace: OpenChoreoNamespace, Target: TargetControlPlane}
}

// ComponentTypesValues configures aep-component-types: organizations with
// in-platform builds get namespaced copies of the types that allow their
// namespaced build Workflow.
func ComponentTypesValues(buildOrganizations []string) M {
	orgs := []any{}
	for _, o := range buildOrganizations {
		orgs = append(orgs, o)
	}
	return M{"buildOrganizations": orgs}
}

// BuildsValues configures aep-builds on aep-cp: one build Workflow per
// organization, bound to its WorkflowPlane, pushing to the registry and
// publishing Workloads through the OpenChoreo API.
func BuildsValues(b api.ControlPlaneBuilds, organizations []string, apiURL, tokenURL string) (M, error) {
	orgs := []any{}
	for _, o := range organizations {
		orgs = append(orgs, M{"namespace": o, "workflowPlane": BuildsWorkflowPlane})
	}
	openchoreo := M{"apiUrl": apiURL, "tokenUrl": tokenURL}
	id, err := publisherClientID(b.PublisherClientIDTemplate, organizations)
	if err != nil {
		return nil, err
	}
	if id != "" {
		openchoreo["publisherClientId"] = id
	}
	return M{"plane": "control", "organizations": orgs, "registry": M{"host": b.RegistryHost}, "openchoreo": openchoreo}, nil
}

// publisherClientID renders the publisher client ID template for each
// organization. The chart defaults to aep-<namespace>-publisher and accepts
// one override for all organizations, so a template must render either that
// default or one ID shared by every organization.
func publisherClientID(text string, organizations []string) (string, error) {
	if text == "" {
		return "", nil
	}
	tmpl, err := template.New("publisher").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	ids := map[string]bool{}
	custom := false
	for _, o := range organizations {
		var b bytes.Buffer
		if err = tmpl.Execute(&b, struct{ Organization string }{o}); err != nil {
			return "", fmt.Errorf("content.builds.publisherClientIDTemplate: %w", err)
		}
		ids[b.String()] = true
		custom = custom || b.String() != "aep-"+o+"-publisher"
	}
	switch {
	case !custom:
		return "", nil
	case len(ids) > 1:
		return "", fmt.Errorf("content.builds.publisherClientIDTemplate renders %d different client IDs; aep-builds %s accepts one ID for all organizations or its default aep-<organization>-publisher", len(ids), BuildsChartVersion)
	}
	for id := range ids {
		return id, nil
	}
	return "", nil
}

// oidcTokenURL is the token endpoint of the control plane's IdP, by default
// in the Keycloak layout below the issuer, as in ControlPlaneValues.
func oidcTokenURL(o api.ControlPlaneOIDC) string {
	return orDefault(o.TokenURL, o.Issuer+"/protocol/openid-connect/token")
}

// embeddedResourceType decodes one embedded ClusterResourceType manifest.
func embeddedResourceType(file string) (*unstructured.Unstructured, error) {
	b, err := resourcetypes.Files.ReadFile(file)
	if err != nil {
		return nil, err
	}
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	u := &unstructured.Unstructured{}
	if err = u.UnmarshalJSON(j); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return u, nil
}

// contentResult summarises the delivered content for ContentReady.
type contentResult struct {
	resourceTypes []string
	componentType bool
	builds        []string
	buildsEnabled bool
}

func (c contentResult) String() string {
	parts := []string{"resource types " + orDefault(strings.Join(c.resourceTypes, ", "), "none")}
	if c.componentType {
		parts = append(parts, "component types")
	}
	if c.buildsEnabled {
		parts = append(parts, "builds for "+orDefault(strings.Join(c.builds, ", "), "no organizations"))
	}
	return strings.Join(parts, "; ")
}

// content delivers step 9: the embedded ClusterResourceTypes and the
// component type and build charts, as spec.content selects. Content that is
// switched off is removed when the control plane owns it.
func (s *controlPlaneSession) content(ctx context.Context) error {
	remote, e := s.identity(ctx)
	if e != nil {
		return e
	}
	result := contentResult{}
	var removing []string
	if result.resourceTypes, removing, e = s.resourceTypes(ctx, remote); e != nil {
		return e
	}
	spec := s.cp.Spec.Content
	result.componentType = spec.ComponentTypes == nil || *spec.ComponentTypes
	result.buildsEnabled = spec.Builds != nil
	if result.buildsEnabled {
		if result.builds, e = s.buildOrganizations(ctx, remote); e != nil {
			return e
		}
	}
	var releases []string
	if result.componentType {
		if e = s.applyChart(ctx, contentChart(ComponentTypesChart, ComponentTypesChartVersion), ControlPlaneChart, ComponentTypesValues(result.builds)); e != nil {
			return e
		}
		releases = append(releases, ComponentTypesChart)
	} else if gone, e := s.removeChart(ctx, ComponentTypesChart); e != nil {
		return e
	} else if gone {
		removing = append(removing, ComponentTypesChart)
	}
	if result.buildsEnabled {
		values, e := BuildsValues(*spec.Builds, result.builds, "https://api."+s.cp.Spec.Domain, oidcTokenURL(s.cp.Spec.OIDC))
		if e != nil {
			return e
		}
		if e = s.applyChart(ctx, contentChart(BuildsChart, BuildsChartVersion), ControlPlaneChart, values); e != nil {
			return e
		}
		releases = append(releases, BuildsChart)
	} else if gone, e := s.removeChart(ctx, BuildsChart); e != nil {
		return e
	} else if gone {
		removing = append(removing, BuildsChart)
	}
	if len(removing) > 0 {
		return pending("waiting for switched-off content to be removed: %s", strings.Join(removing, ", "))
	}
	for _, name := range releases {
		if e = s.releaseReady(ctx, name); e != nil {
			return e
		}
	}
	s.contentMessage = result.String()
	return nil
}

// resourceTypes applies the selected embedded ClusterResourceTypes to
// aep-cp and removes owned ones that are no longer selected. It returns the
// applied and the still removed names.
func (s *controlPlaneSession) resourceTypes(ctx context.Context, remote *Cluster) (applied, removing []string, err error) {
	files, e := fs.Glob(resourcetypes.Files, "*.yaml")
	if e != nil {
		return nil, nil, e
	}
	selected := s.cp.Spec.Content.ResourceTypes
	for _, file := range files {
		u, e := embeddedResourceType(file)
		if e != nil {
			return nil, nil, e
		}
		if len(selected) > 0 && !slices.Contains(selected, strings.TrimSuffix(file, ".yaml")) {
			gone, e := s.removeOwned(ctx, remote, u.GetAPIVersion(), "clusterresourcetypes", "", u.GetName())
			if e != nil {
				return nil, nil, s.contractDrift(ctx, e)
			}
			if gone {
				removing = append(removing, u.GetName())
			}
			continue
		}
		if e = s.adopt(ctx, remote, "clusterresourcetypes", u); e != nil {
			return nil, nil, s.contractDrift(ctx, e)
		}
		if e = remote.applyAs(ctx, s.owner, "clusterresourcetypes", u); e != nil {
			return nil, nil, s.contractDrift(ctx, e)
		}
		applied = append(applied, u.GetName())
	}
	return applied, removing, nil
}

// adopt labels an existing unlabelled object of the same name as owned when
// spec.adoption confirms the takeover of a hand-built control plane.
// Objects labelled for another owner are never adopted: applyAs refuses them.
func (s *controlPlaneSession) adopt(ctx context.Context, c *Cluster, resource string, u *unstructured.Unstructured) error {
	if s.cp.Spec.Adoption == nil {
		return nil
	}
	old, e := c.get(ctx, u.GetAPIVersion(), resource, u.GetNamespace(), u.GetName())
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if labels := old.GetLabels(); labels[ControlPlaneLabel] != "" || labels[OwnerLabel] != "" {
		return nil
	}
	patch, e := json.Marshal(M{"metadata": M{"labels": M{ControlPlaneLabel: string(s.cp.UID)}, "resourceVersion": old.GetResourceVersion()}})
	if e != nil {
		return e
	}
	_, e = c.resource(u.GetAPIVersion(), resource, u.GetNamespace()).Patch(ctx, u.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	return e
}

// buildOrganizations are the Organizations on this control plane with a
// corporate VCS, the input of in-platform builds, whose OpenChoreo
// namespace exists on aep-cp. Organizations being deleted are left out.
func (s *controlPlaneSession) buildOrganizations(ctx context.Context, remote *Cluster) ([]string, error) {
	var organizations api.OrganizationList
	if e := s.r.List(ctx, &organizations); e != nil {
		return nil, e
	}
	var out []string
	for i := range organizations.Items {
		o := &organizations.Items[i]
		if !o.DeletionTimestamp.IsZero() || o.Spec.VCS == nil || !s.r.Options.usesControlPlane(o, s.cp.Name) {
			continue
		}
		_, e := remote.get(ctx, "v1", "namespaces", "", o.Name)
		if apierrors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return nil, s.contractDrift(ctx, e)
		}
		out = append(out, o.Name)
	}
	slices.Sort(out)
	return out, nil
}

// removeChart deletes the owned HelmRelease and OCIRepository of a content
// chart that is switched off; Flux uninstalls it from aep-cp.
func (s *controlPlaneSession) removeChart(ctx context.Context, chart string) (bool, error) {
	release, e := s.removeOwned(ctx, s.r.Management, helmAPI, "helmreleases", s.ns, releaseName(chart))
	if e != nil {
		return false, e
	}
	source, e := s.removeOwned(ctx, s.r.Management, "source.toolkit.fluxcd.io/v1", "ocirepositories", s.ns, releaseName(chart))
	return release || source, e
}

// removeOwned deletes an object owned by this control plane and reports
// whether it still existed; objects of anyone else are left alone.
func (s *controlPlaneSession) removeOwned(ctx context.Context, c *Cluster, av, resource, ns, name string) (bool, error) {
	u, e := c.get(ctx, av, resource, ns, name)
	if apierrors.IsNotFound(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if u.GetLabels()[ControlPlaneLabel] != string(s.cp.UID) {
		return false, nil
	}
	return true, s.deleteObject(ctx, c, resource, u)
}
