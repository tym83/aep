package release

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// PublishCharts uploads the exact deterministic OCI bytes used by Generate.
// Existing versions with different content fail through put's immutable policy.
func (c *Client) PublishCharts(ctx context.Context, root string, output io.Writer) error {
	for _, base := range []string{"charts", "policies"} {
		paths, err := filepath.Glob(filepath.Join(root, base, "*", "Chart.yaml"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			a, err := PackageChart(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			a.Layout = filepath.Join(root, a.Layout)
			d, err := c.fetch(ctx, a, false)
			if err != nil {
				return err
			}
			repository := strings.TrimPrefix(a.Ref, "oci://")
			if c.PublishRegistry != "" {
				repository = strings.Replace(repository, "ghcr.io/tym83/aep", strings.TrimRight(c.PublishRegistry, "/"), 1)
			}
			dst, err := c.ref(repository + ":" + a.Version)
			if err != nil {
				return err
			}
			img, err := d.Image()
			if err != nil {
				return err
			}
			if err := c.put(ctx, dst, a.Digest, img, nil); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(output, "%s@%s\n", dst.Context().Name(), a.Digest); err != nil {
				return err
			}
		}
	}
	return nil
}

// DiscoverSignatures records signatures published after local chart packaging.
func (c *Client) DiscoverSignatures(ctx context.Context, m *Manifest) error {
	for i := range m.Artifacts {
		if m.Artifacts[i].Digest == "" {
			return fmt.Errorf("resolve build placeholders before discovering signatures")
		}
		if err := c.discoverSignature(ctx, &m.Artifacts[i]); err != nil {
			return err
		}
	}
	return nil
}
