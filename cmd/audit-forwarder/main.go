package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tym83/aep/internal/audit"
	"github.com/tym83/aep/internal/objectstore"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "verify" {
		f := flag.NewFlagSet("verify", flag.ContinueOnError)
		prev := f.String("prev-hash", "", "trusted predecessor (empty = genesis)")
		last := f.String("last-hash", "", "trusted final hash")
		if e := f.Parse(os.Args[2:]); e != nil {
			return e
		}
		if f.NArg() != 1 {
			return fmt.Errorf("usage: audit-forwarder verify [flags] export.jsonl")
		}
		r, e := os.Open(f.Arg(0))
		if e != nil {
			return e
		}
		defer r.Close()
		h, e := audit.Verify(r, *prev, *last)
		if e == nil {
			fmt.Println(h)
		}
		return e
	}
	mgmt := flag.String("mgmt-kubeconfig", "", "management kubeconfig; empty = in cluster")
	dir := flag.String("spool", "/var/lib/audit", "durable PVC directory")
	stream := flag.String("stream", "management", "unique stable stream ID")
	max := flag.Int("spool-bytes", 64<<20, "maximum durable state size; requires twice this free disk space")
	listen := flag.String("listen", ":8443", "HTTPS listen address")
	cert := flag.String("tls-cert", "", "server certificate")
	key := flag.String("tls-key", "", "server key")
	token := flag.String("token-file", "", "webhook bearer token file")
	osURL := flag.String("opensearch-url", "", "OpenSearch HTTPS URL")
	index := flag.String("opensearch-index", "aep-audit", "index")
	sys := flag.String("syslog-address", "", "TLS syslog host:port")
	endpoint := flag.String("s3-endpoint", "", "S3 HTTPS URL")
	bucket := flag.String("s3-bucket", "", "bucket")
	prefix := flag.String("s3-prefix", "audit", "prefix")
	flag.Parse()
	if *cert == "" || *key == "" || *token == "" || strings.ContainsAny(*stream, "/\\ \t\n") {
		return fmt.Errorf("TLS certificate, key, token file and safe stream ID required")
	}
	t, err := os.ReadFile(*token)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(t))) < 32 {
		return fmt.Errorf("webhook token must be at least 32 characters")
	}
	var sinks []audit.Sink
	if *osURL != "" {
		sinks = append(sinks, audit.OpenSearch{URL: *osURL, Index: *index, Username: os.Getenv("OPENSEARCH_USERNAME"), Password: os.Getenv("OPENSEARCH_PASSWORD")})
	}
	if *sys != "" {
		sinks = append(sinks, audit.Syslog{Address: *sys})
	}
	if *endpoint != "" {
		s, e := objectstore.New(*endpoint, *bucket, *prefix)
		if e != nil {
			return e
		}
		sinks = append(sinks, audit.S3{Store: s})
	}
	if len(sinks) == 0 {
		return fmt.Errorf("configure at least one sink")
	}
	s, err := audit.Open(*dir, *stream, *max)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, err := clientcmd.BuildConfigFromFlags("", *mgmt)
	if err != nil {
		return err
	}
	c, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	for _, r := range []string{"changerequests", "organizations"} {
		go audit.Watch(ctx, c, s, r)
	}
	go func() {
		for ctx.Err() == nil {
			if e := s.Flush(ctx, sinks); e != nil {
				log.Printf("audit export retry: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	mux := http.NewServeMux()
	receiver := audit.Receiver{Spool: s, Token: strings.TrimSpace(string(t)), Cluster: *stream}
	mux.Handle("/kubernetes", receiver)
	mux.Handle("/openchoreo", receiver)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		p := s.Pending()
		age := float64(0)
		if len(p) > 0 {
			if t, e := time.Parse(time.RFC3339Nano, p[0].RecordedAt); e == nil {
				age = time.Since(t).Seconds()
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "aep_audit_pending_events %d\naep_audit_oldest_pending_seconds %f\n", len(p), age)
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16384, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	go func() {
		<-ctx.Done()
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = srv.Shutdown(c)
	}()
	err = srv.ListenAndServeTLS(*cert, *key)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
