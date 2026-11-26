// Package inventory embeds the reviewed release pins into the operator binary.
package inventory

import (
	_ "embed"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

//go:embed manifest.yaml
var data []byte

type Artifact struct {
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
	a, err := Lookup("image", repo, version)
	if err != nil {
		return "", err
	}
	if suffix != "" && suffix != a.Digest {
		return "", fmt.Errorf("image digest differs from release inventory: %s", ref)
	}
	return a.Ref + ":" + a.Version + "@" + a.Digest, nil
}
