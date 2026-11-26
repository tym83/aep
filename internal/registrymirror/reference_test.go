package registrymirror

import "testing"

func TestRewrite(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"docker.io/ubuntu:24", "mirror.local/cache/docker.io/library/ubuntu:24"},
		{"index.docker.io/ubuntu:24", "mirror.local/cache/docker.io/library/ubuntu:24"},
		{"registry-1.docker.io/ubuntu@sha256:abc", "mirror.local/cache/docker.io/library/ubuntu@sha256:abc"},
		{"ubuntu:24", "mirror.local/cache/docker.io/library/ubuntu:24"},
		{"vllm/vllm-openai:v1", "mirror.local/cache/docker.io/vllm/vllm-openai:v1"},
		{"ghcr.io/a/b@sha256:abc", "mirror.local/cache/ghcr.io/a/b@sha256:abc"},
		{"oci://ghcr.io/a/chart", "oci://mirror.local/cache/ghcr.io/a/chart"},
	} {
		if got := Rewrite(tc.in, "mirror.local/cache"); got != tc.want {
			t.Fatalf("%s != %s", got, tc.want)
		}
		if got := Rewrite(tc.want, "mirror.local/cache"); got != tc.want {
			t.Fatal("not idempotent", got)
		}
	}
}
