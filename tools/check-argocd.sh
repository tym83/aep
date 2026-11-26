#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p .cache/argocd
if [ ! -f .cache/argocd/argo-cd/Chart.yaml ]; then
  helm pull argo-cd --repo https://argoproj.github.io/argo-helm --version 9.7.1 --untar --untardir .cache/argocd
fi
ARGO_VALUES_OUTPUT="$PWD/.cache/argocd/values.json" go test ./internal/operator -run '^TestArgoIsolation$' -count=1
helm lint .cache/argocd/argo-cd -f .cache/argocd/values.json
helm template argocd .cache/argocd/argo-cd --namespace argocd -f .cache/argocd/values.json > .cache/argocd/rendered.yaml
ARGO_RENDERED_INPUT="$PWD/.cache/argocd/rendered.yaml" go test ./internal/operator -run '^TestRenderedArgoSecurity$' -count=1
