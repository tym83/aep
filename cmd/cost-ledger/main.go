package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/cost"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"time"
)

func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
func run() error {
	var kubeconfig, dsn, template, endpoint, bucket, access, secret, region, installation string
	var secure, once bool
	flag.StringVar(&kubeconfig, "mgmt-kubeconfig", "", "Management kubeconfig; default in-cluster")
	flag.StringVar(&dsn, "dsn", os.Getenv("COST_DSN"), "PostgreSQL DSN; alternatively COST_DSN from a Secret")
	flag.StringVar(&template, "tier-opencost-url-template", "", "OpenCost base URL with {org} and {tier}; no URL means unallocated")
	flag.StringVar(&endpoint, "s3-endpoint", os.Getenv("S3_ENDPOINT"), "S3 host[:port]")
	flag.StringVar(&bucket, "s3-bucket", os.Getenv("S3_BUCKET"), "Existing export bucket")
	flag.StringVar(&access, "s3-access-key", os.Getenv("S3_ACCESS_KEY"), "S3 access key; prefer Secret environment")
	flag.StringVar(&secret, "s3-secret-key", os.Getenv("S3_SECRET_KEY"), "S3 secret key; prefer Secret environment")
	flag.StringVar(&region, "s3-region", "us-east-1", "S3 region")
	flag.StringVar(&installation, "installation", "aep", "Stable billing account ID")
	flag.BoolVar(&secure, "s3-tls", true, "Use TLS for S3")
	flag.BoolVar(&once, "once", false, "Collect the current hour and export pending days, then exit")
	flag.Parse()
	if dsn == "" {
		return fmt.Errorf("dsn is required")
	}
	ctx := ctrl.SetupSignalHandler()
	dbctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	store, e := cost.Open(dbctx, dsn, true)
	cancel()
	if e != nil {
		return e
	}
	defer store.DB.Close()
	var cfg *rest.Config
	if kubeconfig != "" {
		cfg, e = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, e = rest.InClusterConfig()
	}
	if e != nil {
		return e
	}
	cfg.Timeout = 30 * time.Second
	client, e := dynamic.NewForConfig(cfg)
	if e != nil {
		return e
	}
	collector := &cost.Collector{Client: client}
	if template != "" {
		collector.OpenCost = &cost.OpenCost{URLTemplate: template}
	}
	var exporter *cost.Exporter
	if endpoint != "" || bucket != "" {
		exporter, e = cost.NewExporter(endpoint, bucket, access, secret, region, installation, secure)
		if e != nil {
			return e
		}
	}
	collect := func() error {
		at := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
		work, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		rows, e := collector.Collect(work, at)
		if e != nil {
			return e
		}
		if e = store.SaveHour(work, at, rows); e != nil {
			return e
		}
		if exporter != nil {
			return exporter.ExportPending(work, store)
		}
		return nil
	}
	for {
		e := collect()
		if once {
			return e
		}
		if e != nil {
			log.Printf("cost collection/export failed: %v", e)
		}
		delay := time.Until(time.Now().UTC().Truncate(time.Hour).Add(time.Hour))
		if e != nil && delay > time.Minute {
			delay = time.Minute
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
