package operator

import (
	"context"
	"errors"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"sync"
	"time"
)

type Options struct {
	ArgoCDHostTemplate, ArgoCDChartURL, VCSWebhookURL string
	// ArgoCDACMEServer, when set, makes the operator issue the Argo CD host
	// certificate through cert-manager in the tier cluster with this ACME server.
	ArgoCDACMEServer                                                                      string
	RegistryInsecure                                                                      bool
	ImageRegistryMirror, ChartRegistryMirror, RegistrySecret, RegistryCASecret            string
	PolicyBundleRepository, KyvernoChartURL                                               string
	PruneBootstrapBindings                                                                bool
	BootstrapBindingSystemClients                                                         string
	RestoreVerification                                                                   bool
	Namespace, CPSecret, CPFile, CPNamespace, CPCluster, GatewayHostTemplate, BridgeImage string
	TenantEgressWorkaround, KeycloakEnabled                                               bool
	KeycloakURL, KeycloakRealm, KeycloakCredentialsSecret                                 string
	// DefaultControlPlane names the ControlPlane serving Organizations
	// without controlPlaneRef when no legacy aep-cp kubeconfig is configured.
	DefaultControlPlane string
}
type Reconciler struct {
	client.Client
	Management   *Cluster
	Options      Options
	Recorder     record.EventRecorder
	ResolveHost  hostResolver
	hookMu       sync.Mutex
	hookAttempts map[string]time.Time
}
type session struct {
	vcsVerified bool
	r           *Reconciler
	o           *api.Organization
	cp          *Cluster
	tiers       map[string]*Cluster
	gateway     string
	// resolved is the aep-cp serving the Organization, once resolved.
	resolved *controlPlaneTarget
}

var conditionTypes = []string{"TenantsReady", "ClustersReady", "NetworkPathsReady", "DataPlanesConnected", "PoliciesReady", "OpenChoreoNamespaceReady", "BridgesReady", "AccessReady", "VCSReady", "ArgoCDReady"}

func retryLimiter() workqueue.TypedRateLimiter[ctrl.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](5*time.Millisecond, 2*time.Minute)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	o := &api.Organization{}
	if e := r.Get(ctx, req.NamespacedName, o); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	s := &session{r: r, o: o, tiers: map[string]*Cluster{}}
	s.hardenControlPlane(ctx)
	if e := r.Status().Update(ctx, o); e != nil {
		return ctrl.Result{}, e
	}
	if !o.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, o)
	}
	if slices.Contains(o.Finalizers, finalizer) {
		if e := s.renewCredentials(ctx); e != nil {
			setCondition(o, "BridgesReady", false, "CredentialRenewalFailed", e.Error())
			if err := r.Status().Update(ctx, o); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, e
		}

	}
	if e := Validate(o); e != nil {
		return r.reject(ctx, o, e.Error())
	}
	rejection, e := r.controlPlaneRejection(ctx, o)
	if e != nil {
		return ctrl.Result{}, e
	}
	if rejection != "" {
		return r.reject(ctx, o, rejection)
	}
	// Record an accepted reference before the finalizer marks the
	// Organization as reconciled, so a failed status write cannot turn a
	// reference given at creation into an unannotated late change.
	if o.Spec.ControlPlaneRef != "" && o.Status.ControlPlaneRef == "" {
		o.Status.ControlPlaneRef = o.Spec.ControlPlaneRef
		if e := r.Status().Update(ctx, o); e != nil {
			return ctrl.Result{}, e
		}
	}
	if !slices.Contains(o.Finalizers, finalizer) {
		base := o.DeepCopy()
		o.Finalizers = append(o.Finalizers, finalizer)
		if e := r.Patch(ctx, o, client.MergeFrom(base)); e != nil {
			return ctrl.Result{}, e
		}
	}
	return r.runSteps(ctx, o, s.steps())
}

// reject records an invalid Organization; nothing is reconciled until it changes.
func (r *Reconciler) reject(ctx context.Context, o *api.Organization, message string) (ctrl.Result, error) {
	for _, t := range conditionTypes {
		setCondition(o, t, false, "InvalidSpec", message)
	}
	o.Status.Phase = "Rejected"
	return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, o)
}

// steps lists the organization reconcile steps in dependency order.
func (s *session) steps() []reconcileStep {
	return []reconcileStep{
		{name: "TenantsReady", run: s.tenants},
		// Root etcd must be reachable before a control plane can become Ready.
		{name: "NetworkPathsReady", run: s.workaround, partial: true},
		// Tier clusters receive the gateway host of a managed control plane,
		// and everything after them talks to it.
		{name: "ClustersReady", run: s.controlPlaneReady, partial: true, reason: controlPlaneNotReadyReason},
		{name: "ClustersReady", run: s.clusters},
		{name: "NetworkPathsReady", run: s.network},
		// Policies need only the tier cluster. Delivering them after the data
		// plane deadlocks when a policy blocks the data plane's own upgrade:
		// the corrected bundle could never arrive.
		{name: "PoliciesReady", run: s.policies},
		{name: "DataPlanesConnected", run: s.delivery, partial: true},
		{name: "OpenChoreoNamespaceReady", run: s.openChoreo},
		{name: "DataPlanesConnected", run: s.connected},
		{name: "AccessReady", run: s.access},
		{name: "BridgesReady", run: s.bridges},
		{name: "VCSReady", run: s.vcs},
		{name: "ArgoCDReady", run: s.argocd},
	}
}

type reconcileStep struct {
	name string
	run  func(context.Context) error
	// partial steps are the first half of a condition completed by a later
	// step; success does not mark the condition True.
	partial bool
	// nonBlocking failures leave later steps running.
	nonBlocking bool
	// holdsReady failures, when nonBlocking, keep the phase Pending after
	// all other steps succeeded.
	holdsReady bool
	// reason, when it returns non-empty, replaces the failure reason.
	reason func(error) string
	// message replaces the success message.
	message func() string
}

// stepStatus is the status surface of a resource driven by runReconcileSteps.
type stepStatus interface {
	// conditionTypes are reset to Pending when stale for the generation.
	conditionTypes() []string
	conditions() []metav1.Condition
	generation() int64
	setCondition(t string, ok bool, reason, msg string)
	setPhase(string)
	// readyPhase is the phase after every step succeeded.
	readyPhase() string
	updateStatus(context.Context) error
}

// organizationStatus adapts an Organization to the step runner.
type organizationStatus struct {
	r *Reconciler
	o *api.Organization
}

func (s organizationStatus) conditionTypes() []string       { return conditionTypes }
func (s organizationStatus) conditions() []metav1.Condition { return s.o.Status.Conditions }
func (s organizationStatus) generation() int64              { return s.o.Generation }
func (s organizationStatus) setPhase(phase string)          { s.o.Status.Phase = phase }
func (s organizationStatus) updateStatus(ctx context.Context) error {
	return s.r.Status().Update(ctx, s.o)
}
func (s organizationStatus) setCondition(t string, ok bool, reason, msg string) {
	setCondition(s.o, t, ok, reason, msg)
}
func (s organizationStatus) readyPhase() string {
	if !meta.IsStatusConditionTrue(s.o.Status.Conditions, "ControlPlaneHardened") {
		return "Pending"
	}
	return "Ready"
}

// organizationStepPolicy marks the Organization steps that do not block the
// others. Policy delivery is observable independently of organization access.
func organizationStepPolicy(o *api.Organization, steps []reconcileStep) []reconcileStep {
	out := slices.Clone(steps)
	for i := range out {
		switch out[i].name {
		case "VCSReady":
			out[i].nonBlocking = true
			out[i].message = func() string {
				if o.Spec.VCS == nil {
					return "Corporate VCS not configured"
				}
				return "Desired state verified"
			}
		case "ArgoCDReady":
			out[i].nonBlocking = true
			out[i].message = func() string {
				if o.Status.ArgoCDURL != "" {
					return o.Status.ArgoCDURL
				}
				return "Argo CD disabled"
			}
		case "PoliciesReady":
			out[i].nonBlocking, out[i].holdsReady = true, true
			out[i].reason = func(err error) string {
				if errors.Is(err, errCosignKeyMissing) {
					return "CosignKeyMissing"
				}
				return ""
			}
		}
	}
	return out
}

func (r *Reconciler) runSteps(ctx context.Context, o *api.Organization, steps []reconcileStep) (ctrl.Result, error) {
	return runReconcileSteps(ctx, organizationStatus{r: r, o: o}, organizationStepPolicy(o, steps))
}

// runReconcileSteps runs steps in order and records one condition per step
// name. A blocking failure marks later steps DependencyPending and stops.
func runReconcileSteps(ctx context.Context, st stepStatus, steps []reconcileStep) (ctrl.Result, error) {
	st.setPhase("Reconciling")
	for _, t := range st.conditionTypes() {
		if c := meta.FindStatusCondition(st.conditions(), t); c == nil || c.ObservedGeneration != st.generation() {
			st.setCondition(t, false, "Pending", "Waiting for dependencies")
		}
	}
	held := false
	for index, step := range steps {
		err := step.run(ctx)
		if err != nil {
			var waiting *pendingError
			reason := "Retrying"
			if errors.As(err, &waiting) {
				reason = "Pending"
			}
			if step.reason != nil {
				if custom := step.reason(err); custom != "" {
					reason = custom
				}
			}
			st.setCondition(step.name, false, reason, err.Error())
			if step.nonBlocking {
				held = held || step.holdsReady
				continue
			}
			for _, dependent := range steps[index+1:] {
				if dependent.name != step.name {
					st.setCondition(dependent.name, false, "DependencyPending", "Waiting for "+step.name)
				}
			}
			st.setPhase("Pending")
			if e := st.updateStatus(ctx); e != nil {
				return ctrl.Result{}, e
			}
			if waiting != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("%s: %w", step.name, err)
		}
		if !step.partial {
			message := "Desired state verified"
			if step.message != nil {
				message = step.message()
			}
			st.setCondition(step.name, true, "Reconciled", message)
		}
	}
	if held {
		st.setPhase("Pending")
		if err := st.updateStatus(ctx); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	st.setPhase(st.readyPhase())
	return ctrl.Result{RequeueAfter: time.Minute}, st.updateStatus(ctx)
}
func setCondition(o *api.Organization, t string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: t, Status: status, ObservedGeneration: o.Generation, Reason: reason, Message: msg})
}
