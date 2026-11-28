package operator

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	resourcetypes "github.com/tym83/aep/resource-types"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ControlPlaneReconciler installs and manages ControlPlane resources: the
// Cozystack tenant and cluster of an aep-cp, the operator identity in it, the
// OpenChoreo control plane chart and its management ingress.
type ControlPlaneReconciler struct {
	client.Client
	Management *Cluster
	Options    Options
	Recorder   record.EventRecorder
	// Connect builds the clients of an aep-cp REST config; nil uses NewCluster.
	Connect func(*rest.Config) (*Cluster, error)

	mu    sync.Mutex
	state map[types.UID]*controlPlaneMemory
}

// controlPlaneMemory is the per-ControlPlane state that only times
// remediation; losing it on restart merely restarts the timers.
type controlPlaneMemory struct {
	readySince   time.Time
	remediations map[string]time.Time
}

// ControlPlaneFinalizer orders the teardown of an installed control plane.
const ControlPlaneFinalizer = "aep.aenix.io/control-plane-cleanup"

// DeletePortalDatabaseAnnotation set to "true" lets deletion remove the portal database.
const DeletePortalDatabaseAnnotation = "aep.aenix.io/delete-portal-database"

// controlPlaneConditionTypes are the conditions of the implemented steps.
var controlPlaneConditionTypes = []string{"TenantReady", "NetworkPathsReady", "ClusterReady", "AccessReady", "PrerequisitesReady", "OpenChoreoReady", "GatewayReady", "IngressReady", "Hardened"}

func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cp := &api.ControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cp.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, cp)
	}
	before := cp.Status.DeepCopy()
	if !evaluateControlPlane(cp) {
		return ctrl.Result{}, r.writeStatus(ctx, cp, before)
	}
	collision, err := r.organizationCollision(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if collision != "" {
		cp.Status.Phase = "Rejected"
		setControlPlaneCondition(cp, "Ready", false, "InvalidSpec", collision)
		return ctrl.Result{RequeueAfter: time.Minute}, r.writeStatus(ctx, cp, before)
	}
	if !slices.Contains(cp.Finalizers, ControlPlaneFinalizer) {
		base := cp.DeepCopy()
		cp.Finalizers = append(cp.Finalizers, ControlPlaneFinalizer)
		if err := r.Patch(ctx, cp, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}
	s := r.session(cp)
	return runReconcileSteps(ctx, controlPlaneStatus{s: s, before: before}, s.steps())
}

// organizationCollision reports Organizations whose tenant namespaces
// include the tenant namespace of cp, or "". Installing there would mix the
// control plane with an Organization's tenant.
func (r *ControlPlaneReconciler) organizationCollision(ctx context.Context, cp *api.ControlPlane) (string, error) {
	var organizations api.OrganizationList
	if err := r.List(ctx, &organizations); err != nil {
		return "", err
	}
	var collisions []string
	for i := range organizations.Items {
		if c := namespaceCollision(cp, &organizations.Items[i]); c != "" {
			collisions = append(collisions, c)
		}
	}
	return strings.Join(collisions, "; "), nil
}

func (r *ControlPlaneReconciler) writeStatus(ctx context.Context, cp *api.ControlPlane, before *api.ControlPlaneStatus) error {
	if reflect.DeepEqual(before, &cp.Status) {
		return nil
	}
	return r.Status().Update(ctx, cp)
}

// evaluateControlPlane records validation and suspension in status and
// reports whether the control plane is to be reconciled.
func evaluateControlPlane(cp *api.ControlPlane) bool {
	cp.Status.ObservedGeneration = cp.Generation
	switch err := ValidateControlPlane(cp); {
	case err != nil:
		cp.Status.Phase = "Rejected"
		setControlPlaneCondition(cp, "Ready", false, "InvalidSpec", err.Error())
	case cp.Spec.Suspend:
		// Observe only: nothing is applied or deleted, conditions keep their last state.
		cp.Status.Phase = "Suspended"
		setControlPlaneCondition(cp, "Ready", false, "Suspended", "Reconciliation is suspended")
	default:
		return true
	}
	return false
}

func (r *ControlPlaneReconciler) memory(uid types.UID) *controlPlaneMemory {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = map[types.UID]*controlPlaneMemory{}
	}
	m := r.state[uid]
	if m == nil {
		m = &controlPlaneMemory{remediations: map[string]time.Time{}}
		r.state[uid] = m
	}
	return m
}

func (r *ControlPlaneReconciler) connect(cfg *rest.Config) (*Cluster, error) {
	if r.Connect != nil {
		return r.Connect(cfg)
	}
	return NewCluster(cfg)
}

// controlPlaneSession is one reconcile pass of a ControlPlane.
type controlPlaneSession struct {
	r     *ControlPlaneReconciler
	cp    *api.ControlPlane
	owner ownerKey
	// ns is the Cozystack tenant namespace holding the cluster.
	ns string
	// remote is the operator identity on aep-cp, once bootstrapped.
	remote *Cluster
	// gatewayHost is the cluster-gateway host placed in its certificate;
	// empty until it is known.
	gatewayHost string
	// hardenReason is the reason of the last Hardened failure.
	hardenReason string
}

func (r *ControlPlaneReconciler) session(cp *api.ControlPlane) *controlPlaneSession {
	s := &controlPlaneSession{r: r, cp: cp, owner: controlPlaneOwner(cp), ns: ControlPlaneNamespace(cp)}
	s.gatewayHost = orDefault(cp.Spec.ClusterGateway.Host, cp.Status.Gateway.Host)
	cp.Status.Outputs.TenantNamespace = s.ns
	cp.Status.Outputs.ClusterName = cp.Spec.Cluster.Name
	cp.Status.Versions = map[string]string{ControlPlaneChart: ControlPlaneChartVersion}
	return s
}

// steps lists the control plane reconcile steps in dependency order.
// Content (ContentReady) and audit (AuditReady) are not implemented yet and
// do not hold Ready.
func (s *controlPlaneSession) steps() []reconcileStep {
	return []reconcileStep{
		{name: "TenantReady", run: s.tenant},
		// Root etcd must be reachable before the cluster can become Ready.
		{name: "NetworkPathsReady", run: s.networkPaths},
		{name: "ClusterReady", run: s.cluster},
		{name: "AccessReady", run: s.access},
		{name: "PrerequisitesReady", run: s.prerequisites},
		{name: "OpenChoreoReady", run: s.openChoreo},
		{name: "GatewayReady", run: s.gateway, message: func() string { return s.cp.Status.Gateway.URL }},
		{name: "IngressReady", run: s.ingress, message: func() string { return s.cp.Status.Endpoints.Console }},
		{name: "Hardened", run: s.harden, reason: func(error) string { return s.hardenReason }},
	}
}

// controlPlaneStatus adapts a ControlPlane to the step runner.
type controlPlaneStatus struct {
	s      *controlPlaneSession
	before *api.ControlPlaneStatus
}

func (c controlPlaneStatus) conditionTypes() []string       { return controlPlaneConditionTypes }
func (c controlPlaneStatus) conditions() []metav1.Condition { return c.s.cp.Status.Conditions }
func (c controlPlaneStatus) generation() int64              { return c.s.cp.Generation }
func (c controlPlaneStatus) setPhase(phase string)          { c.s.cp.Status.Phase = phase }
func (c controlPlaneStatus) readyPhase() string             { return "Ready" }
func (c controlPlaneStatus) setCondition(t string, ok bool, reason, msg string) {
	setControlPlaneCondition(c.s.cp, t, ok, reason, msg)
}

// updateStatus derives Ready from the step conditions and writes status
// only when it changed.
func (c controlPlaneStatus) updateStatus(ctx context.Context) error {
	cp := c.s.cp
	setControlPlaneCondition(cp, "ContentReady", false, "NotImplemented", "Content delivery is not implemented in this operator version; Ready does not wait for it")
	if cp.Spec.Audit == nil {
		setControlPlaneCondition(cp, "AuditReady", true, "Disabled", "Audit is not configured")
	} else {
		setControlPlaneCondition(cp, "AuditReady", false, "NotImplemented", "Audit provisioning is not implemented in this operator version; Ready does not wait for it")
	}
	if cp.Status.Phase == "Ready" {
		setControlPlaneCondition(cp, "Ready", true, "Reconciled", "Control plane is ready")
	} else {
		reason, message := "Pending", "Waiting for dependencies"
		for _, t := range controlPlaneConditionTypes {
			if cond := meta.FindStatusCondition(cp.Status.Conditions, t); cond != nil && cond.Status != metav1.ConditionTrue {
				reason, message = cond.Reason, t+": "+cond.Message
				break
			}
		}
		setControlPlaneCondition(cp, "Ready", false, reason, message)
	}
	return c.s.r.writeStatus(ctx, cp, c.before)
}

func setControlPlaneCondition(cp *api.ControlPlane, t string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{Type: t, Status: status, ObservedGeneration: cp.Generation, Reason: reason, Message: msg})
}

// ControlPlaneNamespace is the Cozystack tenant namespace of the control plane.
func ControlPlaneNamespace(cp *api.ControlPlane) string {
	if cp.Spec.Tenant.Parent == "tenant-root" {
		return "tenant-" + cp.Spec.Tenant.Name
	}
	return cp.Spec.Tenant.Parent + "-" + cp.Spec.Tenant.Name
}

var tenantName = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// ValidateControlPlane repeats the CRD rules, so a stale CRD cannot admit an
// unsafe spec, and adds checks the schema cannot express.
func ValidateControlPlane(cp *api.ControlPlane) error {
	s := cp.Spec
	if len(validation.IsDNS1123Label(cp.Name)) > 0 || len(cp.Name) > 40 {
		return fmt.Errorf("name must be a DNS label of at most 40 characters")
	}
	if !strings.HasPrefix(s.Tenant.Parent, "tenant-") || len(validation.IsDNS1123Label(s.Tenant.Parent)) > 0 {
		return fmt.Errorf("tenant.parent must be a tenant namespace")
	}
	// Cozystack joins tenant names with dashes, so a dash would be ambiguous.
	if !tenantName.MatchString(s.Tenant.Name) {
		return fmt.Errorf("tenant.name must be 1-32 lowercase alphanumeric characters")
	}
	if ns := ControlPlaneNamespace(cp); len(validation.IsDNS1123Label(ns)) > 0 {
		return fmt.Errorf("tenant namespace %s is not a valid namespace name", ns)
	}
	for k, v := range s.Tenant.Quotas {
		if _, err := resource.ParseQuantity(v); err != nil {
			return fmt.Errorf("tenant quota %s: %w", k, err)
		}
	}
	c := s.Cluster
	if len(validation.IsDNS1123Label(c.Name)) > 0 || len(c.Name) > 40 {
		return fmt.Errorf("cluster.name must be a DNS label of at most 40 characters")
	}
	if !strings.HasPrefix(c.Version, "v1.") || c.StorageClass == "" {
		return fmt.Errorf("cluster version and storageClass are required")
	}
	n := c.NodeGroup
	if n.InstanceType == "" || n.MinReplicas < 1 || n.MaxReplicas < n.MinReplicas {
		return fmt.Errorf("cluster.nodeGroup needs an instanceType and 1 <= minReplicas <= maxReplicas")
	}
	if err := validateSize("cluster.nodeGroup.diskSize", n.DiskSize); err != nil {
		return err
	}
	if !strings.Contains(s.Domain, ".") {
		return fmt.Errorf("domain must be a DNS name with at least two labels")
	}
	for _, host := range []string{"console." + s.Domain, "api." + s.Domain} {
		if !dnsName(host) {
			return fmt.Errorf("domain does not form a valid host name %s", host)
		}
	}
	if (s.Ingress.ClusterIssuer == "") == (s.Ingress.TLSSecretRef == nil) {
		return fmt.Errorf("ingress requires exactly one of clusterIssuer or tlsSecretRef")
	}
	if s.Ingress.ClassName == "" || (s.Ingress.TLSSecretRef != nil && s.Ingress.TLSSecretRef.Name == "") {
		return fmt.Errorf("ingress className and tlsSecretRef name must not be empty")
	}
	if err := validateOIDC(s.OIDC); err != nil {
		return err
	}
	for field, value := range map[string]string{"portal.readAPIURL": s.Portal.ReadAPIURL, "portal.changeAPIURL": s.Portal.ChangeAPIURL} {
		if value != "" && !absoluteURL(value, "http", "https") {
			return fmt.Errorf("%s must be an absolute http(s) URL", field)
		}
	}
	if err := validateSize("portal.database.size", s.Portal.Database.Size); err != nil {
		return err
	}
	if s.Portal.Database.Replicas < 1 {
		return fmt.Errorf("portal.database.replicas must be at least 1")
	}
	if h := s.ClusterGateway.Host; h != "" && (!dnsName(h) || !strings.Contains(h, ".")) {
		return fmt.Errorf("clusterGateway.host must be a DNS name")
	}
	if err := validateContent(s.Content); err != nil {
		return err
	}
	if a := s.Audit; a != nil {
		if ns, name, ok := strings.Cut(a.ForwarderService, "/"); !ok || len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1123Label(name)) > 0 {
			return fmt.Errorf("audit.forwarderService must be namespace/name")
		}
	}
	if b := s.Backup; b != nil && len(validation.IsDNS1123Subdomain(b.ReaderKubeconfigSecret)) > 0 {
		return fmt.Errorf("backup.readerKubeconfigSecret must be a Secret name")
	}
	if a := s.Adoption; a != nil && a.Confirm != ControlPlaneNamespace(cp)+"/"+c.Name {
		return fmt.Errorf("adoption.confirm must be %s/%s", ControlPlaneNamespace(cp), c.Name)
	}
	return nil
}

func validateOIDC(o api.ControlPlaneOIDC) error {
	if !absoluteURL(o.Issuer, "https") {
		return fmt.Errorf("oidc.issuer must be an absolute https URL")
	}
	for field, value := range map[string]string{"jwksURL": o.JWKSURL, "authorizationURL": o.AuthorizationURL, "tokenURL": o.TokenURL, "wellKnownURL": o.WellKnownURL} {
		if value != "" && !absoluteURL(value, "https") {
			return fmt.Errorf("oidc.%s must be an absolute https URL", field)
		}
	}
	if o.UsernameClaim == "" || o.CLIClientID == "" || o.PortalClient.ClientID == "" {
		return fmt.Errorf("oidc usernameClaim, cliClientID and portalClient.clientID must not be empty")
	}
	if len(validation.IsDNS1123Subdomain(o.PortalClient.SecretRef.Name)) > 0 {
		return fmt.Errorf("oidc.portalClient.secretRef must name a Secret")
	}
	if o.CABundleSecretRef != nil && len(validation.IsDNS1123Subdomain(o.CABundleSecretRef.Name)) > 0 {
		return fmt.Errorf("oidc.caBundleSecretRef must name a Secret")
	}
	return nil
}

func validateContent(c api.ControlPlaneContent) error {
	known, err := EmbeddedResourceTypes()
	if err != nil {
		return err
	}
	for _, name := range c.ResourceTypes {
		if !slices.Contains(known, name) {
			return fmt.Errorf("content.resourceTypes: unknown resource type %s", name)
		}
	}
	if b := c.Builds; b != nil {
		if b.RegistryHost == "" {
			return fmt.Errorf("content.builds.registryHost is required")
		}
		if _, err := template.New("publisher").Option("missingkey=error").Parse(b.PublisherClientIDTemplate); err != nil {
			return fmt.Errorf("content.builds.publisherClientIDTemplate: %w", err)
		}
	}
	return nil
}

// EmbeddedResourceTypes lists the ClusterResourceType names shipped with the operator.
func EmbeddedResourceTypes() ([]string, error) {
	files, err := fs.Glob(resourcetypes.Files, "*.yaml")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, strings.TrimSuffix(f, ".yaml"))
	}
	return names, nil
}

// dnsName reports a lower-case host name whose labels are DNS labels.
func dnsName(host string) bool {
	if len(validation.IsDNS1123Subdomain(host)) > 0 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(validation.IsDNS1123Label(label)) > 0 {
			return false
		}
	}
	return true
}

func validateSize(field, value string) error {
	q, err := resource.ParseQuantity(value)
	if err != nil || q.Sign() <= 0 {
		return fmt.Errorf("%s must be a positive quantity", field)
	}
	return nil
}

func absoluteURL(value string, schemes ...string) bool {
	u, err := url.Parse(value)
	return err == nil && slices.Contains(schemes, u.Scheme) && u.Host != ""
}
