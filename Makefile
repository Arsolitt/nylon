BIN_DIR ?= bin
LDFLAGS ?= -s -w
NPROC := $(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
GOTESTSUM ?= go run gotest.tools/gotestsum@latest --
export CGO_ENABLED ?= 0

.DEFAULT_GOAL := build
.PHONY: build nylon nylon-genesis nylon-lb test test-integration test-e2e proto clean

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

proto:
	go generate ./cmd/nylon

clean:
	rm -rf $(BIN_DIR)
