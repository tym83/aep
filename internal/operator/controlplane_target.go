package operator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// controlPlaneTarget is the aep-cp serving an Organization: an operator
// managed ControlPlane, or the installation named by the legacy --aepcp-*
// flags.
type controlPlaneTarget struct {
	// cp is the serving ControlPlane; nil for the legacy flags.
	cp *api.ControlPlane
	// namespace is the management tenant namespace holding the aep-cp
	// cluster, cluster its Cozystack Kubernetes name.
	namespace, cluster string
}

func (t controlPlaneTarget) managed() bool { return t.cp != nil }

// controlPlaneName is the ControlPlane serving org: its reference, or the
// default control plane when neither a reference nor a legacy aep-cp
// kubeconfig is configured. Empty means the legacy flags.
func (o Options) controlPlaneName(org *api.Organization) string {
	if ref := orDefault(org.Spec.ControlPlaneRef, org.Status.ControlPlaneRef); ref != "" {
		return ref
	}
	if o.DefaultControlPlane != "" && o.CPSecret == "" && o.CPFile == "" {
		return o.DefaultControlPlane
	}
	return ""
}

// missingControlPlaneError reports a ControlPlane that does not exist.
type missingControlPlaneError struct{ name string }

func (e *missingControlPlaneError) Error() string {
	return fmt.Sprintf("ControlPlane %s does not exist", e.name)
}

// controlPlaneFor resolves the aep-cp serving org. It never switches an
// Organization implicitly: a reference wins, the default applies only
// without legacy flags, and the legacy flags are used exactly as before.
func (r *Reconciler) controlPlaneFor(ctx context.Context, org *api.Organization) (controlPlaneTarget, error) {
	name := r.Options.controlPlaneName(org)
	if name == "" {
		return controlPlaneTarget{namespace: r.Options.CPNamespace, cluster: r.Options.CPCluster}, nil
	}
	cp := &api.ControlPlane{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return controlPlaneTarget{}, &missingControlPlaneError{name: name}
		}
		return controlPlaneTarget{}, err
	}
	out := cp.Status.Outputs
	return controlPlaneTarget{cp: cp, namespace: orDefault(out.TenantNamespace, ControlPlaneNamespace(cp)), cluster: orDefault(out.ClusterName, cp.Spec.Cluster.Name)}, nil
}

// target resolves the serving aep-cp once per session.
func (s *session) target(ctx context.Context) (controlPlaneTarget, error) {
	if s.resolved != nil {
		return *s.resolved, nil
	}
	t, err := s.r.controlPlaneFor(ctx, s.o)
	if err != nil {
		return t, err
	}
	s.resolved = &t
	return t, nil
}

// controlPlaneReadiness are the ControlPlane conditions an Organization
// waits for: the chart, the gateway and the hardened installation.
var controlPlaneReadiness = []string{"OpenChoreoReady", "GatewayReady", "Hardened"}

// controlPlaneNotReady explains why a ControlPlane cannot serve
// Organizations yet, or returns "" when it can.
func controlPlaneNotReady(cp *api.ControlPlane) string {
	for _, t := range controlPlaneReadiness {
		c := meta.FindStatusCondition(cp.Status.Conditions, t)
		if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != cp.Generation {
			message := "not reported"
			if c != nil {
				message = c.Message
			}
			return fmt.Sprintf("ControlPlane %s is not ready: %s: %s", cp.Name, t, message)
		}
	}
	g := cp.Status.Gateway
	if cp.Status.Outputs.KubeconfigSecretRef == nil || g.Host == "" || g.URL == "" || g.MirrorService == "" {
		return fmt.Sprintf("ControlPlane %s has not published its credential and gateway yet", cp.Name)
	}
	return ""
}

// controlPlaneReady holds the Organization until its ControlPlane can
// serve it. Legacy installations are not gated.
func (s *session) controlPlaneReady(ctx context.Context) error {
	t, err := s.target(ctx)
	var missing *missingControlPlaneError
	switch {
	case errors.As(err, &missing):
		return pending("%s", err.Error())
	case err != nil:
		return err
	case !t.managed():
		return nil
	}
	if reason := controlPlaneNotReady(t.cp); reason != "" {
		return pending("%s", reason)
	}
	return nil
}

// controlPlaneNotReadyReason marks a held Organization; API failures keep
// their usual reason.
func controlPlaneNotReadyReason(err error) string {
	var waiting *pendingError
	if errors.As(err, &waiting) {
		return "ControlPlaneNotReady"
	}
	return ""
}

// managedControlPlane connects with the operator-scoped credential the
// ControlPlane published in the operator namespace.
func (s *session) managedControlPlane(ctx context.Context, t controlPlaneTarget) (*Cluster, error) {
	ref := t.cp.Status.Outputs.KubeconfigSecretRef
	if ref == nil {
		return nil, pending("ControlPlane %s has not published its operator credential yet", t.cp.Name)
	}
	sec, err := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	// Status can be written by others; only the ControlPlane's own credential is used.
	if sec.Labels[ControlPlaneLabel] != string(t.cp.UID) {
		return nil, fmt.Errorf("credential Secret %s/%s is not owned by ControlPlane %s", s.r.Options.Namespace, ref.Name, t.cp.Name)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(sec.Data["kubeconfig"])
	if err != nil {
		return nil, err
	}
	return NewCluster(cfg)
}

// organizationNamespaces are the management tenant namespaces of o.
func organizationNamespaces(o *api.Organization) []string {
	out := []string{TenantNamespace(o.Name, "")}
	for _, tier := range Tiers(o) {
		out = append(out, TenantNamespace(o.Name, tier))
	}
	return out
}

// namespaceCollision reports a ControlPlane tenant namespace that is also a
// tenant namespace of Organization o, or "".
func namespaceCollision(cp *api.ControlPlane, o *api.Organization) string {
	if ns := ControlPlaneNamespace(cp); slices.Contains(organizationNamespaces(o), ns) {
		return fmt.Sprintf("tenant namespace %s is used by both ControlPlane %s and Organization %s", ns, cp.Name, o.Name)
	}
	return ""
}

// controlPlaneRejection reports why o cannot use the control planes as
// configured: a reference to a missing ControlPlane, or a tenant namespace
// shared with a ControlPlane. A missing default control plane only holds
// the Organization (controlPlaneReady), since it is operator configuration.
func (r *Reconciler) controlPlaneRejection(ctx context.Context, o *api.Organization) (string, error) {
	if ref := orDefault(o.Spec.ControlPlaneRef, o.Status.ControlPlaneRef); ref != "" {
		if err := r.Get(ctx, client.ObjectKey{Name: ref}, &api.ControlPlane{}); apierrors.IsNotFound(err) {
			return fmt.Sprintf("controlPlaneRef: ControlPlane %s does not exist", ref), nil
		} else if err != nil {
			return "", err
		}
	}
	var planes api.ControlPlaneList
	if err := r.List(ctx, &planes); err != nil {
		return "", err
	}
	var collisions []string
	for i := range planes.Items {
		if c := namespaceCollision(&planes.Items[i], o); c != "" {
			collisions = append(collisions, c)
		}
	}
	return strings.Join(collisions, "; "), nil
}
