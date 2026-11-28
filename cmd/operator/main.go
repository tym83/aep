package main

import (
	"flag"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/operator"
	"github.com/tym83/aep/internal/version"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"strings"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

// settings are the parsed command line beyond the reconciler options.
type settings struct {
	mgmt, fleetHub, fleetCharts, fleetGateway string
	logOpts                                   zap.Options
}

func run() error {
	r, cli, e := parseFlags(flag.CommandLine, os.Args[1:])
	if e != nil {
		return e
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cli.logOpts)))
	return start(r, cli)
}

// parseFlags parses and validates the operator command line.
func parseFlags(fs *flag.FlagSet, args []string) (*operator.Reconciler, *settings, error) {
	r := &operator.Reconciler{}
	o := &r.Options
	cli := &settings{}
	fs.StringVar(&o.ArgoCDChartURL, "argocd-chart-url", "https://argoproj.github.io/argo-helm", "Argo CD Helm repository mirror URL")
	fs.StringVar(&o.ArgoCDHostTemplate, "argocd-host-template", "", "Argo CD ingress hostname template with Organization and Tier")
	fs.StringVar(&o.ArgoCDACMEServer, "argocd-acme-server", "", "ACME directory URL for the Argo CD host certificate in the tier cluster; empty uses the ingress default certificate")
	fs.StringVar(&o.VCSWebhookURL, "vcs-webhook-url", "", "OpenChoreo HTTPS /api/v1alpha1/autobuild endpoint; empty disables hook registration")
	fs.StringVar(&o.ImageRegistryMirror, "image-registry-mirror", "", "Mirror registry prefix for every delivered image")
	fs.StringVar(&o.ChartRegistryMirror, "chart-registry-mirror", "", "Mirror registry prefix for OCI charts")
	fs.BoolVar(&o.RegistryInsecure, "registry-insecure", false, "Use HTTP for Flux OCI sources on the configured chart mirror")
	fs.StringVar(&o.RegistrySecret, "registry-secret", "", "Flux authentication Secret in each source namespace")
	fs.StringVar(&o.RegistryCASecret, "registry-ca-secret", "", "Flux CA Secret in each source namespace")
	fs.StringVar(&o.PolicyBundleRepository, "policy-bundle-repository", "oci://ghcr.io/tym83/aep/policies", "OCI repository containing the versioned AEP policy charts")
	fs.StringVar(&o.KyvernoChartURL, "kyverno-chart-url", "oci://ghcr.io/kyverno/charts/kyverno", "Kyverno OCI chart URL; override for air-gap mirror")
	fs.StringVar(&cli.fleetHub, "fleet-hub-config", "", "OCM hub config; empty uses management")
	fs.StringVar(&cli.fleetCharts, "fleet-chart-dir", "", "Enable fleet using locally staged pinned charts")
	fs.StringVar(&cli.fleetGateway, "fleet-gateway-url", "", "Externally reachable wss:// OpenChoreo gateway")
	fs.StringVar(&cli.mgmt, "mgmt-kubeconfig", "", "Management kubeconfig; default in-cluster")
	fs.StringVar(&o.Namespace, "namespace", "aep-system", "Operator namespace")
	fs.StringVar(&o.CPSecret, "aepcp-kubeconfig-secret", "", "Management namespace/name Secret, key super-admin.svc")
	fs.StringVar(&o.CPFile, "aepcp-kubeconfig", "", "Local control-plane kubeconfig")
	fs.StringVar(&o.DefaultControlPlane, "default-control-plane", "", "ControlPlane resource serving Organizations without controlPlaneRef; when set, the legacy aep-cp kubeconfig flags may be omitted")
	fs.StringVar(&o.CPNamespace, "aepcp-tenant-namespace", "tenant-aep-cp", "Management namespace containing the aep-cp cluster")
	fs.StringVar(&o.CPCluster, "aepcp-cluster", "aepcp", "Cozystack aep-cp cluster name")
	fs.StringVar(&o.GatewayHostTemplate, "gateway-host-template", "cluster-gateway.{{.DashedIP}}.nip.io", "Go template with ClusterIP and DashedIP")
	fs.BoolVar(&o.TenantEgressWorkaround, "tenant-egress-workaround", true, "Allow root etcd and ingress until cozystack#5442 ships")
	fs.StringVar(&o.BridgeImage, "bridge-image", "ghcr.io/tym83/aep-bridge:"+version.Version, "Bridge container image (required)")
	fs.BoolVar(&o.PruneBootstrapBindings, "prune-bootstrap-bindings", true, "Remove chart-managed cluster-wide bootstrap authorization bindings")
	fs.StringVar(&o.BootstrapBindingSystemClients, "bootstrap-binding-system-clients", operator.DefaultBootstrapBindingSystemClients, "Comma-separated client IDs exempt from bootstrap binding pruning; empty exempts none")
	fs.BoolVar(&o.KeycloakEnabled, "keycloak-enabled", false, "Create groups in an existing Keycloak realm")
	fs.StringVar(&o.KeycloakURL, "keycloak-url", "", "Keycloak HTTPS URL")
	fs.StringVar(&o.KeycloakRealm, "keycloak-realm", "", "Existing isolated realm")
	fs.StringVar(&o.KeycloakCredentialsSecret, "keycloak-credentials-secret", "", "namespace/name with username and password")
	fs.BoolVar(&o.RestoreVerification, "restore-verification", false, "Enable monthly same-namespace prod restore checks")
	cli.logOpts.BindFlags(fs)
	if e := fs.Parse(args); e != nil {
		return nil, nil, e
	}
	if o.CPSecret != "" && o.CPFile != "" {
		return nil, nil, fmt.Errorf("specify at most one aep-cp kubeconfig source")
	}
	if o.CPSecret == "" && o.CPFile == "" && o.DefaultControlPlane == "" {
		return nil, nil, fmt.Errorf("specify an aep-cp kubeconfig source or --default-control-plane")
	}
	if o.DefaultControlPlane != "" && len(validation.IsDNS1123Subdomain(o.DefaultControlPlane)) > 0 {
		return nil, nil, fmt.Errorf("default-control-plane must be a ControlPlane name")
	}
	if o.BridgeImage == "" {
		return nil, nil, fmt.Errorf("bridge-image required")
	}
	if o.KeycloakEnabled && (o.KeycloakURL == "" || o.KeycloakRealm == "" || o.KeycloakCredentialsSecret == "") {
		return nil, nil, fmt.Errorf("keycloak-url, keycloak-realm and keycloak-credentials-secret required when keycloak-enabled")
	}
	return r, cli, nil
}

func start(r *operator.Reconciler, cli *settings) error {
	o := &r.Options
	var cfg *rest.Config
	var e error
	if cli.mgmt != "" {
		cfg, e = clientcmd.BuildConfigFromFlags("", cli.mgmt)
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
	cp := &operator.ControlPlaneReconciler{Client: mgr.GetClient(), Management: r.Management, Options: r.Options, Recorder: mgr.GetEventRecorderFor("aep-operator")}
	if e = cp.SetupWithManager(mgr); e != nil {
		return e
	}
	if cli.fleetCharts != "" {
		if !strings.HasPrefix(cli.fleetGateway, "wss://") {
			return fmt.Errorf("fleet-gateway-url must use wss://")
		}
		hub := r.Management
		if cli.fleetHub != "" {
			hc, err := clientcmd.BuildConfigFromFlags("", cli.fleetHub)
			if err != nil {
				return err
			}
			hub, err = operator.NewCluster(hc)
			if err != nil {
				return err
			}
		}
		fleet := &operator.FleetReconciler{Reconciler: r, Hub: hub, Renderer: operator.HelmFleetRenderer{Directory: cli.fleetCharts, Mirror: r.Options.ImageRegistryMirror}, Gateway: cli.fleetGateway}
		if e = ctrl.NewControllerManagedBy(mgr).For(&api.ExternalCluster{}, builder.WithPredicates(operator.FleetPredicate())).Complete(fleet); e != nil {
			return e
		}
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
