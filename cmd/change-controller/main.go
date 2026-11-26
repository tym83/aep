package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	"github.com/tym83/aep/internal/ocapi"
	"github.com/tym83/aep/internal/policy"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	var kubeconfig, apiURL, tokenURL, clientIDTemplate, secretNameTemplate string
	var policyPath, leaderNamespace, orgNamespaceTemplate, instanceID string
	var approvalTTL time.Duration
	var maxAttempts int
	flag.StringVar(&kubeconfig, "mgmt-kubeconfig", "", "Management kubeconfig; default in-cluster")
	flag.StringVar(&apiURL, "oc-api-url", "", "OpenChoreo API base URL (required)")
	flag.StringVar(&tokenURL, "oc-token-url", "", "OAuth token URL for client credentials; a token-url key in the org Secret overrides it")
	flag.StringVar(&clientIDTemplate, "oc-client-id-template", "aep-change-{org}", "Client ID template; a client-id key in the org Secret overrides it")
	flag.StringVar(&secretNameTemplate, "oc-secret-name", "aep-change-{org}", "Secret with client credentials in the organization namespace")
	flag.StringVar(&policyPath, "policy-config", "", "YAML scaling limits for standard changes; defaults apply when empty")
	flag.StringVar(&leaderNamespace, "leader-election-namespace", "aep-system", "Namespace for the leader election lease")
	flag.StringVar(&orgNamespaceTemplate, "org-namespace-template", "tenant-{org}", "Maps an organization to its management namespace")
	flag.StringVar(&instanceID, "instance-id", "", "Lease holder identity; defaults to the hostname")
	flag.DurationVar(&approvalTTL, "approval-ttl", change.DefaultApprovalTTL, "Approval validity for changes without a window")
	flag.IntVar(&maxAttempts, "max-attempts", 8, "Apply attempts before a change fails")
	logOptions := zap.Options{}
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	if apiURL == "" {
		return fmt.Errorf("oc-api-url is required")
	}
	if instanceID == "" {
		host, e := os.Hostname()
		if e != nil {
			return e
		}
		instanceID = host
	}
	cfg, e := policy.Load(policyPath)
	if e != nil {
		return e
	}
	var mgmt *rest.Config
	if kubeconfig != "" {
		mgmt, e = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		mgmt, e = rest.InClusterConfig()
	}
	if e != nil {
		return e
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme} {
		if e := add(scheme); e != nil {
			return e
		}
	}
	mgr, e := ctrl.NewManager(mgmt, ctrl.Options{
		Scheme: scheme, LeaderElection: true, LeaderElectionConfig: mgmt,
		LeaderElectionNamespace: leaderNamespace, LeaderElectionID: "aep-change-controller",
		Metrics: metrics.Options{BindAddress: "0"}, HealthProbeBindAddress: ":8081",
	})
	if e != nil {
		return e
	}
	// Direct clients: approvals and phases must never be read from a stale cache.
	direct, e := client.New(mgmt, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	factory := &ocapi.Factory{
		BaseURL:    apiURL,
		HTTPClient: ocapi.DefaultHTTPClient(),
		Credentials: func(ctx context.Context, org string) (ocapi.Credentials, error) {
			ns := strings.ReplaceAll(orgNamespaceTemplate, "{org}", org)
			name := strings.ReplaceAll(secretNameTemplate, "{org}", org)
			secret := &corev1.Secret{}
			if e := direct.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, secret); e != nil {
				return ocapi.Credentials{}, e
			}
			clientID := string(secret.Data["client-id"])
			if clientID == "" {
				clientID = strings.ReplaceAll(clientIDTemplate, "{org}", org)
			}
			url := string(secret.Data["token-url"])
			if url == "" {
				url = tokenURL
			}
			return ocapi.Credentials{ClientID: clientID, ClientSecret: string(secret.Data["client-secret"]), TokenURL: url}, nil
		},
	}
	r := &change.Reconciler{
		Client:               direct,
		API:                  factory,
		Granter:              change.RecordOnlyGranter{},
		Policy:               cfg,
		ApprovalTTL:          approvalTTL,
		MaxAttempts:          maxAttempts,
		InstanceID:           instanceID,
		OrgNamespaceTemplate: orgNamespaceTemplate,
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
