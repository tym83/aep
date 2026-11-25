package operator

import (
	"context"
	"fmt"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const KyvernoChartVersion = "3.9.2" // Kyverno 1.19.2; legacy ClusterPolicy rules remain supported.
const PolicyBundleVersion = "0.1.0"

func policyRelease(o *api.Organization, tier, name string, values M) *unstructured.Unstructured {
	spec := M{"interval": "5m", "timeout": "10m", "releaseName": name, "targetNamespace": "kyverno", "storageNamespace": "kyverno",
		"chartRef":   M{"kind": "OCIRepository", "name": "aep-" + name},
		"kubeConfig": M{"secretRef": M{"name": "kubernetes-apps-admin-kubeconfig", "key": "super-admin.svc"}},
		"install":    M{"createNamespace": true, "remediation": M{"retries": int64(-1)}}, "upgrade": M{"remediation": M{"retries": int64(-1)}}, "values": values}
	if name != "kyverno" {
		spec["dependsOn"] = []any{M{"name": "aep-kyverno"}}
	}
	if name != "kyverno" && name != "app-cluster-baseline" {
		spec["dependsOn"] = []any{M{"name": "aep-app-cluster-baseline"}}
	}
	hr := obj(helmAPI, "HelmRelease", TenantNamespace(o.Name, tier), "aep-"+name, spec)
	hr.SetLabels(map[string]string{"cozystack.io/target-cluster-name": "kubernetes-apps", "internal.cozystack.io/tenantresource": "true", "sharding.fluxcd.io/key": "tenants"})
	return hr
}

func policyValues(o *api.Organization, tier, key string) map[string]M {
	replicas := int64(1)
	if tier == "prod" {
		replicas = 3
	}
	mode := o.Spec.Policy.Mode
	if mode == "" {
		mode = "audit"
	}
	values := map[string]M{
		"kyverno": {"admissionController": M{"replicas": replicas}, "backgroundController": M{"replicas": replicas}, "cleanupController": M{"replicas": replicas}, "reportsController": M{"replicas": replicas},
			"features":    M{"policyExceptions": M{"enabled": true, "namespace": "aep-system"}},
			"rbac":        M{"roles": M{"aggregate": M{"admin": false}}},
			"openreports": M{"enabled": false, "installCrds": false}},
		"app-cluster-baseline": {"mode": mode, "organizationUID": string(o.UID), "registries": stringsAny(o.Spec.Policy.Registries)},
		"app-cluster-" + tier:  {},
	}
	if tier == "prod" {
		values["app-cluster-prod"]["cosignPublicKey"] = key
	}
	return values
}

func (s *session) policies(ctx context.Context) error {
	key := s.o.Spec.Policy.CosignPublicKey
	if s.o.Spec.Profile != "small" && strings.TrimSpace(key) == "" {
		return fmt.Errorf("spec.policy.cosignPublicKey is required for production")
	}
	repository := s.r.Options.PolicyBundleRepository
	if repository == "" {
		repository = "oci://ghcr.io/tym83/aep/policies"
	}
	for _, tier := range Tiers(s.o) {
		values := policyValues(s.o, tier, key)
		for _, name := range []string{"kyverno", "app-cluster-baseline", "app-cluster-" + tier} {
			url, version := strings.TrimSuffix(repository, "/")+"/"+name, PolicyBundleVersion
			if name == "kyverno" {
				url, version = "oci://ghcr.io/kyverno/charts/kyverno", KyvernoChartVersion
			}
			if name == "kyverno" && s.r.Options.KyvernoChartURL != "" {
				url = s.r.Options.KyvernoChartURL
			}
			source := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", TenantNamespace(s.o.Name, tier), "aep-"+name, M{"interval": "5m", "url": url, "ref": M{"tag": version}})
			if err := s.r.Management.apply(ctx, s.o, "ocirepositories", source); err != nil {
				return err
			}
			if err := s.r.Management.apply(ctx, s.o, "helmreleases", policyRelease(s.o, tier, name, values[name])); err != nil {
				return err
			}
		}
	}
	// Apply all tiers before waiting so one slow release cannot starve another tier.
	for _, tier := range Tiers(s.o) {
		for _, name := range []string{"kyverno", "app-cluster-baseline", "app-cluster-" + tier} {
			hr, err := s.r.Management.get(ctx, helmAPI, "helmreleases", TenantNamespace(s.o.Name, tier), "aep-"+name)
			if err != nil {
				return err
			}
			if !ready(hr) {
				return pending("waiting for %s %s Ready", tier, name)
			}
		}
	}
	return nil
}

// Expiry cleanup runs with the tier-local operator SA, never remote admin credentials.
// Preconditions prevent deleting an exception whose expiry was concurrently extended.
func cleanExceptions(ctx context.Context, c *Cluster, now metav1.Time) error {
	for _, av := range []string{"kyverno.io/v2", "policies.kyverno.io/v1beta1"} {
		items, err := c.list(ctx, av, "policyexceptions", "aep-system")
		if err != nil {
			return err
		}
		for _, u := range items {
			expiry, err := parseExpiry(u.GetAnnotations()["aep.aenix.io/expires"])
			if err != nil || expiry.After(now.Time) {
				continue
			}
			uid, rv := u.GetUID(), u.GetResourceVersion()
			if err := c.resource(av, "policyexceptions", "aep-system").Delete(ctx, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
				return err
			}
		}
	}
	return nil
}
