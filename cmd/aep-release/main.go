// aep-release prepares immutable inventories and disconnected registry transfers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/release"
	"os"
	"sigs.k8s.io/yaml"
)

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("Usage: aep-release <manifest|mirror|bundle|load> [flags]")
		return nil
	}
	cmd := args[0]
	f := flag.NewFlagSet("aep-release "+cmd, flag.ContinueOnError)
	path := f.String("manifest", "release/manifest.yaml", "Release manifest path")
	root := f.String("root", ".", "Repository root for manifest generation")
	check := f.Bool("check", false, "Verify manifest inputs without network access")
	to := f.String("to", "", "Destination registry[/prefix]")
	out := f.String("out", "", "Bundle directory")
	from := f.String("from", "", "Bundle directory")
	c := &release.Client{}
	f.BoolVar(&c.Insecure, "insecure", false, "Use HTTP for registry access (trusted networks only)")
	f.StringVar(&c.CA, "ca", "", "PEM registry CA bundle")
	f.StringVar(&c.Key, "key", "", "Trusted cosign public key PEM")
	if e := f.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if e := c.Init(); e != nil {
		return e
	}
	ctx := context.Background()
	if cmd == "manifest" {
		if *check {
			return release.Check(*root, *path)
		}
		m, e := release.Generate(ctx, *root, c)
		if e != nil {
			return e
		}
		b, e := yaml.Marshal(m)
		if e != nil {
			return e
		}
		return os.WriteFile(*path, b, 0644)
	}
	if cmd == "load" {
		return c.Load(ctx, *from, *to)
	}
	if cmd != "mirror" && cmd != "bundle" {
		return fmt.Errorf("unknown command %q", cmd)
	}
	m, e := release.Read(*path)
	if e != nil {
		return e
	}
	if cmd == "mirror" {
		return c.Mirror(ctx, m, *to)
	}
	return c.Bundle(ctx, m, *out)
}
