# 1-bit-bridge

[![Latest release](https://img.shields.io/github/v/release/acoseac/1-bit-bridge?sort=semver&display_name=tag&label=release)](https://github.com/acoseac/1-bit-bridge/releases/latest)
[![License: FSL-1.1-MIT](https://img.shields.io/badge/license-FSL--1.1--MIT-blue)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/acoseac/1-bit-bridge)](go.mod)
[![iOS app on App Store](https://img.shields.io/badge/iOS%20app-1--bit%20on%20App%20Store-0A84FF?logo=apple&logoColor=white)](https://apps.apple.com/us/app/1-bit/id6762529497)

Companion server for the **[1-bit iOS music player](https://apps.apple.com/us/app/1-bit/id6762529497)** — bit-exact playback of your own music library, including DSD, from anywhere. The bridge runs on a home server (Windows / Linux / macOS / NAS) and exposes your library over HTTPS + bearer-token auth with a pre-built manifest endpoint, so the iOS app syncs its whole library in a single request instead of walking a slow SMB tree.

👉 **[1-bit.app/bridge](https://1-bit.app/bridge/)** for the landing page, setup walkthrough, feature reference, troubleshooting, and privacy policy.

## Why

1-bit's existing SMB transport is great on LAN but painful over Tailscale / similar overlays: SMB's chatty pread pattern multiplies every round-trip, and DERP-relayed connections can push latency past 3 seconds. 1-bit-bridge replaces SMB with an HTTP/2 protocol purpose-built for the app:

- **5 primitives** matching the iOS app's internal `SMBConnectionPool` API (`list`, `stat`, `readRange`, `download`, `downloadStreaming`), so the existing scanner + playback code paths are unchanged.
- **Pre-built library manifest** — the bridge keeps a SQLite-backed index; iOS fetches the full library (or a `?since=<mtime>` delta) in one call. Both scan phases (walk + enrich) are replaced by a single HTTP GET.
- **Bit-exact preserved** — the bridge delivers bytes; iOS still pre-caches the full DSF file before playback, protecting the Hugo 2 DoP lock. The bridge never transcodes.

## Status

**v0.2.3** — wire protocol stays at [`PROTOCOL.md`](PROTOCOL.md) v1 (additive only since v0.1.7). **The bridge can be the player**: on a Linux box with a USB DAC, `bridge serve` plays this bridge's FLAC, WAV, AIFF and ALAC files through ALSA at their own rate and bit depth, as an OpenHome renderer the 1-bit app casts to. It is PCM only for now (a DSD track is refused, never converted: DSD on the bridge's own player needs a listening qualification and comes later), off by default, Linux only and refused in public mode. It seeks within a track, and a 16-bit track plays on a DAC with no 16-bit format, losslessly in a wider container. The console's **Player page** shows its status and DAC, turns it on or off, chooses the DAC and lets the app cast to it. On a box that runs MPD (Volumio, moOde, Raspberry Pi OS), the bridge tells the app about that MPD and its DAC, and `bridge doctor` prints a Player / DAC / Music / Phone checklist. **A one-line installer for Linux** (`curl -fsSL https://1-bit.app/install.sh | sh`) installs and upgrades the bridge as the user's own service, checked against the release's checksums. Favorites, playlists and the library announce their changes to the app's other devices, and a favorite removed on one device stays removed; the library's `.m3u` and `.m3u8` files are served read-only; DLNA answers on the home network only, never on a tailnet. Whole numbers in `bridge.yaml` are read strictly (a fraction or a leading zero stops the bridge from starting, and `bridge doctor` names the line), and the first start after an upgrade from before v0.2.2 re-reads the library's tags once (ExtractorVersion 30). A host whose name isn't ASCII can install, and once the bridge knows a library or data dir, a disk that mounts after it starts isn't taken for an empty one. macOS binaries are Developer-ID-signed and Apple-notarized; Windows binaries are unsigned for now. See the [releases page](https://github.com/acoseac/1-bit-bridge/releases) for per-release notes.

## Install

**Linux, in one line** (moOde, Raspberry Pi OS, Volumio, Debian, Ubuntu and the rest; 64-bit, amd64 or arm64). Run it as the user the bridge should run as, not as root:

```sh
curl -fsSL https://1-bit.app/install.sh | sh
```

It downloads the release it came with (`https://github.com/acoseac/1-bit-bridge/releases/latest/download/install.sh` is the same script) with curl, which it needs, checks the archive's SHA-256 against the release's `checksums.txt`, installs the program as `~/.local/bin/bridge`, and runs `bridge init`, which offers your music folders, writes the settings and installs the per-user systemd service. It asks for `sudo` once, and says why first: to turn on lingering (`loginctl enable-linger`), so the service keeps running after you log out and starts at boot. It finishes with how to pair your phone: the console through an SSH tunnel, or `bridge pair` in the terminal. Run it again later and it upgrades the bridge through `bridge update`, which checks your paired devices first and keeps the previous program for a rollback. A box with anything else of the bridge on it (a `~/.local/bin/bridge` no service of yours runs, settings without a service, drop-ins left for a service that is not there, a system service, a service with drop-ins that set its program or its environment, or a change to its unit your service manager has not reloaded) is refused with nothing changed, and told what to run instead.

Options go after `sh -s --`, for example `curl -fsSL https://1-bit.app/install.sh | sh -s -- --library /mnt/USB`: `--library PATH` (the music folder; without a terminal to ask, the installer needs it), `--version X.Y.Z` (a new install of that release), `--no-init` (a new install of the program only), `--uninstall` (prints the commands that remove the bridge, and changes nothing; your music is never among them) and `--help`, which lists the exit codes too.

To read it before you run it:

```sh
curl -fsSL https://1-bit.app/install.sh -o install.sh
less install.sh
sh install.sh
```

**The bridge's own player and the `audio` group (Linux).** The player (off until you turn it on, on the console's Player page) opens the DAC's node under `/dev/snd`, which the `audio` group owns. A desktop login opens it through its seat's ACL; the bridge's service, which lingering keeps running with nobody at the seat, opens it only when the bridge's user is in that group. When the box has a DAC node and your user is not in its group, the installer says so and prints the two commands below with your user's name, uid and the node's group filled in; it runs neither (adding a user to a group is root's). By hand, as the user the bridge runs as (each asks sudo for root):

```sh
id -nG "$USER"                                  # the account's groups: is audio among them?
sudo usermod -aG audio "$USER"                  # if not
sudo systemctl restart user@"$(id -u)".service  # or reboot
```

The restart is the user's service manager's, `user@<uid>.service`: a lingering manager keeps the groups it started with, and so does every service it runs, so restarting the bridge's own service is not enough, and logging in again does not restart it. Restarting it avoids a reboot, but it stops every service of yours, a desktop or terminal session under it included: run it from another administrative session (in a root shell, put the user's name and uid in place of `"$USER"` and `$(id -u)`). The bridge's service starts again with it. That a lingering manager keeps its groups, and that root's restart of it gives its services the new ones, is read from systemd, not tried on a box. If your user is in the group and you joined it since you logged in, the restart alone is needed. `bridge doctor`'s `player-dac` line, and the console's Diagnostics page, say whether the bridge may open the DAC's node and why not, and the Player page names the cause when a play was refused for it.

**By hand, any platform:** grab a pre-built binary from the [releases page](https://github.com/acoseac/1-bit-bridge/releases) — no Go toolchain required. Pick your OS + arch, then run `bridge init`.

**macOS / Linux:**

```sh
tar -xzf 1-bit-bridge_*_macos_arm64.tar.gz   # or linux_amd64 / linux_arm64 / macos_amd64
./bridge init
```

Writes config + TLS cert under `~/Library/Application Support/1-bit-bridge/` (macOS) or `$XDG_CONFIG_HOME/1-bit-bridge/` (Linux), registers a launchd user agent / systemd user unit, opens the admin console.

**Windows** (PowerShell or File Explorer):

```powershell
# Unzip 1-bit-bridge_*_windows_amd64.zip (or _arm64)
bridge.exe init
```

Two install paths on Windows:

- **Startup-folder launcher (default)** — `bridge init`. Per-user, runs at logon, stops on logout. No admin required. Launcher lives at `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\com.acoseac.1-bit-bridge.cmd`; config under `%LOCALAPPDATA%\1-bit-bridge\`.
- **Windows Service (survives logout)** — `bridge init --service`, run from an **elevated PowerShell**. Installs an SCM service that starts at boot, runs as LocalSystem, logs to `%PROGRAMDATA%\1-bit-bridge\bridge.log`. Because the service runs as LocalSystem, point `--library` at a machine-wide path (e.g. `C:\Music`) rather than `%USERPROFILE%\Music`.

Uninstall: run `bridge init` again and answer "no" to overwrite (Startup launcher), or `sc.exe delete 1-bit-bridge` from an admin shell (Windows Service).

### Preflight: `bridge doctor`

Before (or any time after) running `bridge init`, `bridge doctor` prints a punch list of what's wrong in your environment:

```
[ok]   platform            darwin/arm64
[ok]   config-dir          /Users/me/Library/Application Support/1-bit-bridge
[ok]   data-dir            /Users/me/Library/Application Support/1-bit-bridge/data is a directory
[ok]   tls-cert            present, expires in 312 days
[warn] tls-cert-sans       stale — 3 of 4 name(s) and 3 of 6 address(es) this bridge advertises are not in the certificate
  ↳ clients dialling nuc, nuc.local, nuc.tailnet.ts.net, 192.168.50.24, 100.64.0.24, fd7a:115c:a1e0::1 fail TLS
    hostname verification. Either the data directory was moved to a host this certificate was not minted on, or an
    endpoint was added since. Run `bridge cert rotate` and restart the bridge, then re-pair every paired device —
    a rotation changes the SHA-256 fingerprint iOS pinned at pairing.
[FAIL] port-api            :7788 in use
  ↳ another process owns this port; stop it or pick a different address in bridge.yaml
[ok]   port-admin          free (:7789)
[ok]   library-roots       1 root(s) reachable
[ok]   service-manager     launchctl available
[ok]   browser-opener      open
```

`bridge init` runs doctor automatically and bails on `fail` (use `--skip-doctor` to override — not recommended).

**Admin console** — [http://127.0.0.1:7789/](http://127.0.0.1:7789/). Add/remove library folders, pair iOS devices (QR + copy-buttons), revoke tokens, view scan state + stats, and run the preflight checks + read operational counters from the **Diagnostics** page. Loopback-only, no auth — anyone on the machine already has filesystem access to the token store. (For internet-exposed deployments, see `bridge init --public` in [the public-deployment docs](https://1-bit.app/bridge/features/#public-mode) — that profile binds the admin console to a routable interface and adds a single-user password login.)

**Pairing an iOS device**: on the Devices page, click _Pair new device_, give it a name, optionally edit the bridge URL (defaults to `https://<hostname>.local:7788`), then generate the token. The modal shows a QR encoding an `onebit://pair?...` URL — scan it in the 1-bit app (2.2 or later; earlier versions read only `bridge://` links), or copy the URL/token/fingerprint fields manually.

**Metrics** — the admin listener also serves Prometheus text at [http://127.0.0.1:7789/metrics](http://127.0.0.1:7789/metrics): SQLite lock waits, enrichment cache hit rates, conversion-job durations, log-event counts. Always on, and loopback-only like the rest of the console; on a public-mode bridge a signed-in session reads it, and a scraper needs its address in `metrics.allowCidrs` and a direct connection (loopback is not implied there, since a proxy on the same host connects from it too). The Diagnostics page shows the current values; point a scraper at `/metrics` if you want history.

## Backup and restore

`bridge backup` captures the bridge identity and library state while it runs.
Stop the service before `bridge restore`. When moving to another box, stop
the source too: two boxes running one identity at once are unsupported.
Prefer the newest snapshot; see the [backup guide](docs/backup.md).

## Updates

The bridge checks GitHub for a newer release, on by default: one request to
`https://api.github.com/repos/acoseac/1-bit-bridge/releases/latest` when it
starts and then once every check interval (6 h unless
`update.checkIntervalHours` says otherwise), naming the bridge's version and
nothing of your library. The console's Settings → Updates offers the install,
and `bridge update` does the same from a shell; auto-install
(`update.autoInstall`) is off unless you turn it on.

To turn the checks off, switch off **Check for updates** in Settings → Updates,
which applies at once; or set `update.checkEnabled: false` in `bridge.yaml`,
or `BRIDGE_UPDATE_CHECK_ENABLED=false` in the service's environment, either of
which takes effect when the service next starts. The variable, where it is
set, overrides the file and the console's switch at every start. Off, the
bridge asks GitHub nothing and installs nothing new: no check at its start or
on its schedule, no Check now, no Install, no `bridge update`, and no
automatic install (auto-install keeps its setting and waits). The update
status, the console's Roll back, a start's rollback of an install that
failed, and a new version's probation are never refused. Turned back on in
the console, the next check comes a full interval later; a bridge started
with them on checks at once.

`bridge update` reads the file and the variable when it starts, and again
before it installs, so an off saved in the console while it waits refuses
the install, unless `BRIDGE_UPDATE_CHECK_ENABLED` in the command's own
environment says on, which overrides the file; an off set only in the
service's environment (a systemd unit, a compose file) must be in your
shell's too for the command to see it. An off saved in the console while a
`bridge update` is swapping a new version in waits for the swap and is saved
once it is done. What stays open here, for the bridge's Windows service, is
backlog B831.

A release from before the switch refuses settings that write
`update.checkEnabled`: before rolling back to one, turn the checks on in the
console, which removes the key, and while a new version is on probation over
such a release the console refuses an off. To keep the checks off there,
leave the key out of `bridge.yaml`, set `BRIDGE_UPDATE_CHECK_ENABLED=false`
in the service's environment and restart the service; that release ignores
the variable, so it checks for updates again once it runs. The full
behaviour is in [`PROTOCOL.md`](PROTOCOL.md#updates).

## Build from source

Requires Go 1.27.3+.

```sh
make build          # builds ./bin/bridge for the host OS
make build-all      # cross-compiles darwin/linux/windows × amd64/arm64 into dist/
make test           # unit tests
```

For a local release dry-run: `goreleaser release --snapshot --clean`.

## Contributing

PRs welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the branch / PR conventions, the pre-push checklist, and the **mirror-PR rule** that applies whenever a change touches the wire protocol shared with the [1-bit iOS app](https://github.com/acoseac/1-bit).

## Security

Found a vulnerability? Please report privately via GitHub's [Security Advisories](https://github.com/acoseac/1-bit-bridge/security/advisories/new) — see [`SECURITY.md`](SECURITY.md) for scope, response SLA, and what to include in your report.

## Tailscale modes

The bridge offers three Tailscale integration modes via the `tailscale.mode` key in `bridge.yaml`:

- **`cli`** *(default)* — shells out to the host's installed `tailscale` binary for status detection and Let's Encrypt cert provisioning on `*.ts.net` connections. Requires Tailscale to be installed and running on the host.
- **`tsnet`** — embeds a tailnet node directly in the bridge process via `tailscale.com/tsnet`. No Tailscale binary required; LE cert is renewed in-process. Authenticate with `bridge tsnet auth` on first run, or set `tailscale.authKey` for unattended deployments.
- **`disabled`** — turns off both the CLI auto-pilot and the embedded tsnet node. Use this for LAN-only deployments. The admin tile renders a one-line explanation so operators who flipped to `disabled` accidentally can recover.

Mode changes require a bridge restart. The admin tile shows the current Tailscale state and any errors from the active path.

In `tsnet` mode, what leaves the bridge for Tailscale is what any Tailscale client sends: the node's registration, keys and name and a description of the host to Tailscale's coordination server, traffic through Tailscale's DERP relays when peers cannot connect directly, and, once it serves HTTPS, the requests for its `*.ts.net` certificate. **Tailscale's diagnostic logs are not uploaded unless you allow them**: the bridge sets `TS_NO_LOGS_NO_SUPPORT=true` for the node unless `tailscale.uploadDiagnosticLogs: true` is in `bridge.yaml` (or ticked in Settings, Networking), which also keeps network flow logs off. A `BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS` set to `true` or `false` takes precedence over the key, as every `BRIDGE_` variable does over `bridge.yaml`; empty or unset, the key decides. The switch takes effect when the bridge's process restarts: once the bridge has run its node with the switch off, the uploads stay off until that process exits, so quit the bridge, its menu included, and launch it again, or restart its service (stopping it and choosing Start now in the menu runs it in the same process, and so does the start `bridge init` offers). What stays open is backlog B830. A `TS_NO_LOGS_NO_SUPPORT=true` in the bridge's own environment keeps the uploads off even with the key set. Where the switch is on and the uploads are off all the same, the admin tile says what holds them off. A tailnet whose policy requires those logs refuses a node without them; the admin tile then says what to change. Checked with a release-shaped build whose TCP connections were confined to the port of a local proxy (by port, not by address) that records every request and refuses it ([`scripts/tsnet-uploads`](scripts/tsnet-uploads/README.md)): without the key, and with the key but that variable set, it asked for Tailscale's log host in none of three 60-second runs, nor in a 60-second run on the data dir an earlier build had left with logs waiting, which that build asked to send within a second; with the key it asked in every run, about two seconds after starting. Before going back to a version from before this setting, remove `tailscale.uploadDiagnosticLogs` from `bridge.yaml`: an older bridge refuses the key at load, whatever its value, and does not start; it also uploads Tailscale's diagnostic logs unless `TS_NO_LOGS_NO_SUPPORT=true` is in its environment.

The node's name is `tailscale.hostname` when set. Otherwise a node already registered keeps the name it has (a library rename no longer renames it, so its MagicDNS name stays what paired devices know), and a new node registers as `1-bit-bridge` (Tailscale adds a suffix if another node has that name). After `bridge tsnet logout` the next start is a new node. A backup does not carry the node's state (`<dataDir>/tailscale`): a restore leaves the node on that box as it was, name included, and on a box without that state the node is new and authenticates again.

## Manual run (without `bridge init`)

For anyone who wants to skip the service install or run the bridge out of an arbitrary directory:

```sh
./bridge init --no-service --yes --library /path/to/music --dir ./bridge-data
./bridge serve --config ./bridge-data/bridge.yaml
```

Whole-number values in `bridge.yaml` must not have a leading zero. For example,
`output.loopbackPort: 08080`, which loaded as 8080 at revision `a6393b1e`, now
refuses to load. Write `8080` for decimal, or use an explicit `0o` prefix if you
mean octal. The error names the line to fix. This also applies to counts and
intervals; `0`, `30.0`, `3e1` and explicit-base numbers remain accepted. In
`output.player`, a refused value disables the player and is preserved on save.
`bridge init` writes numbers without leading zeros; when rewriting an existing
file, it warns and drops an offending output setting while keeping valid neighbours.

## Protocol

See [`PROTOCOL.md`](PROTOCOL.md). Every client endpoint is prefixed `/v1/`. The admin console (`/`, `/api/*`, `/static/*`) lives on a separate loopback listener and is **not** part of the wire protocol — iOS never talks to it.

## License

[FSL-1.1-MIT](LICENSE) — the Functional Source License, with an automatic MIT future grant. In plain language (the [`LICENSE`](LICENSE) text governs):

- **Self-hosting stays free, for everyone.** Running the bridge for your own library, personal and internal use, modification, and redistribution are all permitted — nothing changes for operators.
- The restricted use is a **Competing Use** as defined in the license: making the bridge available to others in a commercial product or service that substitutes for it (or for a product or service offered with it), or that offers substantially similar functionality — for example shipping it as another app's companion server, or selling it as a hosted service.
- **Each version automatically becomes available under MIT on the second anniversary of the date that version is made available** (the Future License grant).
- Releases up to and including **v0.1.9** were published under MIT and remain MIT.

The wire protocol documentation ([`PROTOCOL.md`](PROTOCOL.md)) remains public.
