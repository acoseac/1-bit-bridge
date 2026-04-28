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
#     -e BRIDGE_ADMIN_ADDRESS=:7789 \
#     -e BRIDGE_DATA_DIR=/data \
#     1-bit-bridge:dev
#
# The admin console binds loopback inside the container, so to reach
# it from the host you'd need to either exec into the container
# (`docker exec -it ... wget -O- http://127.0.0.1:7789/`) or change
# BRIDGE_ADMIN_ADDRESS to a port that's mapped (security trade-off:
# anyone on the host can hit the unauthenticated admin API). Most
# users only need /api accessible to iOS — keep admin loopback-
# only and use `docker exec`.
#
# See `docs/docker.md` for a docker-compose example with a
# multi-root layout and TLS-cert volume placement.

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.19

# --- builder ---
FROM golang:${GO_VERSION}-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Cache the module layer separately so a code-only change doesn't
# re-download deps.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary, stripped, no cgo. modernc.org/sqlite is pure-Go so
# CGO=0 is safe across our SQLite path.
ARG VERSION=docker
ENV CGO_ENABLED=0 \
    GOOS=linux

RUN go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/bridge \
    ./cmd/bridge

# --- runtime ---
FROM alpine:${ALPINE_VERSION}

# ca-certificates: needed by the updater poller (api.github.com)
# and the enricher (musicbrainz.org / coverartarchive.org).
# tzdata: lets the quiet-hours window in the auto-installer
# evaluate against the operator's local TZ via TZ env.
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S bridge && \
    adduser -S -G bridge bridge

COPY --from=builder /out/bridge /usr/local/bin/bridge

# /data is the canonical persistent volume — bridge.yaml, the
# SQLite manifest DB, the TLS material, and the artwork cache
# all live under here.
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
