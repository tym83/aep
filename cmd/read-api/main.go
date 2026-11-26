package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/cost"
	"github.com/tym83/aep/internal/readapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func run() error {
	var dsn string
	flag.StringVar(&dsn, "cost-dsn", os.Getenv("COST_DSN"), "Read-only cost PostgreSQL DSN")
	var namespace, kubeconfig, listen, issuer, jwksURL, audience, usernameClaim, groupsClaim, orgNamespaceTemplate, tlsCert, tlsKey, consoleURLTemplate string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Management kubeconfig; default in-cluster")
	flag.StringVar(&listen, "listen", ":8080", "Listen address")
	flag.StringVar(&issuer, "issuer", "", "OIDC issuer URL (required)")
	flag.StringVar(&jwksURL, "jwks-url", "", "JWKS URL; defaults to <issuer>/protocol/openid-connect/certs")
	flag.StringVar(&audience, "audience", "", "Required token audience")
	flag.StringVar(&usernameClaim, "username-claim", "preferred_username", "Token claim carrying the username")
	flag.StringVar(&groupsClaim, "groups-claim", "groups", "Token claim carrying the groups")
	flag.StringVar(&orgNamespaceTemplate, "org-namespace-template", "tenant-{org}", "Maps an organization to its management namespace")
	flag.StringVar(&consoleURLTemplate, "console-url-template", "", "Console HTTP(S) URL template with Namespace, Kind and Name")
	flag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file; serve plain HTTP when empty")
	flag.StringVar(&tlsKey, "tls-key", "", "TLS key file")
	flag.StringVar(&namespace, "namespace", "aep-system", "Management namespace containing compliance snapshots")
	logOptions := zap.Options{}
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	if issuer == "" || audience == "" {
		return fmt.Errorf("issuer and audience are required")
	}
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("tls-cert and tls-key must be provided together")
	}
	var mgmt *rest.Config
	var e error
	if kubeconfig != "" {
		mgmt, e = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		mgmt, e = rest.InClusterConfig()
	}
	if e != nil {
		return e
	}
	scheme := runtime.NewScheme()
	if e := corev1.AddToScheme(scheme); e != nil {
		return e
	}
	if e := api.AddToScheme(scheme); e != nil {
		return e
	}
	direct, e := client.New(mgmt, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	verifier, e := authn.NewOIDCVerifier(ctx, issuer, jwksURL, audience, usernameClaim, groupsClaim)
	cancel()
	if e != nil {
		return e
	}
	apps, e := dynamic.NewForConfig(mgmt)
	if e != nil {
		return e
	}
	handler, e := readapi.New(direct, apps, verifier, orgNamespaceTemplate, consoleURLTemplate, readapi.WithComplianceNamespace(namespace))
	if e != nil {
		return e
	}
	if dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		store, err := cost.Open(ctx, dsn, false)
		cancel()
		if err != nil {
			return err
		}
		defer store.DB.Close()
		handler.Costs = store
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if tlsCert != "" && tlsKey != "" {
		return server.ListenAndServeTLS(tlsCert, tlsKey)
	}
	return server.ListenAndServe()
}
