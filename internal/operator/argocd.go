package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"net"
	"net/url"
	"sort"
	"strings"
	"text/template"
)

const ArgoCDChartVersion = "9.7.1"
const ArgoCDChartOCI = "oci://argoproj.github.io/argo-helm/argo-cd"

const ArgoCDAppVersion = "v3.4.4"

func argoNamespace(project string) string {
	if len(project) <= 50 {
		return "gitops-" + project
	}
	return fmt.Sprintf("gitops-%.40s-%x", project, sha256.Sum256([]byte(project)))[:63]
}
func argoHost(pattern, org string) (string, error) {
	t, e := template.New("host").Option("missingkey=error").Parse(pattern)
	if e != nil {
		return "", e
	}
	var b bytes.Buffer
	e = t.Execute(&b, M{"Organization": org, "Tier": "nonprod"})
	if e != nil {
		return "", e
	}
	h := b.String()
	if len(validation.IsDNS1123Subdomain(h)) > 0 {
		return "", fmt.Errorf("argocd-host-template must render a DNS hostname")
	}
	return h, nil
}
func argoProject(o *api.Organization, project string) *unstructured.Unstructured {
	return obj("argoproj.io/v1alpha1", "AppProject", "argocd", "aep-"+project, M{
		"sourceRepos":              []any{repositoryPrefix(o.Spec.VCS) + "*"},
		"destinations":             []any{M{"server": "https://kubernetes.default.svc", "namespace": argoNamespace(project)}},
		"clusterResourceWhitelist": []any{}, "clusterResourceBlacklist": []any{M{"group": "*", "kind": "*"}},
		"namespaceResourceBlacklist": []any{M{"group": "rbac.authorization.k8s.io", "kind": "*"}, M{"group": "argoproj.io", "kind": "*"}, M{"group": "openchoreo.dev", "kind": "*"}},
	})
}

// argoProjectAdmission denies AppProject writes by Argo CD's own service
// accounts. argocd-server creates the default project at startup and exits if
// it cannot, so that single creation is allowed; the operator then takes the
// project over and keeps it empty. Updates and deletions stay denied.
const argoProjectAdmission = "!request.userInfo.username.startsWith('system:serviceaccount:argocd:') || (request.operation == 'CREATE' && object.metadata.name == 'default')"

// argoProjectLabel marks AppProjects the operator owns and prunes.
const argoProjectLabel = "aep.aenix.io/argocd-project"

// argoProjects are applied after the Argo CD release is Ready: the chart
// installs the AppProject CRD, so the same release cannot carry AppProjects
// on a first install. The empty default project prevents implicit access
// through applications without an assigned project.
func argoProjects(o *api.Organization, projects []string) []*unstructured.Unstructured {
	out := []*unstructured.Unstructured{}
	for _, p := range projects {
		out = append(out, argoProject(o, p))
	}
	out = append(out, obj("argoproj.io/v1alpha1", "AppProject", "argocd", "default", M{"sourceRepos": []any{}, "destinations": []any{}, "clusterResourceWhitelist": []any{}}))
	for _, u := range out {
		u.SetLabels(map[string]string{argoProjectLabel: "operator"})
	}
	return out
}

func (s *session) applyArgoProjects(ctx context.Context, c *Cluster, projects []string) error {
	desired := map[string]bool{}
	for _, u := range argoProjects(s.o, projects) {
		desired[u.GetName()] = true
		// Argo CD creates the default project itself at startup; take it over
		// so it stays empty instead of refusing it as foreign.
		if u.GetName() == "default" {
			ri := c.resource(u.GetAPIVersion(), "appprojects", "argocd")
			if live, e := ri.Get(ctx, "default", metav1.GetOptions{}); e == nil && live.GetLabels()[OwnerLabel] == "" {
				labels := live.GetLabels()
				if labels == nil {
					labels = map[string]string{}
				}
				labels[OwnerLabel] = string(s.o.UID)
				labels[argoProjectLabel] = "operator"
				live.SetLabels(labels)
				live.Object["spec"] = u.Object["spec"]
				if _, e = ri.Update(ctx, live, metav1.UpdateOptions{}); e != nil {
					return e
				}
			}
		}
		if e := c.apply(ctx, s.o, "appprojects", u); e != nil {
			return e
		}
	}
	live, e := c.list(ctx, "argoproj.io/v1alpha1", "appprojects", "argocd")
	if e != nil {
		return e
	}
	for i := range live {
		if live[i].GetLabels()[argoProjectLabel] == "operator" && !desired[live[i].GetName()] {
			if e = s.deleteObject(ctx, c, "appprojects", &live[i]); e != nil {
				return e
			}
		}
	}
	return nil
}

func argoValues(o *api.Organization, host, issuer string, projects []string) M {
	objects := []any{}
	namespaces := []string{}
	policy := "g, aep-" + o.Name + "-platform, role:admin\n"
	policy += "g, aep-" + o.Name + "-developer, role:readonly\n"
	for _, action := range []string{"create", "update", "delete"} {
		policy += fmt.Sprintf("p, role:admin, clusters, %s, *, deny\n", action)
	}
	for _, p := range projects {
		ns := argoNamespace(p)
		namespaces = append(namespaces, ns)
		n := obj("v1", "Namespace", "", ns, nil)
		n.SetAnnotations(map[string]string{"helm.sh/resource-policy": "keep"})
		n.SetLabels(map[string]string{"aep.aenix.io/organization": string(o.UID), "aep.aenix.io/argocd": "allowed", "pod-security.kubernetes.io/enforce": "restricted"})
		objects = append(objects, n.Object)
		role := obj("rbac.authorization.k8s.io/v1", "Role", ns, "aep-argocd", nil)
		role.Object["rules"] = []any{
			rule("", []string{"pods", "services", "configmaps", "secrets", "serviceaccounts", "persistentvolumeclaims", "events"}, []string{"*"}),
			rule("apps", []string{"deployments", "replicasets", "statefulsets", "daemonsets"}, []string{"*"}),
			rule("batch", []string{"jobs", "cronjobs"}, []string{"*"}),
			rule("autoscaling", []string{"horizontalpodautoscalers"}, []string{"*"}),
			rule("policy", []string{"poddisruptionbudgets"}, []string{"*"}),
			rule("networking.k8s.io", []string{"networkpolicies"}, []string{"*"}),
			rule("gateway.networking.k8s.io", []string{"httproutes"}, []string{"*"}),
		}
		objects = append(objects, role.Object)
		binding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", ns, "aep-argocd", nil)
		binding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "aep-argocd"}
		binding.Object["subjects"] = []any{M{"kind": "ServiceAccount", "name": "argocd-application-controller", "namespace": "argocd"}}
		objects = append(objects, binding.Object)
		read := obj("rbac.authorization.k8s.io/v1", "Role", ns, "aep-argocd-read", nil)
		read.Object["rules"] = []any{rule("", []string{"pods", "pods/log", "events"}, []string{"get", "list", "watch"})}
		objects = append(objects, read.Object)
		rb := binding.DeepCopy()
		rb.SetName("aep-argocd-read")
		rb.Object["roleRef"].(M)["name"] = "aep-argocd-read"
		rb.Object["subjects"] = []any{M{"kind": "ServiceAccount", "name": "argocd-server", "namespace": "argocd"}}
		objects = append(objects, rb.Object)
		r := "role:aep-" + p
		policy += fmt.Sprintf("p, %s, projects, get, aep-%s, allow\n", r, p)
		for _, action := range []string{"get", "create", "update", "delete", "sync"} {
			policy += fmt.Sprintf("p, %s, applications, %s, %s/*, allow\n", r, action, "aep-"+p)
		}
		policy += fmt.Sprintf("p, %s, logs, get, %s/*, allow\ng, aep-%s-developer, %s\n", r, "aep-"+p, o.Name+"-"+p, r)
	}
	// Keycloak may emit either leaf names or full group paths.
	for _, line := range strings.Split(policy, "\n") {
		if strings.HasPrefix(line, "g, aep-") {
			policy += strings.Replace(line, "g, aep-", "g, /aep-", 1) + "\n"
		}
	}
	// Only the existing tier operator can write the runtime credentials.
	role := obj("rbac.authorization.k8s.io/v1", "Role", "argocd", "aep-operator", nil)
	role.Object["rules"] = []any{rule("", []string{"secrets"}, []string{"get", "create", "patch", "delete"})}
	objects = append(objects, role.Object)
	rb := obj("rbac.authorization.k8s.io/v1", "RoleBinding", "argocd", "aep-operator", nil)
	rb.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "aep-operator"}
	rb.Object["subjects"] = []any{M{"kind": "ServiceAccount", "name": "aep-operator", "namespace": "aep-system"}}
	objects = append(objects, rb.Object)
	// Admission is independent of Argo RBAC, including the instance administrator.
	vap := obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", "", "aep-argocd-projects", M{"failurePolicy": "Fail", "matchConstraints": M{"resourceRules": []any{M{"apiGroups": []any{"argoproj.io"}, "apiVersions": []any{"*"}, "resources": []any{"appprojects"}, "operations": []any{"CREATE", "UPDATE", "DELETE"}}}}, "validations": []any{M{"expression": argoProjectAdmission, "message": "AppProjects are managed by the organization operator"}}})
	objects = append(objects, vap.Object)
	objects = append(objects, obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", "", "aep-argocd-projects", M{"policyName": "aep-argocd-projects", "validationActions": []any{"Deny"}}).Object)
	objects = append(objects, argoApplicationPolicy().Object,
		obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", "", "aep-argocd-repositories", M{"policyName": "aep-argocd-repositories", "validationActions": []any{"Deny"}}).Object)
	resources := M{"requests": M{"cpu": "100m", "memory": "128Mi"}, "limits": M{"cpu": "1", "memory": "1Gi"}}
	security := M{"runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": M{"drop": []any{"ALL"}}, "seccompProfile": M{"type": "RuntimeDefault"}}
	// A namespace-restricted cluster credential prevents cluster-wide cache watches.
	cluster := obj("v1", "Secret", "argocd", "aep-in-cluster", nil)
	cluster.SetLabels(map[string]string{"argocd.argoproj.io/secret-type": "cluster"})
	cluster.Object["stringData"] = M{"name": "in-cluster", "server": "https://kubernetes.default.svc", "namespaces": strings.Join(append([]string{"argocd"}, namespaces...), ","), "clusterResources": "false", "config": "{}"}
	objects = append(objects, cluster.Object)
	return M{"fullnameOverride": "argocd", "createClusterRoles": false, "global": M{"domain": host, "image": M{"tag": ArgoCDAppVersion}, "securityContext": M{"runAsNonRoot": true, "seccompProfile": M{"type": "RuntimeDefault"}}},
		"dex": M{"enabled": false, "resources": resources}, "notifications": M{"enabled": false, "resources": resources}, "applicationSet": M{"replicas": int64(0), "resources": resources, "containerSecurityContext": security},
		"controller": M{"resources": resources, "containerSecurityContext": security}, "repoServer": M{"resources": resources, "containerSecurityContext": security, "automountServiceAccountToken": false},
		"redis": M{"resources": resources, "containerSecurityContext": security}, "redisSecretInit": M{"resources": resources, "containerSecurityContext": security, "securityContext": M{"runAsNonRoot": true, "seccompProfile": M{"type": "RuntimeDefault"}}},
		"server":  M{"resources": resources, "containerSecurityContext": security, "ingress": M{"enabled": true, "ingressClassName": "nginx", "hostname": host, "tls": true, "annotations": M{"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS"}}},
		"configs": M{"params": M{"reposerver.allow.oob.symlinks": "false"}, "cm": M{"helm.valuesFileSchemes": " ", "url": "https://" + host, "admin.enabled": "false", "exec.enabled": "false", "oidc.config": fmt.Sprintf("name: Keycloak\nissuer: %s\nclientID: aep-%s-argocd\nclientSecret: $aep-oidc:clientSecret\nrequestedScopes: [openid, profile, email, groups]\n", issuer, o.Name)}, "rbac": M{"policy.default": "role:none", "policy.csv": policy, "scopes": "[groups]"}}, "extraObjects": objects}
}
func (s *session) argocd(ctx context.Context) error {
	if !argoEnabled(s.o) {
		s.o.Status.ArgoCDURL = ""
		return s.cleanupArgo(ctx)
	}
	if !s.vcsVerified {
		return pending("Argo CD requires verified corporate VCS credentials and organization")
	}
	opt := s.r.Options
	host, e := argoHost(opt.ArgoCDHostTemplate, s.o.Name)
	if e != nil {
		return e
	}
	if opt.KeycloakURL == "" || opt.KeycloakRealm == "" {
		return pending("Argo CD requires Keycloak URL and realm")
	}
	issuer := strings.TrimRight(opt.KeycloakURL, "/") + "/realms/" + url.PathEscape(opt.KeycloakRealm)
	projects, e := s.cp.list(ctx, ocAPI, "projects", s.o.Name)
	if e != nil {
		return e
	}
	names := []string{}
	for _, p := range projects {
		names = append(names, p.GetName())
	}
	sort.Strings(names)
	values := MirrorValues("argo-cd", opt.ImageRegistryMirror, argoValues(s.o, host, issuer, names))
	resolve := s.r.ResolveHost
	if resolve == nil {
		resolve = net.DefaultResolver.LookupIPAddr
	}
	network, e := argoEgress(ctx, s.o.Spec.VCS.URL, opt.KeycloakURL, resolve)
	if e != nil {
		return e
	}
	values["extraObjects"] = append(values["extraObjects"].([]any), network.Object)
	oidc, err := s.r.Management.Kube.CoreV1().Secrets(opt.Namespace).Get(ctx, "argocd-"+s.o.Name+"-oidc", metav1.GetOptions{})
	if err == nil && len(oidc.Data["rootCA"]) > 0 {
		cm := values["configs"].(M)["cm"].(M)
		cm["oidc.config"] = cm["oidc.config"].(string) + "rootCA: |\n  " + strings.ReplaceAll(strings.TrimSpace(string(oidc.Data["rootCA"])), "\n", "\n  ") + "\n"
	}

	ca, e := s.vcsCA(ctx)
	if e != nil {
		return e
	}
	if len(ca) > 0 {
		u, _ := url.Parse(s.o.Spec.VCS.URL)
		values["configs"].(M)["tls"] = M{"certificates": M{u.Hostname(): string(ca)}}
	}
	ns := TenantNamespace(s.o.Name, "nonprod")
	if err := s.mirrorSecrets(ctx, ns); err != nil {
		return err
	}
	source, hr, e := argoDelivery(s.o, ns, opt, values)
	if e != nil {
		return e
	}
	resource := "helmrepositories"
	if source.GetKind() == "OCIRepository" {
		resource = "ocirepositories"
	}
	if e = s.r.Management.apply(ctx, s.o, resource, source); e != nil {
		return e
	}
	spec := hr.Object["spec"].(M)
	spec["targetNamespace"] = "argocd"
	spec["storageNamespace"] = "argocd"
	spec["releaseName"] = "argocd"
	spec["dependsOn"] = []any{M{"name": "aep-app-cluster-baseline"}}
	if e = s.r.Management.apply(ctx, s.o, "helmreleases", hr); e != nil {
		return e
	}
	obsolete := "ocirepositories"
	if resource == "ocirepositories" {
		obsolete = "helmrepositories"
	}
	if e = s.deleteNamed(ctx, s.r.Management, "source.toolkit.fluxcd.io/v1", obsolete, ns, "aep-argocd"); e != nil {
		return e
	}
	hr, e = s.r.Management.get(ctx, helmAPI, "helmreleases", ns, "aep-argocd")
	if e != nil {
		return e
	}
	c, e := s.bootstrap(ctx, "nonprod")
	if e != nil {
		return e
	}
	if e = s.argoCredentials(ctx, c); e != nil {
		if apierrors.IsForbidden(e) || apierrors.IsNotFound(e) {
			return pending("waiting for Argo CD namespace and operator credential Role")
		}
		return e
	}
	if !ready(hr) {
		return pending("waiting for nonprod Argo CD chart Ready")
	}
	if e = s.applyArgoProjects(ctx, c, names); e != nil {
		return e
	}
	s.o.Status.ArgoCDURL = "https://" + host
	return nil
}
func (s *session) argoCredentials(ctx context.Context, c *Cluster) error {
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, "argocd-"+s.o.Name+"-oidc", metav1.GetOptions{})
	if e != nil {
		return pending("provision management Secret argocd-%s-oidc with clientSecret for Keycloak client aep-%s-argocd", s.o.Name, s.o.Name)
	}
	if len(sec.Data["clientSecret"]) == 0 {
		return fmt.Errorf("Argo CD OIDC clientSecret is empty")
	}
	oidc := secretObject("argocd", "aep-oidc", map[string][]byte{"clientSecret": sec.Data["clientSecret"]})
	oidc.SetLabels(map[string]string{"app.kubernetes.io/part-of": "argocd"})
	if e = c.apply(ctx, s.o, "secrets", oidc); e != nil {
		return e
	}
	credentials, e := s.vcsReadCredentials(ctx)
	if e != nil {
		return e
	}
	data := map[string][]byte{"type": []byte("git"), "url": []byte(repositoryPrefix(s.o.Spec.VCS)), "username": []byte(vcsUsername(s.o.Spec.VCS.Provider, credentials)), "password": credentials["token"]}

	repo := secretObject("argocd", "aep-corporate-vcs", data)
	repo.SetLabels(map[string]string{"argocd.argoproj.io/secret-type": "repo-creds"})
	return c.apply(ctx, s.o, "secrets", repo)
}

func argoChartURL(opt Options) string {
	if opt.ArgoCDChartURL != "" {
		return opt.ArgoCDChartURL
	}
	return "https://argoproj.github.io/argo-helm"
}

// HTTPS is the only clone transport: disallow userinfo and all encoded paths.
const argoRepoExpression = "!url.contains('..') && !url.contains('%') && !url.contains('@') && url.startsWith('https://')"

func argoApplicationPolicy() *unstructured.Unstructured {
	check := "[object.spec.source].all(s, [s.repoURL].all(url, " + argoRepoExpression + "))"
	multi := "object.spec.sources.all(s, [s.repoURL].all(url, " + argoRepoExpression + "))"
	return obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", "", "aep-argocd-repositories", M{
		"failurePolicy": "Fail", "matchConstraints": M{"resourceRules": []any{M{"apiGroups": []any{"argoproj.io"}, "apiVersions": []any{"*"}, "resources": []any{"applications"}, "operations": []any{"CREATE", "UPDATE"}}}},
		"matchConditions": []any{M{"name": "argocd-namespace", "expression": "request.namespace == 'argocd'"}},
		"validations":     []any{M{"expression": "(!has(object.spec.source) || " + check + ") && (!has(object.spec.sources) || " + multi + ")", "message": "Repository URLs must use HTTPS without traversal, encoding or userinfo"}},
	})
}

// Keep the HTTP override for connected installations; disconnected delivery uses
// the exact OCI package exported by the release manifest.
func argoDelivery(o *api.Organization, ns string, opt Options, values M) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	source := obj("source.toolkit.fluxcd.io/v1", "HelmRepository", ns, "aep-argocd", M{"interval": "1h", "url": argoChartURL(opt)})
	hr := policyRelease(o, "nonprod", "argocd", values)
	spec := hr.Object["spec"].(M)
	if opt.ChartRegistryMirror != "" || strings.HasPrefix(argoChartURL(opt), "oci://") {
		url := ArgoCDChartOCI
		if strings.HasPrefix(argoChartURL(opt), "oci://") {
			url = argoChartURL(opt)
		}
		source = obj("source.toolkit.fluxcd.io/v1", "OCIRepository", ns, "aep-argocd", M{"interval": "1h", "url": url, "ref": M{"tag": ArgoCDChartVersion}})
		if err := opt.chartSource(source); err != nil {
			return nil, nil, err
		}
		spec["chartRef"] = M{"kind": "OCIRepository", "name": "aep-argocd"}
	} else {
		delete(spec, "chartRef")
		spec["chart"] = M{"spec": M{"chart": "argo-cd", "version": ArgoCDChartVersion, "sourceRef": M{"kind": "HelmRepository", "name": "aep-argocd"}}}
	}
	return source, hr, nil
}
