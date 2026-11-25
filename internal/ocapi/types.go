// Package ocapi is a minimal client for the OpenChoreo REST API covering the
// release binding resources the change service writes. Binding specs are kept
// as unstructured maps: the API replaces the whole spec on update without a
// precondition, so a read-modify-write round trip must not drop fields the
// client does not know about.
package ocapi

import (
	"fmt"
)

// Binding kinds the change service operates on.
const (
	KindReleaseBinding         = "ReleaseBinding"
	KindResourceReleaseBinding = "ResourceReleaseBinding"
	KindProjectReleaseBinding  = "ProjectReleaseBinding"
)

var plurals = map[string]string{
	KindReleaseBinding:         "releasebindings",
	KindResourceReleaseBinding: "resourcereleasebindings",
	KindProjectReleaseBinding:  "projectreleasebindings",
}

// Plural returns the REST resource segment for a binding kind.
func Plural(kind string) (string, error) {
	p, ok := plurals[kind]
	if !ok {
		return "", fmt.Errorf("unsupported binding kind %q", kind)
	}
	return p, nil
}

type ObjectMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"`
}

// Binding is one OpenChoreo release binding with an opaque spec.
type Binding struct {
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Metadata   ObjectMeta     `json:"metadata"`
	Spec       map[string]any `json:"spec"`
}

// Owner returns the spec.owner map, or nil.
func (b *Binding) Owner() map[string]any {
	o, _ := b.Spec["owner"].(map[string]any)
	return o
}

// ReleaseField is the spec field pinning the release for each binding kind.
func ReleaseField(kind string) string {
	switch kind {
	case KindResourceReleaseBinding:
		return "resourceRelease"
	case KindProjectReleaseBinding:
		return "projectRelease"
	default:
		return "releaseName"
	}
}

// NewBinding builds a binding for the given owner, environment and release.
// ownerKeys maps owner field names (projectName, componentName, resourceName).
func NewBinding(kind, name, environment, release string, owner map[string]any) *Binding {
	spec := map[string]any{"owner": owner, "environment": environment}
	if release != "" {
		spec[ReleaseField(kind)] = release
	}
	return &Binding{Kind: kind, Metadata: ObjectMeta{Name: name}, Spec: spec}
}

// MatchesOwner reports whether the binding belongs to the expected owner
// fields; a name collision with a foreign binding must never be written over.
func (b *Binding) MatchesOwner(owner map[string]any) bool {
	actual := b.Owner()
	for k, v := range owner {
		s, _ := actual[k].(string)
		if s != fmt.Sprint(v) {
			return false
		}
	}
	return true
}
