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
docker-build:
	docker build -t $(IMAGE) .

.PHONY: policies-render policies-test policies-package
policies-render:
	helm template test policies/app-cluster-baseline --set organizationUID=test-org > policies/app-cluster-baseline/tests/rendered.yaml
	helm template test policies/app-cluster-nonprod > policies/app-cluster-nonprod/tests/rendered.yaml
	helm template test policies/app-cluster-prod --set cosignPublicKey=test-render-only > policies/app-cluster-prod/tests/rendered.yaml
policies-test: policies-render
	@command -v kyverno >/dev/null || { echo 'Install Kyverno CLI 1.19.2 to run policy tests'; exit 1; }
	kyverno test policies/app-cluster-baseline/tests
	kyverno test policies/app-cluster-nonprod/tests
	kyverno test policies/app-cluster-prod/tests
policies-package:
	mkdir -p dist/policies
	for bundle in baseline nonprod prod; do helm package policies/app-cluster-$$bundle --destination dist/policies; done
