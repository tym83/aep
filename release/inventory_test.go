package inventory

import (
	"strings"
	"testing"
)

func TestImagePins(t *testing.T) {
	for _, a := range Artifacts() {
		if a.Version == "main" || a.Version == "latest" || a.Version == "latest-dev" {
			t.Fatal("mutable inventory tag", a)
		}
		if a.Kind != "image" || strings.HasPrefix(a.Version, "sha256:") {
			continue
		}
		got, err := Image(a.Ref + ":" + a.Version)
		if err != nil || got != a.Ref+":"+a.Version+"@"+a.Digest {
			t.Fatal(got, err)
		}
	}
	if _, err := Image("example.org/missing:v1"); err == nil {
		t.Fatal("unknown image accepted")
	}
}
