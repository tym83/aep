package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestParseFlagsControlPlaneSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		err  string
	}{
		{"legacy secret", []string{"--aepcp-kubeconfig-secret=tenant-aep-cp/admin"}, ""},
		{"legacy file", []string{"--aepcp-kubeconfig=/tmp/cp"}, ""},
		{"no source", nil, "--default-control-plane"},
		{"both sources", []string{"--aepcp-kubeconfig-secret=a/b", "--aepcp-kubeconfig=/tmp/cp"}, "at most one"},
		{"default only", []string{"--default-control-plane=default"}, ""},
		{"default with legacy", []string{"--default-control-plane=default", "--aepcp-kubeconfig-secret=a/b"}, ""},
		{"default with both", []string{"--default-control-plane=default", "--aepcp-kubeconfig-secret=a/b", "--aepcp-kubeconfig=/tmp/cp"}, "at most one"},
		{"invalid default", []string{"--default-control-plane=Not_Valid"}, "ControlPlane name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("operator", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			r, _, err := parseFlags(fs, tc.args)
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range tc.args {
					if v, ok := strings.CutPrefix(a, "--default-control-plane="); ok && r.Options.DefaultControlPlane != v {
						t.Fatalf("default control plane %q", r.Options.DefaultControlPlane)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err=%v, want %q", err, tc.err)
			}
		})
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	r, _, err := parseFlags(fs, []string{"--aepcp-kubeconfig-secret=a/b"})
	if err != nil {
		t.Fatal(err)
	}
	o := r.Options
	if o.DefaultControlPlane != "" || o.CPNamespace != "tenant-aep-cp" || o.CPCluster != "aepcp" || o.Namespace != "aep-system" || !o.TenantEgressWorkaround {
		t.Fatalf("legacy defaults changed: %+v", o)
	}
}
