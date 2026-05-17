# Running 1-bit-bridge in Docker

Homelab / NAS deployments (Unraid, TrueNAS, Synology) tend to
prefer Docker containers over the launchd / systemd / SCM
service-manager paths the `bridge init` flow installs. This
document shows the canonical layout — single-root and multi-root —
and explains the env-var overrides the bridge honours at runtime
so a YAML rewrite isn't required to retarget a containerised
deployment.

> **Status:** Dockerfile and env overrides are first-class
> supported. We do **not** publish a pre-built image yet — pin
> against the repo and `docker build` locally for now.

## Build

```sh
git clone https://github.com/acoseac/1-bit-bridge.git
cd 1-bit-bridge
docker build -t 1-bit-bridge:dev .
```

Multi-arch builds (linux/amd64 + linux/arm64 for Apple Silicon
hosts) work via `docker buildx build --platform=linux/amd64,linux/arm64`.

## First-time setup

The container runs `bridge serve --config /data/bridge.yaml` by
default. That file doesn't exist on the first run — you have two
options:

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

Once `/data/bridge.yaml` exists:

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
