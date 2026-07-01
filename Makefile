.PHONY: build build-amd64 build-arm64 build-all test test-unit test-integration test-e2e test-arm64 lint fmt vet clean run doctor version pack-build pack-multi

BIN_DIR      := bin
BINARY       := $(BIN_DIR)/ko
LDFLAGS      := -s -w
GO           := go
PKG          := ./...
BUILD_TAGS   := -tags=

VERSION_PKG  := github.com/ko-build/ko/internal/version

# Inject version metadata into the binary at link time. Reads from git
# describe so a tagged release (v0.0.1) becomes the binary's version string.
VERSION_VAL  := $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.1-dev)
COMMIT_VAL   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION_VAL) \
                  -X $(VERSION_PKG).Commit=$(COMMIT_VAL) \
                  -X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

build:
	mkdir -p $(BIN_DIR)
	$(GO) build $(BUILD_TAGS) -ldflags '$(LDFLAGS) $(VERSION_LDFLAGS)' -o $(BINARY) ./cmd/ko

build-amd64:
	mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(BUILD_TAGS) -ldflags '$(LDFLAGS) $(VERSION_LDFLAGS)' -o $(BIN_DIR)/ko-linux-amd64 ./cmd/ko

build-arm64:
	mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(BUILD_TAGS) -ldflags '$(LDFLAGS) $(VERSION_LDFLAGS)' -o $(BIN_DIR)/ko-linux-arm64 ./cmd/ko

build-all: build-amd64 build-arm64

# Cross-compile the ko binary for both arches AND pack a multi-arch OCI
# bundle (amd64 + arm64 in one image index). Output bundle lands in
# ./dist/ko-multi.oci.tar.gz.
build-multi-arch: build-amd64 build-arm64
	mkdir -p dist
	$(BIN_DIR)/ko-linux-amd64 pack build --arch all --output dist --version $(VERSION_VAL)

test:
	$(GO) test $(PKG)

test-unit:
	$(GO) test -short $(PKG)

test-integration:
	$(GO) test -tags=integration -timeout 10m ./test/integration/...

test-e2e:
	$(GO) test -tags=e2e -timeout 30m ./test/e2e/...

# Run unit tests under qemu-aarch64 to catch arch-specific bugs. Requires
# qemu-user-static + binfmt_misc set up on the host (CI installs these).
test-arm64:
	GOARCH=arm64 $(GO) test -short -timeout 5m $(PKG)

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

# Convenience: build a multi-arch offline bundle directly (assumes ko is
# already installed and on PATH).
pack-build:
	ko pack build --arch $(ARCH) --output ./dist --version $(VERSION_VAL)

pack-multi:
	mkdir -p dist
	ko pack build --arch all --output ./dist --version $(VERSION_VAL)