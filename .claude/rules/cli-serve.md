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
- **A rewrite replaces the SETTINGS, never the INSTALL.** **Never drop or rewrite it** **never "proceed when init's data dir holds a pair"** **never "keep the paths only when both are set"** → History-rules-cli-serve.md
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
- **`bridge restore` and `bridge manifest clear-missing` hold the data-dir lock for the whole run.** → History-rules-cli-serve.md
- **A snapshot captures the console account and the harvest state, and restore brings the account back signed out** (2026-10-13, backlog B200) → History-rules-cli-serve.md
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
