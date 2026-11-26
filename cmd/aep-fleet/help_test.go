package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestTopLevelHelpListsCommands(t *testing.T) {
	oldArgs, oldOut := os.Args, os.Stdout
	defer func() { os.Args = oldArgs; os.Stdout = oldOut }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"aep-fleet", "-h"}
	os.Stdout = w
	err = run()
	_ = w.Close()
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !strings.Contains(string(data), "stage") || !strings.Contains(string(data), "join") {
		t.Fatal(string(data), err)
	}
}
