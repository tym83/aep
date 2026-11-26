package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tym83/aep/internal/cpbackup"
	"github.com/tym83/aep/internal/objectstore"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
func run() error {
	mgmt := flag.String("mgmt-kubeconfig", "", "management kubeconfig for Organization discovery")
	cp := flag.String("aepcp-kubeconfig", "", "control-plane kubeconfig (empty = in cluster)")
	orgs := flag.String("organizations", "", "comma-separated AEP organization namespaces; empty = discover from management")
	endpoint := flag.String("s3-endpoint", "", "S3 HTTPS URL")
	bucket := flag.String("s3-bucket", "", "bucket")
	prefix := flag.String("s3-prefix", "aep-cp", "prefix")
	key := flag.String("object", "", "object key (required for restore)")
	dry := flag.Bool("dry-run", false, "restore plan without API writes")
	diff := flag.Bool("diff", false, "print restore differences without API writes")
	ack := flag.Bool("acknowledge-new-uids", false, "confirm controllers and gateway are stopped and UID reconciliation is planned")
	timeout := flag.Duration("timeout", 10*time.Minute, "operation deadline")
	flag.Parse()
	if flag.NArg() != 1 || (flag.Arg(0) != "backup" && flag.Arg(0) != "restore") {
		return fmt.Errorf("usage: aep-cp-backup [flags] backup|restore")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	store, e := objectstore.New(*endpoint, *bucket, *prefix)
	if e != nil {
		return e
	}
	cfg, e := clientcmd.BuildConfigFromFlags("", *cp)
	if e != nil {
		return e
	}
	cfg.Timeout = 30 * time.Second
	client, e := dynamic.NewForConfig(cfg)
	if e != nil {
		return e
	}
	d, e := discovery.NewDiscoveryClientForConfig(cfg)
	if e != nil {
		return e
	}
	resources, e := cpbackup.Discover(d)
	if e != nil {
		return e
	}
	if flag.Arg(0) == "restore" {
		if *key == "" {
			return fmt.Errorf("restore requires --object")
		}
		if !*dry && !*diff && !*ack {
			return fmt.Errorf("logical restore changes UIDs; stop controllers/gateway, plan reconciliation, then set --acknowledge-new-uids")
		}
		b, e := store.Get(ctx, *key)
		if e != nil {
			return e
		}
		a, e := cpbackup.Decode(b)
		if e != nil {
			return e
		}
		return cpbackup.Restore(ctx, client, resources, a, *dry || *diff, os.Stdout)
	}
	var namespaces []string
	if *orgs != "" {
		for _, o := range strings.Split(*orgs, ",") {
			if strings.TrimSpace(o) == "" {
				return fmt.Errorf("empty organization")
			}
			namespaces = append(namespaces, strings.TrimSpace(o))
		}
	} else {
		m, e := clientcmd.BuildConfigFromFlags("", *mgmt)
		if e != nil {
			return e
		}
		m.Timeout = 30 * time.Second
		mc, e := dynamic.NewForConfig(m)
		if e != nil {
			return e
		}
		namespaces, e = cpbackup.Organizations(ctx, mc)
		if e != nil {
			return e
		}
	}
	a, e := cpbackup.Export(ctx, client, resources, namespaces)
	if e != nil {
		return e
	}
	b, e := cpbackup.Encode(a)
	if e != nil {
		return e
	}
	if *key == "" {
		*key = time.Now().UTC().Format("2006-01-02/150405.000000000") + "-" + a.SHA256 + ".json"
	}
	if e = store.Put(ctx, *key, b, "application/json"); e != nil {
		return e
	}
	fmt.Printf("object=%s sha256=%s objects=%d\n", *key, a.SHA256, len(a.Payload.Entries))
	return nil
}
