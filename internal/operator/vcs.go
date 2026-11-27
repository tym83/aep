package operator

import (
	"context"
	"fmt"
	"github.com/tym83/aep/internal/vcs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"net/url"
	"strings"
	"time"
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
	return s.vcsNamedSecret(ctx, s.o.Spec.VCS.CredentialsSecretRef.Name)
}
func (s *session) vcsNamedSecret(ctx context.Context, name string) (map[string][]byte, error) {
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return nil, fmt.Errorf("VCS credentials Secret is unavailable")
	}
	if sec.Labels[OwnerLabel] != s.o.Name {
		return nil, fmt.Errorf("VCS Secret must carry organization name label")
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
	if sec.Labels[OwnerLabel] != s.o.Name {
		return nil, fmt.Errorf("VCS CA Secret must carry organization name label")
	}
	return sec.Data["ca.crt"], nil
}
func (s *session) vcs(ctx context.Context) error {
	v := s.o.Spec.VCS
	if v == nil || (s.o.Spec.Tiers["nonprod"].ArgoCD != nil && !argoEnabled(s.o)) {
		return s.cleanupBuildCredentials(ctx)
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
	read, e := s.vcsReadCredentials(ctx)
	if e != nil {
		return e
	}
	buildErr := s.vcsBuildCredentials(ctx, read)
	if buildErr != nil {
		s.vcsEvent("", buildErr)
	}

	if s.r.Options.VCSWebhookURL != "" && s.r.claimHookReconcile(string(s.o.UID)) {
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
				s.vcsEvent(component.GetName(), e)
				continue
			}
			prefix := repositoryPrefix(v)
			if !strings.HasPrefix(repo, prefix) {
				s.vcsEvent(component.GetName(), fmt.Errorf("autoBuild repository is outside the corporate VCS allow-list"))
				continue
			}
			path := strings.TrimSuffix(strings.TrimPrefix(repo, strings.TrimRight(v.URL, "/")+"/"), ".git")
			if strings.ContainsAny(path, "?#%") || strings.Contains(path, "..") {
				s.vcsEvent(component.GetName(), fmt.Errorf("invalid autoBuild repository path"))
				continue
			}
			if !repos[path] {
				if e = c.EnsureWebhook(ctx, path, s.r.Options.VCSWebhookURL, secret); e != nil {
					s.vcsEvent(component.GetName(), e)
					continue
				}
				repos[path] = true
			}
		}
	}
	return buildErr
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

// Read credentials never fall back to the administrative token.
func (s *session) vcsReadCredentials(ctx context.Context) (map[string][]byte, error) {
	data, err := s.vcsNamedSecret(ctx, s.o.Spec.VCS.ReadCredentialsSecretRef.Name)
	if err != nil {
		return nil, err
	}
	if len(data["token"]) > 0 {
		return data, nil
	}
	if s.o.Spec.VCS.Provider != "github" {
		return nil, fmt.Errorf("read-only VCS token is empty")
	}
	ca, err := s.vcsCA(ctx)
	if err != nil {
		return nil, err
	}
	token, err := vcs.GitHubAppRead(ctx, s.o.Spec.VCS.URL, string(data["githubAppID"]), string(data["githubAppInstallationID"]), data["githubAppPrivateKey"], ca)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"username": []byte("x-access-token"), "token": []byte(token)}, nil
}

func (s *session) vcsBuildCredentials(ctx context.Context, read map[string][]byte) error {
	v := s.o.Spec.VCS
	// Only a reference is published on the control plane. ESO in the workflow
	// cluster reads a dedicated Secret through the organization's preconfigured
	// workflow store. Tenant clusters cannot reach the management API, so the
	// Secret is written to aep-system of the nonprod cluster, which already
	// holds the same read credential for Argo CD; the management copy remains
	// for workflow planes outside the organization's clusters.
	buildName := buildSecretName(s.o.Name)
	data := map[string][]byte{"username": []byte(vcsUsername(v.Provider, read)), "password": read["token"]}
	if e := s.r.Management.apply(ctx, s.o, "secrets", secretObject(s.r.Options.Namespace, buildName, data)); e != nil {
		return e
	}
	c, e := s.bootstrap(ctx, "nonprod")
	if e != nil {
		return e
	}
	if e = c.apply(ctx, s.o, "secrets", secretObject("aep-system", buildName, data)); e != nil {
		return e
	}
	wp, e := s.cp.get(ctx, ocAPI, "workflowplanes", s.o.Name, "nonprod")
	if e != nil {
		return pending("VCS verified; provision organization WorkflowPlane nonprod with an ESO Kubernetes store scoped to Secret aep-system/%s of the nonprod cluster", buildName)
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
	return nil
}

func (s *session) vcsEvent(component string, err error) {
	if s.r.Recorder != nil {
		s.r.Recorder.Eventf(s.o, "Warning", "VCSComponentSkipped", "Component %s: %s", component, err.Error())
	}
}

func (r *Reconciler) claimHookReconcile(key string) bool {
	r.hookMu.Lock()
	defer r.hookMu.Unlock()
	if r.hookAttempts == nil {
		r.hookAttempts = map[string]time.Time{}
	}
	now := time.Now()
	if last, ok := r.hookAttempts[key]; ok && now.Sub(last) < 15*time.Minute {
		return false
	}
	r.hookAttempts[key] = now
	return true
}

func buildSecretName(org string) string { return "vcs-" + org + "-build" }
