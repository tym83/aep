package operator

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
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
	spec := M{"endpointSelector": tierSelector(s.o), "egress": []any{
		M{"toEndpoints": []any{M{"matchLabels": M{"k8s:io.kubernetes.pod.namespace": "tenant-root", "k8s:app.kubernetes.io/instance": "etcd"}}}, "toPorts": ports("2379")},
		M{"toEndpoints": []any{M{"matchLabels": M{"k8s:io.kubernetes.pod.namespace": "tenant-root", "cozystack.io/service": "ingress"}}}}}}
	return s.r.Management.apply(ctx, s.o, "ciliumclusterwidenetworkpolicies", obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", "aep-"+s.o.Name+"-root-egress", spec))
}
func RenderNetwork(o *api.Organization, svc *corev1.Service, cluster string) ([]*unstructured.Unstructured, error) {
	port := int32(0)
	for _, p := range svc.Spec.Ports {
		if p.Port == 8443 && p.Protocol == corev1.ProtocolTCP {
			port = p.TargetPort.IntVal
		}
	}
	if port <= 0 || svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return nil, fmt.Errorf("gateway mirror requires numeric targetPort on TCP 8443 and ClusterIP")
	}
	// KubeVirt's cloud controller mirrors tenant LoadBalancer Services without
	// a selector and manages the endpoints itself; the owning cluster is
	// recorded as a label. Fall back to the selector for other providers.
	selected := svc.Labels["cluster.x-k8s.io/cluster-name"]
	if selected == "" {
		selected = svc.Spec.Selector["cluster.x-k8s.io/cluster-name"]
	}
	if selected != "kubernetes-"+cluster {
		return nil, fmt.Errorf("gateway mirror VM selector does not match configured aep-cp cluster")
	}
	vm := M{"matchLabels": M{"k8s:io.kubernetes.pod.namespace": svc.Namespace, "k8s:cluster.x-k8s.io/cluster-name": selected}}
	p := ports(strconv.Itoa(int(port)))
	prefix := "aep-" + o.Name + "-"
	return []*unstructured.Unstructured{
		obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", prefix+"agent-egress", M{"endpointSelector": tierSelector(o), "egress": []any{M{"toEndpoints": []any{vm}, "toPorts": p}}}),
		obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", prefix+"agent-ingress", M{"endpointSelector": vm, "ingress": []any{M{"fromEndpoints": []any{tierSelector(o)}, "toPorts": p}}})}, nil
}
func (s *session) controlPlane(ctx context.Context) error {
	if s.cp != nil {
		return nil
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
	tmpl, e := template.New("host").Option("missingkey=error").Parse(s.r.Options.GatewayHostTemplate)
	if e != nil {
		return e
	}
	var b bytes.Buffer
	if e = tmpl.Execute(&b, map[string]string{"ClusterIP": svc.Spec.ClusterIP, "DashedIP": strings.ReplaceAll(svc.Spec.ClusterIP, ".", "-")}); e != nil {
		return e
	}
	host := b.String()
	s.gateway = "wss://" + host + ":8443/ws"
	if e = s.controlPlane(ctx); e != nil {
		return e
	}
	// Follow the actual gateway Deployment volume, including externally supplied certificates.
	deployments, e := s.cp.Kube.AppsV1().Deployments("openchoreo-control-plane").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=cluster-gateway"})
	if e != nil {
		return e
	}
	for _, deployment := range deployments.Items {
		for _, volume := range deployment.Spec.Template.Spec.Volumes {
			if volume.Name != "server-certs" || volume.Secret == nil {
				continue
			}
			sec, e := s.cp.Kube.CoreV1().Secrets(deployment.Namespace).Get(ctx, volume.Secret.SecretName, metav1.GetOptions{})
			if e != nil {
				return e
			}
			block, _ := pem.Decode(sec.Data["tls.crt"])
			if block == nil {
				continue
			}
			cert, e := x509.ParseCertificate(block.Bytes)
			if e == nil && cert.VerifyHostname(host) == nil && time.Now().Before(cert.NotAfter) && time.Now().After(cert.NotBefore) {
				return nil
			}
		}
	}

	return pending("add %s to aep-cp clusterGateway.tls.dnsNames, reconcile its chart and restart cluster-gateway", host)
}
