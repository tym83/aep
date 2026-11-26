IMAGE ?= ghcr.io/tym83/aep-bridge
CONTROLLER_GEN_VERSION := v0.21.0

.PHONY: build test lint generate docker-build
build:
	go build ./...
test:
	go test ./...
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else go vet ./...; fi
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object crd paths=./api/... output:crd:artifacts:config=config/crd
	cp config/crd/aep.aenix.io_serviceclaims.yaml charts/aep-bridge-appcluster/crds/
	cp config/crd/aep.aenix.io_changerequests.yaml charts/aep-change/crds/
docker-build:
	docker build -t $(IMAGE) .

.PHONY: policies-render policies-test policies-package
policies-render:
	helm template test policies/app-cluster-baseline --set organizationUID=test-org --show-only templates/workloads.yaml --show-only templates/namespaces.yaml --show-only templates/isolation.yaml > policies/app-cluster-baseline/tests/rendered.yaml
	helm template test policies/app-cluster-nonprod > policies/app-cluster-nonprod/tests/rendered.yaml
	helm template test policies/app-cluster-prod --set-file cosignPublicKey=policies/app-cluster-prod/tests/cosign.pub > policies/app-cluster-prod/tests/rendered.yaml
policies-test: policies-render
	@command -v kyverno >/dev/null || { echo 'Install Kyverno CLI 1.19.2 to run policy tests'; exit 1; }
	kyverno test policies/app-cluster-baseline/tests
	kyverno test policies/app-cluster-nonprod/tests
	kyverno test policies/app-cluster-prod/tests
	go test ./internal/operator -run TestProductionImageTrust -count=1
policies-package:
	mkdir -p dist/policies
	for bundle in baseline nonprod prod; do helm package policies/app-cluster-$$bundle --destination dist/policies; done

.PHONY: charts-lint
charts-lint:
	helm lint --strict charts/aep-audit-forwarder charts/aep-cp-backup
	helm template audit charts/aep-audit-forwarder --namespace aep-system > /dev/null
	helm template backup charts/aep-cp-backup --namespace aep-system > /dev/null
	cmp config/crd/aep.aenix.io_changerequests.yaml charts/aep-change/crds/aep.aenix.io_changerequests.yaml
	helm lint --strict charts/aep-change -f charts/ci/change.yaml
	helm template change charts/aep-change --namespace aep-system --include-crds -f charts/ci/change.yaml > /dev/null
	helm lint --strict charts/aep-change -f charts/ci/change.yaml -f charts/ci/ingress.yaml -f charts/ci/change-tls.yaml
	helm template change charts/aep-change --namespace aep-system --include-crds -f charts/ci/change.yaml -f charts/ci/ingress.yaml -f charts/ci/change-tls.yaml > /dev/null
	helm lint --strict charts/aep-read-api -f charts/ci/read.yaml
	helm template read charts/aep-read-api --namespace aep-system -f charts/ci/read.yaml > /dev/null
	helm lint --strict charts/aep-read-api -f charts/ci/read.yaml -f charts/ci/ingress.yaml
	helm template read charts/aep-read-api --namespace aep-system -f charts/ci/read.yaml -f charts/ci/ingress.yaml > /dev/null
	helm lint --strict charts/aep-itsm-bridge -f charts/ci/itsm.yaml
	helm template itsm charts/aep-itsm-bridge --namespace itsm-acme -f charts/ci/itsm.yaml > /dev/null
