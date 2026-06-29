BINARY  := clipboard-bridge
PKG     := ./cmd/clipboard-bridge
DIST    := dist
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-all checksums test cover vet fmt clean

all: build

## build: compile the binary for the host platform
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

## build-all: cross-compile static binaries for linux/amd64 and linux/arm64
build-all: clean
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)-linux-arm64 $(PKG)

## checksums: write SHA256SUMS for the cross-compiled binaries
checksums:
	cd $(DIST) && sha256sum $(BINARY)-linux-amd64 $(BINARY)-linux-arm64 > SHA256SUMS

## test: run unit tests
test:
	go test ./...

## cover: run unit tests with coverage
cover:
	go test -covermode=atomic -coverprofile=coverage.txt ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format all Go sources
fmt:
	gofmt -l -w .

## clean: remove build artifacts
clean:
	rm -rf $(DIST) $(BINARY)
