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
	"text/template"

	api "github.com/tym83/aep/api/v1alpha1"
	resourcetypes "github.com/tym83/aep/resource-types"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ControlPlaneReconciler owns ControlPlane resources. This operator version
// only validates them and reports the result; it creates nothing.
type ControlPlaneReconciler struct {
	client.Client
}

func (r *ControlPlaneReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		For(&api.ControlPlane{}).
		Named("controlplane").
		Complete(r)
}

func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cp := &api.ControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	before := cp.Status.DeepCopy()
	evaluateControlPlane(cp)
	if reflect.DeepEqual(before, &cp.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Update(ctx, cp)
}

// evaluateControlPlane records the validation result in status.
func evaluateControlPlane(cp *api.ControlPlane) {
	cp.Status.ObservedGeneration = cp.Generation
	switch err := ValidateControlPlane(cp); {
	case cp.Spec.Suspend:
		cp.Status.Phase = "Suspended"
		setControlPlaneCondition(cp, "Ready", false, "Suspended", "Reconciliation is suspended")
	case err != nil:
		cp.Status.Phase = "Rejected"
		setControlPlaneCondition(cp, "Ready", false, "InvalidSpec", err.Error())
	default:
		cp.Status.Phase = "Pending"
		setControlPlaneCondition(cp, "Ready", false, "NotImplemented", "Spec is valid; this operator version does not install ControlPlanes yet")
	}
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
