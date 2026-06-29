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

ARG GO_VERSION=1.25
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
# TARGETOS / TARGETARCH are BuildKit-provided per target platform. Left
# without defaults on purpose: under BuildKit (the default builder) they
# carry the target's OS/arch; on a plain `docker build` they resolve to
# the host; on a legacy non-BuildKit builder they expand empty so
# `go build` falls back to the host arch — all three are correct.
ARG TARGETOS
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
# `mkdir /data && chown bridge:bridge /data` BEFORE the USER switch
# is load-bearing (Gemini High + Qodo Bug on PR #80): WORKDIR / VOLUME
# create the directory with root:root ownership by default, so the
# subsequent USER bridge would have a non-writable /data and the
# first-run TLS-mint + manifest-DB-create would fail with permission
# errors. Operators bind-mounting their own pre-owned volume override
# this — but the in-image baseline must be writable for fresh
# `docker run -v 1-bit-bridge-state:/data` deployments to work.
RUN apk add --no-cache ca-certificates tzdata && \
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

# `bridge serve` is the only sensible default. `bridge init` is a
# one-time setup; the operator runs it via `docker run --rm ...
# bridge init --yes --library /library --no-service`. Once
# bridge.yaml exists at /data/bridge.yaml, this entrypoint takes
# over.
ENTRYPOINT ["/usr/local/bin/bridge"]
CMD ["serve", "--config", "/data/bridge.yaml"]
