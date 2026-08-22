SHELL := /usr/bin/env bash

# Put $(go env GOPATH)/bin on PATH so buf can find protoc-gen-go and
# protoc-gen-go-grpc without a `go install` PATH dance.
GOBIN          := $(shell go env GOPATH)/bin
export PATH    := $(GOBIN):$(PATH)

BIN_DIR        := bin
SERVER_BIN     := $(BIN_DIR)/mindd
CTL_BIN        := $(BIN_DIR)/mindctl

VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS        := -ldflags "-s -w \
	-X github.com/vibed-project/mindD/internal/version.Version=$(VERSION) \
	-X github.com/vibed-project/mindD/internal/version.Commit=$(COMMIT) \
	-X github.com/vibed-project/mindD/internal/version.BuildDate=$(BUILD_DATE)"

DOCKER_IMAGE   ?= mindd
DOCKER_TAG     ?= $(VERSION)
HELM_CHART     := deploy/helm/mindd

.PHONY: all build server mindctl proto test lint run-dev tidy clean \
        docker docker-multiarch helm-lint helm-template docs-dev docs-build docs-clean

all: build

build: server mindctl

server:
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(SERVER_BIN) ./cmd/mindd

mindctl:
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(CTL_BIN) ./cmd/mindctl

proto:
	buf generate

proto-python:
	buf generate --template buf.gen.python.yaml
	@for d in sdk/python/src/mindd/{,kv,kv/v1,episodic,episodic/v1,semantic,semantic/v1,artifact,artifact/v1,lease,lease/v1,graph,graph/v1,admin,admin/v1}; do \
		touch "$$d/__init__.py"; \
	done

proto-ts:
	buf generate --template buf.gen.ts.yaml

test:
	go test ./...

test-integration:
	go test -tags=integration ./...

lint:
	@which golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed"; exit 1; }
	golangci-lint run

run-dev: server
	$(SERVER_BIN) --config configs/example.yaml

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) .

DOCKER_PLATFORMS ?= linux/amd64,linux/arm64

docker-multiarch:
	docker buildx build \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) .

helm-lint:
	helm lint $(HELM_CHART)

helm-template:
	helm template msc $(HELM_CHART)

docs-dev:
	cd website && npm install --silent && npm run start

docs-build:
	cd website && npm install --silent && npm run build

docs-clean:
	rm -rf website/build website/.docusaurus website/node_modules
