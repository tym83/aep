#!/usr/bin/env bash
# Helm 3 excludes hooks from --post-renderer. Pin the complete template output.
set -euo pipefail
renderer_dir=$(CDPATH= cd -P -- "$(dirname -- "$0")" && pwd)
helm template "$@" | "$renderer_dir/post-render.sh"
