package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/mcp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", ":8443", "Listen address")
	kube := flag.String("mgmt-kubeconfig", "", "Management kubeconfig; default in-cluster")
	issuer := flag.String("issuer", "", "OIDC issuer (required)")
	jwks := flag.String("jwks-url", "", "OIDC JWKS override")
	aud := flag.String("audience", "", "Required OIDC audience")
	cert := flag.String("tls-cert", "", "TLS certificate")
	key := flag.String("tls-key", "", "TLS key")
	plain := flag.Bool("tls-termination-proxy", false, "Allow HTTP behind a trusted TLS termination proxy")
	cfg := mcp.Config{}
	flag.IntVar(&cfg.MaxRows, "max-rows", 100, "Maximum evidence rows")
	flag.IntVar(&cfg.MaxBytes, "max-bytes", 65536, "Maximum evidence JSON bytes")
	flag.IntVar(&cfg.RequestsPerMinute, "requests-per-minute", 60, "Per-token request limit")
	flag.StringVar(&cfg.MetricsURL, "metrics-query-url-template", "", "Tenant VictoriaMetrics /api/v1/query URL, with {org} and {namespace} placeholders")
	flag.Func("redact-pattern", "Additional evidence redaction regex (repeatable)", func(pattern string) error { cfg.RedactPatterns = append(cfg.RedactPatterns, pattern); return nil })
	flag.Parse()
	if *issuer == "" || *aud == "" {
		return fmt.Errorf("issuer and audience required")
	}
	if (*cert == "") != (*key == "") || (*cert == "" && !*plain) {
		return fmt.Errorf("TLS certificate/key or explicit TLS termination proxy required")
	}
	k, err := clientcmd.BuildConfigFromFlags("", *kube)
	if err != nil {
		return err
	}
	c, err := dynamic.NewForConfig(k)
	if err != nil {
		return err
	}
	verifier, err := authn.NewOIDCVerifier(context.Background(), *issuer, *jwks, *aud, "", "groups")
	if err != nil {
		return err
	}
	s, err := mcp.New(c, verifier, cfg)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	if *cert != "" {
		return server.ListenAndServeTLS(*cert, *key)
	}
	return server.ListenAndServe()
}
