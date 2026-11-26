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
	"regexp"
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
		if !immutableVersion(a.Version) {
			return m, fmt.Errorf("mutable release tag for %s: %s", a.Ref, a.Version)
		}
		if a.Kind != "image" && a.Kind != "chart" {
			return m, fmt.Errorf("unknown kind %q", a.Kind)
		}
	}
	return m, nil
}
func Fingerprint(root string) (string, error) {
	h := sha256.New()
	cmd := exec.Command("git", "ls-files", "-z", "--", "charts", "policies", "internal/operator", "internal/registrymirror", "internal/release", "internal/bridge", "resource-types", "config", "cmd/operator", "cmd/aep-release", "Dockerfile*", "portal/Dockerfile", ".github/workflows", "release/extra-images.yaml", "release/*.go")
	cmd.Dir = root
	tracked, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tracked release inputs: %w", err)
	}
	paths := strings.Split(strings.TrimSuffix(string(tracked), "\x00"), "\x00")
	sort.Strings(paths)
	for _, rel := range paths {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\x00", filepath.ToSlash(rel), b)
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
	if err := CheckCharts(root, m); err != nil {
		return err
	}
	return CheckImages(context.Background(), root, m)
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
		if c.Name == "argo-cd" {
			a, path, cleanup, err := packageArgoChart(ctx, root)
			if err != nil {
				return m, err
			}
			defer cleanup()
			artifacts[a.Ref] = a
			b, err := render(ctx, path, "", operator.ReleaseValues(c.Name))
			if err != nil {
				return m, err
			}
			list, err := Images(b)
			if err != nil {
				return m, err
			}
			for _, image := range list {
				images[image] = true
			}
			continue
		}
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
			if err := client.discoverSignature(ctx, &a); err != nil {
				return m, err
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
	return map[string]any{"dev": false, "organizationUID": "inventory", "cosignPublicKey": "inventory", "organization": "inventory", "clusterID": "inventory", "model": "inventory", "apiKeySecret": "inventory", "secretName": "inventory", "organizations": []string{"inventory"}, "aepcpKubeconfigSecret": "inventory/access", "databaseSecret": "inventory", "credentialsSecret": "inventory", "config": map[string]any{"organization": "inventory", "changeNamespace": "tenant-inventory"}, "oidc": map[string]any{"issuer": "https://id.invalid", "audience": "inventory"}, "openchoreo": map[string]any{"apiUrl": "https://oc.invalid", "tokenUrl": "https://id.invalid/token"}, "llm": map[string]any{"modelName": "inventory", "baseUrl": "http://llm.invalid/v1"}, "oauth": map[string]any{"tokenUrl": "https://id.invalid/token"}, "jwt": map[string]any{"issuer": "https://id.invalid", "audience": "inventory", "jwksUrl": "https://id.invalid/keys"}}
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
		if tagged, _, ok := strings.Cut(raw, "@"); ok {
			if tag, err := name.NewTag(tagged); err == nil && strings.LastIndex(tagged, ":") > strings.LastIndex(tagged, "/") {
				a.Version = tag.TagStr()
			}
		}
		if !immutableVersion(a.Version) {
			return a, fmt.Errorf("mutable release tag: %s", raw)
		}
	}
	if err := c.discoverSignature(ctx, &a); err != nil {
		return a, err
	}
	return a, nil
}
func (c *Client) discoverSignature(ctx context.Context, a *Artifact) error {
	r, err := c.ref(strings.TrimPrefix(a.Ref, "oci://") + "@" + a.Digest)
	if err != nil {
		return err
	}
	sig := r.Context().Tag("sha256-" + strings.TrimPrefix(a.Digest, "sha256:") + ".sig")
	sd, err := remote.Get(sig, c.options(ctx)...)
	if err == nil {
		a.Signature = sd.Digest.String()
	} else if !notFound(err) {
		return fmt.Errorf("discover signature %s: %w", sig, err)
	}
	return nil
}

// CheckImages renders only local paths. A missing upstream cache is an error,
// never an excuse to skip the inventory comparison.
func CheckImages(ctx context.Context, root string, m Manifest) error {
	actual := map[string]bool{}
	collect := func(path string, values map[string]any) error {
		data, err := render(ctx, path, "", values)
		if err != nil {
			return err
		}
		list, err := Images(data)
		if err != nil {
			return err
		}
		for _, ref := range list {
			actual[ref] = true
		}
		return nil
	}
	for _, chart := range operator.DeliveryCharts() {
		if err := collect(filepath.Join(root, "release/.cache", chart.Name), operator.ReleaseValues(chart.Name)); err != nil {
			return err
		}
	}
	for _, base := range []string{"charts", "policies"} {
		paths, err := filepath.Glob(filepath.Join(root, base, "*", "Chart.yaml"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if err := collect(filepath.Dir(path), LocalValues(filepath.Base(filepath.Dir(path)))); err != nil {
				return err
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "release/extra-images.yaml"))
	if err != nil {
		return err
	}
	var extra []string
	if err := yaml.UnmarshalStrict(b, &extra); err != nil {
		return err
	}
	for _, ref := range extra {
		actual[ref] = true
	}
	expected := map[string]bool{}
	for _, a := range m.Artifacts {
		if a.Kind != "image" {
			continue
		}
		expected[a.Ref+":"+a.Version] = true
		expected[a.Ref+"@"+a.Digest] = true
		expected[a.Ref+":"+a.Version+"@"+a.Digest] = true
	}
	used := map[string]bool{}
	for ref := range actual {
		tagged, _, _ := strings.Cut(ref, "@")
		if pos := strings.LastIndex(tagged, ":"); pos > strings.LastIndex(tagged, "/") && !immutableVersion(tagged[pos+1:]) {
			return fmt.Errorf("rendered mutable image tag: %s", ref)
		}
		r, err := name.ParseReference(ref)
		if err != nil {
			return err
		}
		normalized := r.Name()
		if !expected[normalized] {
			return fmt.Errorf("rendered image missing from inventory: %s", ref)
		}
		used[r.Context().Name()+"@"+r.Identifier()] = true
		used[normalized] = true
	}
	for _, a := range m.Artifacts {
		if a.Kind != "image" {
			continue
		}
		if !used[a.Ref+":"+a.Version] && !used[a.Ref+"@"+a.Digest] {
			return fmt.Errorf("inventory image is no longer rendered: %s:%s", a.Ref, a.Version)
		}
	}
	return nil
}

// Digest-only references have no mutable tag. Tagged references must identify a
// source commit or a release, not an arbitrary moving development channel.
var immutableTag = regexp.MustCompile(`^(sha-[a-f0-9]{7,40}|v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?|sha256:[a-f0-9]{64})$`)

func immutableVersion(version string) bool {
	return version == "" || immutableTag.MatchString(version)
}

// Fingerprints detect source drift; inventory completeness must also reject
// accidentally removed chart entries in an otherwise current manifest.
func CheckCharts(root string, m Manifest) error {
	expected := map[string]string{}
	for _, c := range operator.DeliveryCharts() {
		expected[c.URL] = c.Version
	}
	for _, base := range []string{"charts", "policies"} {
		paths, err := filepath.Glob(filepath.Join(root, base, "*", "Chart.yaml"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var c struct {
				Name    string
				Version string
			}
			if err := yaml.Unmarshal(data, &c); err != nil {
				return err
			}
			expected["oci://ghcr.io/tym83/aep/"+base+"/"+c.Name] = c.Version
		}
	}
	for _, a := range m.Artifacts {
		if a.Kind != "chart" {
			continue
		}
		if version, ok := expected[a.Ref]; !ok || version != a.Version {
			return fmt.Errorf("unexpected chart or version: %s:%s", a.Ref, a.Version)
		}
		delete(expected, a.Ref)
	}
	if len(expected) > 0 {
		return fmt.Errorf("charts missing from inventory: %v", expected)
	}
	return nil
}
