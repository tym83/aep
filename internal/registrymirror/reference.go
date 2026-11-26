// Package registrymirror defines the collision-free mapping shared by delivery and release tooling.
package registrymirror

import "strings"

// Rewrite retains the source registry as a path component, and preserves tags/digests.
func Rewrite(ref, mirror string) string {
	if mirror == "" {
		return ref
	}
	prefix := ""
	if strings.HasPrefix(ref, "oci://") {
		prefix = "oci://"
		ref = strings.TrimPrefix(ref, prefix)
	}
	mirror = strings.TrimSuffix(strings.TrimPrefix(mirror, "oci://"), "/")
	if strings.HasPrefix(ref, mirror+"/") {
		return prefix + ref
	}
	first, _, hasPath := strings.Cut(ref, "/")
	if !hasPath {
		ref = "docker.io/library/" + ref
	} else if !strings.ContainsAny(first, ".:") && first != "localhost" {
		ref = "docker.io/" + ref
	}
	host, path, _ := strings.Cut(ref, "/")
	if host == "index.docker.io" {
		host = "docker.io"
	}
	host = strings.ReplaceAll(host, ":", "_")
	return prefix + mirror + "/" + host + "/" + path
}
