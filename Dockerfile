# 1-bit-bridge container image — multi-stage to keep the runtime
# layer minimal. Builder stage compiles a static linux binary; the
# runtime stage is alpine + ca-certificates + the binary, no Go
# toolchain.
#
# Build:
#   docker build -t 1-bit-bridge:dev .
#
# Run (single-root, persistent state mounted at /data):
#   docker run --rm \
#     -p 7788:7788 \
#     -v ~/music:/library:ro \
#     -v 1-bit-bridge-state:/data \
#     -e BRIDGE_LIBRARY_ROOTS=/library \
#     -e BRIDGE_LISTEN_ADDRESS=:7788 \
#     -e BRIDGE_DATA_DIR=/data \
#     1-bit-bridge:dev
#
# `BRIDGE_ADMIN_ADDRESS` is intentionally omitted from the example —
# config validation requires the admin address to bind loopback
# (default `127.0.0.1:7789`) since the admin API has no auth.
# An empty-host form like `:7789` fails Validate. To reach the
# admin console from the host, `docker exec` into the container
# (`docker exec -it 1-bit-bridge wget -O- http://127.0.0.1:7789/`).
# See `docs/docker.md` for a reverse-proxy-with-auth pattern when
# browser access is genuinely needed.
#
# See `docs/docker.md` for a docker-compose example with a
# multi-root layout and TLS-cert volume placement.

# Keep GO_VERSION in step with the `go` directive in go.mod: the alpine
# golang image sets GOTOOLCHAIN=local, so a stale value fails the build
# with "go.mod requires go >= X" instead of auto-downloading a toolchain.
ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.19

# --- builder ---
# Pinned to the native build platform (BuildKit-provided $BUILDPLATFORM)
# so the Go compile runs natively and cross-compiles to $TARGETARCH,
# rather than running the whole builder under QEMU emulation for arm64.
# Requires BuildKit (the default in modern Docker / `docker buildx`).
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Cache the module layer separately so a code-only change doesn't
# re-download deps.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary, stripped, no cgo. modernc.org/sqlite is pure-Go so
# CGO=0 is safe across our SQLite path.
#
# `--build-arg VERSION=v1.2.0` injects into version.ServerVersion so
# `bridge version` and the X-Server-Version response header report the
# build identity rather than the placeholder constant (Gemini Medium
# on PR #80).
ARG VERSION=docker
# TARGETOS / TARGETARCH are BuildKit-provided per target platform and
# drive the cross-compile. TARGETOS defaults to "linux" (the only OS the
# alpine runtime stage supports) so a non-BuildKit `docker build` — where
# these predefined args aren't populated — still builds a linux binary;
# BuildKit overrides it per target. TARGETARCH has no default: empty makes
# `go build` use the builder's native arch (the host on a plain build),
# while BuildKit sets it per target for a multi-arch build.
ARG TARGETOS=linux
ARG TARGETARCH
ENV CGO_ENABLED=0

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=${VERSION}" \
    -o /out/bridge \
    ./cmd/bridge

# --- runtime ---
FROM alpine:${ALPINE_VERSION}

# ca-certificates: needed by the updater poller (api.github.com)
# and the enricher (musicbrainz.org / coverartarchive.org).
# tzdata: lets the quiet-hours window in the auto-installer
# evaluate against the operator's local TZ via TZ env.
#
# sox: drives the offline upscale / CarPlay-optimize pipeline and is
# the primary decoder for audio-analysis (waveform / loudness /
# key-tempo). Alpine's sox is compiled with FLAC support built in, so
# no separate plugin package is needed (the pipeline forces `-t flac`,
# which `internal/doctor` verifies). ffmpeg (ships ffprobe too): the
# analysis fallback decoder for AAC/m4a that Alpine sox can't open.
# lsof: `bridge doctor`'s preflight port check runs `lsof -nP -iTCP…`
# to name the process occupying the API/admin ports (busybox lsof lacks
# those flags, so without it the check falls back to a vaguer Warn). It
# doesn't currently distinguish our own running bridge — `bridge serve`
# writes no PID file yet — so a `doctor` run inside a live-serving
# container still reports those ports in use.
# sox and ffmpeg run as separate executables
# invoked via os/exec (aggregation, not linked into the Go binary), so
# their GPL/LGPL terms don't affect the bridge's MIT license. All three
# are inert unless upscale/analysis are enabled in config.
#
# `mkdir /data && chown bridge:bridge /data` BEFORE the USER switch
# is load-bearing (Gemini High + Qodo Bug on PR #80): WORKDIR / VOLUME
# create the directory with root:root ownership by default, so the
# subsequent USER bridge would have a non-writable /data and the
# first-run TLS-mint + manifest-DB-create would fail with permission
# errors. Operators bind-mounting their own pre-owned volume override
# this — but the in-image baseline must be writable for fresh
# `docker run -v 1-bit-bridge-state:/data` deployments to work.
RUN apk add --no-cache ca-certificates tzdata sox ffmpeg lsof && \
    addgroup -S bridge && \
    adduser -S -G bridge bridge && \
    mkdir -p /data && \
    chown bridge:bridge /data && \
    chmod 0700 /data

COPY --from=builder /out/bridge /usr/local/bin/bridge

# /data is the canonical persistent volume — bridge.yaml, the
# SQLite manifest DB, the TLS material, and the artwork cache
# all live under here. Pre-created above with bridge:bridge
# ownership so the USER-switched runtime can write.
VOLUME /data
WORKDIR /data

EXPOSE 7788

USER bridge

# `--init-if-missing` makes first boot one-command: if /data/bridge.yaml
# doesn't exist yet, serve writes a sparse default (library root defaults to
# /library; BRIDGE_LIBRARY_ROOTS / BRIDGE_LIBRARY_NAME override it at runtime)
# and then serves. On every later boot the existing config is used as-is.
# `bridge init` remains available for explicit / public-mode setup:
#   docker run --rm ... bridge init --yes --library /library --no-service
#
# HEALTHCHECK checks the API listener is accepting connections via the
# `bridge health` subcommand (a TCP dial — no TLS/cert surface) — it reads the
# listen address from the config so it works in loopback (:7788) and public
# (:443/:8443) alike, and (unlike the admin API that `bridge status` uses)
# isn't gated by public-mode auth. start-period covers the first-boot cert
# mint + listener bind.
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD ["/usr/local/bin/bridge", "health", "--config", "/data/bridge.yaml"]

ENTRYPOINT ["/usr/local/bin/bridge"]
CMD ["serve", "--config", "/data/bridge.yaml", "--init-if-missing"]
