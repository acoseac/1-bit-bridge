# v0.2.4 — release notes draft (operator pastes into the drafted GitHub Release)

> Working copy for the operator: a line per operator-visible change, as the PRs
> land (ops/release-playbook.md, section 0 step 3), turned into the release's
> notes on release day (section 4 step 5). The release after v0.2.3, 0.2.4 by
> plan. It covers the 151 commits from v0.2.3 to the release candidate
> `98efe691` (v0.2.3-151-g98efe691); the tag goes on the merge of these
> notes. The operator's decisions on the lines that were marked for
> checking are applied.

---

## What's new in 0.2.4

### The bridge's own player

- On a fresh install, the bridge's own player plays DSD under its default `auto` with no owner approval, natively, as DoP or converted to PCM, decided rate by rate for its DAC; an upgrade keeps the DSD limits 0.2.3's settings set (see "DSD after an upgrade"). DSD512, mono or multichannel DSD and DST-compressed DFF are refused, and a converted rate reaches the DAC as PCM that isn't bit-exact (backlog B728, B990, B1042, B1043, B1045).
- The bridge publishes the bound DAC's DSD choice on `GET /v1/outputs`, and an API, `PUT /v1/outputs/{id}/dsd`, changes it; the wire protocol stays at version 1, and no released app uses the change route yet (backlog B728).
- The console's Player page plays a test tone through the DAC, and its transports table shows how the DAC's DSD choice plays each rate, converted rates included (backlog B601, B1028, B1029).
- A track that fails before it is heard in a gapless queue no longer sends Play back to the track before it, while a playable entry follows the track heard (backlog B903AUD).

### The web console

- The console's player bar can play to the bridge's own player instead of the browser, at the player's fixed level (backlog B634).
- The console can also be opened on the home network, as an explicit opt-in (`adminLANAddress`) (backlog B989).
- The console works by touch on phones and tablets, its lists keep your place and filter by name, each album sort has an index rail a finger can drag, and a DSD row the browser can't play says how the bridge's player would play it (backlog B1021).
- An album held in several editions shows one edition at a time, with a picker, and its Delete's confirmation names each edition whose files it moves (backlog B1023, B1036).
- A Settings save keeps the changes it finds made to `bridge.yaml` by hand or by another program, which apply at the next start unless a `BRIDGE_*` variable sets that setting (and a change of `tailscale.uploadDiagnosticLogs` at the bridge's next launch, below), or refuses with nothing saved and says why (backlog B535).
- The console's upload page runs one upload at a time (backlog B797).

### Setup, CLI and doctor

- An update now counts as healthy after it has answered and stayed up for 10 minutes, and crashed starts before that can put the previous version back (backlog B660). Once 0.2.4 begins the database migration, crash rollback to 0.2.3 is disabled; see "The way back to 0.2.3".
- On Windows the updater waits out a moment's hold on the bridge's binary, and on a Windows Service install `bridge logs` and `bridge doctor` read the Service's own log (backlog B898, B618).
- Snapshots record each file's size and SHA-256 and are checked as they are taken, with one verdict for the database and one for the other files; the console and `bridge doctor` show the newest snapshot's two verdicts and name the newest whose database verified, whatever its other files' verdict, and `bridge backup --verify` checks one again (backlog B653).
- `bridge backup` prunes by the config's `backup.keep` (backlog B1035).
- `bridge restore` refuses to run beside a bridge command that has the library open (backlog B653, B826).
- `bridge doctor` gives each check that touches a library root 5 seconds, and the doctors, `bridge init`, the console and the log warn of a `bridge.yaml` or a data directory inside a library root by its path (one reached through a link, a junction or a bind mount gets no warning) (backlog B650, B884).
- `bridge init` that keeps a config prepares the data directory before it installs or starts the service (backlog B664).
- Commands that make, open or write the data directory refuse one that reads as not mounted, as serve does (backlog B727, B741).
- A config lock the bridge can't use names its remedy (backlog B537).
- `bridge init --force` keeps the enrichment mirrors and opt-outs and writes the rest of `enrich`, `fingerprint` and `atlas` at their defaults, as before, and a Settings save no longer writes a `BRIDGE_*` variable's value into `bridge.yaml` for a setting it leaves as it was (backlog B617, B761).

### Networking and the app

- The embedded Tailscale node uploads no diagnostic logs by default, a stop that gives up waiting for the node's start turns its uploads off for the process, and a library rename no longer renames a registered node whose state reads (backlog B749, B830).
- An empty `tailscaled.state` stops the embedded node's start with the bridge's own explanation of what to do (backlog B828).
- Enrichment, or Deezer's artist photos alone, can be switched off, and so can the update check (backlog B617, B761).
- The bridge indexes WavPack and Monkey's Audio files and filters its catalog rows by the formats a request declares, listing them only to an app that declares it plays them, which the released app doesn't yet; `/v1/list` still lists `.wv` and `.ape` files without a declaration, and their files are served unchanged to any paired app (backlog B489).
- A Christmas smart mix, drawn from the library's Christmas music, runs from 1 December to 5 January, UTC, for a request whose visible tracks include at least 15 Christmas tracks from three albums or three artists, so an app that sees 14 gets no Christmas mix even when the library holds a fifteenth in WavPack (backlog B750).
- Host names that aren't ASCII are published in their IDNA (`xn--`) form, which a newly made certificate carries (backlog B744).
- **The embedded Tailscale node moves to 1.104.5, among the dependency updates**: Tailscale 1.104.2 to 1.104.5, the SQLite driver 1.61.0 to 1.62.0, quic-go 0.64.1 to 0.65.0, golang.org/x/crypto 0.58.0 to 0.59.0 and golang.org/x/image 0.47.0 to 0.48.0, with x/term and x/mod; Go stays 1.27.3. Tailscale 1.104.5's netstack turns RACK loss recovery off unless `TS_NETSTACK_ENABLE_RACK=true` is set, which may slow recovery for tsnet transfers on paths that reorder packets; setting it brings back the stalls of seconds after a burst of packet loss that 1.104.5 turned RACK off for.

### Fixes

- The first scan after the update reads the library's tags again (backlog B743, B489, B428).
- A Vorbis track or disc number written with a total, such as `3/12`, is read, and so is an M4A's `©wrk` work tag (backlog B743, B428).
- An album whose tracks in one folder carry different recording years shows as one album (backlog B756).
- A lost waveform curve can be rendered again at an app's request (backlog B652).
- An upload or a trash restore into a library root that reads as not mounted is refused with nothing written, and a volume mounted over a watched root is watched again (backlog B776, B777).
- The player checklist no longer tells you to point MPD at the DAC the bridge's own player drives (backlog B1022).
- **A stop during serve's start ends it cleanly** (backlog B662): a stop that arrived while serve seeded its upscale target could end it with exit 1 and a database error (`sql: transaction has already been committed or rolled back`).

## Before you upgrade or roll back

A `BRIDGE_*` variable in the service's environment, set to a value the running version accepts, overrides the matching `bridge.yaml` key at every start, 0.2.3's included, and an empty value leaves the key in effect. It can come from a systemd unit's or drop-in's `Environment=` line, a launchd plist, or Compose, whose shipped `compose.yaml` passes `.env`'s values only for the variables it lists. So before an upgrade or a rollback, check both the file and what reaches the service's environment.

### DSD after an upgrade (backlog B728, B990, B1042, B1043, B1045)

On Linux with `output.player` on, a DAC with no DSD choice of its own takes the bridge's default mode: `auto`, unless an earlier version's opt-out carried over or the state file didn't read, either of which makes it `off` (both below). That opt-out is an explicit `dsdMode: off`, or `dop` without `allowDoP: true`. With `output.player.dsdMode` unset, whatever `allowDoP` and `dsdQualification` say, DSD plays under `auto` from the player's first start on 0.2.4, where 0.2.3 played none: to keep it off, set `dsdMode: off` before that start. That start's log says whether the opt-out was saved, with `defaultMode=off` where it holds. The Info line "the legacy DSD keys were migrated into the player's DSD state" saved it. A Warn line saying the migration "was not saved" and the player "holds it for this run", or that it "reached the state's file and is not known to be durable", holds it for that run: fix what its `err` names. A Warn line saying "the player starts without it" means the migration was refused before its write, and the start runs under `auto`. The next player start reads the file, and migrates again only if no marker was saved and a legacy key is still set. The player paragraph of `config/bridge.yaml.example` gives each key's case.

What 0.2.4 keeps of a 0.2.3 setting: an opt-out whose save fails still holds for that run, and the log says so. Only a migration refused before its write starts without it. A 0.2.3 `dsdMode: auto` without `allowDoP: true` plays no DoP until you confirm DoP for the DAC's current firmware, where its firmware reads (`confirmDoP`, below). Until then such a rate converts to PCM, or is refused where the DAC takes no conversion. Under `dop`, or `auto` with `allowDoP: true`, a 0.2.3 approval record's DoP carries over at the rates it approved: the migration's DoP confirmation counts only at rates where a clean DoP answer holds on the current firmware. A migrated choice carries no rate cap. An explicit `native`, or `dop` with `allowDoP: true`, whose approval record is missing or refused carries over with no migrated answers and no DoP confirmation. It never converts to PCM. It refuses a rate that needs a listener's answer (`native` on an unknown generic board) or a DoP confirmation (`dop` at a rate the catalog does not claim).

To see what each rate does: after 0.2.4's first start, with the bound DAC connected and the player running, `GET /v1/outputs`' bridge element says what DSD64, DSD128 and DSD256 each do on that DAC: `dsdPlayback.tuples` (native, or transport `dop`), converted, and refused with each reason. The console's Player page lists the rates played each way, but not the refused ones. To change what a rate does, stop playback (a pause or the console's test tone counts as playing) and send `PUT /v1/outputs/{id}/dsd` (its request, with the `dacToken` that `GET /v1/outputs` gives, is in `PROTOCOL.md`). Its `confirmDoP`, with mode `dop` or `auto`, confirms DoP for the DAC's current firmware, where its firmware reads, and replaces a confirmation the migration wrote. No released app sends this request yet, and the console has no DSD control. The check covers only the DAC bound now. If that start's log says `waitingFor=record_model`, a DAC of the record's model takes 0.2.3's mode and approvals once it is bound, if the record is still there and accepted and that DAC has no choice of its own other than the migration's fallback, so check again then. An operator sets a choice by hand only through that PUT. The migration, and a restore of a snapshot that holds `player-dsd.json`, also write DAC choices. A choice is kept for the DAC's model from its maker, so the units of that model from that maker share it.

Where `player-dsd.json` doesn't read, the player refuses DSD at every rate (`dsd_state_unreadable`), and serve's log says whether its bytes were kept aside (as `player-dsd.json.bad-<time>`, beside it) or the file stays where it is. To start the state afresh, stop serve, move `player-dsd.json` aside and start serve again: the state then starts as a fresh install's does, an earlier version's keys migrated again where `bridge.yaml` still sets them. Or, with serve running and playback stopped, set the bound DAC's choice through `PUT /v1/outputs/{id}/dsd`, which ends the refusal and leaves the default `off`; where the log says the file stays where it is, move it aside before that write. A restart uses fresh-install defaults, and retries any legacy migration, only if `player-dsd.json` is still absent.

### The way back to 0.2.3 (backlog B660, B1037)

Once 0.2.4 has opened its data directory and begun to migrate the database (schema 60 to 62), as its first start does once its settings load, neither the console's Roll back nor the crash rollback puts 0.2.3 back: the way back is by hand, by one of two paths.

On either path, first remove from the live `bridge.yaml` the five keys this release adds, and check that none is left: `adminLANAddress` (even empty), `enrich.enabled`, `enrich.deezerEnabled`, `update.checkEnabled` and `tailscale.uploadDiagnosticLogs`. 0.2.3 refuses to load a file that holds any of them, in `bridge restore` too. It also ignores their `BRIDGE_*` variables, so what those turned off runs there as 0.2.3 runs it: in `tsnet` mode its node uploads Tailscale's diagnostic logs unless `TS_NO_LOGS_NO_SUPPORT=true` is in its environment. And switch `update.autoInstall` off before 0.2.3 starts, in `bridge.yaml` and in any `BRIDGE_UPDATE_AUTO_INSTALL` the service's environment sets (above), as `false` or by removing it, since 0.2.3 ignores a value such as `off` and the file's then holds: 0.2.3 can install 0.2.4 again at its first check.

- **The binary alone, the default.** It keeps everything that changed since the update, devices paired since included. Stop the bridge, put the 0.2.3 binary in place of the bridge's (the updater's backup beside it, the binary's name with `.bak` added, until the start after 0.2.4's install is confirmed removes it, or else the 0.2.3 release's archive; with Docker, the `0.2.3` image on the same volume), and start it. 0.2.3 serves the migrated database as it stands, and serves the WavPack and Monkey's Audio tracks this release indexed to every app, apps that can't play them included, until its scans, which skip those files, drop their rows after `scanner.deleteAfterMissingScans` scans that count the paths as missing (3 by default); the files stay. A root 0.2.3's scanner protects as empty, such as one with no subfolders that holds only `.wv` and `.ape` files, its tracks indexed and no `.bridge-allow-empty` marker in it, can keep serving those rows. 0.2.3 reads neither `player-dsd.json` nor `data-dir-id.json`: its player plays DSD as it did before the update, by `dsdMode`, `allowDoP` and the approval record, and no DSD choice made in 0.2.4 applies there. Its first start may say that it won't roll back to `.bak`, and clear the install it read: that is expected.
- **A snapshot from before the update that holds `bridge.db`, only to have the schema-60 database back.** Copy the snapshot's folder out of the backups folder first if you may want it later: retention keeps a snapshot from before 0.2.4 only among the `keep` newest once a newer one's database has verified, about a week at the defaults. Then stop the service and every other program that has `bridge.db` open, remove the five keys from the live `bridge.yaml`, put the 0.2.3 binary in place as above, and run its own `bridge restore` (0.2.4's opens the restored database and migrates it to 62 again). Check the restored `bridge.yaml`, the snapshot's, before starting the service: switch `update.autoInstall` off in it, and in the service's environment as above, and put back the settings changed since the snapshot that 0.2.3 knows, none of the five keys among them.

Either way, a later 0.2.4 reads the `player-dsd.json` the first one left. Where that file is absent, or reads with no migration marker (`migratedFrom` null), its first start migrates the keys then in effect, and a DAC with a choice of its own keeps it. Where it holds a marker, no fresh migration runs: to have keys changed on 0.2.3 read, move the file aside before upgrading again, which drops the DAC choices made in 0.2.4. A file that doesn't read is the case of the recovery paragraph under "DSD after an upgrade".

### Snapshots and restore (backlog B653, B826)

A snapshot taken before 0.2.4 records no sizes or hashes: `bridge backup --verify` on it, with `--full` or without, checks only that its files are there, opens no database, records no verdict and exits 1. `--verify` checks only a snapshot inside the configured install's backups folder, and refuses a copy elsewhere (exit 2), which `bridge restore` takes.

A restore replaces the database and the `bridge.yaml`, token store, TLS files and `player-dsd.json` the snapshot captured: a device paired after the snapshot must pair again, unless the snapshot holds no token store (one taken before the first pairing), and so must a device that pinned a certificate made since. A captured console account returns with its former password, its sessions signed out. Captured DAC choices also return. Revoke again any token present in the snapshot that you revoked later. Check the restored `bridge.yaml` before starting the service.

`bridge restore` sees only the hold a 0.2.4 command takes on the library: before it, stop every process of an earlier version and any other program that has `bridge.db` open (an `sqlite3` shell, say). It refuses a snapshot on read-only media, on storage without file locks or in a folder you can't write: copy the snapshot's folder to a local disk and restore from the copy.

### Tailscale diagnostic uploads (backlog B749)

In `tsnet` mode the embedded node uploads no diagnostic logs, and no network flow logs, unless `tailscale.uploadDiagnosticLogs: true` (in `bridge.yaml`, or Settings → Networking) allows them: an install updated from an earlier version with neither that key nor the variable below set stops uploading at its first start of 0.2.4, and a tailnet that requires the logs refuses the node until they are allowed, which the admin tile says. `BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS` overrides the key both ways (`true` allows them, `false` keeps them off over a true key), and an empty value leaves the key in effect. With Compose, first copy 0.2.4's `BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS` line into the `environment:` of a `compose.yaml` kept from 0.2.3; a change of that variable in `.env` then takes `docker compose up -d`, which recreates the container: `docker compose restart` keeps the old environment, so a `false` set there would leave the uploads on. To allow uploads, also clear an inherited `TS_NO_LOGS_NO_SUPPORT=true`. A change of the key, or a stop that gave up waiting for the node's start, takes a new bridge process: quit the bridge, its menu included, and launch it again, or restart its service; the menu's Start now, and the start its Setup offers, run the bridge in the menu's own process.

### `bridge doctor`'s 5-second bound (backlog B650)

`bridge doctor` gives each check that touches a library root 5 seconds, and finds and reads `bridge.yaml` before that bound starts: with `bridge.yaml` inside a root whose mount stopped answering, it waits on that root before any check.

### Warnings

- Turn the DAC and the amplifier down before the console's test tone: the bridge can't see their volume (backlog B601).
- `adminLANAddress`, once its listener opens, opens the console to anyone on that network with no password, over plain HTTP: they can watch it and change the bridge's settings, music and devices. A public-mode bridge refuses the address and doesn't start (backlog B989).
- An album's Delete moves the files of all its editions, not only the edition shown, but never an SACD image's or an upstream server's tracks, which it leaves alone; its confirmation names each edition (backlog B1023, B1036).

### Also before you update

- Reload any console tab left open from 0.2.3: where its Player page shows the player's transports, it says "Couldn't read the player" at each refresh until it is reloaded (backlog B1029, B1037).
- With `backup.keep` above 7, or `-1` to keep every snapshot, take the snapshot before the update with 0.2.3's `bridge backup --keep 0`: without `--keep`, 0.2.3's command keeps only the seven newest (backlog B1035).
- `bridge backup` now exits 0 only when its snapshot's two verdicts are `VERIFIED` and recorded (the database's `UNVERIFIED` where the install has no database yet), so a script that runs it reads a snapshot left unverified as a failure (backlog B653).
- Keep the data directory on storage that grants file locks: without them, no snapshot check, prune or scheduled snapshot runs while the newest snapshot waits for its check, which `bridge doctor`'s `backup-lock` line says, and `bridge restore` refuses (backlog B653).
- Serve keeps the data directory's identity in `data-dir-id.json` inside it and in the record `.bridge.yaml.datadir` beside the config, and, once the record holds it, refuses a data directory the record names that doesn't hold that identity. A serve that can't write the record, such as in a config directory it can't write, warns and goes on, and with no record a start can't tell a volume that isn't mounted from a fresh install. Leave `data-dir-id.json` in place and move it with the data directory, set `dataDir` to where you move the directory, and run CLI commands as the user the service runs as, since a command that can't read either file refuses. On a refusal, check that the intended volume is mounted at `dataDir`, then follow the refusal's own words, removing the record it names only where it says to (backlog B727, B741).
- An `autocert.domain` with no IDNA form loads as `autocert-domain.invalid`, with a warning, so the bridge runs under a public name that works nowhere until it is corrected, and a `customEndpoints` entry with none is dropped with a warning: check both before the update (backlog B744).
- `bridge update` reads `update.checkEnabled` as the service does: an off set only in the service's environment must be in your shell's too (backlog B761).
- The refusal of an upload or a trash restore into a root whose volume isn't mounted goes by the tracks the bridge has indexed under that root: under a root with none indexed yet, mount the volume before you write to it (backlog B776).
- An upload running in one console tab shows as interrupted in another: act on it only in the tab that runs it (backlog B797).

The documentation at https://1-bit.swiss/bridge/ and the engineering log in the repository's `ops/engineering-log/` hold the details.
