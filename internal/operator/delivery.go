package operator

import (
	"context"
	"github.com/tym83/aep/internal/kinds"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// StrimziChart is the Kafka operator chart; Kafka claims run on it.
const StrimziChart = "strimzi-kafka-operator"

var charts = []struct{ name, version, url, namespace string }{
	{"kgateway-crds", "v2.3.11", "oci://cr.kgateway.dev/kgateway-dev/charts", "openchoreo-data-plane"},
	{"kgateway", "v2.3.11", "oci://cr.kgateway.dev/kgateway-dev/charts", "openchoreo-data-plane"},
	{"openchoreo-data-plane", "1.4.0-m.3", "oci://cr.openchoreo.dev/openchoreo/helm-charts", "openchoreo-data-plane"},
	{StrimziChart, "0.50.0", "oci://quay.io/strimzi-helm", kinds.KafkaNamespace},
}

func (s *session) delivery(ctx context.Context) error {
	var waiting error
	for _, tier := range Tiers(s.o) {
		c, e := s.bootstrap(ctx, tier)
		if e != nil {
			return e
		}
		data, e := s.ca(ctx, tier)
		if e != nil {
			return e
		}
		if e = s.deliverMirrorPull(ctx, c); e != nil {
			return s.contractDrift(ctx, tier, e)
		}
		secret := secretObject("openchoreo-data-plane", "aep-agent-ca", data)
		secret.Object["type"] = "kubernetes.io/tls"
		if e = c.apply(ctx, s.o, "secrets", secret); e != nil {
			return e
		}
		gatewayCA, e := s.cp.Kube.CoreV1().Secrets("openchoreo-control-plane").Get(ctx, "cluster-gateway-ca", metav1.GetOptions{})
		if e != nil {
			return e
		}
		if len(gatewayCA.Data["ca.crt"]) == 0 {
			return pending("gateway CA Secret has no ca.crt")
		}
		cm := obj("v1", "ConfigMap", "openchoreo-data-plane", "cluster-gateway-ca", nil)
		cm.Object["data"] = M{"ca.crt": string(gatewayCA.Data["ca.crt"])}
		if e = c.apply(ctx, s.o, "configmaps", cm); e != nil {
			return e
		}
		ns := TenantNamespace(s.o.Name, tier)
		if err := s.mirrorSecrets(ctx, ns); err != nil {
			return err
		}
		for i, ch := range charts {
			source := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", ns, "aep-"+ch.name, M{"interval": "5m", "url": ch.url + "/" + ch.name, "ref": M{"tag": ch.version}})
			if err := s.r.Options.chartSource(source); err != nil {
				return err
			}
			if e = s.r.Management.apply(ctx, s.o, "ocirepositories", source); e != nil {
				return e
			}
			values := M{}
			switch ch.name {
			case "openchoreo-data-plane":
				values = DataPlaneValues(s.o.Name+"-"+tier, s.gateway)
			case StrimziChart:
				values = StrimziValues()
			}
			hr := deliveryRelease(ns, i, s.chartValues(ch.name, values))
			if e = s.r.Management.apply(ctx, s.o, "helmreleases", hr); e != nil {
				return e
			}
		}
		if e = s.deliverGatewayParameters(ctx, c); e != nil {
			if _, ok := e.(*pendingError); !ok {
				return s.contractDrift(ctx, tier, e)
			}
			waiting = e
		}
	}
	return waiting
}

// deliveryRelease is the tier HelmRelease of charts[i] in tenant namespace ns.
func deliveryRelease(ns string, i int, values M) *unstructured.Unstructured {
	ch := charts[i]
	spec := M{"interval": "5m", "timeout": "10m", "releaseName": ch.name, "targetNamespace": ch.namespace, "storageNamespace": ch.namespace, "chartRef": M{"kind": "OCIRepository", "name": "aep-" + ch.name}, "kubeConfig": M{"secretRef": M{"name": "kubernetes-apps-admin-kubeconfig", "key": "super-admin.svc"}}, "install": M{"createNamespace": true, "remediation": M{"retries": int64(-1)}}, "upgrade": M{"remediation": M{"retries": int64(-1)}}, "values": values}
	if i > 0 {
		spec["dependsOn"] = []any{M{"name": "aep-" + charts[i-1].name}}
	}
	if ch.name == StrimziChart {
		// Helm never upgrades crds/; keep Strimzi CRDs in step with the operator.
		spec["install"].(M)["crds"] = "CreateReplace"
		spec["upgrade"].(M)["crds"] = "CreateReplace"
	}
	hr := obj(helmAPI, "HelmRelease", ns, "aep-"+ch.name, spec)
	hr.SetLabels(map[string]string{"cozystack.io/target-cluster-name": "kubernetes-apps", "internal.cozystack.io/tenantresource": "true", "sharding.fluxcd.io/key": "tenants"})
	return hr
}
func (s *session) connected(ctx context.Context) error {
	for _, tier := range Tiers(s.o) {
		for _, ch := range charts {
			hr, e := s.r.Management.get(ctx, helmAPI, "helmreleases", TenantNamespace(s.o.Name, tier), "aep-"+ch.name)
			if e != nil {
				return e
			}
			if !ready(hr) {
				return pending("waiting for %s %s Ready", tier, ch.name)
			}
		}
		dp, e := s.cp.get(ctx, ocAPI, "dataplanes", s.o.Name, tier)
		if e != nil {
			return e
		}
		connected, _, _ := unstructured.NestedBool(dp.Object, "status", "agentConnection", "connected")
		if !connected {
			return pending("waiting for %s agent connection", tier)
		}
	}
	return nil
}
