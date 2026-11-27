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
		for _, t := range conditionTypes {
			setCondition(o, t, false, "InvalidSpec", e.Error())
		}
		o.Status.Phase = "Rejected"
		return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, o)
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

// steps lists the organization reconcile steps in dependency order.
func (s *session) steps() []reconcileStep {
	return []reconcileStep{
		{name: "TenantsReady", run: s.tenants},
		// Root etcd must be reachable before a control plane can become Ready.
		{name: "NetworkPathsReady", run: s.workaround, partial: true},
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
}

// Policy delivery is observable independently of organization access.
func (r *Reconciler) runSteps(ctx context.Context, o *api.Organization, steps []reconcileStep) (ctrl.Result, error) {
	o.Status.Phase = "Reconciling"
	for _, t := range conditionTypes {
		if c := meta.FindStatusCondition(o.Status.Conditions, t); c == nil || c.ObservedGeneration != o.Generation {
			setCondition(o, t, false, "Pending", "Waiting for dependencies")
		}
	}
	var policyErr error
	for index, step := range steps {
		err := step.run(ctx)
		if err != nil {
			var waiting *pendingError
			reason := "Retrying"
			if errors.As(err, &waiting) {
				reason = "Pending"
			}
			setCondition(o, step.name, false, reason, err.Error())
			if step.name == "VCSReady" || step.name == "ArgoCDReady" {
				continue
			}
			if step.name == "PoliciesReady" {
				policyErr = err
				if errors.Is(err, errCosignKeyMissing) {
					setCondition(o, step.name, false, "CosignKeyMissing", err.Error())
				}
				continue
			}
			for _, dependent := range steps[index+1:] {
				if dependent.name != step.name {
					setCondition(o, dependent.name, false, "DependencyPending", "Waiting for "+step.name)
				}
			}
			o.Status.Phase = "Pending"
			if e := r.Status().Update(ctx, o); e != nil {
				return ctrl.Result{}, e
			}
			if waiting != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("%s: %w", step.name, err)
		}
		if !step.partial {
			message := "Desired state verified"
			if step.name == "ArgoCDReady" {
				if o.Status.ArgoCDURL != "" {
					message = o.Status.ArgoCDURL
				} else {
					message = "Argo CD disabled"
				}
			}
			if step.name == "VCSReady" && o.Spec.VCS == nil {
				message = "Corporate VCS not configured"
			}
			setCondition(o, step.name, true, "Reconciled", message)
		}
	}
	if policyErr != nil {
		o.Status.Phase = "Pending"
		if err := r.Status().Update(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	o.Status.Phase = "Ready"
	if !meta.IsStatusConditionTrue(o.Status.Conditions, "ControlPlaneHardened") {
		o.Status.Phase = "Pending"
	}
	return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, o)
}
func setCondition(o *api.Organization, t string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: t, Status: status, ObservedGeneration: o.Generation, Reason: reason, Message: msg})
}
