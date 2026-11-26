#!/bin/sh
set -eu
if command -v aep-release >/dev/null 2>&1; then
  exec aep-release render-images --registry-mirror "${AEP_REGISTRY_MIRROR:-}"
fi
root=$(CDPATH= cd -P -- "$(dirname -- "$0")/../../.." && pwd)
cd "$root"
exec go run ./cmd/aep-release render-images --registry-mirror "${AEP_REGISTRY_MIRROR:-}"
