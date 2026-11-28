package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tym83/aep/config"
	"io"
	authv1 "k8s.io/api/authentication/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"time"
)

func rule(group string, resources, verbs []string) M {
	return M{"apiGroups": stringsAny([]string{group}), "resources": stringsAny(resources), "verbs": stringsAny(verbs)}
}
func stringsAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
func operatorRules() []any {
	return []any{
		rule("", []string{"nodes"}, []string{"list"}),
		rule("aep.aenix.io", []string{"serviceclaims"}, []string{"list"}),
	}
}
func roleScopes(org string) map[string][]any {
	return map[string][]any{
		"openchoreo-data-plane": {rule("", []string{"secrets", "configmaps"}, []string{"get", "create", "patch"}),
			rule("gateway.kgateway.dev", []string{"gatewayparameters"}, []string{"get", "create", "patch"})},
		"kube-system": {rule("", []string{"secrets"}, []string{"list"})},
		"aep-system": {M{"apiGroups": []any{""}, "resources": []any{"serviceaccounts/token"}, "resourceNames": []any{"aep-bridge", "aep-operator"}, "verbs": []any{"create"}},
			// Build credentials for the workflow plane's local secret store and
			// the registry pull credential that Kyverno clones into environments.
			rule("", []string{"secrets"}, []string{"create"}),
			M{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{buildSecretName(org), RegistryPullSecret}, "verbs": []any{"get", "patch", "delete"}}},
	}
}

var bundlePaths = []string{"crd/aep.aenix.io_serviceclaims.yaml", "app-cluster/rbac.yaml", "app-cluster/admission.yaml"}

// contractRevision identifies the bootstrap contract installed in a tier
// cluster, so a changed contract is reinstalled on clusters bootstrapped by
// an older operator.
func contractRevision(org string) (string, error) {
	h := sha256.New()
	rules, e := json.Marshal([]any{operatorRules(), roleScopes(org), mirrorPullNamespaces, mirrorPullRules()})
	if e != nil {
		return "", e
	}
	h.Write(rules)
	for _, path := range bundlePaths {
		data, e := config.Bundle.ReadFile(path)
		if e != nil {
			return "", e
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
func mirrorPullRules() []any {
	return []any{rule("", []string{"secrets"}, []string{"create"}),
		M{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{MirrorPullSecret}, "verbs": []any{"get", "patch", "delete"}}}
}
func (s *session) bootstrapRoles(ctx context.Context, c *Cluster) error {
	for ns, rules := range roleScopes(s.o.Name) {
		role := obj("rbac.authorization.k8s.io/v1", "Role", ns, "aep-operator", nil)
		role.Object["rules"] = rules
		if e := c.apply(ctx, s.o, "roles", role); e != nil {
			return e
		}
		binding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", ns, "aep-operator", nil)
		binding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "aep-operator"}
		binding.Object["subjects"] = []any{M{"kind": "ServiceAccount", "name": "aep-operator", "namespace": "aep-system"}}
		if e := c.apply(ctx, s.o, "rolebindings", binding); e != nil {
			return e
		}
	}
	return nil
}

// mirrorPullNamespaces receive the registry mirror pull credential before the
// charts delivered there start pulling images (openchoreo-data-plane is
// already writable through the data plane Role).
var mirrorPullNamespaces = []string{"kyverno", "argocd"}

// installContract installs the bootstrap contract with the tier's super-admin.
func (s *session) installContract(ctx context.Context, admin *Cluster) error {
	for _, n := range []string{"aep-system", "openchoreo-data-plane"} {
		if e := admin.apply(ctx, s.o, "namespaces", obj("v1", "Namespace", "", n, nil)); e != nil {
			return e
		}
	}
	// Kyverno and Argo CD charts create their namespaces themselves.
	for _, n := range mirrorPullNamespaces {
		if e := admin.ensureNamespace(ctx, n); e != nil {
			return e
		}
	}
	// A separate Role: the Argo CD chart owns Role argocd/aep-operator.
	for _, n := range mirrorPullNamespaces {
		role := obj("rbac.authorization.k8s.io/v1", "Role", n, "aep-mirror-pull-writer", nil)
		role.Object["rules"] = mirrorPullRules()
		if e := admin.apply(ctx, s.o, "roles", role); e != nil {
			return e
		}
		binding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", n, "aep-mirror-pull-writer", nil)
		binding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "aep-mirror-pull-writer"}
		binding.Object["subjects"] = []any{M{"kind": "ServiceAccount", "name": "aep-operator", "namespace": "aep-system"}}
		if e := admin.apply(ctx, s.o, "rolebindings", binding); e != nil {
			return e
		}
	}
	if e := s.bundle(ctx, admin); e != nil {
		return e
	}
	if e := admin.apply(ctx, s.o, "serviceaccounts", obj("v1", "ServiceAccount", "aep-system", "aep-operator", nil)); e != nil {
		return e
	}
	role := obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "aep-operator", nil)
	role.Object["rules"] = operatorRules()
	if e := admin.apply(ctx, s.o, "clusterroles", role); e != nil {
		return e
	}
	binding := obj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "aep-operator", nil)
	binding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "aep-operator"}
	binding.Object["subjects"] = []any{M{"kind": "ServiceAccount", "name": "aep-operator", "namespace": "aep-system"}}
	if e := admin.apply(ctx, s.o, "clusterrolebindings", binding); e != nil {
		return e
	}
	return s.bootstrapRoles(ctx, admin)
}
func (s *session) bootstrap(ctx context.Context, tier string) (*Cluster, error) {
	if c := s.tiers[tier]; c != nil {
		return c, nil
	}
	ns := TenantNamespace(s.o.Name, tier)
	credentialName := "operator-access-" + s.o.Name + "-" + tier
	revision, err := contractRevision(s.o.Name)
	if err != nil {
		return nil, err
	}
	stored, err := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, credentialName, metav1.GetOptions{})
	if err == nil {
		if stored.Labels[OwnerLabel] != string(s.o.UID) {
			return nil, fmt.Errorf("operator credential ownership mismatch")
		}
		cfg, e := clientcmd.RESTConfigFromKubeConfig(stored.Data["kubeconfig"])
		if e != nil {
			return nil, e
		}
		c, e := NewCluster(cfg)
		if e != nil {
			return nil, e
		}
		expires, e := time.Parse(time.RFC3339, string(stored.Data["expires"]))
		if e != nil {
			return nil, e
		}
		if string(stored.Data["contract"]) != revision {
			admin, e := FromSecret(ctx, s.r.Management, ns+"/kubernetes-apps-admin-kubeconfig")
			if e != nil {
				return nil, e
			}
			if e = s.installContract(ctx, admin); e != nil {
				return nil, e
			}
			if e = s.saveOperatorCredential(ctx, credentialName, cfg, expires); e != nil {
				return nil, e
			}
		}
		if time.Until(expires) < 12*time.Hour {
			token, expiry, e := requestToken(ctx, c, "aep-operator")
			if e != nil {
				return nil, fmt.Errorf("operator token renewal failed; remove owned credential Secret to explicitly rebootstrap: %w", e)
			}
			cfg.BearerToken = token
			if e = s.saveOperatorCredential(ctx, credentialName, cfg, expiry); e != nil {
				return nil, e
			}
			c, e = NewCluster(cfg)
			if e != nil {
				return nil, e
			}
		}
		s.tiers[tier] = c
		return c, nil
	}
	if !errors.IsNotFound(err) {
		return nil, err
	}
	// Super-admin is used only to install the bootstrap contract (also when it changes) and mint the initial operator token.
	admin, e := FromSecret(ctx, s.r.Management, ns+"/kubernetes-apps-admin-kubeconfig")
	if e != nil {
		return nil, e
	}
	if e = s.installContract(ctx, admin); e != nil {
		return nil, e
	}
	token, expiry, e := requestToken(ctx, admin, "aep-operator")
	if e != nil {
		return nil, e
	}
	cfg := rest.AnonymousClientConfig(admin.Config)
	cfg.BearerToken = token
	if e = s.saveOperatorCredential(ctx, credentialName, cfg, expiry); e != nil {
		return nil, e
	}
	c, e := NewCluster(cfg)
	if e == nil {
		s.tiers[tier] = c
	}
	return c, e
}
func requestToken(ctx context.Context, c *Cluster, name string) (string, time.Time, error) {
	seconds := int64(86400)
	t, e := c.Kube.CoreV1().ServiceAccounts("aep-system").CreateToken(ctx, name, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if e != nil {
		return "", time.Time{}, e
	}
	return t.Status.Token, t.Status.ExpirationTimestamp.Time, nil
}
func (s *session) bundle(ctx context.Context, c *Cluster) error {
	for _, path := range bundlePaths {
		data, e := config.Bundle.ReadFile(path)
		if e != nil {
			return e
		}
		decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			u := &unstructured.Unstructured{}
			e = decoder.Decode(&u.Object)
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if len(u.Object) == 0 {
				continue
			}
			if u.GetKind() == "Namespace" {
				continue
			}
			resources := map[string]string{"CustomResourceDefinition": "customresourcedefinitions", "ServiceAccount": "serviceaccounts", "ClusterRole": "clusterroles", "ClusterRoleBinding": "clusterrolebindings", "ValidatingAdmissionPolicy": "validatingadmissionpolicies", "ValidatingAdmissionPolicyBinding": "validatingadmissionpolicybindings"}
			r := resources[u.GetKind()]
			if r == "" {
				return fmt.Errorf("unsupported bundle kind %s", u.GetKind())
			}
			if e = c.apply(ctx, s.o, r, u); e != nil {
				return e
			}
		}
	}
	return nil
}
func secretObject(ns, name string, data map[string][]byte) *unstructured.Unstructured {
	u := obj("v1", "Secret", ns, name, nil)
	d := M{}
	for k, v := range data {
		d[k] = base64.StdEncoding.EncodeToString(v)
	}
	u.Object["data"] = d
	return u
}

func (s *session) saveOperatorCredential(ctx context.Context, name string, c *rest.Config, expiry time.Time) error {
	revision, e := contractRevision(s.o.Name)
	if e != nil {
		return e
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["app"] = &clientcmdapi.Cluster{Server: c.Host, CertificateAuthorityData: c.CAData, TLSServerName: c.ServerName}
	cfg.AuthInfos["operator"] = &clientcmdapi.AuthInfo{Token: c.BearerToken}
	cfg.Contexts["app"] = &clientcmdapi.Context{Cluster: "app", AuthInfo: "operator"}
	cfg.CurrentContext = "app"
	data, e := clientcmd.Write(*cfg)
	if e != nil {
		return e
	}
	return s.r.Management.apply(ctx, s.o, "secrets", secretObject(s.r.Options.Namespace, name, map[string][]byte{"kubeconfig": data, "expires": []byte(expiry.Format(time.RFC3339)), "contract": []byte(revision)}))
}

// contractDrift turns a tier's refusal into a contract reinstall. The stored
// revision only records what the operator last installed; when the tier denies
// an action that contract grants, the RBAC was changed or never landed, so the
// next reconcile must reinstall it with the super-admin credential.
func (s *session) contractDrift(ctx context.Context, tier string, err error) error {
	if !errors.IsForbidden(err) {
		return err
	}
	name := "operator-access-" + s.o.Name + "-" + tier
	secrets := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace)
	stored, e := secrets.Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return fmt.Errorf("%w; %v", err, e)
	}
	if stored.Labels[OwnerLabel] != string(s.o.UID) {
		return fmt.Errorf("%w; operator credential ownership mismatch", err)
	}
	stored.Data["contract"] = nil
	if _, e = secrets.Update(ctx, stored, metav1.UpdateOptions{}); e != nil {
		return fmt.Errorf("%w; %v", err, e)
	}
	delete(s.tiers, tier)
	return fmt.Errorf("%s tier denied the operator, reinstalling its bootstrap contract: %w", tier, err)
}
