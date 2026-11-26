// Package cpbackup implements portable logical backups, not UID-preserving etcd recovery.
package cpbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const Version = "aep.cpbackup/v1"

type Entry struct {
	Resource schema.GroupVersionResource `json:"resource"`
	Object   *unstructured.Unstructured  `json:"object"`
	SHA256   string                      `json:"sha256"`
}
type Skipped struct {
	Resource  schema.GroupVersionResource `json:"resource"`
	Namespace string                      `json:"namespace,omitempty"`
	Name      string                      `json:"name,omitempty"`
	Reason    string                      `json:"reason"`
}
type Payload struct {
	Skipped       []Skipped `json:"skipped,omitempty"`
	Version       string    `json:"version"`
	Created       string    `json:"created"`
	Organizations []string  `json:"organizations"`
	Entries       []Entry   `json:"entries"`
}
type Archive struct {
	Payload Payload `json:"payload"`
	SHA256  string  `json:"sha256"`
}

func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func IsOpenChoreo(group string) bool {
	return group == "openchoreo.dev" || strings.HasSuffix(group, ".openchoreo.dev")
}
func Clean(u *unstructured.Unstructured) *unstructured.Unstructured {
	u = u.DeepCopy()
	delete(u.Object, "status")
	for _, f := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "ownerReferences", "finalizers", "selfLink", "generateName"} {
		unstructured.RemoveNestedField(u.Object, "metadata", f)
	}
	a := u.GetAnnotations()
	delete(a, "kubectl.kubernetes.io/last-applied-configuration")
	u.SetAnnotations(a)
	return u
}
func New(orgs []string, entries []Entry) Archive {
	orgs = append([]string(nil), orgs...)
	sort.Strings(orgs)
	for i := range entries {
		entries[i].Object = Clean(entries[i].Object)
		entries[i].SHA256 = hash(entries[i].Object)
	}
	sort.Slice(entries, func(i, j int) bool { return identity(entries[i]) < identity(entries[j]) })
	p := Payload{Version: Version, Created: time.Now().UTC().Format(time.RFC3339Nano), Organizations: orgs, Entries: entries}
	return Archive{p, hash(p)}
}
func identity(e Entry) string {
	return e.Resource.String() + "/" + e.Object.GetNamespace() + "/" + e.Object.GetName()
}
func (a Archive) Validate() error {
	if a.Payload.Version != Version || hash(a.Payload) != a.SHA256 {
		return fmt.Errorf("archive version or checksum mismatch")
	}
	if _, e := time.Parse(time.RFC3339Nano, a.Payload.Created); e != nil {
		return e
	}
	orgs := map[string]bool{}
	for _, o := range a.Payload.Organizations {
		if o == "" || orgs[o] {
			return fmt.Errorf("invalid organization list")
		}
		orgs[o] = true
	}
	seen := map[string]bool{}
	for _, e := range a.Payload.Entries {
		if e.Object == nil || e.Object.GetName() == "" || e.SHA256 != hash(e.Object) {
			return fmt.Errorf("invalid object checksum or identity")
		}
		if excludedKind(e.Object.GetKind()) {
			return fmt.Errorf("archive contains transient or rendered kind: %s", e.Object.GetKind())
		}
		av := e.Resource.GroupVersion().String()
		if av != e.Object.GetAPIVersion() {
			return fmt.Errorf("resource apiVersion mismatch")
		}
		if !IsOpenChoreo(e.Resource.Group) && !(e.Resource.Group == "" && e.Resource.Version == "v1" && (e.Resource.Resource == "namespaces" || e.Resource.Resource == "secrets")) {
			return fmt.Errorf("resource outside backup scope")
		}
		ns := e.Object.GetNamespace()
		if ns != "" && !orgs[ns] {
			return fmt.Errorf("namespace outside organization scope")
		}
		if e.Resource.Group == "" && ((e.Resource.Resource == "namespaces" && (!orgs[e.Object.GetName()] || ns != "" || e.Object.GetKind() != "Namespace")) || (e.Resource.Resource == "secrets" && (ns == "" || e.Object.GetKind() != "Secret"))) {
			return fmt.Errorf("invalid core object scope")
		}
		if hash(Clean(e.Object)) != e.SHA256 {
			return fmt.Errorf("archive contains server-managed fields")
		}
		id := identity(e)
		if seen[id] {
			return fmt.Errorf("duplicate object %s", id)
		}
		seen[id] = true
	}
	return nil
}
func Decode(b []byte) (Archive, error) {
	var a Archive
	e := json.Unmarshal(b, &a)
	if e == nil {
		e = a.Validate()
	}
	return a, e
}
func rank(kind string) int {
	switch kind {
	case "Namespace":
		return 0
	case "Secret":
		return 1
	}
	if strings.HasSuffix(kind, "Binding") {
		return 11
	}
	if strings.HasSuffix(kind, "Release") {
		return 10
	}
	if strings.Contains(kind, "Plane") {
		return 2
	}
	if kind == "Environment" {
		return 3
	}
	if strings.Contains(kind, "Pipeline") {
		return 4
	}
	if strings.Contains(kind, "Type") || strings.Contains(kind, "Trait") || (kind == "Workflow" || kind == "ComponentWorkflow" || kind == "ClusterWorkflow" || kind == "ClusterComponentWorkflow") || strings.Contains(kind, "SecretReference") {
		return 5
	}
	if kind == "Project" {
		return 6
	}
	if kind == "Component" {
		return 7
	}
	if kind == "Resource" || kind == "Workload" {
		return 8
	}
	if strings.Contains(kind, "Binding") {
		return 11
	}
	if strings.Contains(kind, "Release") {
		return 10
	}
	return 9
}
func Ordered(a Archive) []Entry {
	out := append([]Entry(nil), a.Payload.Entries...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := rank(out[i].Object.GetKind()), rank(out[j].Object.GetKind())
		if a != b {
			return a < b
		}
		return identity(out[i]) < identity(out[j])
	})
	return out
}
