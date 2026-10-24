---
paths:
  - "cmd/bridge/**"
  - "internal/doctor/**"
  - "internal/handshakelog/**"
  - "internal/ctxerr/**"
  - "internal/backup/**"
  - "internal/tsnet/**"
  - "internal/tailscale/**"
  - "internal/api/events*.go"
  - "internal/api/event_broker*.go"
---
# The CLI and the serve wiring (`cmd/bridge`)

> **Condensed 2026-10-18.** Each long entry here is one line — its bold title plus its own bolded "Don't…/Never…" sentences — ending `→ History-rules-cli-serve.md`: that is `ops/History-rules-cli-serve.md`, where the entry sits verbatim with its reasoning, measurements and rejected alternatives. **Still binding.** Edit the line here and its long form there in the same commit; new long-form detail goes to that file, never back into this one.

The subcommands, `bridge init` and its preflight, `bridge doctor`, and `runServe`: its gates, loops, locks, shutdown and drains.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## The CLI and the serve wiring (`cmd/bridge`)

- **`bridge init`'s preflight grades the install that is THERE — all of it, not just its certs.** → History-rules-cli-serve.md
- **…and grades the ports it is about to SAVE, which is a different question** (#970) → History-rules-cli-serve.md
- **…and where no install's config loads, the preflight grades the ports this run WRITES, from the one definition the config is built from** (2026-09-28) **Don't seed the preflight from anything else, or build the config's addresses anywhere else** → History-rules-cli-serve.md
- **…and on a certain rewrite of an install whose config loads (`--yes --force`, or an interactive yes), the preflight grades only the install's ports the rewrite KEEPS** (2026-09-28) **Don't compare a stat** → History-rules-cli-serve.md
- **The "is it us?" fallback must NOT reach a port the run is choosing.** → History-rules-cli-serve.md
- **…and clearing it did nothing on a host without lsof, because the verdict read which tools the host HAS** (#1021) → History-rules-cli-serve.md
- **…and grades no port at all from a config it could not load** (#1022) → History-rules-cli-serve.md
- **…and config-dir does not probe as a user who cannot read the config** (#1023) → History-rules-cli-serve.md
- **…and over a config that is there and does not load, init recognises the bridge it replaces by the pid file in the data dir it WRITES, and only where the probe SEES that bridge on the port** (#1027) → History-rules-cli-serve.md
- **…and a live recorded bridge the probe did not see is explained by what the probe SAW, never by the one case the arm was written for** (#1028) **Never lsof** → History-rules-cli-serve.md
- **…and a live recorded bridge the probe RULES OUT is not ours on that port, so the port FAILs** (#1029) → History-rules-cli-serve.md
- **…and a bridge no probe can read is ruled out by the port's OTHER holders, and the uid arm answers only for a listener no readable process holds** (#1030) → History-rules-cli-serve.md
- **…and a listener another UID created is not the bridge's, so a holder this user cannot read FAILs too** (#1032) → History-rules-cli-serve.md
- **…and a listener created in a cgroup that does not nest with the bridge's is not the bridge's either, so a hidden holder of its OWN uid FAILs too** (#1033) → History-rules-cli-serve.md
- **…and on macOS a live recorded bridge that lsof lists listening elsewhere is ruled out by that LISTING, never by its uid** (#1034) → History-rules-cli-serve.md
- **`configuredPort` asks what an address NAMES; `splitHostPort` asks what can be DIALED, and they differ on exactly port 0.** → History-rules-cli-serve.md
- **A doctor hint is read by an OPERATOR, so no string in `internal/doctor` names a `Deps` field** (2026-09-28) → History-rules-cli-serve.md
- **`bridge init` decides every refusal BEFORE it writes `bridge.yaml`, and keeps what an install already has: its TLS pair and a public install's admin ACCOUNT** (#1038) **Don't mint over a store that does not load** **don't keep silently** → History-rules-cli-serve.md
- **A rewrite replaces the SETTINGS, never the INSTALL.** **Never drop or rewrite it** **never "proceed when init's data dir holds a pair"** **never "keep the paths only when both are set"** **Don't drop the output block, and don't read it as a field of `priorInstallFile`.** **Don't read the output block apart from the file when the whole file decodes: keep Load's own decode, and of a file the decoder refuses keep only what is certain; don't print a refused setting's value, or a managed control this build does not know.** → History-rules-cli-serve.md
- **A flag the run would not write is never dropped without a word, and a posture flag stops no first install** (2026-09-29, backlog B61) **Don't refuse a run over one that loses nothing by it** → History-rules-cli-serve.md


The largest package in the repo (`main.go` alone 4,698 lines) had **no section here** until 2026-09-06, and some of its invariants still sit by subject in other sections: the `bgWriters` join under Scanner, `SetPostScanHook` under Job pools, the cadence rearm under Config. → History-rules-cli-serve.md

- **"Always construct, never stop" removes a gate wherever the enclosing `if` WAS the gate.** **Don't gate these reads on the live predicate** → History-rules-cli-serve.md
- **…and a switch-off also stops the work QUEUED for it: each pool asks its live gate when a worker TAKES a job, and drops a job the gate refuses** (2026-10-15, backlog B155, the owner's decision) **Don't fold Drop into finishJob's section** **Don't drop a job at Enqueue, release a dropped job outside finishJob's critical section, count a drop as done or failed, ask the gate before the stop check (a job the stop took is shutdown's), key the transcode gate on `Background` or `BatchID`, or end a burst on the queue's length.** → History-rules-cli-serve.md
- **…and a value runServe DECIDES from the config while it builds Deps is a boot snapshot, however live its reader is** (2026-09-28) → History-rules-cli-serve.md
- **…and a LIVE reader of the flag alone splits the gate too: every consumer that answers "is upscaling on" reads `upscaleActiveFn`, the flag AND a usable sox** (2026-09-28) → History-rules-cli-serve.md
- **…and a surface that reports the gate beside its tool reads both from ONE probe: a cache over the shared one is a second clock** (2026-09-28, backlog B45) **Don't give a surface a cache of its own to save a probe: give the shared one to it.** → History-rules-cli-serve.md
- **A sweeper's `enabled` predicate fails CLOSED on nil**, and the gate check
  belongs in the loop's callback, not buried in the pass. `analysisSweeper.active()`
  returns false for a nil sweeper or a nil predicate; `runFingerprintSweeper`'s
  nil arm reads the other way and is its own call. A disabled pass records NO
  status, so the Jobs card keeps its last real breakdown.
- **Feature gates on a mutation handler run BEFORE path resolution and the
  folder walk.** Both checks depend only on the decoded body, and a request that
  will be refused should not first cost a recursive `WalkDir`. Asserting on the
  refusal STATUS cannot catch the ordering — 503-before and 404-after are both
  "refused"; assert on which one comes back for a nonexistent path.
- **Every subcommand tail resolves its config through `loadCLIConfig`.** → History-rules-cli-serve.md
- **A GC or reaper whose "in use" set comes back EMPTY must refuse, not proceed.** → History-rules-cli-serve.md
- **`LiveHost` must accept the routing key's spelling.** **Don't memoise it in package-level state** → History-rules-cli-serve.md
- **An operator-facing promise is a contract with an expiry date.** → History-rules-cli-serve.md

- **One `bridge serve` per data dir, decided before its first write: runServe takes an exclusive kernel lock on `<dataDir>/server.lock` (`lockServeDataDir`, over `fsutil.TryLock`) right after the read-only config checks, ahead of the TLS mint, the token store, `server.pid`, the migrations, the updater's boot marker and every sweeper** (2026-10-02, backlog B208) → History-rules-cli-serve.md
- **A lock file is opened through `fsutil.OpenLockFile`, which asks again when its create loses a race** (2026-10-14, backlog B185) **Don't open a lock file with a bare `os.Root.OpenFile(name, O_CREATE)`** → History-rules-cli-serve.md
- **`bridge restore`, `bridge reset-player` and `bridge manifest clear-missing` hold the data-dir lock for the whole run; restore and reset refuse if the lock cannot be taken, and reset repeats both of restore's admin-port checks around its confirmation.** → History-rules-cli-serve.md
- **A snapshot captures the console account and the harvest state, and restore brings the account back signed out** (2026-10-13, backlog B200) → History-rules-cli-serve.md
- **A live backup retries Windows sharing/access failures while opening files replaced by rename, with atomicwrite’s bounded, cancellable backoff; persistent errors still refuse it.** **Only a reservation refused as data gets backup’s player reset advice; cancellation, an expired context and file-access errors never do. Exhausted sharing/access retries say the file was busy and to retry the backup.** → History-rules-cli-serve.md
- **Every backup source copied by `copyFileCtx` opens through `fsutil.OpenAsFile` inside the sharing-violation retry: a named pipe is refused at once, naming the file without its value, and the partial snapshot is removed. Don't replace this with a plain open; keep missing optional sources skipped.** → History-rules-cli-serve.md
- **A snapshot captures the player's identity and history consistently: after capturing the player files, recheck the history, UUID and reservation UUID and refuse changes before publishing the manifest; advancing counters alone is allowed. Restore gives the box the snapshot's identity only when neither history records it as given up: read by the player's rule before any file is written, and written by the identity file's own writer after a durable jump of both reservation marks and durable discard of a replaced identity's queue** (B505, B470 5a0) **Don't restore `player-uuid` as bytes, and don't let a copy that does not hold one UUID reach the loop.** → History-rules-cli-serve.md
- **Giving up a player's identity uses `bridge reset-player`, with the service stopped: durably record the identities given up, then remove queue, UUID and reservation in that order; rerunning completes an interrupted reset, and serve refuses to start the player while resetPending is set or its UUID is missing beside a reservation or queue. Only all three files absent and no pending reset is a no-op. A malformed UUID file is recoverable: record every canonical UUID within the reader's bound, then remove it with os.Remove and the same directory barriers; valid UUID removal still uses ForgetPlayerUUID. Restore read access to an unreadable player-uuid or player-ids.json before rerunning reset; move a non-file at player-uuid aside first. The command is top-level, beside backup and restore; output player reports MPD and has no reset alias.** **A reset gives up for good the identity it removes and any the box ran earlier (one an earlier restore replaced on this box), unless, after this box last ran it, another box took it over from this box, by restoring a snapshot this box made, and has not reset it since, running or powered off. Their values were held only by the floor the reset drops. No snapshot of a given-up identity taken before the reset, on this box or another, may restore its player identity again: remove `player-uuid` and `player-ids.json` from every such snapshot, or delete those snapshots. Any other box that still holds a given-up identity's files (one powered off since a migration, say) must be reset too before it runs again. An identity another box took over from this box by a restore, after this box last ran it, and has not reset since, is not given up: that box's floor holds it, and its snapshots stay valid. Never repair a reservation from a snapshot or delete it while keeping the identity.** → History-rules-cli-serve.md
- **`player-history.json` is versioned, read through `fsutil.OpenAsFile` and written only by its owner-preserving strict durable writer. Restore unions both histories and records the UUID it replaces (and a foreign reservation UUID) before replacing the floor or UUID, retaining any incoming UUID already in covered even after success; reset records the live UUID (or canonical UUIDs recovered from its malformed file), the canonical reservation UUID even without player-uuid, and every covered identity before dropping the floor. Backup carries and lists the history even without a UUID. A corrupt, unreadable or non-file history refuses the operation. Restore refuses an identity given up in either history before any write, with `snapshotIdentityRemedy`; even an identity safely taken over by another box is refused back here after reset: refusing is safe, reissuing is not. Never discard history on reset or overwrite it with a snapshot's older list.** → History-rules-cli-serve.md
- **`probeBridge` cannot answer for an ephemeral admin port, and must say so.** → History-rules-cli-serve.md
- **`flag.Parse` stops at the first non-flag argument, so an unguarded
  subcommand silently WIDENS its scope.** `bridge enrichment retry
  Artist/Album` parsed with `--path` empty, and an empty scope is the whole
  library: a whole-library `enriched_at` reset and a delta to every paired
  device in place of one album. `library remove` had guarded this since PR #78.
  Any command whose empty scope means "everything" needs `fs.NArg()`.
- **The positional-scope class covers `--path`, not just `--filter`.** → History-rules-cli-serve.md
- **A `stopFn` that signals is not a join.** A stop deferred ahead of `Store.Close()` must wait for its loop, grace-bounded. **And the work has to be cancellable for the wait to mean anything**. → History-rules-cli-serve.md
- **A cancel has to reach the process that WRITES, and `exec.CommandContext` reaches only its direct child.** **Never set `Cmd.WaitDelay` there** → History-rules-cli-serve.md
- **A cancelled pass is not a failed one, and a join makes the difference visible.** → History-rules-cli-serve.md
- **…and the backup ticker applies the rule by asking the ERROR too, because one error can carry both** (#998) → History-rules-cli-serve.md
- **Every loop `runServe` starts reports a shutdown as a STOP, and a log site in one asks `ctxerr.WithoutCancellation` with the context its PASS runs on** (#999, #1000–#1005) → History-rules-cli-serve.md
- **A goroutine serve starts is stopped by a context the TEARDOWN cancels, and joined before anything it uses is closed** (#1009) → History-rules-cli-serve.md
- **An HTTP/3 drain is bounded, drained ONCE, and its socket closed a moment AFTER the grace, never at it** (#1010) → History-rules-cli-serve.md
- **An event stream ends when shutdown starts, and an ordinary request keeps the grace** (2026-10-08, backlog B216) → History-rules-cli-serve.md
- **On a shutdown the LAN and the tailnet drain TOGETHER, under one grace** (#1019) → History-rules-cli-serve.md
- **A snapshot a shutdown cancels STOPS, and `VACUUM INTO` went on past its cancel in three places** (2026-09-29, backlog B63) **Don't raise the source's cache, don't give the snapshot SQLite's busy wait back, and don't Ping before the VACUUM** → History-rules-cli-serve.md
- **Shutdown waits for the backup ticker to close its snapshot file, past the shared writer grace** (2026-10-09, backlog B309) **Don't drop it.** → History-rules-cli-serve.md
- **Anything reading Go source in a test must normalize CRLF first.** → History-rules-cli-serve.md
- **Serve starts the loopback track route from the localOutputs gate and stops it before the store closes, and the console's doctor reads the player checklist from the serving process** (B473 B2c): the gate is asked once at boot and `OnActive` covers an MPD that appears later; the route's `Enabled` is `apiSrv.LocalOutputsActiveNow`, so every request asks the gate's answer as it stands and none dials MPD; the checklist source reads `apiSrv.Resolver().Roots`, the route's own `BaseURL` and `StartErr`, and the auth store's `List`; `adminDoctorRunner` takes `checklistDoctorChecks` over the serving source, never `bridge doctor`'s one-shot builder; `serveOpts.localOutputsGOOS` and `outputLoopbackBoot` let a boot test stand in for Linux and wait on the boot decision instead of polling. `TestServeAsksTheGateOnEveryLoopbackRequest` and `TestServeChecklistReadsTheLiveRootsAndTheRoute` pin the wiring. Rules in `output.md`.
- **Serve starts the player's hotplug watcher on Linux once the live config holder exists, and its teardown cancels and joins it** (B470 PR 4a2): `startPlayerHotplug` runs the `hotplug.Service` on the holder's `Load` and `Changed`, reading the box through `hostTrees` (a boot test's `serveOpts.playerTrees` in their place), so the player turned on or off, or its binding changed, while serve runs starts, stops or re-reads the watch; the service runs on a context its stop alone cancels (since B470 PR 4b3: on serve's own it stopped as the shutdown began, before the player its reports feed), the stop waits for it within `shutdownGrace`, and the service itself returns within its watch's stop deadline. Off Linux nothing starts. `serveOpts.playerGOOS`, `hotplugSource` and `hotplugBoot` let a boot test stand in for Linux, run the watcher on a source that hears nothing, and wait on the decision; `hotplugObserve` and `hotplugTiming` hold its readings and stretch its stop. The watcher reads the player block through `livePlayerSettings`, which serve's player reads too. No path changes `output.player` while serve runs today; the console's DAC page will (PR 6a). `TestServeRunsThePlayersWatcherWhileThePlayerIsOn`, `TestServeStartsNoPlayerWatcherOffLinux`, `TestServeStartsThePlayersWatcherWhenTheConfigTurnsItOn` and `TestThePlayersWatcherStopJoinsItsService` pin the wiring. Rules in `player.md`.
- **Serve starts its player from the boot snapshot on a goroutine of its own, defers its stop after the watcher's stop and the store's close so the engine and the backend are joined, bounded, before either closes, and shares the data dir's lock with it (`dataDirHold`) while anything it started runs: a state write that outlives the stop, and a Run a panic ended, are the exceptions to the join** (B470 PR 4b3) **Don't defer the player's stop before the watcher's or the store's, don't wait for the engine or the backend without a bound, promise successful device release before its outcome, or release the data dir's lock while the player's engine may still write there.** → History-rules-cli-serve.md
- **Serve builds one SSDP subsystem before either player or DLNA starts and injects it into both owners** (B470 PR 5a2b): Source works with DLNA off; both share interface memberships; and with no device registered the subsystem opens no socket. The full teardown is deferred after UPnP upstream so MediaServer withdrawal and HTTP stop precede resolver teardown; an early player-only fallback covers returns before that defer. Teardown first refuses Source, withdraws both owners and closes/joins the subsystem with one absolute SSDP deadline; DLNA HTTP and the player's HTTP/engine/backend stops then have separate bounds, ahead of watcher/store teardown. **Don't build a second subsystem, intersect Source addresses with DLNA's set, let DLNA fall back to a private subsystem, or reset the SSDP budget per owner.** `TestServeSharesDiscoveryAcrossItsEligibleDevices`, `TestServeWithdrawsSourceBeforeStoppingItsEngine`, `TestServeDiscoveryRestartsWithStableIdentity`, `TestServeStopsDLNABeforeItsUPnPResolver`, `TestServeDiscoverySharesOneShortShutdownDeadline` and the existing wiring guard pin it. The deadline test compares every byebye write from both owners; restart requires a Source identity and checks byebye USNs; ineligible starts are checked after serve settles. → History-rules-cli-serve.md
- **Serve's player starts its OpenHome surface after launching its engine, when the boot snapshot's `output.player.openhome` is on, and its shutdown stops the surface first, before the engine and the backend** (B470 PR 5a2a): `planServePlayer` decides it with the player's gates (`servePlayerPlan.openhome`; the opt-in on and the player simply off is `openhomeOff`, one Warn at boot and nothing started); `bridgePlayer.startOpenHome` runs `openhome.Start` with the player's UUID for its UDN, the boot snapshot's room and port and the bridge's version, over `lan.Look(hostLAN)`, and a surface that cannot start leaves the player running without it; `stopOpenHome` comes first in `bridgePlayer.shutdown`, and the player's stop adds the discovery allowance (`servePlayerBounds.discovery`) and the surface's HTTP bound (`servePlayerBounds.surface`, `openhome.Limits.StopBound`) to its own. `hostLAN` is the one name in `cmd/bridge` for the host's interfaces, which the suite's TestMain points at a box with no interface before any test runs; `serveOpts.playerOpenHome` hands a boot test its box (`lantest`), its limits and seams, and the surface once started. `TestServeRunsTheOpenHomeSurfaceOnlyWithItsOptInAndThePlayer` pins the wiring, and serve's gate row "an OpenHome port serve binds already" the port clash. Rules in `openhome.md`.
- **Serve runs the sample-format header pass at once and after every successful scan, on a `postScanNudges` channel and `scanCtx`, joined through `bgWriters`** (2026-10-23, backlog B513) **Don't give it a `SetPostScanHook` of its own, don't drop the pass at once, and don't log a path** → History-rules-cli-serve.md

- **`bridge init`'s preflight grades the install that is THERE, and `ensureDoctorClean` prints warns.** → History-rules-cli-serve.md

- **`bridge doctor` finds its config through `resolveConfigPath` (explicit, then `./bridge.yaml`, then the platform dir) and loads only a path it FOUND.** → History-rules-cli-serve.md · doc: docker.md
- **…and where it finds no config, tls-cert grades the pair in the data dir `bridge init` writes, as the port checks grade the ports it writes** (2026-09-29, backlog B61) → History-rules-cli-serve.md
- **The image's `lsof` package is load-bearing — don't drop it to slim the image.** → History-rules-cli-serve.md
- **The HEALTHCHECK's connect-and-close is silenced at the LISTENER, never by making the probe speak TLS.** → History-rules-cli-serve.md
- **…and the servers' error log keeps no client address** (#1055) → History-rules-cli-serve.md
- **…and a kept line's REASON names the peer again, after the `->` of a socket error, which is redacted too** (2026-09-29, backlog B172) **Don't redact every lone address after `read tcp`** → History-rules-cli-serve.md
- **`RedactPeers` walks the line once** (2026-10-12, backlog B246) → History-rules-cli-serve.md

**When you correct a rule in a hardened section, check the top-of-file list for
its twin.** The top list is older, shorter, and read first.

## Scanner, deletion passes, and manifest writes

- **Shutdown joins every background writer**, and the wait must be written INLINE in the defer, never routed through a variable assigned later in `runServe`; both this wait and the watcher's are grace-BOUNDED. → History-rules-cli-serve.md
