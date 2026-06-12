.PHONY: build build-all test test-fast check fmt vet clean run

BINARY      := bridge
PKG         := ./cmd/bridge
DIST        := dist

# P bounds Go's build/test parallelism (the `-p` flag). The `-race` test
# build plus the 6-target cross-compile spike peak RAM enough to OOM a
# memory-constrained machine (e.g. 18 GB with apps open) — which shows up
# as the build crawling against the macOS memory compressor while the CPU
# sits idle, or the process getting OOM-killed outright. Capping concurrent
# compiles trades a little wall-clock for staying under the RAM ceiling.
# Raise it on a roomy box: `make test P=8` (or P=$(nproc) / P=$(sysctl -n hw.ncpu)).
P ?= 4

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
	GOOS=darwin  GOARCH=arm64 go build -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64  $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64  $(PKG)
	GOOS=linux   GOARCH=arm64 go build -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64   $(PKG)
	GOOS=linux   GOARCH=amd64 go build -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64   $(PKG)
	GOOS=windows GOARCH=amd64 go build -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe $(PKG)
	GOOS=windows GOARCH=arm64 go build -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-arm64.exe $(PKG)

# Full pre-push gate's test step: race-enabled, parallelism-capped (see P).
test:
	go test -p $(P) -race ./...

# Fast inner-loop tests: no race detector — much faster to build and a
# fraction of the RAM, so it won't thrash on a loaded machine. Use while
# iterating; run `make test` (race) before pushing. A single package is
# faster still: `go test ./internal/<pkg>/`.
test-fast:
	go test -p $(P) ./...

# Per-change gate: format + vet + race tests, WITHOUT the 6-target
# build-all (the dominant cost). Run the full `make fmt vet test build-all`
# once before pushing; CI (.github/workflows) runs the same gate per PR.
check: fmt vet test

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -rf bin $(DIST)

run:
	go run $(PKG) serve --config config/bridge.yaml.example
