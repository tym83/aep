package release

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/tym83/aep/internal/registrymirror"
)

type Client struct {
	InsecureRegistry string
	PublishRegistry  string
	AllowRetag       bool
	Verify           string
	Keys             map[string]string
	WorkDir          string
	MaxSize          int64
	CA, Key          string
	transport        http.RoundTripper
}

func (c *Client) Init() error {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if c.CA != "" {
		b, e := os.ReadFile(c.CA)
		if e != nil {
			return e
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return fmt.Errorf("no CA certificates in %s", c.CA)
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	c.transport = t
	return nil
}
func (c *Client) options(ctx context.Context) []remote.Option {
	base := c.transport
	if base == nil {
		base = remote.DefaultTransport
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithTransport(registryTransport{base, c.InsecureRegistry})}
	return opts
}
func (c *Client) ref(s string) (name.Reference, error) {
	opts := []name.Option{}
	r, err := name.ParseReference(strings.TrimPrefix(s, "oci://"))
	if err != nil {
		return nil, err
	}
	if c.InsecureRegistry != "" && r.Context().RegistryStr() == c.InsecureRegistry {
		opts = append(opts, name.Insecure)
	}
	return name.ParseReference(strings.TrimPrefix(s, "oci://"), opts...)
}
func denied(e error) bool {
	var te *transport.Error
	return errors.As(e, &te) && (te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden)
}
func notFound(e error) bool {
	var te *transport.Error
	return errors.As(e, &te) && te.StatusCode == http.StatusNotFound
}

type fetched struct {
	v1.Descriptor
	image v1.Image
	index v1.ImageIndex
}

func (d *fetched) Image() (v1.Image, error) {
	if d.image == nil {
		return nil, fmt.Errorf("not an image")
	}
	return d.image, nil
}
func (d *fetched) ImageIndex() (v1.ImageIndex, error) {
	if d.index == nil {
		return nil, fmt.Errorf("not an index")
	}
	return d.index, nil
}
func (c *Client) fetch(ctx context.Context, a Artifact, signature bool) (*fetched, error) {
	digest := a.Digest
	if signature {
		digest = a.Signature
	}
	if a.Layout != "" && !signature {
		lp, e := layout.FromPath(a.Layout)
		if e != nil {
			return nil, e
		}
		h, e := v1.NewHash(digest)
		if e != nil {
			return nil, e
		}
		img, e := lp.Image(h)
		if e != nil {
			return nil, e
		}
		media, e := img.MediaType()
		if e != nil {
			return nil, e
		}
		return &fetched{Descriptor: v1.Descriptor{Digest: h, MediaType: media}, image: img}, nil
	}
	r, e := c.ref(strings.TrimPrefix(a.Ref, "oci://") + "@" + digest)
	if e != nil {
		return nil, e
	}
	d, e := remote.Get(r, c.options(ctx)...)
	if e != nil {
		return nil, e
	}
	out := &fetched{Descriptor: d.Descriptor}
	if d.MediaType.IsIndex() {
		out.index, e = d.ImageIndex()
	} else {
		out.image, e = d.Image()
	}
	return out, e
}

func (c *Client) destination(a Artifact, to string, signature bool) (name.Reference, error) {
	ref := registrymirror.Rewrite(strings.TrimPrefix(a.Ref, "oci://"), to)
	if signature {
		return c.ref(ref + ":sha256-" + strings.TrimPrefix(a.Digest, "sha256:") + ".sig")
	}
	if a.Version != "" && !strings.HasPrefix(a.Version, "sha256:") {
		return c.ref(ref + ":" + a.Version)
	}
	return c.ref(ref + "@" + a.Digest)
}
func (c *Client) put(ctx context.Context, dst name.Reference, digest string, img v1.Image, idx v1.ImageIndex) error {
	// Compare the destination tag as well as the digest: an interrupted upgrade may
	// have copied blobs without advancing the tag. Never skip authentication errors.
	d, e := remote.Get(dst, c.options(ctx)...)
	if e == nil && d.Digest.String() == digest {
		return nil
	}
	if e != nil && !notFound(e) {
		return e
	}
	if e == nil && !c.AllowRetag {
		return fmt.Errorf("refusing to retag %s from %s to %s; use --allow-retag", dst, d.Digest, digest)
	}
	if idx != nil {
		e = remote.WriteIndex(dst, idx, c.options(ctx)...)
	} else {
		e = remote.Write(dst, img, c.options(ctx)...)
	}
	if e != nil {
		return e
	}
	d, e = remote.Get(dst, c.options(ctx)...)
	if e != nil {
		return e
	}
	if d.Digest.String() != digest {
		return fmt.Errorf("registry changed digest for %s", dst)
	}
	return nil
}
func (c *Client) Mirror(ctx context.Context, m Manifest, to string) error {
	if to == "" {
		return fmt.Errorf("--to required")
	}
	var err error
	m, err = c.publishedSignatures(ctx, m)
	if err != nil {
		return err
	}
	for _, a := range m.Artifacts {
		if err := c.requireSignature(a); err != nil {
			return err
		}
	}
	for _, a := range m.Artifacts {
		var sig v1.Image
		if a.Signature != "" {
			d, e := c.fetch(ctx, a, true)
			if e != nil {
				return e
			}
			sig, e = d.Image()
			if e != nil {
				return e
			}
			if e = c.verify(a, sig); e != nil {
				return e
			}
		}
		d, e := c.fetch(ctx, a, false)
		if e != nil {
			return e
		}
		dst, e := c.destination(a, to, false)
		if e != nil {
			return e
		}
		if d.MediaType.IsIndex() {
			idx, e := d.ImageIndex()
			if e != nil {
				return e
			}
			e = c.put(ctx, dst, a.Digest, nil, idx)
			if e != nil {
				return e
			}
		} else {
			img, e := d.Image()
			if e != nil {
				return e
			}
			if e = c.put(ctx, dst, a.Digest, img, nil); e != nil {
				return e
			}
		}
		if sig != nil {
			dst, e = c.destination(a, to, true)
			if e != nil {
				return e
			}
			if e = c.put(ctx, dst, a.Signature, sig, nil); e != nil {
				return e
			}
		}
	}
	return nil
}

type registryTransport struct {
	base         http.RoundTripper
	insecureHost string
}

func (t registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" && req.URL.Host != t.insecureHost {
		return nil, fmt.Errorf("HTTP registry access requires --insecure-registry=%s", req.URL.Host)
	}
	return t.base.RoundTrip(req)
}
