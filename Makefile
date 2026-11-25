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
