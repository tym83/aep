package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tym83/aep/internal/etcdprefix"
	"github.com/tym83/aep/internal/objectstore"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// etcdFlags address one Kamaji tenant prefix with that tenant's etcd user.
type etcdFlags struct {
	endpoints, ca, cert, key, prefix *string
	replace                          *bool
}

func (f *etcdFlags) register() {
	f.endpoints = flag.String("etcd-endpoints", "", "comma-separated etcd URLs of the Kamaji datastore")
	f.ca = flag.String("etcd-ca", "", "datastore CA file (ca.crt of <cluster>-datastore-certificate)")
	f.cert = flag.String("etcd-cert", "", "tenant etcd client certificate file (server.crt)")
	f.key = flag.String("etcd-key", "", "tenant etcd client key file (server.key)")
	f.prefix = flag.String("etcd-prefix", "", "tenant key prefix, /<DB_SCHEMA> of <cluster>-datastore-config")
	f.replace = flag.Bool("replace", false, "etcd-restore: clear the target prefix of a new, not yet used control plane first")
}

func (f *etcdFlags) client() (*clientv3.Client, error) {
	if *f.endpoints == "" || *f.ca == "" || *f.cert == "" || *f.key == "" || *f.prefix == "" {
		return nil, fmt.Errorf("etcd commands require --etcd-endpoints, --etcd-ca, --etcd-cert, --etcd-key and --etcd-prefix")
	}
	pair, err := tls.LoadX509KeyPair(*f.cert, *f.key)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(*f.ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no CA certificates in %s", *f.ca)
	}
	return clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*f.endpoints, ","),
		DialTimeout: 15 * time.Second,
		TLS:         &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, MinVersion: tls.VersionTLS12},
	})
}

func (f *etcdFlags) backup(ctx context.Context, store *objectstore.Store, key, recipient string) error {
	c, err := f.client()
	if err != nil {
		return err
	}
	defer c.Close()
	s, err := etcdprefix.Dump(ctx, c, *f.prefix)
	if err != nil {
		return err
	}
	sealed, err := etcdprefix.Encrypt(s, recipient)
	if err != nil {
		return err
	}
	if key == "" {
		key = "etcd/" + time.Now().UTC().Format("2006-01-02/150405") + "-" + s.SHA256[:16] + ".age"
	}
	if err = store.Put(ctx, key, sealed, "application/octet-stream"); err != nil {
		return err
	}
	fmt.Printf("object=%s revision=%d keys=%d sha256=%s\n", key, s.Revision, len(s.KVs), s.SHA256)
	return nil
}

func (f *etcdFlags) restore(ctx context.Context, store *objectstore.Store, key, identity string) error {
	if key == "" {
		return fmt.Errorf("etcd-restore requires --object")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	defer c.Close()
	b, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	id, err := os.Open(identity)
	if err != nil {
		return err
	}
	defer id.Close()
	s, err := etcdprefix.Decrypt(b, id)
	if err != nil {
		return err
	}
	n, err := etcdprefix.Load(ctx, c, s, *f.prefix, *f.replace)
	if err != nil {
		return err
	}
	fmt.Printf("restored keys=%d from revision=%d of %s into %s; restart the new API server\n", n, s.Revision, s.Prefix, *f.prefix)
	return nil
}
