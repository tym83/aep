package main

import "testing"

func TestCommandFlags(t *testing.T) {
	for _, args := range [][]string{nil, {"load", "--check"}, {"mirror", "--from=x"}, {"manifest", "--to=x"}, {"bundle", "--insecure"}, {"load", "--max-size=-1"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, cmd := range []string{"manifest", "mirror", "load", "bundle"} {
		if err := run([]string{cmd, "-h"}); err != nil {
			t.Fatal(err)
		}
	}
}
