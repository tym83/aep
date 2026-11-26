package operator

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A local OCI fixture distinguishes a missing signature from an unreachable registry.
// No external registry, signing service, or cluster is needed.
func TestProductionImageTrust(t *testing.T) {
	cli, err := exec.LookPath("kyverno")
	if err != nil {
		t.Skip("Kyverno CLI required")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm required")
	}
	config := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, configDigest, len(config)))
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		switch r.URL.Path {
		case "/v2/":
			w.Write([]byte("{}"))
		case "/v2/app/manifests/" + digest, "/v2/app/manifests/v1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
			w.Write(manifest)
		case "/v2/app/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(config)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"no signatures"}]}`))
		}
	}))
	defer registry.Close()
	image := strings.TrimPrefix(registry.URL, "http://") + "/app"
	key, err := os.ReadFile("../../policies/app-cluster-prod/tests/cosign.pub")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, namespace, reference string
		trust                      M
		extra                      []string
		reject                     bool
	}{
		{"platform-unconfigured-enforce", "kube-system", image + ":v1", M{"mode": "enforce"}, nil, false},
		{"platform-tag-audit", "kube-system", image + ":v1", nil, nil, false},
		{"addon-tag-audit", "addon-monitoring", image + ":v1", nil, []string{"addon-monitoring"}, false},
		{"unsigned-cell-digest", "dp-cell", image + "@" + digest, nil, nil, true},
		{"unsigned-user-digest", "user-apps", image + "@" + digest, nil, nil, true},
		{"platform-allowed-tag-and-digest", "kube-system", image + ":v1@" + digest, M{"mode": "enforce", "allowedImages": []string{image + "@" + digest}}, nil, false},
		{"platform-allowed-digest", "kube-system", image + "@" + digest, M{"mode": "enforce", "allowedImages": []string{image + "@" + digest}}, nil, false},
		{"platform-unlisted-tag", "kube-system", image + ":v1", M{"mode": "enforce", "allowedImages": []string{image + "@" + digest}}, nil, true},
		{"platform-unsigned-vendor", "cozy-cilium", image + "@" + digest, M{"mode": "enforce", "vendorPublicKeys": []string{string(key)}}, nil, true},
		{"platform-allowlist-or-vendor", "cozy-cilium", image + "@" + digest, M{"mode": "enforce", "vendorPublicKeys": []string{string(key)}, "allowedImages": []string{image + "@" + digest}}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			values, _ := json.Marshal(M{"cosignPublicKey": string(key), "platformNamespaces": tc.extra, "platformImageTrust": tc.trust})
			// Omitted settings retain the chart's default audit configuration.
			if tc.trust == nil {
				values, _ = json.Marshal(M{"cosignPublicKey": string(key), "platformNamespaces": tc.extra})
			}
			valuesPath := filepath.Join(dir, "values.json")
			if err := os.WriteFile(valuesPath, values, 0600); err != nil {
				t.Fatal(err)
			}
			policy, err := exec.Command("helm", "template", "test", "../../policies/app-cluster-prod", "-f", valuesPath).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, policy)
			}
			policyPath := filepath.Join(dir, "policy.yaml")
			if err := os.WriteFile(policyPath, policy, 0600); err != nil {
				t.Fatal(err)
			}
			pod, _ := json.Marshal(M{"apiVersion": "v1", "kind": "Pod", "metadata": M{"name": "app", "namespace": tc.namespace}, "spec": M{"containers": []any{M{"name": "app", "image": tc.reference}}}})
			podPath := filepath.Join(dir, "pod.json")
			if err := os.WriteFile(podPath, pod, 0600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(cli, "apply", policyPath, "--resource", podPath, "--registry", "--audit-warn", "--detailed-results").CombinedOutput()
			if (err != nil) != tc.reject {
				t.Fatalf("reject=%v err=%v\n%s", tc.reject, err, out)
			}
			if strings.HasPrefix(tc.name, "unsigned-") && !strings.Contains(string(out), "cosign-signatures") {
				t.Fatalf("unsigned digest must fail the signature rule: %s", out)
			}
			if tc.reject && !strings.Contains(string(out), "error: 0") {
				t.Fatalf("must fail policy validation, not registry access:\n%s", out)
			}
		})
	}
}
