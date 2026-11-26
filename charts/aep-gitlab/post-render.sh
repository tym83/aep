#!/bin/sh
# Use the reviewed release binary in installations, or the source checkout in CI.
set -eu
mirror=${AEP_REGISTRY_MIRROR:-}
if command -v aep-release >/dev/null 2>&1; then
  exec aep-release render-images --registry-mirror "$mirror"
fi
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"
exec go run ./cmd/aep-release render-images --registry-mirror "$mirror"
