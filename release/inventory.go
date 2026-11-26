// Package inventory embeds the reviewed release pins into the operator binary.
package inventory

import (
	_ "embed"
	"fmt"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/tym83/aep/internal/version"
	"strings"

	"sigs.k8s.io/yaml"
)

//go:embed manifest.yaml
var data []byte

type Artifact struct {
	Own     bool   `json:"own,omitempty"`
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

func Artifacts() []Artifact {
	var m struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	if err := yaml.UnmarshalStrict(data, &map[string]any{}); err != nil {
		panic(err)
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	return m.Artifacts
}
func Lookup(kind, ref, version string) (Artifact, error) {
	for _, a := range Artifacts() {
		if a.Kind == kind && a.Ref == ref && (version == "" || version == a.Version || version == a.Digest) {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("artifact absent from release inventory: %s %s %s", kind, ref, version)
}
func Image(ref string) (string, error) {
	repo, suffix, _ := strings.Cut(ref, "@")
	pos := strings.LastIndex(repo, ":")
	version := suffix
	if pos > strings.LastIndex(repo, "/") {
		version = repo[pos+1:]
		repo = repo[:pos]
	}
	for _, a := range Artifacts() {
		if a.Kind != "image" || !a.Own || a.Ref != repo {
			continue
		}
		if pos <= strings.LastIndex(repo, "/") || version == "" {
			return "", fmt.Errorf("own image requires a tag: %s", ref)
		}
		if _, err := name.NewTag(repo+":"+version, name.StrictValidation); err != nil {
			return "", err
		}
		if suffix != "" {
			if _, err := name.NewDigest(repo + "@" + suffix); err != nil {
				return "", err
			}
			if a.Version == version && a.Digest != "" && a.Digest != suffix {
				return "", fmt.Errorf("image digest differs from release inventory: %s", ref)
			}
			return ref, nil
		}
		if version != buildVersion() {
			return "", fmt.Errorf("own image version differs from operator build: %s", ref)
		}
		if a.Version == version && a.Digest != "" {
			return ref + "@" + a.Digest, nil
		}
		return ref, nil
	}
	a, err := Lookup("image", repo, version)
	if err != nil {
		return "", err
	}
	if suffix != "" && suffix != a.Digest {
		return "", fmt.Errorf("image digest differs from release inventory: %s", ref)
	}
	return a.Ref + ":" + a.Version + "@" + a.Digest, nil
}

func buildVersion() string { return version.Version }
