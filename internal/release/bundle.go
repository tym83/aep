package release

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"sigs.k8s.io/yaml"
)

func (c *Client) Bundle(ctx context.Context, m Manifest, out string) error {
	if out == "" {
		return fmt.Errorf("--out required")
	}
	var err error
	m, err = c.publishedSignatures(ctx, m)
	if err != nil {
		return err
	}
	if e := os.MkdirAll(out, 0755); e != nil {
		return e
	}
	work, e := os.MkdirTemp(out, ".layout-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(work)
	lp, e := layout.Write(work, empty.Index)
	if e != nil {
		return e
	}
	for _, a := range m.Artifacts {
		if err := c.requireSignature(a); err != nil {
			return err
		}
		for _, sig := range []bool{false, true} {
			if sig && a.Signature == "" {
				continue
			}
			d, e := c.fetch(ctx, a, sig)
			if e != nil {
				return e
			}
			if d.MediaType.IsIndex() {
				if sig {
					return fmt.Errorf("signature must be an image for %s", a.Ref)
				}
				idx, e := d.ImageIndex()
				if e != nil {
					return e
				}
				if e = lp.AppendIndex(idx); e != nil {
					return e
				}
			} else {
				img, e := d.Image()
				if e != nil {
					return e
				}
				if sig {
					if e = c.verify(a, img); e != nil {
						return e
					}
				}
				if e = lp.AppendImage(img); e != nil {
					return e
				}
			}
		}
	}
	file, e := os.Create(filepath.Join(out, "layout.tar"))
	if e != nil {
		return e
	}
	tw := tar.NewWriter(file)
	e = filepath.Walk(work, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if i.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(work, p)
		if e != nil {
			return e
		}
		if e = tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0644, Size: i.Size()}); e != nil {
			return e
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		defer f.Close()
		_, e = io.Copy(tw, f)
		return e
	})
	closeErr := tw.Close()
	fileErr := file.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if fileErr != nil {
		return fileErr
	}
	b, e := yaml.Marshal(m)
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(out, "manifest.yaml"), b, 0644); e != nil {
		return e
	}
	checks := map[string]string{}
	for _, n := range []string{"manifest.yaml", "layout.tar"} {
		h, e := checksum(filepath.Join(out, n))
		if e != nil {
			return e
		}
		checks[n] = h
	}
	b, e = json.MarshalIndent(checks, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(out, "checksums.json"), b, 0644)
}
func checksum(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (c *Client) Load(ctx context.Context, from, to string) error {
	if from == "" || to == "" {
		return fmt.Errorf("--from and --to required")
	}
	b, e := os.ReadFile(filepath.Join(from, "checksums.json"))
	if e != nil {
		return e
	}
	var sums map[string]string
	if e = json.Unmarshal(b, &sums); e != nil {
		return e
	}
	for _, n := range []string{"manifest.yaml", "layout.tar"} {
		got, e := checksum(filepath.Join(from, n))
		if e != nil {
			return e
		}
		if sums[n] != got {
			return fmt.Errorf("checksum mismatch: %s", n)
		}
	}
	m, e := Read(filepath.Join(from, "manifest.yaml"))
	if e != nil {
		return e
	}
	work, e := os.MkdirTemp(c.WorkDir, "aep-load-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(work)
	f, e := os.Open(filepath.Join(from, "layout.tar"))
	if e != nil {
		return e
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var total int64
	limit := c.MaxSize
	if limit == 0 {
		limit = 100 << 30
	}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if h.Size < 0 || h.Size > limit-total {
			return fmt.Errorf("bundle exceeds extraction size limit %d", limit)
		}
		total += h.Size
		p := filepath.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive entry %q", h.Name)
		}
		dst := filepath.Join(work, p)
		if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
			return e
		}
		out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, tr)
		ce := out.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	lp, e := layout.FromPath(work)
	if e != nil {
		return e
	}
	idx, e := lp.ImageIndex()
	if e != nil {
		return e
	}
	// Verify every signature before performing the first registry write.
	for _, a := range m.Artifacts {
		if e := c.requireSignature(a); e != nil {
			return e
		}
		if a.Signature != "" {
			h, e := v1.NewHash(a.Signature)
			if e != nil {
				return e
			}
			img, e := lp.Image(h)
			if e != nil {
				return e
			}
			if e = c.verify(a, img); e != nil {
				return e
			}
		}
	}
	for _, a := range m.Artifacts {
		for _, sig := range []bool{false, true} {
			digest := a.Digest
			if sig {
				digest = a.Signature
			}
			if digest == "" {
				continue
			}
			h, e := v1.NewHash(digest)
			if e != nil {
				return e
			}
			dst, e := c.destination(a, to, sig)
			if e != nil {
				return e
			}
			img, ie := lp.Image(h)
			if ie == nil {
				if e = c.put(ctx, dst, digest, img, nil); e != nil {
					return e
				}
			} else {
				sub, e := idx.ImageIndex(h)
				if e != nil {
					return fmt.Errorf("missing artifact %s: %w", digest, e)
				}
				if e = c.put(ctx, dst, digest, nil, sub); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
