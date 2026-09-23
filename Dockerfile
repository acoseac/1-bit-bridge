# 1-bit-bridge container image — multi-stage to keep the runtime
# layer minimal. Builder stage compiles a static linux binary; the
# runtime stage is alpine + ca-certificates + the binary, no Go
# toolchain.
#
# Build — this file REQUIRES BuildKit, which the docker CLI reaches through
# the buildx plugin:
#   docker buildx build --load -t 1-bit-bridge:dev .      (or: make docker)
# Without the plugin, `docker build` falls back to Docker's deprecated legacy
# builder, which stops at the builder stage with an error naming buildx (see
# the comment on that FROM line). docs/docker.md → "Build it yourself" has
# the install line per distro.
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
ARG ALPINE_VERSION=3.22

# --- builder ---
# Pinned to the native build platform (BuildKit-provided $BUILDPLATFORM)
# so the Go compile runs natively and cross-compiles to $TARGETARCH,
# rather than running the whole builder under QEMU emulation for arm64.
#
# Requires BuildKit, and the `:-` fallback is how this line says so. BuildKit
# always sets BUILDPLATFORM, so there the fallback is never read. The legacy
# builder does not set it, and a bare ${BUILDPLATFORM} then dies with `failed
# to parse platform : "" is an invalid OS component…`, which names neither
# BuildKit nor buildx; with the fallback, the error IS the instruction. It
# must stay an invalid platform spelled with letters, digits and hyphens only:
# a space breaks this line under BuildKit too ("FROM requires either one or
# three arguments"), and a `/` parses as an os/arch pair that the legacy
# builder then tries to pull. Two alternatives were measured and refused
# (Docker 29.1 / BuildKit 0.26):
#   - `ARG BUILDPLATFORM=linux/amd64` above the FROM gets the legacy builder
#     through, but a declared default is not a fallback under BuildKit — it
#     REPLACES the automatic value, so on every arm64 build host this stage
#     would run under QEMU (or die with `exec format error` where none is
#     registered), the cost this pin exists to avoid.
#   - `:-linux` (the daemon's own arch) built a working image on the legacy
#     builder (measured on amd64), and was refused anyway: a second build
#     path no CI runs, for a builder Docker has deprecated. Fail loudly on
#     one path instead.
FROM --platform=${BUILDPLATFORM:-this-Dockerfile-requires-BuildKit--build-with-docker-buildx} golang:${GO_VERSION}-alpine AS builder

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
# drive the cross-compile. Declared WITHOUT defaults, deliberately: under
# BuildKit a declared default does not fill in a missing value, it REPLACES
# the per-target one (measured: `ARG TARGETARCH=bogus` builds with
# TARGETARCH=bogus). A TARGETARCH default would put that one arch's binary
# into every leg of the multi-arch image. There is no non-BuildKit build to
# default for — the builder FROM above refuses to start without BuildKit.
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
# sox: drives the offline upscale / CarPlay-optimize pipeline and is
# the primary decoder for audio-analysis (waveform / loudness /
# key-tempo). Alpine's sox is compiled with FLAC support built in, so
# no separate plugin package is needed (the pipeline forces `-t flac`,
# which `internal/doctor` verifies). ffmpeg (ships ffprobe too): the
# analysis fallback decoder for AAC/m4a that Alpine sox can't open.
# chromaprint: ships fpcalc, which the acoustic-fingerprinting fallback
# shells out to when text matching finds no MusicBrainz candidate. It
# links its own FFmpeg for decoding, so it is independent of the sox
# above. Inert unless `fingerprint.enabled` is set AND an AcoustID API
# key is configured — `internal/doctor` verifies both.
# lsof: `bridge doctor`'s port checks run `lsof -nP -iTCP:<port>
# -sTCP:LISTEN -t` to confirm that the PID `bridge serve` records in
# <dataDir>/server.pid (/data/data/server.pid under the auto-init config)
# is the one listening, so `docker exec <container> bridge doctor --config
# /data/bridge.yaml` reports port-api / port-admin as "bound by our own
# bridge (pid 1)". Keep the --config: without it doctor looks for its
# config only under ~/.config/1-bit-bridge, never reads /data/bridge.yaml,
# cannot find the pidfile, and FAILs both ports as held by another
# process. The package itself matters: Alpine's own /usr/bin/lsof is the
# busybox applet, which ignores those options and lists every open file,
# so doctor would credit any occupied port to the running bridge.
# sox, ffmpeg and fpcalc all run as separate executables invoked via
# os/exec (aggregation, not linked into the Go binary), so their
# GPL/LGPL terms don't affect the bridge's own license (FSL-1.1-MIT) — chromaprint is
# LGPL-2.1, the same arrangement. All four are inert unless
# upscale/analysis/fingerprint are enabled in config.
#
# `mkdir /data && chown bridge:bridge /data` BEFORE the USER switch
# is load-bearing (Gemini High + Qodo Bug on PR #80): WORKDIR / VOLUME
# create the directory with root:root ownership by default, so the
# subsequent USER bridge would have a non-writable /data and the
# first-run TLS-mint + manifest-DB-create would fail with permission
# errors. Operators bind-mounting their own pre-owned volume override
# this — but the in-image baseline must be writable for fresh
# `docker run -v 1-bit-bridge-state:/data` deployments to work.
# The DSD → PCM renditions decode through ffmpeg, so the image asserts its
# own ffmpeg carries the decoders at BUILD time — a base-image change that
# drops them then fails here rather than at the first render in the field.
# ALL FOUR dsd_* names, because that is exactly what the runtime probe
# requires (transcode.ffmpegCapabilities sets HasDSD only when every one is
# present); asserting a subset would let such an image through the build and
# fail it in production, which is the failure this check exists to prevent.
# `dst` is checked in the same loop but is a SEPARATE capability: without it
# plain DSF/DFF still renders and only DST-compressed DSDIFF is skipped.
RUN apk add --no-cache ca-certificates tzdata sox ffmpeg chromaprint lsof && \
    decoders="$(ffmpeg -hide_banner -decoders)" && \
    for d in dsd_lsbf dsd_lsbf_planar dsd_msbf dsd_msbf_planar dst; do \
        echo "$decoders" | grep -qE "^ *[A-Z.]{6} +$d " \
          || { echo "ffmpeg is missing the $d decoder — the DSD renditions cannot run"; exit 1; }; \
    done && \
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

# 7788/tcp = HTTP/2 API; 7788/udp = HTTP/3 (QUIC). EXPOSE is image metadata
# only — operators still publish both (`-p 7788:7788/tcp -p 7788:7788/udp`);
# omitting the udp mapping silently drops HTTP/3 down to HTTP/2.
EXPOSE 7788/tcp 7788/udp

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
