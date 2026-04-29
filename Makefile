.PHONY: build build-all test fmt vet clean run

BINARY      := bridge
PKG         := ./cmd/bridge
DIST        := dist

# VERSION is injected into internal/version.ServerVersion so that
# `make build` artefacts report something meaningful in the admin
# console and update-poll User-Agent. Auto-derives from `git describe`
# on a working tree (yields e.g. "v0.1.1-4-gabcdef-dirty"); falls back
# to "dev" on a fresh clone with no tags or on an extracted source
# tarball (no .git). Explicit override wins: `make build VERSION=0.1.2`.
#
# Goreleaser sets ServerVersion via its own ldflags clause in
# .goreleaser.yaml — release builds don't go through this Makefile.
# But CI / dev builds / local `make build` artefacts must mirror that
# injection or the binary reports the source default "0.0.1" forever.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w \
	-X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

build-all:
	@mkdir -p $(DIST)
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64  $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64  $(PKG)
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64   $(PKG)
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64   $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe $(PKG)
	GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-arm64.exe $(PKG)

test:
	go test -race ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -rf bin $(DIST)

run:
	go run $(PKG) serve --config config/bridge.yaml.example
