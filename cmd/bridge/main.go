package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/bridge"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	r := &bridge.Reconciler{}
	var mgmtPath, appPath, tokenPath, configPath, allowed string
	flag.StringVar(&mgmtPath, "mgmt-kubeconfig", "", "Management kubeconfig; default in-cluster")
	flag.StringVar(&appPath, "app-kubeconfig", "", "Application kubeconfig (required)")
	flag.StringVar(&tokenPath, "app-token-file", "", "Rotating application bearer-token file; overrides kubeconfig tokenFile")
	flag.StringVar(&r.Organization, "organization", "", "Organization label")
	flag.StringVar(&r.Tier, "tier", "", "Tier: prod or nonprod")
	flag.StringVar(&r.TenantNamespace, "tenant-namespace", "", "Management tenant namespace")
	flag.StringVar(&r.ClusterID, "cluster-id", "", "Stable application cluster identity")
	flag.StringVar(&allowed, "allowed-kinds", "Postgres,Redis", "Comma-separated allowed kinds")
	flag.StringVar(&r.TierDeletionPolicy, "tier-deletion-policy", "Retain", "Retain or Delete; prod always retains")
	flag.StringVar(&configPath, "config", "", "YAML parameter allowlists and tier limits")
	logOptions := zap.Options{}
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	if appPath == "" || r.ClusterID == "" || r.Organization == "" || r.TenantNamespace == "" {
		return fmt.Errorf("app-kubeconfig, cluster-id, organization and tenant-namespace are required")
	}
	if r.Tier != "prod" && r.Tier != "nonprod" {
		return fmt.Errorf("tier must be prod or nonprod")
	}
	if r.TierDeletionPolicy != "Retain" && r.TierDeletionPolicy != "Delete" {
		return fmt.Errorf("tier-deletion-policy must be Retain or Delete")
	}
	if len(validation.IsValidLabelValue(r.Organization)) > 0 || len(validation.IsDNS1123Label(r.TenantNamespace)) > 0 {
		return fmt.Errorf("invalid organization or tenant namespace")
	}
	var e error
	r.Policy, e = policy.Load(configPath)
	if e != nil {
		return e
	}
	for _, k := range strings.Split(allowed, ",") {
		k = strings.TrimSpace(k)
		if _, ok := kinds.Table[k]; !ok {
			return fmt.Errorf("unknown allowed kind %q", k)
		}
		if _, ok := r.Policy.Kinds[k]; !ok {
			return fmt.Errorf("missing policy for %s", k)
		}
		r.AllowedKinds = append(r.AllowedKinds, k)
	}
	var mgmt *rest.Config
	if mgmtPath != "" {
		mgmt, e = clientcmd.BuildConfigFromFlags("", mgmtPath)
	} else {
		mgmt, e = rest.InClusterConfig()
	}
	if e != nil {
		return e
	}
	app, e := clientcmd.BuildConfigFromFlags("", appPath)
	if e != nil {
		return e
	}
	if tokenPath != "" {
		app.BearerTokenFile = tokenPath
	}
	if app.BearerTokenFile == "" {
		return fmt.Errorf("application kubeconfig tokenFile or --app-token-file is required for token rotation")
	}
	app.BearerToken = ""
	app.CertFile = ""
	app.KeyFile = ""
	app.CertData = nil
	app.KeyData = nil
	app.ExecProvider = nil
	app.AuthProvider = nil
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, discoveryv1.AddToScheme} {
		if e := add(scheme); e != nil {
			return e
		}
	}
	mgr, e := ctrl.NewManager(app, ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionConfig: mgmt, LeaderElectionNamespace: r.TenantNamespace, LeaderElectionID: policy.Name(r.ClusterID, r.Organization, r.Tier, "bridge"), Metrics: metrics.Options{BindAddress: "0"}, HealthProbeBindAddress: ":8081"})
	if e != nil {
		return e
	}
	// Direct clients avoid granting list/watch on Secrets and prevent stale ownership reads.
	r.App, e = client.New(app, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	r.Management, e = client.New(mgmt, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	r.Dynamic, e = dynamic.NewForConfig(mgmt)
	if e != nil {
		return e
	}
	if e = r.SetupWithManager(mgr); e != nil {
		return e
	}
	if e = mgr.AddHealthzCheck("healthz", healthz.Ping); e != nil {
		return e
	}
	if e = mgr.AddReadyzCheck("readyz", healthz.Ping); e != nil {
		return e
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
