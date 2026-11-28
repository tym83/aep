package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"strings"
	"time"
)

func ControlPlaneReady(u *unstructured.Unstructured) bool {
	v, _, _ := unstructured.NestedString(u.Object, "status", "kubernetesResources", "version", "status")
	return v == "Ready"
}
func NodesReady(nodes []corev1.Node, expected int) bool {
	n := 0
	for _, node := range nodes {
		if !node.DeletionTimestamp.IsZero() {
			continue
		}
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				n++
				break
			}
		}
	}
	return n >= expected
}
func BootstrapRemediation(cpReady bool, nodeCount int, hasToken bool, since, now time.Time) bool {
	return cpReady && nodeCount == 0 && !hasToken && !since.IsZero() && now.Sub(since) > 10*time.Minute
}
func (s *session) clusters(ctx context.Context) error {
	for _, tier := range Tiers(s.o) {
		ns := TenantNamespace(s.o.Name, tier)
		spec := s.o.Spec.Tiers[tier].Cluster
		n := spec.NodeGroups["md0"]
		addons := tierAddons(s.o, s.r.Options, tier)
		for _, u := range []*unstructured.Unstructured{
			obj(appsAPI, "Kubernetes", ns, "apps", M{"version": spec.Version, "storageClass": "replicated", "addons": addons}),
			obj(appsAPI, "KubernetesNodes", ns, "apps-md0", M{"cluster": "apps", "version": spec.Version, "storageClass": "replicated", "diskSize": "20Gi", "instanceType": n.InstanceType, "minReplicas": int64(n.MinReplicas), "maxReplicas": int64(n.MaxReplicas), "roles": tierNodeRoles(addons)})} {
			r := "kuberneteses"
			if u.GetKind() == "KubernetesNodes" {
				r = "kubernetesnodeses"
			}
			if e := s.r.Management.apply(ctx, s.o, r, u); e != nil {
				return e
			}
		}
		if e := waitCluster(ctx, s.r.Management, tierCluster{s: s, tier: tier}, ns, "apps", int(n.MinReplicas)); e != nil {
			return e
		}
	}
	return nil
}

// clusterOwner is what waitCluster needs from the resource that owns a
// Cozystack Kubernetes cluster.
type clusterOwner interface {
	// label names the cluster in pending messages.
	label() string
	// readySince returns when the control plane was first seen Ready,
	// recording now on the first call.
	readySince() time.Time
	// notReady forgets the time the control plane became Ready.
	notReady()
	// connect returns the operator identity in the cluster.
	connect(context.Context) (*Cluster, error)
	// remediate force-reconciles a management HelmRelease at most once per
	// remediation interval.
	remediate(ctx context.Context, ns, release, reason string) error
}

// waitCluster waits until the Cozystack Kubernetes application cluster in
// namespace ns has a Ready control plane, Ready addon HelmReleases and at
// least minNodes Ready nodes, remediating the known stuck states.
func waitCluster(ctx context.Context, mgmt *Cluster, owner clusterOwner, ns, cluster string, minNodes int) error {
	release := "kubernetes-" + cluster
	cp, e := mgmt.get(ctx, "kamaji.clastix.io/v1alpha1", "tenantcontrolplanes", ns, release)
	if apierrors.IsNotFound(e) {
		return pending("waiting for %s TenantControlPlane to be created", owner.label())
	}
	if e != nil {
		return e
	}
	if !ControlPlaneReady(cp) {
		owner.notReady()
		return pending("waiting for %s control plane version Ready", owner.label())
	}
	since := owner.readySince()
	c, e := owner.connect(ctx)
	if e != nil {
		return e
	}
	nodes, e := c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if e != nil {
		return e
	}
	if len(nodes.Items) == 0 && time.Since(since) > 10*time.Minute {
		secrets, e := c.Kube.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
		if e != nil {
			return e
		}
		hasToken := false
		for _, sec := range secrets.Items {
			if sec.Type == corev1.SecretTypeBootstrapToken {
				hasToken = true
			}
		}
		if BootstrapRemediation(true, len(nodes.Items), hasToken, since, time.Now()) {
			if e = owner.remediate(ctx, ns, release, "MissingBootstrapToken"); e != nil {
				return e
			}
		}
	}
	releases, e := mgmt.list(ctx, helmAPI, "helmreleases", ns)
	if e != nil {
		return e
	}
	found := false
	for _, hr := range releases {
		if hr.GetLabels()["cozystack.io/target-cluster-name"] != release && !strings.HasPrefix(hr.GetName(), release+"-") {
			continue
		}
		// Releases delivered by AEP target the cluster but are no addons.
		if hr.GetLabels()[OwnerLabel] != "" || hr.GetLabels()[ControlPlaneLabel] != "" {
			continue
		}
		found = true
		if !ready(&hr) {
			cs, _, _ := unstructured.NestedSlice(hr.Object, "status", "conditions")
			for _, v := range cs {
				m, _ := v.(map[string]any)
				msg, _ := m["message"].(string)
				if strings.Contains(msg, "context deadline exceeded") {
					if e = owner.remediate(ctx, ns, hr.GetName(), "AddonAPITimeout"); e != nil {
						return e
					}
					break
				}
			}
			return pending("waiting for HelmRelease %s/%s", ns, hr.GetName())
		}
	}
	if !found {
		return pending("waiting for cluster addon HelmReleases in %s", ns)
	}
	if !NodesReady(nodes.Items, minNodes) {
		return pending("waiting for %d Ready nodes in %s", minNodes, ns)
	}
	return nil
}

// tierCluster adapts one Organization tier to waitCluster.
type tierCluster struct {
	s    *session
	tier string
}

func (t tierCluster) label() string { return t.tier }
func (t tierCluster) readySince() time.Time {
	if t.s.o.Status.ControlPlaneReadySince == nil {
		t.s.o.Status.ControlPlaneReadySince = map[string]metav1.Time{}
	}
	since, ok := t.s.o.Status.ControlPlaneReadySince[t.tier]
	if !ok {
		since = metav1.Now()
		t.s.o.Status.ControlPlaneReadySince[t.tier] = since
	}
	return since.Time
}
func (t tierCluster) notReady() { delete(t.s.o.Status.ControlPlaneReadySince, t.tier) }
func (t tierCluster) connect(ctx context.Context) (*Cluster, error) {
	return t.s.bootstrap(ctx, t.tier)
}
func (t tierCluster) remediate(ctx context.Context, ns, release, reason string) error {
	return t.s.remediate(ctx, ns, release, reason)
}
func (s *session) remediate(ctx context.Context, ns, name, reason string) error {
	for _, r := range s.o.Status.Remediations {
		if r.Namespace == ns && r.Release == name && time.Since(r.Time.Time) < 10*time.Minute {
			return nil
		}
	}
	now := metav1.Now()
	payload := []byte(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"` + now.Format(time.RFC3339Nano) + `","reconcile.fluxcd.io/forceAt":"` + now.Format(time.RFC3339Nano) + `"}}}`)
	if _, e := s.r.Management.resource(helmAPI, "helmreleases", ns).Patch(ctx, name, types.MergePatchType, payload, metav1.PatchOptions{}); e != nil {
		return e
	}
	s.o.Status.Remediations = append(s.o.Status.Remediations, api.Remediation{Time: now, Namespace: ns, Release: name, Reason: reason})
	if len(s.o.Status.Remediations) > 50 {
		s.o.Status.Remediations = s.o.Status.Remediations[len(s.o.Status.Remediations)-50:]
	}
	if s.r.Recorder != nil {
		s.r.Recorder.Eventf(s.o, "Warning", reason, "Force-reconciling HelmRelease %s/%s", ns, name)
	}
	return nil
}

// tierAddons are the Cozystack addons of a tier cluster. Tenant Argo CD is
// published through the tier ingress, proxied from the platform ingress with
// TLS passthrough for exactly its hostname. The platform ingress reaches the
// tier controller only on nodes with the ingress-nginx role, which clusters()
// sets on the node group whenever this addon is enabled.
func tierAddons(o *api.Organization, opt Options, tier string) M {
	addons := M{"certManager": M{"enabled": true}, "gatewayAPI": M{"enabled": true}}
	if tier == "nonprod" && argoEnabled(o) {
		if host, err := argoHost(opt.ArgoCDHostTemplate, o.Name); err == nil {
			addons["ingressNginx"] = M{"enabled": true, "exposeMethod": "Proxied", "hosts": []any{host}}
		}
	}
	return addons
}

// tierNodeRoles labels the tier workers that host the tier ingress controller.
func tierNodeRoles(addons M) []any {
	if _, ok := addons["ingressNginx"]; ok {
		return []any{"ingress-nginx"}
	}
	return []any{}
}
