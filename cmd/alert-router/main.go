package main

import (
	"crypto/x509"
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/alertrouter"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", ":8080", "Listen address")
	kube := flag.String("cp-kubeconfig", "", "Control-plane kubeconfig; default in-cluster")
	template := flag.String("agent-url-template", "http://sre-agent.aep-{org}.svc:8080", "Per-organization agent base URL")
	caFile := flag.String("client-ca", "", "Observer client certificate CA PEM file")
	secretFile := flag.String("hmac-secret-file", "", "Shared HMAC secret file (at least 32 bytes; trailing whitespace trimmed)")
	cert := flag.String("tls-cert", "", "Server certificate PEM")
	key := flag.String("tls-key", "", "Server key PEM")
	flag.Parse()
	if (*cert == "") != (*key == "") {
		return fmt.Errorf("TLS certificate and key required together")
	}
	if *caFile != "" && *cert == "" {
		return fmt.Errorf("mTLS requires server TLS")
	}
	var secret []byte
	var roots *x509.CertPool
	if *secretFile != "" {
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			return err
		}
		secret = []byte(strings.TrimSpace(string(b)))
	}
	if *caFile != "" {
		b, err := os.ReadFile(*caFile)
		if err != nil {
			return err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(b) {
			return fmt.Errorf("invalid client CA")
		}
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *kube)
	if err != nil {
		return err
	}
	c, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	router, err := alertrouter.New(c, *template, secret, roots)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: router, TLSConfig: router.TLSConfig(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second}
	if *cert != "" {
		return server.ListenAndServeTLS(*cert, *key)
	}
	return server.ListenAndServe()
}
