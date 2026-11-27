package release

import "testing"

func TestManifestWithout(t *testing.T) {
	m := Manifest{Schema: 1, Artifacts: []Artifact{
		{Kind: "image", Ref: "index.docker.io/vllm/vllm-openai", Digest: "sha256:a"},
		{Kind: "chart", Ref: "oci://cr.kgateway.dev/kgateway-dev/charts/kgateway", Digest: "sha256:b"},
	}}
	got, left, err := m.Without([]string{"index.docker.io/vllm/vllm-openai", "oci://cr.kgateway.dev/kgateway-dev/charts/kgateway"})
	if err != nil || len(got.Artifacts) != 0 || len(left) != 2 {
		t.Fatal(got, left, err)
	}
	if len(m.Artifacts) != 2 {
		t.Fatal("Without modified the original manifest")
	}
	if _, _, err = m.Without([]string{"index.docker.io/vllm/vllm"}); err == nil {
		t.Fatal("a ref matching nothing must fail")
	}
}
