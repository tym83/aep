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
		for _, u := range []*unstructured.Unstructured{
			obj(appsAPI, "Kubernetes", ns, "apps", M{"version": spec.Version, "storageClass": "replicated", "addons": M{"certManager": M{"enabled": true}, "gatewayAPI": M{"enabled": true}}}),
			obj(appsAPI, "KubernetesNodes", ns, "apps-md0", M{"cluster": "apps", "version": spec.Version, "storageClass": "replicated", "diskSize": "20Gi", "instanceType": n.InstanceType, "minReplicas": int64(n.MinReplicas), "maxReplicas": int64(n.MaxReplicas)})} {
			r := "kuberneteses"
			if u.GetKind() == "KubernetesNodes" {
				r = "kubernetesnodeses"
			}
			if e := s.r.Management.apply(ctx, s.o, r, u); e != nil {
				return e
			}
		}
		cp, e := s.r.Management.get(ctx, "kamaji.clastix.io/v1alpha1", "tenantcontrolplanes", ns, "kubernetes-apps")
		if apierrors.IsNotFound(e) {
			return pending("waiting for %s TenantControlPlane to be created", tier)
		}
		if e != nil {
			return e
		}
		if !ControlPlaneReady(cp) {
			delete(s.o.Status.ControlPlaneReadySince, tier)
			return pending("waiting for %s control plane version Ready", tier)
		}
		if s.o.Status.ControlPlaneReadySince == nil {
			s.o.Status.ControlPlaneReadySince = map[string]metav1.Time{}
		}
		since, ok := s.o.Status.ControlPlaneReadySince[tier]
		if !ok {
			since = metav1.Now()
			s.o.Status.ControlPlaneReadySince[tier] = since
		}
		c, e := s.bootstrap(ctx, tier)
		if e != nil {
			return e
		}
		nodes, e := c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if e != nil {
			return e
		}
		if len(nodes.Items) == 0 && time.Since(since.Time) > 10*time.Minute {
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
			if BootstrapRemediation(true, len(nodes.Items), hasToken, since.Time, time.Now()) {
				if e = s.remediate(ctx, ns, "kubernetes-apps", "MissingBootstrapToken"); e != nil {
					return e
				}
			}
		}
		releases, e := s.r.Management.list(ctx, helmAPI, "helmreleases", ns)
		if e != nil {
			return e
		}
		found := false
		for _, hr := range releases {
			if hr.GetLabels()["cozystack.io/target-cluster-name"] != "kubernetes-apps" && !strings.HasPrefix(hr.GetName(), "kubernetes-apps-") {
				continue
			}
			if hr.GetLabels()[OwnerLabel] != "" {
				continue
			}
			found = true
			if !ready(&hr) {
				cs, _, _ := unstructured.NestedSlice(hr.Object, "status", "conditions")
				for _, v := range cs {
					m, _ := v.(map[string]any)
					msg, _ := m["message"].(string)
					if strings.Contains(msg, "context deadline exceeded") {
						if e = s.remediate(ctx, ns, hr.GetName(), "AddonAPITimeout"); e != nil {
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
		if !NodesReady(nodes.Items, int(n.MinReplicas)) {
			return pending("waiting for %d Ready nodes in %s", n.MinReplicas, ns)
		}
	}
	return nil
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
