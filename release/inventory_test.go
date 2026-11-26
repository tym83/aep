package inventory

import (
	"github.com/tym83/aep/internal/version"
	"strings"
	"testing"
)

func TestImagePins(t *testing.T) {
	for _, a := range Artifacts() {
		if a.Version == "main" || a.Version == "latest" || a.Version == "latest-dev" {
			t.Fatal("mutable inventory tag", a)
		}
		if a.Own || a.Kind != "image" || strings.HasPrefix(a.Version, "sha256:") {
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

func TestOwnReleaseDigest(t *testing.T) {
	oldData, oldVersion := data, version.Version
	defer func() { data, version.Version = oldData, oldVersion }()
	version.Version = "v1.2.3"
	digest := "sha256:" + strings.Repeat("a", 64)
	data = []byte(`{"artifacts":[{"kind":"image","own":true,"ref":"example.org/own","version":"v1.2.3","digest":"` + digest + `"}]}`)
	got, err := Image("example.org/own:v1.2.3")
	if err != nil || got != "example.org/own:v1.2.3@"+digest {
		t.Fatal(got, err)
	}
	if _, err := Image("example.org/own:v1.2.3@sha256:" + strings.Repeat("b", 64)); err == nil {
		t.Fatal("different release digest accepted")
	}
}
