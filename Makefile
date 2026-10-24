.PHONY: build build-all test test-fast measure-dsd check fmt vet clean run check-go-version check-build-tags check-buildx docker docs-check

BINARY      := bridge
IMAGE       := 1-bit-bridge
PKG         := ./cmd/bridge
DIST        := dist

# Mirror .goreleaser.yaml; check-build-tags refuses drift.
override GO_TAGS := ts_omit_webclient,ts_omit_debugeventbus

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

build: check-build-tags
	go build -tags "$(GO_TAGS)" -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

build-all: check-build-tags
	@mkdir -p $(DIST)
	GOOS=darwin  GOARCH=arm64 go build -tags "$(GO_TAGS)" -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64  $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -tags "$(GO_TAGS)" -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64  $(PKG)
	GOOS=linux   GOARCH=arm64 go build -tags "$(GO_TAGS)" -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64   $(PKG)
	GOOS=linux   GOARCH=amd64 go build -tags "$(GO_TAGS)" -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64   $(PKG)
	GOOS=windows GOARCH=amd64 go build -tags "$(GO_TAGS)" -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe $(PKG)
	GOOS=windows GOARCH=arm64 go build -tags "$(GO_TAGS)" -p $(P) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-arm64.exe $(PKG)

# Full pre-push gate's test step: race-enabled, parallelism-capped (see P).
# -timeout 30m (not the default 10m/package): internal/adminauth's suite pays
# real bcrypt cost-12 per simulated login across its rate-limit tests, which
# cumulatively exceeds 10m under -race on slower CI runners (TestPersistedHash
# IsBcryptShape was the recurring timeout victim — it's just where the package
# timeout goroutine-dump landed, not a hang). The race detector still catches a
# genuine deadlock at the 30m ceiling.
test:
	go test -tags "$(GO_TAGS)" -p $(P) -race -timeout 30m ./...

# Fast inner-loop tests: no race detector — much faster to build and a
# fraction of the RAM, so it won't thrash on a loaded machine. Use while
# iterating; run `make test` (race) before pushing. A single package is
# faster still: `go test -tags "$(GO_TAGS)" ./internal/<pkg>/`.
test-fast:
	go test -tags "$(GO_TAGS)" -p $(P) ./...

# The DSD render measurements. Gated behind BRIDGE_DSD_FIXTURE_TESTS because
# they need a real sox + ffmpeg (with the four dsd_* decoders); they SKIP
# rather than fail without one, which is why they must be RUN somewhere on
# purpose — in a summary line a skip and a pass look identical. Until
# 2026-09-09 that variable appeared nowhere outside the test file, so the
# alias-rejection differential, the level-parity clip-guard pin and the CCIF
# intermodulation check had never run anywhere since they were written.
#
# Verified 2026-09-09 on Debian trixie with apt sox 14.4.2 + ffmpeg: the
# numbers reproduce CLAUDE.md's to the decimal — stopband -116.84 dB, alias
# delta +0.00 dB faithful / +0.38 dB compact, rendition -0.97 dBTP at a
# +5.00 dB applied gain. Under three seconds once the toolchain is there.
measure-dsd:
	BRIDGE_DSD_FIXTURE_TESTS=1 go test -tags "$(GO_TAGS)" ./internal/transcode/ \
		-run 'TestDSDRender_|TestDSDLowpass|TestRunDSD' -count=1 -v

# Per-change gate: format + vet + race tests, WITHOUT the 6-target
# build-all (the dominant cost). Run the full `make fmt vet test build-all`
# once before pushing; CI (.github/workflows) runs the same gate per PR.
#
# Sequential $(MAKE) sub-invocations rather than `check: fmt vet test`
# prerequisites: under parallel make (`make -j check` / a -j in MAKEFLAGS)
# prerequisites run concurrently, and `fmt` rewrites .go files while
# `vet`/`test` are compiling them — a flaky-failure race. Recipe lines
# always run in order and fail-fast.
check:
	$(MAKE) fmt
	$(MAKE) vet
	$(MAKE) test

fmt:
	go fmt ./...

vet:
	go vet -tags "$(GO_TAGS)" ./...

clean:
	rm -rf bin $(DIST)

run:
	go run -tags "$(GO_TAGS)" $(PKG) serve --config config/bridge.yaml.example

# GoReleaser owns the tags; this also runs in gate.yml via check-go-version.
check-build-tags:
	go run ./scripts/third-party-licenses -check-build-tags

# Guard: the Dockerfile's `ARG GO_VERSION` full patch version MUST match go.mod's
# `go` directive. The alpine golang image sets GOTOOLCHAIN=local, so a stale
# ARG fails the image build with "go.mod requires go >= X" rather than
# auto-downloading a toolchain (this broke the first v0.1.7 image). CI runs
# this via .github/workflows/gate.yml; run it locally before bumping either.
check-go-version: check-build-tags
	@gomod=$$(awk '/^go /{print $$2; exit}' go.mod); \
	dockerfile=$$(awk -F= '/^ARG GO_VERSION=/{print $$2; exit}' Dockerfile); \
	if [ -z "$$gomod" ] || [ -z "$$dockerfile" ]; then \
		echo "check-go-version: could not parse versions (go.mod='$$gomod' Dockerfile='$$dockerfile')"; exit 1; \
	fi; \
	if [ "$$gomod" != "$$dockerfile" ]; then \
		echo "check-go-version: MISMATCH — Dockerfile ARG GO_VERSION=$$dockerfile != go.mod go $$gomod"; \
		echo "  fix: set 'ARG GO_VERSION=$$gomod' in Dockerfile (GOTOOLCHAIN=local fails the alpine build on a stale value)"; \
		exit 1; \
	fi; \
	echo "check-go-version: OK (go.mod go $$gomod, Dockerfile GO_VERSION $$dockerfile)"

# Guard: the instruction files Claude Code loads on its own stay within their
# byte budgets: CLAUDE.md <= 64 KB, each .claude/rules/*.md <= 100 KB, and
# CLAUDE.md plus the rules files one tracked file pulls in <= 480 KB; a rules
# file without `paths:` and a skill without its name and description fail too.
# Over budget, move an entry rather than cutting it (scripts/doc-split/README.md).
# Python 3 stdlib only, so it needs no Go toolchain; CI runs it via gate.yml.
docs-check:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/check-doc-budgets.py --self-test
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/check-doc-budgets.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/rules-for.py --self-test
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/doc-split/move_entry.py --self-test

# Guard: the Dockerfile requires BuildKit (see the comment on its builder
# FROM line), which the docker CLI reaches only through the buildx plugin.
# Without the plugin `docker build` falls back to the deprecated legacy
# builder and fails at that FROM line; this names the missing piece first.
check-buildx:
	@docker buildx version >/dev/null 2>&1 || { \
		echo "check-buildx: 'docker buildx' is unavailable — the Dockerfile requires BuildKit, which needs the buildx plugin" >&2; \
		echo "  install it (Ubuntu/Debian docker.io: sudo apt install docker-buildx); docs/docker.md → 'Build it yourself' lists the rest" >&2; \
		exit 1; \
	}; \
	echo "check-buildx: OK ($$(docker buildx version))"

# Convenience: build the container image for the host arch, injecting VERSION
# so `bridge version` reports the build identity (mirrors the ServerVersion
# injection `make build` does). `docker buildx build`, not `docker build`: the
# Dockerfile requires BuildKit, and `--load` lands the image in the local
# store whichever buildx driver is selected. Multi-arch builds — see
# docs/docker.md. Runs both guards first for a clear early failure.
docker: check-go-version check-buildx
	docker buildx build --load --build-arg VERSION="$(VERSION)" -t $(IMAGE):dev .
