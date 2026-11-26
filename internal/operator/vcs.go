package operator

import (
	"context"
	"fmt"
	"github.com/tym83/aep/internal/vcs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"net/url"
	"strings"
)

func vcsUsername(provider string, data map[string][]byte) string {
	if len(data["username"]) > 0 {
		return string(data["username"])
	}
	if provider == "gitlab" {
		return "oauth2"
	}
	return "x-access-token"
}
func (s *session) vcsSecret(ctx context.Context) (map[string][]byte, error) {
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, s.o.Spec.VCS.CredentialsSecretRef.Name, metav1.GetOptions{})
	if e != nil {
		return nil, fmt.Errorf("VCS credentials Secret is unavailable")
	}
	return sec.Data, nil
}
func (s *session) vcsCA(ctx context.Context) ([]byte, error) {
	if s.o.Spec.VCS.CABundleSecretRef == nil {
		return nil, nil
	}
	name := s.o.Spec.VCS.CABundleSecretRef.Name
	if name == "" {
		return nil, nil
	}
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
	if e != nil || len(sec.Data["ca.crt"]) == 0 {
		return nil, fmt.Errorf("VCS CA Secret must contain ca.crt")
	}
	return sec.Data["ca.crt"], nil
}
func (s *session) vcs(ctx context.Context) error {
	v := s.o.Spec.VCS
	if v == nil {
		return nil
	}
	data, e := s.vcsSecret(ctx)
	if e != nil {
		return e
	}
	ca, e := s.vcsCA(ctx)
	if e != nil {
		return e
	}
	token := string(data["token"])
	app := token == "" && v.Provider == "github"
	if app {
		token, e = vcs.GitHubApp(ctx, v.URL, string(data["githubAppID"]), string(data["githubAppInstallationID"]), data["githubAppPrivateKey"], ca)
		if e != nil {
			return e
		}
	}
	c, e := vcs.New(v.Provider, v.URL, token, ca)
	if e != nil {
		return e
	}
	if !app {
		if e = c.Verify(ctx); e != nil {
			return e
		}
	}
	path := v.GroupPath
	if v.Provider != "gitlab" {
		path = v.Org
	}
	if e = c.EnsureGroup(ctx, path, v.EnsureGroup); e != nil {
		return e
	}
	s.vcsVerified = true
	// Only a reference is published on the control plane. ESO reads a dedicated
	// management Secret through the organization's preconfigured workflow store.
	buildName := "vcs-" + s.o.Name + "-build"
	if e = s.r.Management.apply(ctx, s.o, "secrets", secretObject(s.r.Options.Namespace, buildName, map[string][]byte{"username": []byte(vcsUsername(v.Provider, data)), "password": []byte(token)})); e != nil {
		return e
	}
	wp, e := s.cp.get(ctx, ocAPI, "workflowplanes", s.o.Name, "nonprod")
	if e != nil {
		return pending("VCS verified; provision organization WorkflowPlane nonprod with an ESO Kubernetes store scoped to management Secret %s", buildName)
	}
	store, _, _ := unstructured.NestedString(wp.Object, "spec", "secretStoreRef", "name")
	if store == "" {
		return pending("WorkflowPlane nonprod requires secretStoreRef for VCS credentials")
	}
	ref := obj(ocAPI, "SecretReference", s.o.Name, "aep-corporate-vcs", M{"targetPlane": M{"kind": "WorkflowPlane", "name": "nonprod"}, "template": M{"type": "kubernetes.io/basic-auth"}, "data": []any{M{"secretKey": "username", "remoteRef": M{"key": buildName, "property": "username"}}, M{"secretKey": "password", "remoteRef": M{"key": buildName, "property": "password"}}}})
	ref.SetLabels(map[string]string{"openchoreo.dev/secret-type": "git-credentials", "kubernetes.io/secret-type": "basic-auth", "openchoreo.dev/workflow-plane-kind": "WorkflowPlane", "openchoreo.dev/workflow-plane-name": "nonprod"})
	if e = s.cp.apply(ctx, s.o, "secretreferences", ref); e != nil {
		return e
	}
	if s.r.Options.VCSWebhookURL != "" {
		u, e := url.Parse(s.r.Options.VCSWebhookURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("VCS webhook URL must be HTTPS")
		}
		// OpenChoreo currently uses one webhook secret per provider, not per organization.
		sec, e := s.cp.Kube.CoreV1().Secrets("openchoreo-control-plane").Get(ctx, "git-webhook-secrets", metav1.GetOptions{})
		if e != nil {
			return pending("OpenChoreo git-webhook-secrets is unavailable")
		}
		secret := string(sec.Data[v.Provider+"-secret"])
		if secret == "" {
			return pending("OpenChoreo webhook secret for %s is empty", v.Provider)
		}
		components, e := s.cp.list(ctx, ocAPI, "components", s.o.Name)
		if e != nil {
			return e
		}
		repos := map[string]bool{}
		for _, component := range components {
			enabled, _, _ := unstructured.NestedBool(component.Object, "spec", "autoBuild")
			if !enabled {
				continue
			}
			repo, e := s.repositoryURL(ctx, component)
			if e != nil {
				return e
			}
			prefix := repositoryPrefix(v)
			if !strings.HasPrefix(repo, prefix) {
				return fmt.Errorf("autoBuild repository is outside the corporate VCS allow-list")
			}
			path := strings.TrimSuffix(strings.TrimPrefix(repo, strings.TrimRight(v.URL, "/")+"/"), ".git")
			if strings.ContainsAny(path, "?#%") || strings.Contains(path, "..") {
				return fmt.Errorf("invalid autoBuild repository path")
			}
			if !repos[path] {
				if e = c.EnsureWebhook(ctx, path, s.r.Options.VCSWebhookURL, secret); e != nil {
					return e
				}
				repos[path] = true
			}
		}
	}
	return nil
}

// repositoryURL follows OpenChoreo's workflow schema extension instead of assuming
// every corporate workflow uses the built-in repository parameter layout.
func (s *session) repositoryURL(ctx context.Context, component unstructured.Unstructured) (string, error) {
	name, _, _ := unstructured.NestedString(component.Object, "spec", "workflow", "name")
	kind, _, _ := unstructured.NestedString(component.Object, "spec", "workflow", "kind")
	resource, ns := "clusterworkflows", ""
	if kind == "Workflow" {
		resource, ns = "workflows", s.o.Name
	}
	workflow, e := s.cp.get(ctx, ocAPI, resource, ns, name)
	if e != nil {
		return "", e
	}
	schema, _, _ := unstructured.NestedMap(workflow.Object, "spec", "parameters", "openAPIV3Schema")
	paths := repositoryPaths(schema, nil)
	if len(paths) != 1 {
		return "", fmt.Errorf("workflow %s must define one repository URL schema extension", name)
	}
	path := append([]string{"spec", "workflow", "parameters"}, paths[0]...)
	repo, _, _ := unstructured.NestedString(component.Object, path...)
	if repo == "" {
		return "", pending("component %s has no repository URL value", component.GetName())
	}
	return repo, nil
}
func repositoryPaths(schema M, path []string) [][]string {
	if marked, _ := schema["x-openchoreo-component-parameter-repository-url"].(bool); marked {
		return [][]string{path}
	}
	var out [][]string
	props, _ := schema["properties"].(map[string]any)
	for key, v := range props {
		child, _ := v.(map[string]any)
		next := append(append([]string{}, path...), key)
		out = append(out, repositoryPaths(child, next)...)
	}
	return out
}
