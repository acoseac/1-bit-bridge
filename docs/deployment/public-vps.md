# Public-VPS deployment

Run 1-bit-bridge on a public-internet host (Hetzner, DigitalOcean, your own VPS) and reach it from the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS app over WAN. Files come from a Backblaze B2 (or any S3-compatible) bucket via rclone FUSE; TLS uses native Let's Encrypt via ACME TLS-ALPN-01; the admin console gets username/password auth + Secure SameSite=Strict session cookies.

This posture is a deliberate, single-config-block flip:

```yaml
deployment:
  mode: public
```

That one switch:

- Allows the admin console to bind non-loopback addresses (gated behind session auth).
- Defaults mDNS off (no LAN to advertise on) and Tailscale off (VPS deployments rarely run it).
- Filters `/v1/health.endpoints` so iOS never sees the VPS's internal LAN addresses or hostnames.
- Suppresses the TLS fingerprint banner in `bridge init` and `bridge serve` (iOS validates the LE chain via standard ATS rather than pinning).

The rest of the bridge — manifest scanner, MusicBrainz enricher, upscale pool, pairing flow — is identical to the loopback install.

## Prerequisites

- A VPS with **TCP/443 reachable from the public internet** (ACME's TLS-ALPN-01 challenge only validates on :443).
- A DNS A/AAAA record pointing at the VPS's public IP. Operators have used the host's own `<host>.example.com` directly OR a dedicated `bridge.example.com` subdomain — both work.
- Either:
  - **Variant A (recommended)** — `setcap` / capability to bind :443 from a non-root user, OR run the service as root (`User=root` in systemd), OR a port-forward / load-balancer that maps WAN:443 → bridge's listenAddress (and set `autocert.external443Mapping: true`).
  - **Variant B** — A TLS-terminating reverse proxy (Caddy / nginx) in front of the bridge.
- `rclone` installed (system package or [download](https://rclone.org/install/)) — used to FUSE-mount the B2 bucket.
- A Backblaze B2 application key with read access to the music bucket.

## 1. Provision the rclone mount

Tag `rclone config` to add a remote pointing at your B2 bucket. Then create a systemd unit (or launchd plist on macOS / Scheduled Task on Windows) that mounts the bucket at a stable path:

```ini
# /etc/systemd/system/rclone-music.service
[Unit]
Description=rclone FUSE mount for the music library
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/bin/rclone mount b2:my-music-bucket /mnt/music \
  --vfs-cache-mode full \
  --vfs-cache-max-age 24h \
  --vfs-cache-max-size 50G \
  --read-only \
  --allow-other
ExecStop=/bin/fusermount -uz /mnt/music
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`--read-only` is load-bearing: the bridge never writes to its library roots (no transcoding, no in-place tag rewriting), so denying writes here protects against accidental damage from any subprocess that misbehaves. `--vfs-cache-mode full` keeps the iOS download experience snappy for repeat plays.

`--allow-other` lets the bridge's user account read from a mount owned by the rclone-runner account. Required when you run the bridge as a non-root user different from the one that owns the rclone process. Edit `/etc/fuse.conf` to set `user_allow_other` if your distro disables it by default.

```sh
sudo systemctl enable --now rclone-music
ls /mnt/music  # confirm tracks are visible
```

## 2. Configure the bridge

`bridge init` has a `--public` mode that handles every public-mode-specific decision:

```sh
# Variant A — bridge terminates TLS itself via ACME
sudo bridge init \
  --public \
  --domain bridge.example.com \
  --email ops@example.com \
  --library /mnt/music \
  --name "My Music" \
  --yes

# Variant B — reverse proxy (Caddy / nginx) fronts TLS
sudo bridge init \
  --public \
  --domain bridge.example.com \
  --admin-tls-proxy \
  --library /mnt/music \
  --name "My Music" \
  --yes
```

`--library` is optional in public mode — you can run `bridge init --public ...` BEFORE the rclone mount is up and add the root via the admin console once it's mounted. The plan recipe above mounts first for symmetry; either order works.

The init output ends in a one-time banner:

```
┌─ Admin credentials — shown ONCE ────────────────────────┐
│ Save these now. The plaintext is not stored anywhere.   │
│                                                         │
│   Username:  admin                                      │
│   Password:  X7kQp9...                                  │
│                                                         │
│ Rotate with:  bridge admin reset-password               │
└─────────────────────────────────────────────────────────┘
```

**Copy that password out NOW** — it lives only in this banner. The on-disk `<dataDir>/adminauth.json` carries only the bcrypt hash.

## 3. Firewall

```sh
sudo ufw allow 443/tcp        # iOS clients dial here
sudo ufw allow 7789/tcp       # admin console (or restrict to your IP)
sudo ufw enable
```

For UDP-side HTTP/3 (optional but recommended for iOS over cellular):

```sh
sudo ufw allow 443/udp
```

## 4. Start the bridge

```sh
sudo systemctl enable --now 1-bit-bridge
sudo systemctl status 1-bit-bridge
```

The first TLS handshake against `bridge.example.com` triggers the LE cert mint — expect a 10–60 s delay on iOS's first pair attempt. Retry if the initial attempt fails; subsequent connections hit the cached cert.

## 5. Pair iOS

In the iOS app, tap "Add bridge" → enter `https://bridge.example.com:443`. The bridge serves the LE cert; iOS validates via standard ATS (no fingerprint pin on the public path).

The admin console at `https://bridge.example.com:7789/` (or whatever you set `adminAddress` to) requires a login with the admin / one-time password from `bridge init`. From there you can add library roots, mint device tokens, and approve pairings.

## Sample bridge.yaml

```yaml
libraryRoots:
  - /mnt/music
listenAddress: ":443"
adminAddress: "0.0.0.0:7789"
dataDir: "./data"
scanIntervalSec: 3600
libraryName: "My Music"

deployment:
  mode: public

autocert:
  enabled: true
  domain: bridge.example.com
  email: ops@example.com
  # cacheDir: ""        # defaults to <dataDir>/acme — DO NOT wipe (burns LE quota)
  # useStaging: false   # opt-in for LE staging during dev / testing

# tailscale.mode auto-resolves to "disabled" in public mode.
# mdns.enabled auto-resolves to false in public mode.

customEndpoints:
  - https://bridge.example.com
```

## Operating

| Task | Command |
|---|---|
| Rotate admin password | `bridge admin reset-password` |
| Add a library root via CLI | `bridge library add /path` |
| Mint a device token | Admin console → Devices → "Add device" |
| Check autocert status | Admin console → Settings → Public HTTPS (autocert) |
| Toggle Tailscale | Admin console → Settings → Networking → tailscale.mode |

## Optional: Tailscale-routed public bridge

Tailscale + public mode coexist fine. The bridge serves the public LE cert on `bridge.example.com` SNI AND the Tailscale-issued LE cert on `*.<tailnet>.ts.net` SNI — the SNI switcher routes both correctly. Set:

```yaml
tailscale:
  mode: cli   # or tsnet
```

Operators who want WAN routing through Tailscale (no public DNS, no exposed :443) skip autocert entirely and rely on the Tailscale-issued cert:

```yaml
deployment:
  mode: public
  adminTLSTerminatedByProxy: true   # admin runs on private tailnet interface
tailscale:
  mode: tsnet
autocert:
  domain: bridge.example.com   # still required for the Origin allowlist
```

The bridge advertises the Tailscale magic-DNS URL via `/v1/health`, paired iOS devices use that URL, and the bridge stays unreachable from outside the tailnet.

## Troubleshooting

**"unauthorized" from iOS after pair**
The iOS app shipped before v1.x doesn't trust the LE cert chain on a non-mDNS hostname. Update to the version that carries the public-host pinning exemption (see `MIN_CLIENT_VERSION` in the release notes).

**Admin login refuses Secure cookies**
Browsers refuse Secure cookies over plain HTTP. Either run autocert (Variant A) so the admin listener serves HTTPS, or front the admin with a TLS-terminating reverse proxy (Variant B) and set `deployment.adminTLSTerminatedByProxy: true`.

**ACME rate limit**
Let's Encrypt limits to 5 duplicate certs / week per registered domain. Wiping `<dataDir>/acme/` repeatedly during deployment-tuning burns this budget fast. Use `autocert.useStaging: true` while testing (untrusted cert, no rate limit).

**Library appears empty after mount drop**
The scanner's FUSE drop guards (PR #289) protect against this — an empty mount that previously had tracks doesn't trigger the deletion pass. Confirm `mount | grep rclone` shows the mount before re-running scans.

## Differences from loopback mode

| | Loopback | Public |
|---|---|---|
| Admin auth | None (loopback bind) | Session cookie + bcrypt password |
| TLS to iOS | Self-signed + pinning | LE chain via ACME |
| mDNS | On | Off |
| Tailscale | `cli` default | `disabled` default |
| `/v1/health.endpoints` | LAN + mDNS + Tailscale + custom | Custom + autocert domain only |
| TLS fingerprint banner | Shown at init | Suppressed |
| Admin credentials | n/a | Minted once at init |
