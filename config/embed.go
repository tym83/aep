// Package config embeds the application-cluster contract shipped with the bridge.
package config

import "embed"

//go:embed app-cluster/*.yaml crd/aep.aenix.io_serviceclaims.yaml
var Bundle embed.FS
