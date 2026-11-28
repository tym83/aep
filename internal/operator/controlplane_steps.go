package operator

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// OpenChoreoNamespace is the aep-cp namespace of the control plane chart.
const OpenChoreoNamespace = "openchoreo-control-plane"

// controlPlaneOperatorAccount is the operator identity on aep-cp,
// kube-system/aep-operator.
const controlPlaneOperatorAccount = "aep-operator"

const controlPlaneOperatorNamespace = "kube-system"

func (s *controlPlaneSession) tenant(ctx context.Context) error {
	t := s.cp.Spec.Tenant
	u := obj(appsAPI, "Tenant", t.Parent, t.Name, M{"gateway": false, "ingress": false, "etcd": false, "monitoring": false, "seaweedfs": false, "host": "", "resourceQuotas": TenantResourceQuotas(t.Quotas)})
	if e := s.r.Management.applyAs(ctx, s.owner, "tenants", u); e != nil {
		return e
	}
	got, e := s.r.Management.get(ctx, appsAPI, "tenants", t.Parent, t.Name)
	if e != nil {
		return e
	}
	if !ready(got) {
		if msg := failureMessage(got); msg != "" {
			return pending("Tenant %s/%s is not Ready: %s", t.Parent, t.Name, msg)
		}
		return pending("waiting for Tenant %s/%s Ready", t.Parent, t.Name)
	}
	return nil
}

func (s *controlPlaneSession) rootEgressName() string { return "aep-cp-" + s.cp.Name + "-root-egress" }

func (s *controlPlaneSession) networkPaths(ctx context.Context) error {
	if !s.r.Options.TenantEgressWorkaround {
		return s.deleteNamed(ctx, s.r.Management, "cilium.io/v2", "ciliumclusterwidenetworkpolicies", "", s.rootEgressName())
	}
	selector := M{"matchExpressions": []any{M{"key": "k8s:io.kubernetes.pod.namespace", "operator": "In", "values": []any{s.ns}}}}
	return s.r.Management.applyAs(ctx, s.owner, "ciliumclusterwidenetworkpolicies", obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", s.rootEgressName(), rootEgressSpec(selector)))
}

func (s *controlPlaneSession) cluster(ctx context.Context) error {
	c := s.cp.Spec.Cluster
	n := c.NodeGroup
	addons := M{"certManager": M{"enabled": true}, "gatewayAPI": M{"enabled": true}}
	for _, item := range []struct {
		resource string
		u        *unstructured.Unstructured
	}{
		{"kuberneteses", obj(appsAPI, "Kubernetes", s.ns, c.Name, M{"version": c.Version, "storageClass": c.StorageClass, "addons": addons})},
		{"kubernetesnodeses", obj(appsAPI, "KubernetesNodes", s.ns, c.Name+"-md0", M{"cluster": c.Name, "version": c.Version, "storageClass": c.StorageClass, "diskSize": n.DiskSize, "instanceType": n.InstanceType, "minReplicas": int64(n.MinReplicas), "maxReplicas": int64(n.MaxReplicas), "roles": []any{}})},
	} {
		if e := s.r.Management.applyAs(ctx, s.owner, item.resource, item.u); e != nil {
			return e
		}
	}
	return waitCluster(ctx, s.r.Management, controlPlaneCluster{s}, s.ns, c.Name, int(n.MinReplicas))
}

// controlPlaneCluster adapts a ControlPlane to waitCluster. Its timers live
// in reconciler memory: the ControlPlane API has no status for them.
type controlPlaneCluster struct{ s *controlPlaneSession }

func (c controlPlaneCluster) label() string { return c.s.cp.Spec.Cluster.Name }
func (c controlPlaneCluster) readySince() time.Time {
	m := c.s.r.memory(c.s.cp.UID)
	c.s.r.mu.Lock()
	defer c.s.r.mu.Unlock()
	if m.readySince.IsZero() {
		m.readySince = time.Now()
	}
	return m.readySince
}
func (c controlPlaneCluster) notReady() {
	m := c.s.r.memory(c.s.cp.UID)
	c.s.r.mu.Lock()
	defer c.s.r.mu.Unlock()
	m.readySince = time.Time{}
}
func (c controlPlaneCluster) connect(ctx context.Context) (*Cluster, error) {
	return c.s.identity(ctx)
}
func (c controlPlaneCluster) remediate(ctx context.Context, ns, release, reason string) error {
	m := c.s.r.memory(c.s.cp.UID)
	key := ns + "/" + release
	c.s.r.mu.Lock()
	last := m.remediations[key]
	c.s.r.mu.Unlock()
	if time.Since(last) < 10*time.Minute {
		return nil
	}
	now := metav1.Now()
	if e := forceReconcile(ctx, c.s.r.Management, ns, release, now); e != nil {
		return e
	}
	c.s.r.mu.Lock()
	m.remediations[key] = now.Time
	c.s.r.mu.Unlock()
	if c.s.r.Recorder != nil {
		c.s.r.Recorder.Eventf(c.s.cp, "Warning", reason, "Force-reconciling HelmRelease %s/%s", ns, release)
	}
	return nil
}

// controlPlaneOperatorRules are the cluster-wide rights of the operator
// identity on aep-cp: OpenChoreo resources, organization namespaces and
// their admission rules and RBAC.
func controlPlaneOperatorRules() []any {
	crud := []string{"get", "list", "watch", "create", "patch", "delete"}
	return []any{
		rule("openchoreo.dev", []string{"*"}, []string{"*"}),
		rule("", []string{"namespaces"}, crud),
		rule("admissionregistration.k8s.io", []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"}, crud),
		rule("rbac.authorization.k8s.io", []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}, crud),
	}
}

// controlPlaneNamespaceRules are the rights of the operator identity in the
// control plane chart namespace: delivered Secrets and ConfigMaps, the portal
// database Service, gateway restarts, kgateway pull credentials and the
// authorization bootstrap hook's pull secret.
func controlPlaneNamespaceRules() []any {
	crud := []string{"get", "list", "create", "patch", "delete"}
	return []any{
		rule("", []string{"secrets", "configmaps", "services"}, crud),
		rule("discovery.k8s.io", []string{"endpointslices"}, crud),
		rule("apps", []string{"deployments"}, []string{"get", "list", "patch"}),
		rule("", []string{"serviceaccounts"}, []string{"get", "patch"}),
		rule("", []string{"pods"}, []string{"get", "list", "delete"}),
		rule("gateway.kgateway.dev", []string{"gatewayparameters"}, []string{"get", "create", "patch", "delete"}),
	}
}

// controlPlaneContractRevision identifies the installed identity contract,
// so a changed contract is reinstalled with the super-admin credential.
func controlPlaneContractRevision() (string, error) {
	b, e := json.Marshal([]any{controlPlaneOperatorRules(), controlPlaneNamespaceRules()})
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16], nil
}

func (s *controlPlaneSession) credentialName() string { return "operator-access-cp-" + s.cp.Name }

// admin connects with the cluster's super-admin kubeconfig, which is used
// only to install the identity contract and mint the first token.
func (s *controlPlaneSession) admin(ctx context.Context) (*Cluster, error) {
	name := "kubernetes-" + s.cp.Spec.Cluster.Name + "-admin-kubeconfig"
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return nil, pending("waiting for Secret %s/%s", s.ns, name)
	}
	if e != nil {
		return nil, e
	}
	cfg, e := clientcmd.RESTConfigFromKubeConfig(sec.Data["super-admin.svc"])
	if e != nil {
		return nil, e
	}
	return s.r.connect(cfg)
}

func (s *controlPlaneSession) installContract(ctx context.Context, admin *Cluster) error {
	if e := admin.ensureNamespace(ctx, OpenChoreoNamespace); e != nil {
		return e
	}
	subject := []any{M{"kind": "ServiceAccount", "name": controlPlaneOperatorAccount, "namespace": controlPlaneOperatorNamespace}}
	role := obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", controlPlaneOperatorAccount, nil)
	role.Object["rules"] = controlPlaneOperatorRules()
	binding := obj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", controlPlaneOperatorAccount, nil)
	binding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": controlPlaneOperatorAccount}
	binding.Object["subjects"] = subject
	nsRole := obj("rbac.authorization.k8s.io/v1", "Role", OpenChoreoNamespace, controlPlaneOperatorAccount, nil)
	nsRole.Object["rules"] = controlPlaneNamespaceRules()
	nsBinding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", OpenChoreoNamespace, controlPlaneOperatorAccount, nil)
	nsBinding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": controlPlaneOperatorAccount}
	nsBinding.Object["subjects"] = subject
	for _, item := range []struct {
		resource string
		u        *unstructured.Unstructured
	}{
		{"serviceaccounts", obj("v1", "ServiceAccount", controlPlaneOperatorNamespace, controlPlaneOperatorAccount, nil)},
		{"clusterroles", role}, {"clusterrolebindings", binding}, {"roles", nsRole}, {"rolebindings", nsBinding},
	} {
		if e := admin.applyAs(ctx, s.owner, item.resource, item.u); e != nil {
			return e
		}
	}
	return nil
}

// secretValue decodes one key of an unstructured Secret.
func secretValue(u *unstructured.Unstructured, key string) []byte {
	encoded, _, _ := unstructured.NestedString(u.Object, "data", key)
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return b
}

func (s *controlPlaneSession) saveCredential(ctx context.Context, c *rest.Config, expiry time.Time, revision string) error {
	data, e := operatorKubeconfig(c)
	if e != nil {
		return e
	}
	return s.r.Management.applyAs(ctx, s.owner, "secrets", secretObject(s.r.Options.Namespace, s.credentialName(), map[string][]byte{"kubeconfig": data, "expires": []byte(expiry.Format(time.RFC3339)), "contract": []byte(revision)}))
}

// identity returns the scoped operator identity on aep-cp, installing its
// contract with the super-admin credential when missing or changed, and
// renewing its token before expiry.
func (s *controlPlaneSession) identity(ctx context.Context) (*Cluster, error) {
	if s.remote != nil {
		return s.remote, nil
	}
	revision, e := controlPlaneContractRevision()
	if e != nil {
		return nil, e
	}
	stored, e := s.r.Management.get(ctx, "v1", "secrets", s.r.Options.Namespace, s.credentialName())
	if apierrors.IsNotFound(e) {
		admin, e := s.admin(ctx)
		if e != nil {
			return nil, e
		}
		if e = s.installContract(ctx, admin); e != nil {
			return nil, e
		}
		token, expiry, e := requestTokenIn(ctx, admin, controlPlaneOperatorNamespace, controlPlaneOperatorAccount)
		if e != nil {
			return nil, e
		}
		cfg := rest.AnonymousClientConfig(admin.Config)
		cfg.BearerToken = token
		if e = s.saveCredential(ctx, cfg, expiry, revision); e != nil {
			return nil, e
		}
		s.remote, e = s.r.connect(cfg)
		return s.remote, e
	}
	if e != nil {
		return nil, e
	}
	if stored.GetLabels()[ControlPlaneLabel] != string(s.cp.UID) {
		return nil, fmt.Errorf("operator credential %s/%s ownership mismatch", s.r.Options.Namespace, s.credentialName())
	}
	cfg, e := clientcmd.RESTConfigFromKubeConfig(secretValue(stored, "kubeconfig"))
	if e != nil {
		return nil, e
	}
	expires, e := time.Parse(time.RFC3339, string(secretValue(stored, "expires")))
	if e != nil {
		return nil, e
	}
	if string(secretValue(stored, "contract")) != revision {
		admin, e := s.admin(ctx)
		if e != nil {
			return nil, e
		}
		if e = s.installContract(ctx, admin); e != nil {
			return nil, e
		}
		if e = s.saveCredential(ctx, cfg, expires, revision); e != nil {
			return nil, e
		}
	}
	c, e := s.r.connect(cfg)
	if e != nil {
		return nil, e
	}
	if time.Until(expires) < 12*time.Hour {
		token, expiry, e := requestTokenIn(ctx, c, controlPlaneOperatorNamespace, controlPlaneOperatorAccount)
		if e != nil {
			return nil, fmt.Errorf("operator token renewal failed; remove owned credential Secret to explicitly rebootstrap: %w", e)
		}
		cfg.BearerToken = token
		if e = s.saveCredential(ctx, cfg, expiry, revision); e != nil {
			return nil, e
		}
		if c, e = s.r.connect(cfg); e != nil {
			return nil, e
		}
	}
	s.remote = c
	return c, nil
}

// contractDrift turns a refusal of the identity into a contract reinstall:
// the stored revision only records what was installed last.
func (s *controlPlaneSession) contractDrift(ctx context.Context, err error) error {
	if !apierrors.IsForbidden(err) {
		return err
	}
	stored, e := s.r.Management.get(ctx, "v1", "secrets", s.r.Options.Namespace, s.credentialName())
	if e != nil {
		return fmt.Errorf("%w; %v", err, e)
	}
	cfg, e := clientcmd.RESTConfigFromKubeConfig(secretValue(stored, "kubeconfig"))
	if e != nil {
		return fmt.Errorf("%w; %v", err, e)
	}
	expires, e := time.Parse(time.RFC3339, string(secretValue(stored, "expires")))
	if e != nil {
		return fmt.Errorf("%w; %v", err, e)
	}
	if e = s.saveCredential(ctx, cfg, expires, ""); e != nil {
		return fmt.Errorf("%w; %v", err, e)
	}
	s.remote = nil
	return fmt.Errorf("aep-cp denied the operator, reinstalling its identity contract: %w", err)
}

func (s *controlPlaneSession) access(ctx context.Context) error {
	remote, e := s.identity(ctx)
	if e != nil {
		return e
	}
	// The identity must still hold its contract; a denial means its RBAC changed.
	if _, e = remote.get(ctx, "v1", "namespaces", "", OpenChoreoNamespace); e != nil {
		return s.contractDrift(ctx, e)
	}
	s.cp.Status.Outputs.KubeconfigSecretRef = &api.SecretNameReference{Name: s.credentialName()}
	return nil
}
