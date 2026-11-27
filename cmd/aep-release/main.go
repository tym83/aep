// aep-release prepares immutable inventories and disconnected registry transfers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tym83/aep/internal/release"
	"sigs.k8s.io/yaml"
)

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: manifest, mirror, bundle, load, publish-charts")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Println("Usage: aep-release <manifest|mirror|bundle|load|publish-charts|render-images> [flags]")
		return nil
	}
	cmd := args[0]
	if cmd == "render-images" {
		fs := flag.NewFlagSet("aep-release render-images", flag.ContinueOnError)
		mirror := fs.String("registry-mirror", "", "Mirror prefix")
		if err := fs.Parse(args[1:]); err != nil {
			if err == flag.ErrHelp {
				return nil
			}
			return err
		}
		return release.RenderImages(os.Stdin, os.Stdout, *mirror)
	}
	if cmd != "manifest" && cmd != "mirror" && cmd != "bundle" && cmd != "load" && cmd != "publish-charts" {
		return fmt.Errorf("unknown command %q", cmd)
	}
	f := flag.NewFlagSet("aep-release "+cmd, flag.ContinueOnError)
	var path, root, to, out, from string
	var check, refresh, signatures bool
	var buildVersion string
	var skip []string
	c := &release.Client{Keys: map[string]string{}}
	if cmd != "load" && cmd != "publish-charts" {
		f.StringVar(&path, "manifest", "release/manifest.yaml", "Manifest path relative to --root")
	}
	f.StringVar(&root, "root", ".", "Base directory for repository and local OCI layouts")
	if cmd == "publish-charts" {
		f.StringVar(&c.PublishRegistry, "chart-registry", "ghcr.io/tym83/aep", "Chart registry and AEP prefix")
	}
	if cmd == "manifest" || cmd == "mirror" || cmd == "bundle" {
		f.StringVar(&buildVersion, "build-version", "", "Commit or release tag for own build placeholders")
	}
	if cmd == "manifest" {
		f.BoolVar(&signatures, "discover-signatures", false, "Record signatures after publishing local charts")
		f.BoolVar(&refresh, "refresh-local", false, "Repackage local charts while retaining upstream pins")
		f.BoolVar(&check, "check", false, "Verify tracked inputs and rendered images offline")
	}
	if cmd == "mirror" || cmd == "load" {
		f.StringVar(&to, "to", "", "Destination registry[/prefix]")
		f.BoolVar(&c.AllowRetag, "allow-retag", false, "Permit replacing an existing tag with different content")
	}
	if cmd == "bundle" {
		f.StringVar(&out, "out", "", "Bundle output directory")
	}
	if cmd == "mirror" || cmd == "bundle" {
		f.Func("skip", "Artifact ref to leave out, e.g. index.docker.io/vllm/vllm-openai (repeatable)", func(s string) error {
			skip = append(skip, s)
			return nil
		})
	}
	if cmd == "load" {
		f.StringVar(&from, "from", "", "Bundle directory")
		f.StringVar(&c.WorkDir, "work-dir", "", "Extraction parent directory; defaults to system temporary directory")
		f.Int64Var(&c.MaxSize, "max-size", 100<<30, "Maximum total extracted bytes")
	}
	f.StringVar(&c.InsecureRegistry, "insecure-registry", "", "Exact registry host[:port] allowed to use HTTP")
	f.StringVar(&c.CA, "ca", "", "PEM registry CA bundle")
	if cmd != "manifest" && cmd != "publish-charts" {
		f.StringVar(&c.Key, "key", "", "Trusted cosign public key; requires signatures for every artifact")
		f.StringVar(&c.Verify, "verify", "", "Comma-separated repository prefixes requiring trusted signatures")
		f.Func("trust-key", "Trusted prefix=public-key-file (repeatable)", func(s string) error {
			prefix, key, ok := strings.Cut(s, "=")
			if !ok || prefix == "" || key == "" {
				return fmt.Errorf("expected prefix=key-file")
			}
			c.Keys[prefix] = key
			return nil
		})
	}
	if e := f.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if c.MaxSize < 0 {
		return fmt.Errorf("--max-size must be positive")
	}
	if e := c.Init(); e != nil {
		return e
	}
	ctx := context.Background()
	if cmd == "publish-charts" {
		return c.PublishCharts(ctx, root, os.Stdout)
	}
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if cmd == "manifest" {
		if check {
			return release.Check(root, path)
		}
		var m release.Manifest
		var e error
		if refresh || signatures || buildVersion != "" {
			m, e = release.Read(path)
			if e == nil && buildVersion != "" {
				m, e = c.ResolveOwn(ctx, m, buildVersion)
			}
			if e == nil && buildVersion != "" {
				e = release.WriteBuildValues(root, m, buildVersion)
			}
			if e == nil {
				m, e = release.RefreshLocal(root, m)
			}
		} else {
			m, e = release.Generate(ctx, root, c)
		}
		if e != nil {
			return e
		}
		if signatures {
			if e = c.DiscoverSignatures(ctx, &m); e != nil {
				return e
			}
		}
		b, e := yaml.Marshal(m)
		if e != nil {
			return e
		}
		return os.WriteFile(path, b, 0644)
	}
	if cmd == "load" {
		return c.Load(ctx, from, to)
	}
	m, e := release.Read(path)
	if e != nil {
		return e
	}
	m, e = c.ResolveOwn(ctx, m, buildVersion)
	if e != nil {
		return e
	}
	m, skipped, e := m.Without(skip)
	if e != nil {
		return e
	}
	for _, a := range skipped {
		fmt.Fprintf(os.Stderr, "skipped %s %s@%s: the destination will not hold it\n", a.Kind, a.Ref, a.Digest)
	}
	for i := range m.Artifacts {
		if m.Artifacts[i].Layout != "" && !filepath.IsAbs(m.Artifacts[i].Layout) {
			m.Artifacts[i].Layout = filepath.Join(root, m.Artifacts[i].Layout)
		}
	}
	if cmd == "mirror" {
		return c.Mirror(ctx, m, to)
	}
	return c.Bundle(ctx, m, out)
}
