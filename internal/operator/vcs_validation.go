package operator

import (
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
	"net/url"
	"regexp"
	"strings"
)

func validateIntegrations(o *api.Organization) error {
	for tier, t := range o.Spec.Tiers {
		if t.ArgoCD == nil {
			continue
		}
		if tier != "nonprod" {
			return fmt.Errorf("Argo CD is supported only in nonprod; prod deployments must use OpenChoreo promotion")
		}
		if t.ArgoCD.Version != "" && t.ArgoCD.Version != ArgoCDChartVersion {
			return fmt.Errorf("argocd.version must be the tested chart version %s", ArgoCDChartVersion)
		}
	}
	v := o.Spec.VCS
	if v == nil {
		return nil
	}
	if v.Provider != "gitlab" && v.Provider != "github" && v.Provider != "gitea" {
		return fmt.Errorf("vcs.provider must be gitlab, github or gitea")
	}
	u, e := url.Parse(v.URL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("vcs.url must be an HTTPS origin without path, credentials, query or fragment")
	}
	if len(validation.IsDNS1123Subdomain(v.CredentialsSecretRef.Name)) > 0 {
		return fmt.Errorf("vcs.credentialsSecretRef.name must name a Secret in the operator management namespace")
	}
	if len(validation.IsDNS1123Subdomain(v.ReadCredentialsSecretRef.Name)) > 0 {
		return fmt.Errorf("vcs.readCredentialsSecretRef.name is required for read-only cloning")
	}
	if v.ReadCredentialsSecretRef.Name == v.CredentialsSecretRef.Name {
		return fmt.Errorf("read and admin credentials must use separate Secrets")
	}
	if v.CABundleSecretRef != nil && len(validation.IsDNS1123Subdomain(v.CABundleSecretRef.Name)) > 0 {
		return fmt.Errorf("invalid vcs.caBundleSecretRef.name")
	}
	path := v.GroupPath
	if v.Provider != "gitlab" {
		path = v.Org
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$`).MatchString(path) {
		return fmt.Errorf("vcs requires a safe groupPath (GitLab) or org (GitHub/Gitea)")
	}
	if v.Provider != "gitlab" && (strings.Contains(path, "/") || v.GroupPath != "") {
		return fmt.Errorf("GitHub/Gitea require a single org, not groupPath")
	}
	if v.Provider == "gitlab" && v.Org != "" {
		return fmt.Errorf("GitLab requires groupPath, not org")
	}
	if v.Provider != "gitlab" && v.EnsureGroup {
		return fmt.Errorf("ensureGroup is supported only for GitLab; provision the corporate organization first")
	}
	return nil
}
func repositoryPrefix(v *api.VCSSpec) string {
	p := v.GroupPath
	if v.Provider != "gitlab" {
		p = v.Org
	}
	return strings.TrimRight(v.URL, "/") + "/" + p + "/"
}

func argoEnabled(o *api.Organization) bool {
	a := o.Spec.Tiers["nonprod"].ArgoCD
	return o.Spec.VCS != nil && a != nil && a.Enabled
}
