#!/bin/sh
# Stage artifacts into a directory for an operator image or read-only PVC.
# Run from the repository root. Helm only downloads/renders; nothing is deployed.
set -eu
destination=${1:?artifact directory required}
mkdir -p "$destination"
helm pull oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds --version v2.3.11 --destination "$destination"
helm pull oci://cr.kgateway.dev/kgateway-dev/charts/kgateway --version v2.3.11 --destination "$destination"
helm pull oci://cr.openchoreo.dev/openchoreo/helm-charts/openchoreo-data-plane --version 1.4.0-m.3 --destination "$destination"
helm pull oci://ghcr.io/kyverno/charts/kyverno --version 3.9.2 --destination "$destination"
for chart in app-cluster-baseline app-cluster-nonprod app-cluster-prod; do
  helm package "policies/$chart" --destination "$destination"
done
helm package charts/aep-bridge-appcluster --destination "$destination"
# Helm strips a leading v from archive names; the operator matrix retains it.
for chart in kgateway kgateway-crds; do
  if [ -f "$destination/$chart-2.3.11.tgz" ]; then
    mv "$destination/$chart-2.3.11.tgz" "$destination/$chart-v2.3.11.tgz"
  fi
done
