package main

import (
	"flag"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/operator"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	r := &operator.Reconciler{}
	o := &r.Options
	flag.StringVar(&o.ArgoCDHostTemplate, "argocd-host-template", "", "Argo CD ingress hostname template with Organization and Tier")
	flag.StringVar(&o.VCSWebhookURL, "vcs-webhook-url", "", "OpenChoreo HTTPS /api/v1alpha1/autobuild endpoint; empty disables hook registration")
	flag.StringVar(&o.PolicyBundleRepository, "policy-bundle-repository", "oci://ghcr.io/tym83/aep/policies", "OCI repository containing the versioned AEP policy charts")
	flag.StringVar(&o.KyvernoChartURL, "kyverno-chart-url", "oci://ghcr.io/kyverno/charts/kyverno", "Kyverno OCI chart URL; override for air-gap mirror")
	var mgmt string
	flag.StringVar(&mgmt, "mgmt-kubeconfig", "", "Management kubeconfig; default in-cluster")
	flag.StringVar(&o.Namespace, "namespace", "aep-system", "Operator namespace")
	flag.StringVar(&o.CPSecret, "aepcp-kubeconfig-secret", "", "Management namespace/name Secret, key super-admin.svc")
	flag.StringVar(&o.CPFile, "aepcp-kubeconfig", "", "Local control-plane kubeconfig")
	flag.StringVar(&o.CPNamespace, "aepcp-tenant-namespace", "tenant-aep-cp", "Management namespace containing the aep-cp cluster")
	flag.StringVar(&o.CPCluster, "aepcp-cluster", "aepcp", "Cozystack aep-cp cluster name")
	flag.StringVar(&o.GatewayHostTemplate, "gateway-host-template", "cluster-gateway.{{.DashedIP}}.nip.io", "Go template with ClusterIP and DashedIP")
	flag.BoolVar(&o.TenantEgressWorkaround, "tenant-egress-workaround", true, "Allow root etcd and ingress until cozystack#5442 ships")
	flag.StringVar(&o.BridgeImage, "bridge-image", "", "Bridge container image (required)")
	flag.BoolVar(&o.PruneBootstrapBindings, "prune-bootstrap-bindings", true, "Remove chart-managed cluster-wide bootstrap authorization bindings")
	flag.StringVar(&o.BootstrapBindingSystemClients, "bootstrap-binding-system-clients", operator.DefaultBootstrapBindingSystemClients, "Comma-separated client IDs exempt from bootstrap binding pruning; empty exempts none")
	flag.BoolVar(&o.KeycloakEnabled, "keycloak-enabled", false, "Create groups in an existing Keycloak realm")
	flag.StringVar(&o.KeycloakURL, "keycloak-url", "", "Keycloak HTTPS URL")
	flag.StringVar(&o.KeycloakRealm, "keycloak-realm", "", "Existing isolated realm")
	flag.StringVar(&o.KeycloakCredentialsSecret, "keycloak-credentials-secret", "", "namespace/name with username and password")
	flag.BoolVar(&o.RestoreVerification, "restore-verification", false, "Enable monthly same-namespace prod restore checks")
	logOpts := zap.Options{}
	logOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOpts)))
	if (o.CPSecret == "") == (o.CPFile == "") {
		return fmt.Errorf("specify exactly one aep-cp kubeconfig source")
	}
	if o.BridgeImage == "" {
		return fmt.Errorf("bridge-image required")
	}
	if o.KeycloakEnabled && (o.KeycloakURL == "" || o.KeycloakRealm == "" || o.KeycloakCredentialsSecret == "") {
		return fmt.Errorf("Keycloak URL, realm and credentials required")
	}
	var cfg *rest.Config
	var e error
	if mgmt != "" {
		cfg, e = clientcmd.BuildConfigFromFlags("", mgmt)
	} else {
		cfg, e = rest.InClusterConfig()
	}
	if e != nil {
		return e
	}
	r.Management, e = operator.NewCluster(cfg)
	if e != nil {
		return e
	}
	scheme := runtime.NewScheme()
	if e = api.AddToScheme(scheme); e != nil {
		return e
	}
	mgr, e := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionNamespace: o.Namespace, LeaderElectionID: "aep-operator", Metrics: metrics.Options{BindAddress: "0"}, HealthProbeBindAddress: ":8081"})
	if e != nil {
		return e
	}
	r.Client = mgr.GetClient()
	r.Recorder = mgr.GetEventRecorderFor("aep-operator")
	if e = r.SetupWithManager(mgr); e != nil {
		return e
	}
	if e = mgr.AddHealthzCheck("healthz", healthz.Ping); e != nil {
		return e
	}
	if e = mgr.AddReadyzCheck("readyz", healthz.Ping); e != nil {
		return e
	}
	if o.RestoreVerification {
		if e = mgr.Add(&operator.RestoreVerifier{Reconciler: r}); e != nil {
			return e
		}
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
