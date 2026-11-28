// Package resourcetypes embeds the OpenChoreo ClusterResourceTypes that the
// operator applies to a control plane.
package resourcetypes

import "embed"

// Files holds every ClusterResourceType manifest in this directory.
//
//go:embed *.yaml
var Files embed.FS
