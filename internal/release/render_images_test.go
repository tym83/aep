package release

import (
	"bytes"
	inventory "github.com/tym83/aep/release"
	"strings"
	"testing"
)

func TestPostRendererPinsAndMirrorsEveryContainer(t *testing.T) {
	var pin inventory.Artifact
	for _, a := range inventory.Artifacts() {
		if a.Kind == "image" {
			pin = a
			break
		}
	}
	input := `apiVersion: apps/v1
kind: Deployment
metadata: {name: test}
spec:
  template:
    spec:
      initContainers:
        - {name: init, image: ` + pin.Ref + ":" + pin.Version + `}
      containers:
        - {name: app, image: ` + pin.Ref + ":" + pin.Version + `}
`
	var out bytes.Buffer
	if err := RenderImages(strings.NewReader(input), &out, "mirror.local"); err != nil {
		t.Fatal(err)
	}
	images, err := Images(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || !strings.HasPrefix(images[0], "mirror.local/") || !strings.HasSuffix(images[0], "@"+pin.Digest) {
		t.Fatal(images)
	}
	out.Reset()
	if err = RenderImages(strings.NewReader(strings.ReplaceAll(input, pin.Ref, "unknown.invalid/missing")), &out, "mirror.local"); err == nil {
		t.Fatal("unknown image escaped inventory")
	}
}
