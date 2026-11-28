package operator

import (
	"context"
	"github.com/tym83/aep/internal/kinds"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// StrimziChart is the Kafka operator chart; Kafka claims run on it.
const StrimziChart = "strimzi-kafka-operator"

// ChartTarget is the kind of cluster a delivered chart is installed on.
type ChartTarget string

const (
	// TargetTier charts run in every organization tier cluster.
	TargetTier ChartTarget = "tier"
	// TargetControlPlane charts run on an OpenChoreo control plane (aep-cp).
	TargetControlPlane ChartTarget = "cp"
	// TargetFleet charts run on external clusters joining the fleet.
	TargetFleet ChartTarget = "fleet"
)

// LayerSelector selects the Helm chart layer of an OCI artifact that also
// carries other layers; Flux otherwise takes the first layer.
type LayerSelector struct{ MediaType, Operation string }

// DeliveryChart is one pinned upstream chart: where it comes from and where it
// is installed.
type DeliveryChart struct {
	Name, Version string
	// URL is the full OCI chart reference without tag or digest.
	URL       string
	Namespace string
	Target    ChartTarget
	// LayerSelector is set only for artifacts with layers besides the chart.
	LayerSelector *LayerSelector
}

var helmChartLayer = &LayerSelector{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Operation: "copy"}

// charts are delivered as generic Flux releases. Tier charts install in table
// order, each depending on the previous one.
var charts = []DeliveryChart{
	{Name: "kgateway-crds", Version: "v2.3.11", URL: "oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds", Namespace: "openchoreo-data-plane", Target: TargetTier},
	{Name: "kgateway", Version: "v2.3.11", URL: "oci://cr.kgateway.dev/kgateway-dev/charts/kgateway", Namespace: "openchoreo-data-plane", Target: TargetTier},
	{Name: "openchoreo-data-plane", Version: "1.4.0-m.3", URL: "oci://cr.openchoreo.dev/openchoreo/helm-charts/openchoreo-data-plane", Namespace: "openchoreo-data-plane", Target: TargetTier},
	{Name: StrimziChart, Version: "0.50.0", URL: "oci://quay.io/strimzi-helm/" + StrimziChart, Namespace: kinds.KafkaNamespace, Target: TargetTier},
	{Name: ControlPlaneChart, Version: ControlPlaneChartVersion, URL: "oci://cr.openchoreo.dev/openchoreo/helm-charts/" + ControlPlaneChart, Namespace: "openchoreo-control-plane", Target: TargetControlPlane},
	{Name: ExternalSecretsChart, Version: ExternalSecretsChartVersion, URL: "oci://ghcr.io/external-secrets/charts/" + ExternalSecretsChart, Namespace: "external-secrets", Target: TargetControlPlane, LayerSelector: helmChartLayer},
}

// chartsFor returns the generic delivery charts of one target in table order.
func chartsFor(target ChartTarget) []DeliveryChart {
	out := []DeliveryChart{}
	for _, c := range charts {
		if c.Target == target {
			out = append(out, c)
		}
	}
	return out
}

// tierCharts are the charts every tier receives through delivery.
var tierCharts = chartsFor(TargetTier)

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
		for i, ch := range tierCharts {
			source := obj("source.toolkit.fluxcd.io/v1", "OCIRepository", ns, "aep-"+ch.Name, M{"interval": "5m", "url": ch.URL, "ref": M{"tag": ch.Version}})
			if err := s.r.Options.chartSource(source); err != nil {
				return err
			}
			if e = s.r.Management.apply(ctx, s.o, "ocirepositories", source); e != nil {
				return e
			}
			values := M{}
			switch ch.Name {
			case "openchoreo-data-plane":
				values = DataPlaneValues(s.o.Name+"-"+tier, s.gateway)
			case StrimziChart:
				values = StrimziValues()
			}
			after := ""
			if i > 0 {
				after = tierCharts[i-1].Name
			}
			hr := deliveryRelease(ns, ch, after, s.chartValues(ch.Name, ch.Namespace, values))
			if e = s.r.Management.apply(ctx, s.o, "helmreleases", hr); e != nil {
				return e
			}
		}
		if e = s.deliverGatewayParameters(ctx, c, "openchoreo-data-plane"); e != nil {
			if _, ok := e.(*pendingError); !ok {
				return s.contractDrift(ctx, tier, e)
			}
			waiting = e
		}
	}
	return waiting
}

// deliveryRelease is the tier HelmRelease of ch in tenant namespace ns; after
// names the chart it depends on, if any.
func deliveryRelease(ns string, ch DeliveryChart, after string, values M) *unstructured.Unstructured {
	spec := M{"interval": "5m", "timeout": "10m", "releaseName": ch.Name, "targetNamespace": ch.Namespace, "storageNamespace": ch.Namespace, "chartRef": M{"kind": "OCIRepository", "name": "aep-" + ch.Name}, "kubeConfig": M{"secretRef": M{"name": "kubernetes-apps-admin-kubeconfig", "key": "super-admin.svc"}}, "install": M{"createNamespace": true, "remediation": M{"retries": int64(-1)}}, "upgrade": M{"remediation": M{"retries": int64(-1)}}, "values": values}
	if after != "" {
		spec["dependsOn"] = []any{M{"name": "aep-" + after}}
	}
	if ch.Name == StrimziChart {
		// Helm never upgrades crds/; keep Strimzi CRDs in step with the operator.
		spec["install"].(M)["crds"] = "CreateReplace"
		spec["upgrade"].(M)["crds"] = "CreateReplace"
	}
	hr := obj(helmAPI, "HelmRelease", ns, "aep-"+ch.Name, spec)
	hr.SetLabels(map[string]string{"cozystack.io/target-cluster-name": "kubernetes-apps", "internal.cozystack.io/tenantresource": "true", "sharding.fluxcd.io/key": "tenants"})
	return hr
}
func (s *session) connected(ctx context.Context) error {
	for _, tier := range Tiers(s.o) {
		for _, ch := range tierCharts {
			hr, e := s.r.Management.get(ctx, helmAPI, "helmreleases", TenantNamespace(s.o.Name, tier), "aep-"+ch.Name)
			if e != nil {
				return e
			}
			if !ready(hr) {
				return pending("waiting for %s %s Ready", tier, ch.Name)
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
