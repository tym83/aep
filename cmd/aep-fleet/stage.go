package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/operator"
	"github.com/tym83/aep/internal/registrymirror"
	inventory "github.com/tym83/aep/release"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Stage only immutable release charts, using the same upstream-host-preserving mirror layout.
func stage(args []string) error {
	fs := flag.NewFlagSet("aep-fleet stage", flag.ContinueOnError)
	dir := fs.String("directory", "", "Destination for pinned chart archives")
	mirror := fs.String("registry-mirror", "", "Air-gap registry prefix")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *dir == "" {
		return fmt.Errorf("directory required")
	}
	if err := os.MkdirAll(*dir, 0755); err != nil {
		return err
	}
	names := map[string]bool{"aep-bridge-appcluster": true, "app-cluster-baseline": true, "app-cluster-nonprod": true, "app-cluster-prod": true}
	for _, c := range operator.DeliveryCharts() {
		// Fleet clusters never receive Argo CD or control plane charts.
		if c.Name != "argo-cd" && c.Target != operator.TargetControlPlane {
			names[c.Name] = true
		}
	}
	for _, a := range inventory.Artifacts() {
		name := filepath.Base(a.Ref)
		if a.Kind != "chart" || !names[name] {
			continue
		}
		cmd := exec.CommandContext(context.Background(), "helm", "pull", registrymirror.Rewrite(a.Ref, *mirror)+"@"+a.Digest, "--destination", *dir)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		// OCI Helm archives normalize the leading version v.
		if strings.HasPrefix(a.Version, "v") {
			src := filepath.Join(*dir, name+"-"+strings.TrimPrefix(a.Version, "v")+".tgz")
			dst := filepath.Join(*dir, name+"-"+a.Version+".tgz")
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
		delete(names, name)
	}
	if len(names) > 0 {
		return fmt.Errorf("fleet charts missing from inventory: %v", names)
	}
	return nil
}
