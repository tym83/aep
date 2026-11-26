// Package release inventories and transports immutable release artifacts.
package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/tym83/aep/internal/operator"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

type Artifact struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	Version   string `json:"version,omitempty"`
	Digest    string `json:"digest"`
	Signature string `json:"signature,omitempty"`
	Layout    string `json:"layout,omitempty"`
}
type Manifest struct {
	Schema    int        `json:"schema"`
	Inputs    string     `json:"inputs"`
	Artifacts []Artifact `json:"artifacts"`
}

func Read(path string) (Manifest, error) {
	var m Manifest
	b, e := os.ReadFile(path)
	if e != nil {
		return m, e
	}
	e = yaml.UnmarshalStrict(b, &m)
	if e != nil {
		return m, e
	}
	if m.Schema != 1 || len(m.Artifacts) == 0 {
		return m, fmt.Errorf("empty or unsupported manifest")
	}
	for _, a := range m.Artifacts {
		if _, e = name.NewDigest(strings.TrimPrefix(a.Ref, "oci://") + "@" + a.Digest); e != nil {
			return m, e
		}
		if a.Kind != "image" && a.Kind != "chart" {
			return m, fmt.Errorf("unknown kind %q", a.Kind)
		}
	}
	return m, nil
}
func Fingerprint(root string) (string, error) {
	h := sha256.New()
	for _, dir := range []string{"charts", "policies", "internal/operator", "internal/registrymirror", "internal/release", "cmd/operator", "portal/Dockerfile", ".github/workflows", "release/extra-images.yaml"} {
		err := filepath.Walk(filepath.Join(root, dir), func(p string, i os.FileInfo, e error) error {
			if e != nil {
				return e
			}
			if i.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(root, p)
			fmt.Fprintf(h, "%s\x00%s\x00", rel, b)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func Check(root, path string) error {
	m, e := Read(path)
	if e != nil {
		return e
	}
	h, e := Fingerprint(root)
	if e != nil {
		return e
	}
	if m.Inputs != h {
		return fmt.Errorf("release manifest is stale: run aep-release manifest after publishing artifacts")
	}

	// Verify committed local OCI blobs, not just the source fingerprint.
	for _, a := range m.Artifacts {
		if a.Layout == "" {
			continue
		}
		lp := filepath.Join(root, a.Layout)
		if e := filepath.Walk(filepath.Join(lp, "blobs", "sha256"), func(p string, i os.FileInfo, e error) error {
			if e != nil {
				return e
			}
			if i.IsDir() {
				return nil
			}
			got, e := checksum(p)
			if e != nil {
				return e
			}
			if got != i.Name() {
				return fmt.Errorf("corrupt OCI blob %s", p)
			}
			return nil
		}); e != nil {
			return e
		}
		c := &Client{}
		a.Layout = lp
		if _, e := c.fetch(context.Background(), a, false); e != nil {
			return e
		}
	}
	return nil
}

// Images walks all documents, including hooks, init containers and custom resources.
func Images(data []byte) ([]string, error) {
	seen := map[string]bool{}
	dec := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "containers" || k == "initContainers" || k == "ephemeralContainers" {
					if list, ok := v.([]any); ok {
						for _, item := range list {
							if container, ok := item.(map[string]any); ok {
								if s, ok := container["image"].(string); ok && s != "" {
									seen[s] = true
								}
							}
						}
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	for {
		var v map[string]any
		e := dec.Decode(&v)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		walk(v)
	}
	out := []string{}
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}
func render(ctx context.Context, chart, version string, values map[string]any) ([]byte, error) {
	b, e := json.Marshal(values)
	if e != nil {
		return nil, e
	}
	args := []string{"template", "aep", chart, "--include-crds", "--namespace", "aep-system", "--values", "-"}
	if version != "" {
		args = append(args, "--version", version)
	}
	c := exec.CommandContext(ctx, "helm", args...)
	c.Stdin = bytes.NewReader(b)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, e := c.Output()
	if e != nil {
		return nil, fmt.Errorf("render %s: %w: %s", chart, e, stderr.String())
	}
	return out, nil
}
func Generate(ctx context.Context, root string, client *Client) (Manifest, error) {
	m := Manifest{Schema: 1}
	h, e := Fingerprint(root)
	if e != nil {
		return m, e
	}
	m.Inputs = h
	artifacts := map[string]Artifact{}
	images := map[string]bool{}
	addChart := func(ref, version, path string, values map[string]any) error {
		a, e := client.Resolve(ctx, "chart", ref, version)
		if e != nil {
			return e
		}
		artifacts[ref] = a
		// Fetch by the resolved digest when rendering remotely, preventing tag races.
		if strings.HasPrefix(path, "oci://") {
			path = path + "@" + a.Digest
			version = ""
		}
		b, e := render(ctx, path, version, values)
		if e != nil {
			return e
		}
		list, e := Images(b)
		if e != nil {
			return e
		}
		for _, s := range list {
			images[s] = true
		}
		return nil
	}
	for _, c := range operator.DeliveryCharts() {
		values := operator.ReleaseValues(c.Name)
		if e = addChart(c.URL, c.Version, c.URL, values); e != nil {
			return m, e
		}
	}
	for _, base := range []string{"charts", "policies"} {
		paths, e := filepath.Glob(filepath.Join(root, base, "*", "Chart.yaml"))
		if e != nil {
			return m, e
		}
		for _, p := range paths {
			b, e := os.ReadFile(p)
			if e != nil {
				return m, e
			}
			var c struct {
				Name    string
				Version string
			}
			if e = yaml.Unmarshal(b, &c); e != nil {
				return m, e
			}
			values := LocalValues(c.Name)
			a, e := PackageChart(root, filepath.Dir(p))
			if e != nil {
				return m, e
			}
			artifacts[a.Ref] = a
			rendered, e := render(ctx, filepath.Dir(p), "", values)
			if e != nil {
				return m, e
			}
			list, e := Images(rendered)
			if e != nil {
				return m, e
			}
			for _, s := range list {
				images[s] = true
			}
		}
	}
	b, e := os.ReadFile(filepath.Join(root, "release/extra-images.yaml"))
	if e != nil {
		return m, e
	}
	var extra []string
	if e = yaml.UnmarshalStrict(b, &extra); e != nil {
		return m, e
	}
	for _, s := range extra {
		images[s] = true
	}
	ordered := make([]string, 0, len(images))
	for s := range images {
		ordered = append(ordered, s)
	}
	sort.Strings(ordered)
	for _, s := range ordered {
		fmt.Fprintln(os.Stderr, "Resolving", s)
		a, e := client.Resolve(ctx, "image", s, "")
		if e != nil {
			return m, e
		}
		artifacts[s] = a
	}
	for _, a := range artifacts {
		m.Artifacts = append(m.Artifacts, a)
	}
	sort.Slice(m.Artifacts, func(i, j int) bool {
		if m.Artifacts[i].Ref == m.Artifacts[j].Ref {
			return m.Artifacts[i].Version < m.Artifacts[j].Version
		}
		return m.Artifacts[i].Ref < m.Artifacts[j].Ref
	})
	if e = compactLayout(root, m); e != nil {
		return m, e
	}
	return m, nil
}
func LocalValues(chart string) map[string]any {
	return map[string]any{"dev": true, "organizationUID": "inventory", "cosignPublicKey": "inventory", "organization": "inventory", "clusterID": "inventory", "model": "inventory", "apiKeySecret": "inventory", "secretName": "inventory", "organizations": []string{"inventory"}, "aepcpKubeconfigSecret": "inventory/access", "databaseSecret": "inventory", "credentialsSecret": "inventory", "oidc": map[string]any{"issuer": "https://id.invalid", "audience": "inventory"}, "openchoreo": map[string]any{"apiUrl": "https://oc.invalid", "tokenUrl": "https://id.invalid/token"}, "llm": map[string]any{"modelName": "inventory", "baseUrl": "http://llm.invalid/v1"}, "oauth": map[string]any{"tokenUrl": "https://id.invalid/token"}, "jwt": map[string]any{"issuer": "https://id.invalid", "audience": "inventory", "jwksUrl": "https://id.invalid/keys"}}
}
func (c *Client) Resolve(ctx context.Context, kind, ref, version string) (Artifact, error) {
	raw := strings.TrimPrefix(ref, "oci://")
	if version != "" {
		raw += ":" + version
	}
	r, e := c.ref(raw)
	if e != nil {
		return Artifact{}, e
	}
	d, e := remote.Get(r, c.options(ctx)...)
	if e != nil {
		return Artifact{}, fmt.Errorf("resolve %s: %w", raw, e)
	}
	a := Artifact{Kind: kind, Ref: r.Context().Name(), Version: version, Digest: d.Digest.String()}
	if kind == "chart" {
		a.Ref = "oci://" + a.Ref
	} else {
		a.Version = r.Identifier()
	}
	sig := r.Context().Tag("sha256-" + d.Digest.Hex + ".sig")
	sd, e := remote.Get(sig, c.options(ctx)...)
	if e == nil {
		a.Signature = sd.Digest.String()
	} else if !notFound(e) {
		return a, fmt.Errorf("discover signature %s: %w", sig, e)
	}
	return a, nil
}
