BIN_DIR ?= bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS ?= -s -w -X github.com/encodeous/nylon/internal/buildinfo.Version=$(VERSION) -X github.com/encodeous/nylon/internal/buildinfo.Commit=$(COMMIT)
NPROC := $(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
GOTESTSUM ?= go run gotest.tools/gotestsum@latest --
export CGO_ENABLED ?= 0

.DEFAULT_GOAL := build
.PHONY: build nylon nylon-genesis nylon-lb test test-integration test-e2e test-all image-nylon-lb image-nylon-lb-debug push-nylon-lb proto clean

build: nylon nylon-genesis nylon-lb

nylon:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/nylon ./cmd/nylon

nylon-genesis:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/nylon-genesis ./cmd/nylon-genesis

nylon-lb:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/nylon-lb ./cmd/nylon-lb

test:
	$(GOTESTSUM) --race -tags=router_test ./...

test-integration:
	$(GOTESTSUM) --race -tags=integration ./integration/...

test-e2e:
	$(GOTESTSUM) -tags=e2e ./e2e/... -parallel $(NPROC)

test-all: test test-integration test-e2e

## Container images (nylon-lb). Override on the command line:
##   make push-nylon-lb REGISTRY=registry.example.com/infra IMAGE_NAME=nylon/nylon-lb \
##        IMAGE_TAG=0.4.0 PLATFORMS=linux/amd64
DOCKER ?= docker
REGISTRY ?=                                   # e.g. ghcr.io/encodeous; empty = local-only image name
IMAGE_NAME ?= nylon-lb
IMAGE_TAG ?= $(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64
IMAGE_EXTRA_ARGS ?=                           # extra buildx flags, e.g. additional -t tags
IMAGE_REF := $(if $(REGISTRY),$(REGISTRY)/$(IMAGE_NAME),$(IMAGE_NAME)):$(IMAGE_TAG)
LB_DOCKERFILE := cmd/nylon-lb/Dockerfile

image-nylon-lb:
	$(DOCKER) build $(IMAGE_EXTRA_ARGS) -f $(LB_DOCKERFILE) --target runtime \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_REF) .

image-nylon-lb-debug:
	$(DOCKER) build $(IMAGE_EXTRA_ARGS) -f $(LB_DOCKERFILE) --target debug \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_REF) .

push-nylon-lb:
	$(DOCKER) buildx build $(IMAGE_EXTRA_ARGS) -f $(LB_DOCKERFILE) --target runtime \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_REF) --push .

proto:
	go generate ./cmd/nylon

clean:
	rm -rf $(BIN_DIR)
