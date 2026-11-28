package operator

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	"strconv"
	"strings"
	"text/template"
	"time"
)

func tierSelector(o *api.Organization) M {
	ns := []any{}
	for _, t := range Tiers(o) {
		ns = append(ns, TenantNamespace(o.Name, t))
	}
	return M{"matchExpressions": []any{M{"key": "k8s:io.kubernetes.pod.namespace", "operator": "In", "values": ns}}}
}
func ports(port string) []any { return []any{M{"ports": []any{M{"port": port, "protocol": "TCP"}}}} }
func (s *session) workaround(ctx context.Context) error {
	if !s.r.Options.TenantEgressWorkaround {
		return s.deleteNamed(ctx, s.r.Management, "cilium.io/v2", "ciliumclusterwidenetworkpolicies", "", "aep-"+s.o.Name+"-root-egress")
	}
	return s.r.Management.apply(ctx, s.o, "ciliumclusterwidenetworkpolicies", obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", "aep-"+s.o.Name+"-root-egress", rootEgressSpec(tierSelector(s.o))))
}

// rootEgressSpec lets the selected tenant pods reach root etcd and the root
// ingress until cozystack#5442 ships.
func rootEgressSpec(selector M) M {
	return M{"endpointSelector": selector, "egress": []any{
		M{"toEndpoints": []any{M{"matchLabels": M{"k8s:io.kubernetes.pod.namespace": "tenant-root", "k8s:app.kubernetes.io/instance": "etcd"}}}, "toPorts": ports("2379")},
		M{"toEndpoints": []any{M{"matchLabels": M{"k8s:io.kubernetes.pod.namespace": "tenant-root", "cozystack.io/service": "ingress"}}}}}}
}
func RenderNetwork(o *api.Organization, svc *corev1.Service, cluster string) ([]*unstructured.Unstructured, error) {
	port, selected, err := gatewayMirror(svc, cluster)
	if err != nil {
		return nil, err
	}
	vm := M{"matchLabels": M{"k8s:io.kubernetes.pod.namespace": svc.Namespace, "k8s:cluster.x-k8s.io/cluster-name": selected}}
	p := ports(strconv.Itoa(int(port)))
	prefix := "aep-" + o.Name + "-"
	return []*unstructured.Unstructured{
		obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", prefix+"agent-egress", M{"endpointSelector": tierSelector(o), "egress": []any{M{"toEndpoints": []any{vm}, "toPorts": p}}}),
		obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", prefix+"agent-ingress", M{"endpointSelector": vm, "ingress": []any{M{"fromEndpoints": []any{tierSelector(o)}, "toPorts": p}}})}, nil
}

// gatewayMirror validates the management mirror of the cluster-gateway
// LoadBalancer Service of aep-cp cluster and returns its node port and the
// cluster label of the VMs behind it.
func gatewayMirror(svc *corev1.Service, cluster string) (int32, string, error) {
	port := int32(0)
	for _, p := range svc.Spec.Ports {
		if p.Port == 8443 && p.Protocol == corev1.ProtocolTCP {
			port = p.TargetPort.IntVal
		}
	}
	if port <= 0 || svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return 0, "", fmt.Errorf("gateway mirror requires numeric targetPort on TCP 8443 and ClusterIP")
	}
	selected := mirroredCluster(svc)
	if selected != "kubernetes-"+cluster {
		return 0, "", fmt.Errorf("gateway mirror VM selector does not match configured aep-cp cluster")
	}
	return port, selected, nil
}

// mirroredCluster is the cluster behind a management mirror Service.
// KubeVirt's cloud controller mirrors tenant LoadBalancer Services without
// a selector and manages the endpoints itself; the owning cluster is
// recorded as a label. Fall back to the selector for other providers.
func mirroredCluster(svc *corev1.Service) string {
	if selected := svc.Labels["cluster.x-k8s.io/cluster-name"]; selected != "" {
		return selected
	}
	return svc.Spec.Selector["cluster.x-k8s.io/cluster-name"]
}

func (s *session) controlPlane(ctx context.Context) error {
	if s.cp != nil {
		return nil
	}
	if s.r.Options.CPFile == "" && s.r.Options.CPSecret == "" {
		// Only --default-control-plane allows this; Organizations cannot use a
		// ControlPlane resource yet.
		return fmt.Errorf("no legacy aep-cp kubeconfig configured and ControlPlane resolution is not implemented yet")
	}
	var e error
	if s.r.Options.CPFile != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", s.r.Options.CPFile)
		if err != nil {
			return err
		}
		s.cp, e = NewCluster(cfg)
	} else {
		s.cp, e = FromSecret(ctx, s.r.Management, s.r.Options.CPSecret)
	}
	return e
}
func (s *session) network(ctx context.Context) error {
	svcs, e := s.r.Management.Kube.CoreV1().Services(s.r.Options.CPNamespace).List(ctx, metav1.ListOptions{LabelSelector: "cluster.x-k8s.io/tenant-service-name=cluster-gateway-external"})
	if e != nil {
		return e
	}
	if len(svcs.Items) != 1 {
		return pending("expected exactly one gateway mirror Service in %s", s.r.Options.CPNamespace)
	}
	svc := &svcs.Items[0]
	policies, e := RenderNetwork(s.o, svc, s.r.Options.CPCluster)
	if e != nil {
		return e
	}
	for _, u := range policies {
		if e := s.r.Management.apply(ctx, s.o, "ciliumclusterwidenetworkpolicies", u); e != nil {
			return e
		}
	}
	host, e := gatewayHost(s.r.Options.GatewayHostTemplate, svc.Spec.ClusterIP)
	if e != nil {
		return e
	}
	s.gateway = "wss://" + host + ":8443/ws"
	if e = s.controlPlane(ctx); e != nil {
		return e
	}
	cert, _, e := gatewayCertificate(ctx, s.cp, host)
	if e != nil {
		return e
	}
	if cert != nil {
		return nil
	}

	return pending("add %s to aep-cp clusterGateway.tls.dnsNames, reconcile its chart and restart cluster-gateway", host)
}

// gatewayHost renders the cluster-gateway host template for a mirror ClusterIP.
func gatewayHost(hostTemplate, clusterIP string) (string, error) {
	tmpl, e := template.New("host").Option("missingkey=error").Parse(hostTemplate)
	if e != nil {
		return "", e
	}
	var b bytes.Buffer
	if e = tmpl.Execute(&b, map[string]string{"ClusterIP": clusterIP, "DashedIP": strings.ReplaceAll(clusterIP, ".", "-")}); e != nil {
		return "", e
	}
	return b.String(), nil
}

// gatewayCertificate follows the actual cluster-gateway Deployment volume,
// including externally supplied certificates, and returns the first currently
// valid certificate for host with its Deployment, or nil when none is.
func gatewayCertificate(ctx context.Context, c *Cluster, host string) (*x509.Certificate, *appsv1.Deployment, error) {
	deployments, e := c.Kube.AppsV1().Deployments("openchoreo-control-plane").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=cluster-gateway"})
	if e != nil {
		return nil, nil, e
	}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		for _, volume := range deployment.Spec.Template.Spec.Volumes {
			if volume.Name != "server-certs" || volume.Secret == nil {
				continue
			}
			sec, e := c.Kube.CoreV1().Secrets(deployment.Namespace).Get(ctx, volume.Secret.SecretName, metav1.GetOptions{})
			if e != nil {
				return nil, nil, e
			}
			block, _ := pem.Decode(sec.Data["tls.crt"])
			if block == nil {
				continue
			}
			cert, e := x509.ParseCertificate(block.Bytes)
			if e == nil && cert.VerifyHostname(host) == nil && time.Now().Before(cert.NotAfter) && time.Now().After(cert.NotBefore) {
				return cert, deployment, nil
			}
		}
	}
	return nil, nil, nil
}
