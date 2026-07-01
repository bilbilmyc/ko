.PHONY: build build-amd64 build-arm64 build-all test test-unit test-integration test-e2e lint fmt vet clean run doctor version

BIN_DIR      := bin
BINARY       := $(BIN_DIR)/ko
LDFLAGS      := -s -w
GO           := go
PKG          := ./...
BUILD_TAGS   := -tags=

VERSION_PKG  := github.com/ko-build/ko/internal/version

build:
	mkdir -p $(BIN_DIR)
	$(GO) build $(BUILD_TAGS) -ldflags '$(LDFLAGS) -X $(VERSION_PKG).Version=$$(git describe --tags --always --dirty 2>/dev/null || echo v0.0.1-dev) -X $(VERSION_PKG).Commit=$$(git rev-parse --short HEAD 2>/dev/null || echo unknown) -X $(VERSION_PKG).BuildDate=$$(date -u +%Y-%m-%dT%H:%M:%SZ)' -o $(BINARY) ./cmd/ko

build-amd64:
	mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(BUILD_TAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/ko-linux-amd64 ./cmd/ko

build-arm64:
	mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(BUILD_TAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/ko-linux-arm64 ./cmd/ko

build-all: build-amd64 build-arm64

test:
	$(GO) test $(PKG)

test-unit:
	$(GO) test -short $(PKG)

test-integration:
	$(GO) test -tags=integration -timeout 10m ./test/integration/...

test-e2e:
	$(GO) test -tags=e2e -timeout 30m ./test/e2e/...

lint:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

vet:
	$(GO) vet $(PKG)

clean:
	rm -rf $(BIN_DIR) dist

run: build
	$(BINARY) $(ARGS)

doctor: build
	$(BINARY) doctor

version: build
	$(BINARY) version
