package operator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/version"
	inventory "github.com/tym83/aep/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ServerCertSerialAnnotation records on the cluster-gateway pod template the
// serial of the certificate it was restarted for.
const ServerCertSerialAnnotation = "aep.aenix.io/server-cert-serial"

// authzBootstrapName is the chart's authorization bootstrap hook Job and
// ServiceAccount (fullnameOverride openchoreo).
const authzBootstrapName = "openchoreo-authz-bootstrap"

const portalDatabase = "portal"

// controlPlaneChart returns the delivery chart name retargeted for aep-cp:
// every aep-cp chart is released in the control plane chart namespace.
func controlPlaneChart(name string) DeliveryChart {
	for _, c := range charts {
		if c.Name == name {
			c.Namespace = OpenChoreoNamespace
			return c
		}
	}
	panic("unknown delivery chart " + name)
}

func releaseName(chart string) string { return "aep-cp-" + chart }

// applyChart applies the OCIRepository and HelmRelease of ch in the tenant
// namespace, installing into aep-cp after the release of chart after.
func (s *controlPlaneSession) applyChart(ctx context.Context, ch DeliveryChart, after string, values M) error {
	name := releaseName(ch.Name)
	source := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", s.ns, name, M{"interval": "5m", "url": ch.URL, "ref": M{"tag": ch.Version}})
	if e := s.r.Options.chartSource(source); e != nil {
		return e
	}
	if e := s.r.Management.applyAs(ctx, s.owner, "ocirepositories", source); e != nil {
		return e
	}
	dependsOn := ""
	if after != "" {
		dependsOn = releaseName(after)
	}
	hr := fluxRelease(s.ns, name, s.cp.Spec.Cluster.Name, ch, dependsOn, s.r.Options.chartValues(ch.Name, ch.Namespace, values))
	if ch.Name == ControlPlaneChart {
		// The authorization bootstrap hook alone may take ten minutes.
		hr.Object["spec"].(M)["timeout"] = "20m"
	}
	return s.r.Management.applyAs(ctx, s.owner, "helmreleases", hr)
}

// releaseReady waits for a delivered HelmRelease to be Ready for its generation.
func (s *controlPlaneSession) releaseReady(ctx context.Context, chart string) error {
	hr, e := s.r.Management.get(ctx, helmAPI, "helmreleases", s.ns, releaseName(chart))
	if e != nil {
		return e
	}
	if !ready(hr) {
		if msg := failureMessage(hr); msg != "" {
			return pending("HelmRelease %s/%s is not Ready: %s", s.ns, hr.GetName(), msg)
		}
		return pending("waiting for HelmRelease %s/%s Ready", s.ns, hr.GetName())
	}
	return nil
}

// prerequisiteCharts install kgateway for the control plane gateway.
var prerequisiteCharts = []string{"kgateway-crds", "kgateway"}

func (s *controlPlaneSession) prerequisites(ctx context.Context) error {
	remote, e := s.identity(ctx)
	if e != nil {
		return e
	}
	if e = copyMirrorPull(ctx, s.r.Options, s.r.Management, remote, s.owner, []string{OpenChoreoNamespace}); e != nil {
		return s.contractDrift(ctx, e)
	}
	if e = copyRegistrySecrets(ctx, s.r.Options, s.r.Management, s.owner, s.r.Options.Namespace, s.ns); e != nil {
		return e
	}
	for i, name := range prerequisiteCharts {
		after := ""
		if i > 0 {
			after = prerequisiteCharts[i-1]
		}
		if e = s.applyChart(ctx, controlPlaneChart(name), after, M{}); e != nil {
			return e
		}
	}
	if e = applyGatewayParameters(ctx, s.r.Options, remote, s.owner, OpenChoreoNamespace); e != nil {
		if _, waiting := e.(*pendingError); waiting {
			return e
		}
		return s.contractDrift(ctx, e)
	}
	for _, name := range prerequisiteCharts {
		if e = s.releaseReady(ctx, name); e != nil {
			return e
		}
	}
	return nil
}

// portalTag splits the portal image into repository and tag, defaulting to
// the aep-portal image of this operator release.
func (s *controlPlaneSession) portalImage() (string, string, error) {
	ref := s.cp.Spec.Portal.Image
	if ref == "" {
		pinned, err := inventory.Image(PortalImage + ":" + version.Version)
		if err != nil {
			return "", "", err
		}
		ref = pinned
	}
	repo, digest, _ := strings.Cut(ref, "@")
	tag := ""
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo, tag = repo[:i], repo[i+1:]
	}
	if digest != "" {
		tag += "@" + digest
	}
	if tag == "" {
		return "", "", fmt.Errorf("portal.image %s needs a tag or digest", ref)
	}
	return repo, tag, nil
}

// settings are the chart inputs of this control plane.
func (s *controlPlaneSession) settings() (ControlPlaneSettings, error) {
	spec := s.cp.Spec
	repo, tag, err := s.portalImage()
	if err != nil {
		return ControlPlaneSettings{}, err
	}
	o := spec.OIDC
	return ControlPlaneSettings{Domain: spec.Domain, Issuer: o.Issuer, JWKSURL: o.JWKSURL, AuthorizationURL: o.AuthorizationURL, TokenURL: o.TokenURL, WellKnownURL: o.WellKnownURL,
		UsernameClaim: o.UsernameClaim, CLIClientID: o.CLIClientID, PortalClientID: o.PortalClient.ClientID, GatewayHost: s.gatewayHost,
		PortalRepository: repo, PortalTag: tag, ReadAPIURL: spec.Portal.ReadAPIURL, ChangeAPIURL: spec.Portal.ChangeAPIURL, OIDCCA: o.CABundleSecretRef != nil}, nil
}

// applyOpenChoreo applies the control plane chart release.
func (s *controlPlaneSession) applyOpenChoreo(ctx context.Context) error {
	settings, e := s.settings()
	if e != nil {
		return e
	}
	return s.applyChart(ctx, controlPlaneChart(ControlPlaneChart), "kgateway", ControlPlaneValues(settings))
}

// operatorSecret reads key of a Secret in the operator namespace; key defaults to fallback.
func (s *controlPlaneSession) operatorSecret(ctx context.Context, name, key, fallback string) ([]byte, error) {
	key = orDefault(key, fallback)
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return nil, pending("waiting for Secret %s/%s", s.r.Options.Namespace, name)
	}
	if e != nil {
		return nil, e
	}
	if len(sec.Data[key]) == 0 {
		return nil, pending("Secret %s/%s has no key %s", s.r.Options.Namespace, name, key)
	}
	return sec.Data[key], nil
}

func (s *controlPlaneSession) openChoreo(ctx context.Context) error {
	remote, e := s.identity(ctx)
	if e != nil {
		return e
	}
	if e = s.portalDatabase(ctx, remote); e != nil {
		return e
	}
	if e = s.portalSecrets(ctx, remote); e != nil {
		return e
	}
	if e = s.applyOpenChoreo(ctx); e != nil {
		return e
	}
	if e = s.authzBootstrapPullSecret(ctx, remote); e != nil {
		return s.contractDrift(ctx, e)
	}
	return s.releaseReady(ctx, ControlPlaneChart)
}

// portalDatabase provisions the Cozystack Postgres of the portal and makes it
// reachable on aep-cp as Service aep-portal-db, a selectorless Service whose
// EndpointSlice points at the management ClusterIP of the database.
func (s *controlPlaneSession) portalDatabase(ctx context.Context, remote *Cluster) error {
	db := s.cp.Spec.Portal.Database
	pg := obj(appsAPI, "Postgres", s.ns, portalDatabase, M{"replicas": int64(db.Replicas), "size": db.Size, "resourcesPreset": db.ResourcesPreset,
		"users": M{portalDatabase: M{}}, "databases": M{portalDatabase: M{"roles": M{"admin": []any{portalDatabase}}}}})
	if e := s.r.Management.applyAs(ctx, s.owner, "postgreses", pg); e != nil {
		return e
	}
	got, e := s.r.Management.get(ctx, appsAPI, "postgreses", s.ns, portalDatabase)
	if e != nil {
		return e
	}
	if !ready(got) {
		return pending("waiting for Postgres %s/%s Ready", s.ns, portalDatabase)
	}
	m := kinds.Table["Postgres"]
	creds, e := s.r.Management.Kube.CoreV1().Secrets(s.ns).Get(ctx, m.Secret(portalDatabase), metav1.GetOptions{})
	if apierrors.IsNotFound(e) || (e == nil && len(creds.Data[portalDatabase]) == 0) {
		return pending("waiting for portal database credentials %s/%s", s.ns, m.Secret(portalDatabase))
	}
	if e != nil {
		return e
	}
	svc, e := s.r.Management.Kube.CoreV1().Services(s.ns).Get(ctx, m.Service(portalDatabase), metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return pending("waiting for Service %s/%s", s.ns, m.Service(portalDatabase))
	}
	if e != nil {
		return e
	}
	ip := net.ParseIP(svc.Spec.ClusterIP)
	if ip == nil {
		return fmt.Errorf("portal database Service %s/%s has no routable ClusterIP", s.ns, svc.Name)
	}
	addressType := "IPv4"
	if ip.To4() == nil {
		addressType = "IPv6"
	}
	secret := secretObject(OpenChoreoNamespace, PortalDatabaseSecret, map[string][]byte{"user": []byte(portalDatabase), "password": creds.Data[portalDatabase], "database": []byte(portalDatabase)})
	port := M{"name": "postgres", "port": int64(m.Port), "protocol": "TCP"}
	service := obj("v1", "Service", OpenChoreoNamespace, PortalDatabaseService, M{"type": "ClusterIP", "ports": []any{M{"name": "postgres", "port": int64(m.Port), "protocol": "TCP", "targetPort": int64(m.Port)}}})
	slice := obj("discovery.k8s.io/v1", "EndpointSlice", OpenChoreoNamespace, PortalDatabaseService, nil)
	slice.SetLabels(map[string]string{"kubernetes.io/service-name": PortalDatabaseService, "endpointslice.kubernetes.io/managed-by": "aep-operator"})
	slice.Object["addressType"] = addressType
	slice.Object["ports"] = []any{port}
	slice.Object["endpoints"] = []any{M{"addresses": []any{svc.Spec.ClusterIP}, "conditions": M{"ready": true}}}
	for _, item := range []struct {
		resource string
		u        *unstructured.Unstructured
	}{{"secrets", secret}, {"services", service}, {"endpointslices", slice}} {
		if e = remote.applyAs(ctx, s.owner, item.resource, item.u); e != nil {
			return s.contractDrift(ctx, e)
		}
	}
	return nil
}

// portalSecrets delivers Secret backstage-secrets: a session secret generated
// once and kept, the portal client secret and an unused Jenkins key the
// chart requires, and the optional IdP CA bundle.
func (s *controlPlaneSession) portalSecrets(ctx context.Context, remote *Cluster) error {
	o := s.cp.Spec.OIDC
	clientSecret, e := s.operatorSecret(ctx, o.PortalClient.SecretRef.Name, o.PortalClient.SecretRef.Key, "client-secret")
	if e != nil {
		return e
	}
	backend := []byte(nil)
	existing, e := remote.get(ctx, "v1", "secrets", OpenChoreoNamespace, PortalSecret)
	switch {
	case e == nil && existing.GetLabels()[ControlPlaneLabel] == string(s.cp.UID):
		backend = secretValue(existing, "backend-secret")
	case e != nil && !apierrors.IsNotFound(e):
		return s.contractDrift(ctx, e)
	}
	if len(backend) == 0 {
		random := make([]byte, 32)
		if _, e = rand.Read(random); e != nil {
			return e
		}
		backend = []byte(hex.EncodeToString(random))
	}
	secret := secretObject(OpenChoreoNamespace, PortalSecret, map[string][]byte{"backend-secret": backend, "client-secret": clientSecret, "jenkins-api-key": []byte("unused")})
	if e = remote.applyAs(ctx, s.owner, "secrets", secret); e != nil {
		return s.contractDrift(ctx, e)
	}
	if ref := o.CABundleSecretRef; ref != nil {
		ca, e := s.operatorSecret(ctx, ref.Name, ref.Key, "ca.crt")
		if e != nil {
			return e
		}
		cm := obj("v1", "ConfigMap", OpenChoreoNamespace, OIDCCAConfigMap, nil)
		cm.Object["data"] = M{"ca.crt": string(ca)}
		if e = remote.applyAs(ctx, s.owner, "configmaps", cm); e != nil {
			return s.contractDrift(ctx, e)
		}
	}
	return nil
}

func hasPullSecret(refs []corev1.LocalObjectReference) bool {
	return slices.ContainsFunc(refs, func(r corev1.LocalObjectReference) bool { return r.Name == MirrorPullSecret })
}

// authzBootstrapPullSecret lets the chart's authorization bootstrap hook pull
// its kubectl image from an authenticated mirror. The hook Job sets no
// imagePullSecrets and Helm recreates its ServiceAccount on every hook run,
// so the operator adds the mirror pull secret to that ServiceAccount and
// deletes, once, each hook pod admitted before the patch that cannot pull;
// the Job then recreates it with the secret.
func (s *controlPlaneSession) authzBootstrapPullSecret(ctx context.Context, remote *Cluster) error {
	if !s.r.Options.mirrorPull() {
		return nil
	}
	accounts := remote.Kube.CoreV1().ServiceAccounts(OpenChoreoNamespace)
	sa, e := accounts.Get(ctx, authzBootstrapName, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		return nil
	}
	if !hasPullSecret(sa.ImagePullSecrets) {
		patch, e := json.Marshal(M{"imagePullSecrets": append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: MirrorPullSecret})})
		if e != nil {
			return e
		}
		if _, e = accounts.Patch(ctx, sa.Name, types.MergePatchType, patch, metav1.PatchOptions{}); e != nil {
			return e
		}
	}
	pods, e := remote.Kube.CoreV1().Pods(OpenChoreoNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=authz-bootstrap"})
	if e != nil {
		return e
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if hasPullSecret(pod.Spec.ImagePullSecrets) || !pullFailing(pod) {
			continue
		}
		uid := pod.UID
		if e = remote.Kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); client.IgnoreNotFound(e) != nil {
			return e
		}
		if s.r.Recorder != nil {
			s.r.Recorder.Eventf(s.cp, "Normal", "AuthzBootstrapPodReplaced", "Deleted authorization bootstrap pod %s that could not pull from the mirror", pod.Name)
		}
	}
	return nil
}

func pullFailing(pod *corev1.Pod) bool {
	for _, c := range pod.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
			return true
		}
	}
	return false
}

func (s *controlPlaneSession) gateway(ctx context.Context) error {
	remote, e := s.identity(ctx)
	if e != nil {
		return e
	}
	svcs, e := s.r.Management.Kube.CoreV1().Services(s.ns).List(ctx, metav1.ListOptions{LabelSelector: "cluster.x-k8s.io/tenant-service-name=cluster-gateway-external"})
	if e != nil {
		return e
	}
	if len(svcs.Items) != 1 {
		return pending("expected exactly one gateway mirror Service in %s", s.ns)
	}
	svc := &svcs.Items[0]
	port, _, e := gatewayMirror(svc, s.cp.Spec.Cluster.Name)
	if e != nil {
		return e
	}
	host := s.cp.Spec.ClusterGateway.Host
	if host == "" {
		if host, e = gatewayHost(s.r.Options.GatewayHostTemplate, svc.Spec.ClusterIP); e != nil {
			return e
		}
	}
	g := &s.cp.Status.Gateway
	g.Host, g.URL, g.MirrorService, g.TargetPort = host, "wss://"+host+":8443/ws", svc.Namespace+"/"+svc.Name, port
	if host != s.gatewayHost {
		s.gatewayHost = host
		if e = s.applyOpenChoreo(ctx); e != nil {
			return e
		}
		return pending("waiting for the control plane chart to add %s to the cluster-gateway certificate", host)
	}
	if e = s.releaseReady(ctx, ControlPlaneChart); e != nil {
		return e
	}
	cert, deployment, e := gatewayCertificate(ctx, remote, host)
	if e != nil {
		return s.contractDrift(ctx, e)
	}
	if cert == nil {
		return pending("waiting for a cluster-gateway certificate valid for %s", host)
	}
	serial := cert.SerialNumber.Text(16)
	if g.ServedCertSerial == serial {
		return nil
	}
	if deployment.Spec.Template.Annotations[ServerCertSerialAnnotation] == serial {
		if !rolledOut(deployment) {
			return pending("waiting for cluster-gateway to restart with certificate %s", serial)
		}
		g.ServedCertSerial = serial
		return nil
	}
	started, e := podsStartedAfter(ctx, remote, deployment, cert.NotBefore)
	if e != nil {
		return s.contractDrift(ctx, e)
	}
	if started {
		g.ServedCertSerial = serial
		return nil
	}
	// One restart per serial: the annotation records the serial it was for.
	patch, e := json.Marshal(M{"spec": M{"template": M{"metadata": M{"annotations": M{ServerCertSerialAnnotation: serial}}}}})
	if e != nil {
		return e
	}
	if _, e = remote.Kube.AppsV1().Deployments(deployment.Namespace).Patch(ctx, deployment.Name, types.MergePatchType, patch, metav1.PatchOptions{}); e != nil {
		return s.contractDrift(ctx, e)
	}
	if s.r.Recorder != nil {
		s.r.Recorder.Eventf(s.cp, "Normal", "GatewayRestarted", "Restarted cluster-gateway to serve certificate %s for %s", serial, host)
	}
	return pending("restarting cluster-gateway to serve certificate %s", serial)
}

func rolledOut(d *appsv1.Deployment) bool {
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	st := d.Status
	return st.ObservedGeneration >= d.Generation && st.UpdatedReplicas >= replicas && st.Replicas == st.UpdatedReplicas && st.AvailableReplicas >= 1
}

// podsStartedAfter reports whether every pod of d started after t, so they
// already loaded a certificate issued at t.
func podsStartedAfter(ctx context.Context, c *Cluster, d *appsv1.Deployment, t time.Time) (bool, error) {
	if d.Spec.Selector == nil {
		return false, nil
	}
	pods, e := c.Kube.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set(d.Spec.Selector.MatchLabels).String()})
	if e != nil {
		return false, e
	}
	if len(pods.Items) == 0 {
		return false, nil
	}
	for _, pod := range pods.Items {
		if pod.Status.StartTime == nil || !pod.Status.StartTime.After(t) {
			return false, nil
		}
	}
	return true, nil
}

// gatewayDefaultMirror finds the management mirror Service of the control
// plane's gateway-default Service.
func (s *controlPlaneSession) gatewayDefaultMirror(ctx context.Context) (*corev1.Service, error) {
	svcs, e := s.r.Management.Kube.CoreV1().Services(s.ns).List(ctx, metav1.ListOptions{LabelSelector: "cluster.x-k8s.io/tenant-service-name=gateway-default"})
	if e != nil {
		return nil, e
	}
	var found []*corev1.Service
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if ns := svc.Labels["cluster.x-k8s.io/tenant-service-namespace"]; ns != "" && ns != OpenChoreoNamespace {
			continue
		}
		if mirroredCluster(svc) != "kubernetes-"+s.cp.Spec.Cluster.Name {
			continue
		}
		found = append(found, svc)
	}
	if len(found) != 1 {
		return nil, pending("expected exactly one mirror Service of gateway-default in %s, found %d", s.ns, len(found))
	}
	if !slices.ContainsFunc(found[0].Spec.Ports, func(p corev1.ServicePort) bool { return p.Port == 80 }) {
		return nil, fmt.Errorf("gateway-default mirror Service %s/%s has no port 80", s.ns, found[0].Name)
	}
	return found[0], nil
}

func (s *controlPlaneSession) publicTLSSecret() string {
	if ref := s.cp.Spec.Ingress.TLSSecretRef; ref != nil {
		return ref.Name
	}
	return "aep-cp-public-tls"
}

// Ingresses publish the control plane through the management ingress.
const (
	PublicIngress     = "aep-cp-public"
	DenyLogsIngress   = "aep-cp-deny-direct-logs"
	directLogsPattern = "/api/v1/namespaces/[^/]+/releasebindings/[^/]+/k8sresources/logs"
)

// controlPlaneIngresses render the public entry of the control plane: TLS
// terminates on the management ingress and plain HTTP goes to the gateway
// mirror. OpenChoreo authorizes its pod log route with releasebinding:view
// and no environment, so the route is denied for everyone at the public
// entry; pod logs are served by the AEP read API instead.
func controlPlaneIngresses(spec ControlPlaneIngressSettings) []*unstructured.Unstructured {
	backend := M{"service": M{"name": spec.Service, "port": M{"number": int64(80)}}}
	path := []any{M{"path": "/", "pathType": "Prefix", "backend": backend}}
	public := obj("networking.k8s.io/v1", "Ingress", spec.Namespace, PublicIngress, M{"ingressClassName": spec.ClassName,
		"tls":   []any{M{"hosts": []any{spec.Console, spec.API}, "secretName": spec.TLSSecret}},
		"rules": []any{M{"host": spec.Console, "http": M{"paths": path}}, M{"host": spec.API, "http": M{"paths": path}}}})
	annotations := map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": "16m", "nginx.ingress.kubernetes.io/proxy-read-timeout": "3600", "nginx.ingress.kubernetes.io/proxy-send-timeout": "3600"}
	if spec.ClusterIssuer != "" {
		annotations["cert-manager.io/cluster-issuer"] = spec.ClusterIssuer
	}
	public.SetAnnotations(annotations)
	deny := obj("networking.k8s.io/v1", "Ingress", spec.Namespace, DenyLogsIngress, M{"ingressClassName": spec.ClassName,
		"rules": []any{M{"host": spec.API, "http": M{"paths": []any{M{"path": directLogsPattern, "pathType": "ImplementationSpecific", "backend": backend}}}}}})
	deny.SetAnnotations(map[string]string{"nginx.ingress.kubernetes.io/use-regex": "true", "nginx.ingress.kubernetes.io/denylist-source-range": "0.0.0.0/0,::/0"})
	return []*unstructured.Unstructured{public, deny}
}

// ControlPlaneIngressSettings are the inputs of controlPlaneIngresses.
type ControlPlaneIngressSettings struct {
	Namespace, Service, ClassName, ClusterIssuer, TLSSecret, Console, API string
}

func (s *controlPlaneSession) ingress(ctx context.Context) error {
	mirror, e := s.gatewayDefaultMirror(ctx)
	if e != nil {
		return e
	}
	console, apiHost := "console."+s.cp.Spec.Domain, "api."+s.cp.Spec.Domain
	settings := ControlPlaneIngressSettings{Namespace: s.ns, Service: mirror.Name, ClassName: s.cp.Spec.Ingress.ClassName, ClusterIssuer: s.cp.Spec.Ingress.ClusterIssuer, TLSSecret: s.publicTLSSecret(), Console: console, API: apiHost}
	for _, u := range controlPlaneIngresses(settings) {
		if e = s.r.Management.applyAs(ctx, s.owner, "ingresses", u); e != nil {
			return e
		}
	}
	s.cp.Status.Endpoints = api.ControlPlaneEndpoints{Console: "https://" + console, API: "https://" + apiHost, APIHost: apiHost, InternalAPI: "http://" + mirror.Name + "." + s.ns + ".svc"}
	sec, e := s.r.Management.Kube.CoreV1().Secrets(s.ns).Get(ctx, settings.TLSSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(e) || (e == nil && len(sec.Data["tls.crt"]) == 0) {
		return pending("waiting for TLS Secret %s/%s", s.ns, settings.TLSSecret)
	}
	return e
}

func (s *controlPlaneSession) harden(ctx context.Context) error {
	s.hardenReason = "InspectionFailed"
	remote, e := s.identity(ctx)
	if e != nil {
		return pending("%v", e)
	}
	// The control plane's own portal client holds the catalog reader binding.
	allowed := strings.TrimPrefix(s.r.Options.BootstrapBindingSystemClients+","+s.cp.Spec.OIDC.PortalClient.ClientID, ",")
	ok, reason, message := hardenBootstrapBindings(ctx, remote, s.r.Options.PruneBootstrapBindings, allowed, s.r.Recorder, s.cp)
	if !ok {
		s.hardenReason = reason
		return pending("%s", message)
	}
	return nil
}
