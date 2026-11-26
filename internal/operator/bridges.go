package operator

import (
	"context"
	"encoding/json"
	"github.com/tym83/aep/internal/policy"
	"github.com/tym83/aep/internal/registrymirror"
	inventory "github.com/tym83/aep/release"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"strings"
	"time"
)

func bridgeRules() []any {
	return []any{
		rule("apps.cozystack.io", []string{"postgreses", "redises"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
		rule("core.cozystack.io", []string{"tenantsecrets"}, []string{"get", "list", "watch"}),
		rule("helm.toolkit.fluxcd.io", []string{"helmreleases"}, []string{"get", "list", "watch"}),
		rule("backups.cozystack.io", []string{"backups"}, []string{"get", "list"}),
		rule("backups.cozystack.io", []string{"plans"}, []string{"get", "list", "watch", "create", "update", "patch"}),
		rule("", []string{"services"}, []string{"get"}), rule("", []string{"events"}, []string{"create", "patch"}),
		rule("coordination.k8s.io", []string{"leases"}, []string{"get", "create", "update", "patch"}),
	}
}
func (s *session) bridges(ctx context.Context) error {
	for _, tier := range Tiers(s.o) {
		c, e := s.bootstrap(ctx, tier)
		if e != nil {
			return e
		}
		name := "aep-bridge-" + s.o.Name + "-" + tier
		ns := s.r.Options.Namespace
		tenant := TenantNamespace(s.o.Name, tier)
		sec, e := s.r.Management.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		refresh := errors.IsNotFound(e)
		if e != nil && !refresh {
			return e
		}
		if e == nil {
			expiry, err := time.Parse(time.RFC3339, string(sec.Data["expires"]))
			refresh = err != nil || time.Until(expiry) < 12*time.Hour
		}
		if refresh {
			token, expires, e := requestToken(ctx, c, "aep-bridge")
			if e != nil {
				return e
			}
			cfg := clientcmdapi.NewConfig()
			cfg.Clusters["app"] = &clientcmdapi.Cluster{Server: c.Config.Host, CertificateAuthorityData: c.Config.CAData, TLSServerName: c.Config.ServerName}
			cfg.AuthInfos["bridge"] = &clientcmdapi.AuthInfo{TokenFile: "/credentials/token"}
			cfg.Contexts["app"] = &clientcmdapi.Context{Cluster: "app", AuthInfo: "bridge"}
			cfg.CurrentContext = "app"
			data, e := clientcmd.Write(*cfg)
			if e != nil {
				return e
			}
			u := secretObject(ns, name, map[string][]byte{"kubeconfig": data, "token": []byte(token), "expires": []byte(expires.Format(time.RFC3339))})
			if e = s.r.Management.apply(ctx, s.o, "secrets", u); e != nil {
				return e
			}
		}
		sa := obj("v1", "ServiceAccount", ns, name, nil)
		if e = s.r.Management.apply(ctx, s.o, "serviceaccounts", sa); e != nil {
			return e
		}
		role := obj("rbac.authorization.k8s.io/v1", "Role", tenant, name, nil)
		role.Object["rules"] = bridgeRules()
		if e = s.r.Management.apply(ctx, s.o, "roles", role); e != nil {
			return e
		}
		binding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", tenant, name, nil)
		binding.Object["roleRef"] = M{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name}
		binding.Object["subjects"] = []any{M{"kind": "ServiceAccount", "namespace": ns, "name": name}}
		if e = s.r.Management.apply(ctx, s.o, "rolebindings", binding); e != nil {
			return e
		}
		p := policy.Default()
		services := s.o.Spec.Tiers[tier].Services
		p.BackupPolicy = services.BackupPolicy
		p.Backup, e = s.backupPolicy(ctx, tier)
		if e != nil {
			return e
		}
		for kind, l := range p.Kinds {
			if services.MaxPreset != "" {
				l.MaxPreset = services.MaxPreset
			}
			p.Kinds[kind] = l
		}
		data, e := json.Marshal(p)
		if e != nil {
			return e
		}
		cm := obj("v1", "ConfigMap", ns, name, nil)
		cm.Object["data"] = M{"policy.yaml": string(data)}
		if e = s.r.Management.apply(ctx, s.o, "configmaps", cm); e != nil {
			return e
		}
		args := []string{"--organization=" + s.o.Name, "--tier=" + tier, "--tenant-namespace=" + tenant, "--cluster-id=" + string(s.o.UID) + "-" + tier, "--allowed-kinds=" + strings.Join(services.AllowedKinds, ","), "--app-kubeconfig=/credentials/kubeconfig", "--app-token-file=/credentials/token", "--config=/policy/policy.yaml", "--tier-deletion-policy=Retain"}
		labels := M{"app.kubernetes.io/name": name}
		bridgeImage, err := inventory.Image(s.r.Options.BridgeImage)
		if err != nil {
			return err
		}
		dep := obj("apps/v1", "Deployment", ns, name, M{"replicas": int64(1), "selector": M{"matchLabels": labels}, "template": M{"metadata": M{"labels": labels, "annotations": M{"aep.aenix.io/policy-hash": policy.Hash(M{"policy": string(data)})}}, "spec": M{"serviceAccountName": name, "securityContext": M{"runAsNonRoot": true, "seccompProfile": M{"type": "RuntimeDefault"}}, "containers": []any{M{"name": "bridge", "image": registrymirror.Rewrite(bridgeImage, s.r.Options.ImageRegistryMirror), "securityContext": M{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": M{"drop": []any{"ALL"}}}, "args": stringsAny(args), "volumeMounts": []any{M{"name": "credentials", "mountPath": "/credentials", "readOnly": true}, M{"name": "policy", "mountPath": "/policy", "readOnly": true}}, "readinessProbe": M{"httpGet": M{"path": "/readyz", "port": int64(8081)}}, "resources": M{"requests": M{"cpu": "50m", "memory": "64Mi"}, "limits": M{"memory": "256Mi"}}}}, "volumes": []any{M{"name": "credentials", "secret": M{"secretName": name}}, M{"name": "policy", "configMap": M{"name": name}}}}}})
		if e = s.r.Management.apply(ctx, s.o, "deployments", dep); e != nil {
			return e
		}
		got, e := s.r.Management.get(ctx, "apps/v1", "deployments", ns, name)
		if e != nil {
			return e
		}
		observed, _, _ := unstructured.NestedInt64(got.Object, "status", "observedGeneration")
		available, _, _ := unstructured.NestedInt64(got.Object, "status", "availableReplicas")
		if observed < got.GetGeneration() || available < 1 {
			return pending("waiting for bridge %s Available", name)
		}
	}
	return nil
}
