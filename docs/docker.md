# Running 1-bit-bridge in Docker

Homelab / NAS deployments (Unraid, TrueNAS, Synology) tend to
prefer Docker containers over the launchd / systemd / SCM
service-manager paths the `bridge init` flow installs. This
document shows the canonical layout — single-root and multi-root —
and explains the env-var overrides the bridge honours at runtime
so a YAML rewrite isn't required to retarget a containerised
deployment.

> **Status:** Dockerfile and env overrides are first-class
> supported. As of **v0.1.7** a pre-built multi-arch image
> (`linux/amd64` + `linux/arm64`) is published to the GitHub
> Container Registry for every release — building from source
> (below) stays supported for local hacking.

## Quickstart

The fastest path — one file, one command. Since **v0.1.8** the container
**auto-initialises on first boot** (it writes a default `bridge.yaml` if one
doesn't exist yet), so there's no separate `bridge init` step.

```yaml
# docker-compose.yml
services:
  bridge:
    image: ghcr.io/acoseac/1-bit-bridge:latest
    container_name: 1-bit-bridge
    ports:
      - "7788:7788/tcp"
      - "7788:7788/udp"        # HTTP/3 — omit this and the bridge falls back to HTTP/2
    volumes:
      - ~/music:/library:ro    # your library, read-only (see the permission note)
      - bridge-state:/data     # named volume — config, DB, TLS cert auto-generate here
    # environment:
    #   BRIDGE_LIBRARY_ROOTS: /library     # the default; override for a different/multi mount
    #   BRIDGE_LIBRARY_NAME: "Living Room"
    #   BRIDGE_UPSCALE_ENABLED: "true"     # optional audio features (see below)
    #   BRIDGE_ANALYSIS_ENABLED: "true"
    restart: unless-stopped
volumes:
  bridge-state:
```

```sh
docker compose up -d
docker compose logs -f          # watch the first scan
```

That's it — the bridge scans `/library`, writes its state into the
`bridge-state` volume, and `docker ps` shows a health status once it's up.
Pair iOS against `https://<host>:7788` with the fingerprint from
`docker exec 1-bit-bridge bridge cert info`.

**Two things that trip people up:**

- **Your library must be readable by the container user (UID 100).** The
  bridge runs as a non-root user; if the `/library` bind-mount is owned by
  someone else and isn't world-readable, the scan logs `library root
  unreadable …` and indexes **0 tracks**. Fix it host-side —
  `chmod -R a+rX /path/to/music`, `chown` it to UID 100, or add
  `user: "0:0"` to run the container as root. The `:ro` mount is never
  written to.
- **Use a named volume for `/data`** (as above), not a host bind-mount. A
  bind-mount like `./data:/data` is created root-owned, so the non-root
  process can't write its config/DB/cert into it; a named volume inherits the
  image's writable `/data`.

Prefer plain `docker run`, want to create the config explicitly, or running
public-mode? See [First-time setup](#first-time-setup) and
[Public mode](#public-mode-internet-exposed) below.

## Pull the published image

```sh
docker pull ghcr.io/acoseac/1-bit-bridge:latest      # newest release
docker pull ghcr.io/acoseac/1-bit-bridge:0.1.7       # a specific version
```

The image is public — no `docker login` needed to pull. It runs as a
non-root `bridge` user, exposes `7788`, and keeps all state under the
`/data` volume (see [First-time setup](#first-time-setup) below). Tags:
`latest`, the full `MAJOR.MINOR.PATCH` (e.g. `0.1.7`), and the
`MAJOR.MINOR` series (e.g. `0.1`).

## Build it yourself

```sh
git clone https://github.com/acoseac/1-bit-bridge.git
cd 1-bit-bridge
docker build -t 1-bit-bridge:dev .
```

Multi-arch builds (linux/amd64 + linux/arm64 for Apple Silicon
hosts) work via `docker buildx build --platform=linux/amd64,linux/arm64`.
The builder cross-compiles natively per target arch (it pins the build
stage to `$BUILDPLATFORM`), so an arm64 image builds without QEMU-emulating
the whole Go compile.

## First-time setup

The [Quickstart](#quickstart) auto-init path handles the common case with no
setup step. Create the config explicitly instead when you want public mode, a
hand-tuned YAML, or to see the pairing fingerprint printed up front:

### Option A: one-shot `bridge init` inside the container

```sh
docker run --rm -it \
    -v 1-bit-bridge-state:/data \
    -v ~/music:/library:ro \
    1-bit-bridge:dev \
    init --yes --no-service \
         --dir /data \
         --library /library \
         --name "Living Room Library"
```

This writes `/data/bridge.yaml`, mints a TLS cert at
`/data/server.crt` / `/data/server.key`, and exits without
installing a service unit (containers don't need launchd /
systemd).

### Option B: pre-supply bridge.yaml on the host

Drop a hand-written `bridge.yaml` into your `/data` volume before
the first `docker run`. Minimum viable config:

```yaml
libraryRoots:
  - /library
listenAddress: ":7788"
adminAddress: "127.0.0.1:7789"
dataDir: /data
libraryName: "Living Room Library"
```

The bridge will still mint a TLS cert on first run if one isn't
present.

## Running

The plain `docker run` form (the [Quickstart](#quickstart) uses compose). The
image's default command carries `--init-if-missing`, so this auto-inits on
first run too — a pre-supplied or `bridge init`-generated config is used as-is
if present:

```sh
docker run -d \
    --name 1-bit-bridge \
    -p 7788:7788/tcp \
    -p 7788:7788/udp \
    -v ~/music:/library:ro \
    -v 1-bit-bridge-state:/data \
    1-bit-bridge:dev
```

**Note:** Both TCP and UDP must be published for the API port (7788) to support HTTP/3 upgrades. If you omit the `/udp` mapping, the bridge will fall back to HTTP/2.

iOS devices pair against `https://<host>:7788` with the
fingerprint shown by `docker exec 1-bit-bridge bridge cert info`
(or via the admin console — see below).

## Environment overrides

The bridge picks up these env vars at startup, **after** loading
`bridge.yaml`. Env wins over YAML; YAML wins over hardcoded
defaults. Empty / unset env = no change.

| Variable | YAML field | Notes |
|---|---|---|
| `BRIDGE_LISTEN_ADDRESS` | `listenAddress` | Main HTTPS bind, e.g. `:7788`. |
| `BRIDGE_ADMIN_ADDRESS` | `adminAddress` | Loopback admin console. Stays loopback even when overridden — there's no auth. |
| `BRIDGE_DATA_DIR` | `dataDir` | Persistent volume path inside the container. |
| `BRIDGE_LIBRARY_NAME` | `libraryName` | Display name for the library. |
| `BRIDGE_DISABLE_HTTP3` | `disableHttp3` | Set to `true` to bypass UDP listeners. |
| `BRIDGE_LIBRARY_ROOTS` | `libraryRoots` | Colon-separated list. Example: `/library:/library2`. |
| `BRIDGE_UPSCALE_ENABLED` | `upscale.enabled` | `true`/`false`. Enables offline PCM upscaling + CarPlay-optimize. Uses the bundled audio toolchain — see [Audio features](#audio-features-upscale--analysis). |
| `BRIDGE_ANALYSIS_ENABLED` | `analysis.enabled` | `true`/`false`. Enables waveform / loudness / key-tempo analysis. Same toolchain. |

`BRIDGE_DISABLE_HTTP3`, `BRIDGE_UPSCALE_ENABLED`, and
`BRIDGE_ANALYSIS_ENABLED` are parsed with Go's `strconv.ParseBool`:
`true`/`false` (also `1`/`0`, `t`/`f`). A value it can't parse — `yes`,
`on` — is silently ignored, leaving the YAML/default in place.

### Why env overrides

Patching a YAML inside a baked image at runtime is doable but
ugly. Env overrides are the idiomatic container-config knob:
your `docker-compose.yml` or k8s `Deployment` spec can drive the
listen address / data dir / library mount points without ever
touching the YAML, and a `docker run -e BRIDGE_LIBRARY_ROOTS=...`
override on the CLI takes effect on the next start.

## docker-compose example (multi-root)

```yaml
# docker-compose.yml — bridge with two music libraries
services:
  bridge:
    image: 1-bit-bridge:dev
    container_name: 1-bit-bridge
    ports:
      - "7788:7788/tcp"
      - "7788:7788/udp"
    volumes:
      - ./bridge-data:/data
      - /mnt/nas/music:/library1:ro
      - /mnt/usb/music:/library2:ro
    environment:
      BRIDGE_LIBRARY_ROOTS: /library1:/library2
      BRIDGE_LIBRARY_NAME: "Multi-Drive Library"
      TZ: Europe/Berlin
    restart: unless-stopped
```

## Audio features (upscale + analysis)

As of **v0.1.8** the image bundles the audio toolchain — `sox`,
`ffmpeg`, and `ffprobe` — so the optional **offline upscaling /
CarPlay-optimize** and **audio-analysis** (waveform, loudness,
key/tempo) features work inside the container. Both are **off by
default** and cost nothing until enabled. Bundling `ffmpeg` pushes the
image to roughly **260 MB** (it dominates the size) — the deliberate
trade for in-container audio processing.

> There's no on-the-fly transcoding: the bridge pre-converts to FLAC
> sidecars offline and serves them bit-exact, the same as any other
> file. See [PROTOCOL.md](../PROTOCOL.md).

### Enabling

Flip either feature on with an env var (idiomatic for containers):

```sh
docker run -d --name 1-bit-bridge \
    -p 7788:7788/tcp -p 7788:7788/udp \
    -v ~/music:/library:ro \
    -v 1-bit-bridge-state:/data \
    -e BRIDGE_UPSCALE_ENABLED=true \
    -e BRIDGE_ANALYSIS_ENABLED=true \
    ghcr.io/acoseac/1-bit-bridge:latest
```

Equivalently, set `upscale.enabled: true` / `analysis.enabled: true` in
`bridge.yaml`, or use the admin console's Settings toggle (restart to
apply). Env wins over YAML, so `-e BRIDGE_UPSCALE_ENABLED=false` is a
clean kill-switch over a YAML that has it on. Use `true` / `false` — a
value `strconv.ParseBool` can't decode (`yes`, `on`) is silently
ignored, so a typo reads as "unchanged," not "off."

Verify the toolchain resolved:

```sh
docker exec 1-bit-bridge bridge doctor
```

Look at the **`audio-toolchain`** line — it reports `sox vX, FLAC
supported` when the toolchain is present (`docker exec` inherits the
container's `BRIDGE_*_ENABLED`, so the check runs rather than reporting
a no-op "not enabled"). The `port-api` / `port-admin` checks will show
**in use** here — that's expected, not a problem: `bridge serve` is
already holding those ports in the same container, and the bridge writes
no PID file for `doctor` to recognise its own listener, so it can't tell
itself apart from a foreign process. Those checks are a preflight signal
for a fresh host, not a running container.

### Variant storage (`variantsDir`)

Upscaled/optimized FLAC sidecars are written under
`upscale.variantsDir`, default `<dataDir>/transcoded` (inside the
`/data` volume). To keep them off the state volume, point it at a
dedicated volume. It **must be an absolute path and must not resolve
under any library root** — the bridge refuses to start otherwise.

**Permissions gotcha (non-root user):** the container runs as the
non-root system user `bridge` (UID 100 — the value the
[Troubleshooting](#troubleshooting) section cites; confirm with
`docker exec 1-bit-bridge id bridge`). A **separately-mounted**
`variantsDir` volume (or a host bind-mount used for `/data`) is created
`root:root` by Docker, and the `bridge` user then can't write sidecars
(`permission denied`). Pre-create the directory and `chown` it to that
UID/GID before starting:

```sh
mkdir -p ./transcoded && sudo chown 100:100 ./transcoded
# then: -v ./transcoded:/transcoded  and  upscale.variantsDir: /transcoded
```

The read-only library mount (`… :ro`) is immune — the bridge never
writes there.

### Worker counts on large / shared hosts

The bridge sizes its worker pools from `runtime.NumCPU()`
(`upscale.workers` = `min(NumCPU-1, 4)`; `analysis.workers` =
`max(1, NumCPU/2)`, uncapped). On a many-core or shared host a big
analysis run can spin up enough workers to saturate the box. For a
predictable bound, set `upscale.workers` / `analysis.workers`
explicitly in `bridge.yaml` — deterministic regardless of the
container's CPU-limit flavor. Docker `--cpus` / `--cpuset-cpus` are a
useful complementary throttle on top.

> Optional hygiene: mounting `/tmp` as `tmpfs` is fine, but note it's
> not the bottleneck here — the sox pipeline writes its `.tmp` sidecar
> under `variantsDir` (atomic rename) and analysis decodes stream over
> stdout pipes, so the heavy write path is the `variantsDir` volume.

## Admin console access

The admin console (default `127.0.0.1:7789` inside the container)
is unauthenticated — exposing it to the host port-forward layer
means anyone on the host can hit it. Two safer patterns:

### `docker exec` for one-off ops

```sh
docker exec 1-bit-bridge bridge status
docker exec 1-bit-bridge bridge cert info
docker exec 1-bit-bridge bridge library add /library2
```

### Reverse proxy with auth (advanced)

If you genuinely need the admin UI from a browser, run a reverse
proxy (Caddy / Traefik / nginx) with HTTP basic auth in front of
`http://127.0.0.1:7789` from inside the same Docker network. The
bridge's CSRF defenses still apply, but the loopback-only
binding inside the container is what stops anonymous LAN access.

## Public mode (internet-exposed)

By default the container runs in **loopback mode** — the iOS API on
`7788` and the admin console bound to `127.0.0.1:7789` with no
password. That's the right posture for a LAN / Tailscale deployment,
and everything above assumes it.

For a bridge intentionally exposed to the public internet, the bridge
has a **public mode** (`deployment.mode: public`) that adds a
single-user admin password and a Let's Encrypt certificate. Generate
the config once with `--public` — it prints the admin password a
single time, so capture it:

```sh
docker run --rm -it \
    -v 1-bit-bridge-state:/data \
    -v ~/music:/library:ro \
    1-bit-bridge:dev \
    init --public --yes --no-service \
         --dir /data --library /library \
         --domain bridge.example.com --email you@example.com
```

Public mode binds the API on `:443` (Let's Encrypt's TLS-ALPN-01
challenge validates only on TCP/443). The container runs as the
non-root `bridge` user, which can't bind a privileged port by default —
allow it with `--sysctl net.ipv4.ip_unprivileged_port_start=0`, and
publish `443` (TCP + UDP for HTTP/3):

```sh
docker run -d --name 1-bit-bridge \
    --sysctl net.ipv4.ip_unprivileged_port_start=0 \
    -p 443:443/tcp -p 443:443/udp \
    -v 1-bit-bridge-state:/data \
    -v ~/music:/library:ro \
    1-bit-bridge:dev
```

> Prefer not to bind a privileged port in-container? Init with
> `--listen-address :8443` instead, map `-p 443:8443/tcp -p 443:8443/udp`,
> and set `autocert.external443Mapping: true` in `bridge.yaml`. That
> opt-in tells the bridge a front door maps `WAN:443 → :8443` — which the
> public-mode config validation otherwise requires, since ACME only
> validates on 443.

The public domain must resolve to the host and TCP/443 must be
reachable for the certificate to issue. Rotate the admin password
later with `docker exec 1-bit-bridge bridge admin reset-password`.
Public mode also refuses to start the DLNA MediaServer (an
internet-facing host must not expose an unauthenticated browser).

### docker-compose (public mode + audio, mirroring a tuned VPS)

This mirrors the posture of a tuned public VPS: public mode + Let's
Encrypt, HTTP/3 on, a read-only library, audio features enabled, and
variant storage on its own volume. Public mode itself (domain, ACME
email, admin password) lives in `bridge.yaml` — env vars are too flat
to express it — so `init --public` once (above), then let compose drive
the rest:

```yaml
# docker-compose.yml — internet-exposed bridge with audio features
services:
  bridge:
    image: ghcr.io/acoseac/1-bit-bridge:latest
    container_name: 1-bit-bridge
    sysctls:
      # let the non-root user bind :443 for the ACME TLS-ALPN-01 challenge
      net.ipv4.ip_unprivileged_port_start: "0"
    ports:
      - "443:443/tcp"
      - "443:443/udp"          # HTTP/3 — omit and you silently drop to h2
    volumes:
      - ./bridge-data:/data    # holds bridge.yaml (from init --public)
      - /mnt/music:/library:ro # library, read-only
      - ./transcoded:/transcoded  # upscale sidecars; chown to the bridge UID (see above)
    environment:
      BRIDGE_UPSCALE_ENABLED: "true"
      BRIDGE_ANALYSIS_ENABLED: "true"
      TZ: Europe/Helsinki
    tmpfs:
      - /tmp
    # cpus: "2"                # optional throttle; also bound workers in bridge.yaml
    restart: unless-stopped
```

A few knobs in the `init --public`-generated `bridge.yaml` echo the VPS
tuning:

```yaml
deployment:
  mode: public
autocert:
  enabled: true
  domain: bridge.example.com
  email: you@example.com
scanIntervalSec: 21600        # 6h (the default) — keep it high on a
                              # network/rclone-backed library to cut churn
upscale:
  enabled: true
  variantsDir: /transcoded    # matches the volume above; absolute, off /data
  workers: 2                  # deterministic bound (optional)
analysis:
  enabled: true
  workers: 2
artwork:
  cacheMaxBytes: 2147483648   # 2 GiB on-disk cover cap (optional, small disks)
```

**The QUIC UDP tuning is host-level, not a container setting.** The
socket-buffer sysctls that materially help HTTP/3 on lossy links —
`net.core.rmem_max` / `net.core.wmem_max = 7340032` and
`net.core.default_qdisc = fq` — are host-global (not namespaced), so a
container / compose `sysctls:` block can't set them (only `net.ipv4.*`
and a few others are allowed inside a container). Put them on the
host/VM:

```sh
# /etc/sysctl.d/999-bridge-quic.conf  — the 999- prefix matters: later
# drop-ins win, and some cloud images ship a 99-*.conf that would clobber
# a lower number. Apply with `sudo sysctl --system`, then VERIFY with
# `sysctl net.core.rmem_max` (the apply-time output can lie).
net.core.rmem_max = 7340032
net.core.wmem_max = 7340032
net.core.default_qdisc = fq
```

Mounting the library itself (e.g. an rclone/B2 FUSE mount) is a host
concern — mount it on the host and bind-mount the path in, as
`/mnt/music` above.

For the full security model — firewalling, the admin-console
exposure options, and the reverse-proxy variant (`--public --proxy`,
where TLS is terminated upstream and `--email` isn't needed) — see the
[Public-VPS deployment runbook](deployment/public-vps.md).

## Logs

The container writes logs to stdout/stderr (no service-manager
log file). Use:

```sh
docker logs -f 1-bit-bridge
```

`bridge logs` and `bridge logs -f` from outside the container
won't work — those CLI subcommands tail
`packaging.DefaultLogPath()`, which is for the host-side service
install. Use `docker logs` for container deployments.

## Troubleshooting

- **iOS can't connect**: confirm `7788` is published (`-p 7788:7788`)
  and that the host's firewall (`ufw`, `firewalld`, Synology
  Security Advisor) isn't blocking it. Check
  `docker exec 1-bit-bridge bridge cert info` for the fingerprint
  iOS expects.
- **Library doesn't appear**: confirm the host bind mount is
  readable to UID 100 (the `bridge` user inside the image). On
  Synology / Unraid you may need `--user 0:0` to run as root, or
  fix host-side ownership.
- **First scan never finishes**: tail `docker logs -f`. Most
  failures are NFS-mount or permission flaps — the scanner spares
  affected subtrees from the deletion pass on transient errors,
  so a re-scan after fixing the issue lands cleanly.
