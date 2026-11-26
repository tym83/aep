package main

import (
	"flag"
	"testing"
)

func TestNoImplicitKubeconfigFlag(t *testing.T) {
	if flag.CommandLine.Lookup("kubeconfig") != nil {
		t.Fatal("dependency registered unwanted kubeconfig flag")
	}
}
