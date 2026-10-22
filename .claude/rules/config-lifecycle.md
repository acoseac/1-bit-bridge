---
paths:
  - "internal/config/**"
  - "internal/packaging/**"
  - "internal/supervision/**"
  - "internal/updater/**"
  - "internal/atomicwrite/**"
  - "internal/logging/**"
  - "internal/auth/**"
  - "internal/tailscale/**"
  - "internal/fsutil/owner*.go"
  - "internal/fsutil/container*.go"
  - "cmd/bridge/main.go"
  - "cmd/bridge/init.go"
  - "cmd/bridge/jobstate*.go"
  - "cmd/bridge/serve_restart*.go"
  - "cmd/bridge/lifecycle*.go"
  - "cmd/bridge/service_*.go"
  - "cmd/bridge/update*.go"
  - "cmd/bridge/variants*.go"
  - "internal/admin/settings_apply*.go"
  - "internal/admin/managed_controls*.go"
  - "internal/admin/handlers_api.go"
  - "internal/admin/static/app.js"
  - "internal/backup/backup.go"
  - "internal/manifest/playlist_covers*.go"
  - "Dockerfile"
---
# Config, settings and process lifecycle

The config and its live and boot halves, managed settings and controls, service install and restart, atomic writes and file ownership, the token store's writes, and the self-updater's binary swap.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Config, settings and process lifecycle

- **A hardening step on a CREATION path protects nothing that already
  exists.** The `chmod 711` for the hosted product's media parent shipped
  inside `bridge-tenant create`, so it reached no existing host — including
  the one it was written for, where the directory was still world-listable
  after the deploy. Anything meant to be true of a host belongs in the
  installer, which runs on every deploy. Same shape as a migration that only
  fires on a fresh install.
- **On a bridge somebody else runs, hiding a control is not refusing it.**
  `deployment.managedSettings` covers settings FIELDS;
  `deployment.managedControls` covers ACTIONS (`restart`, `updates`, `roots`,
  `variantsDir`, `backups`) — hidden by the console AND refused by the
  handler, because each is a plain authenticated request a session holder
  can send by hand. The gate is applied at the ROUTE TABLE, which is where
  you go to ask what a session can do. **`POST /api/roots` is the one that
  was a security hole**: it takes any absolute directory that exists (no
  containment rule, deliberately, for a NAS mount), and the byte routes then
  serve what is under it — on a shared host, every world-readable file on
  the box. An unrecognised control name is a no-op PLUS a startup warning,
  never a config error: refusing would make a binary rolled BACK during an
  incident fail to start for every tenant at once. The typo guard therefore
  lives in the program that WRITES the file (the conductor's
  `managed_controls_test.go`), pinned in both directions.
- **A managed deployment must not be given advice that needs a shell on the
  host.** `bridge doctor` run as a live tenant produced eleven `ok` and two
  `warn`, both unactionable — "no user systemd session, use `bridge init
  --no-service`" and "no browser opener, install one" — so a healthy
  appliance read as two problems. Those two and `log-file-size` (which also
  prints a host path into the tenant's report) skip on `Deps.Managed`; the
  log EXPORT refuses for the same reason, ahead of the terminal / journald /
  `docker logs` branches, and for all three export routes rather than only
  status. Same for the Diagnostics `/metrics` pointer: a scraper there needs
  its address in `metrics.allowCidrs`, which the control plane owns, so the
  paragraph would tell the reader to point a scraper at a URL that refuses
  it. This said until 2026-09-30 that the pointer "is loopback-gated and
  answers 403", a claim about the gate B171 replaced (under **Auth,
  pairing, TLS and security posture**).
- **A container's doctor report must not advise a host it is not on
  either** (backlog B190). In the Docker image a healthy bridge's report
  ended "14 ok, 3 warn", two of them `service-manager` ("use `bridge init
  --no-service`", to an image that already runs `serve` and whose documented
  `docker run … init` already passes the flag) and `browser-opener` ("no
  opener found", in an image with no browser). In a container both still
  run, and a failing one is reported "check skipped" with no advice, while
  a passing one (a system container with its own systemd, Toolbx's shared
  opener) still says what it found. The verdict is `Deps.InContainer`,
  which `fsutil.InContainer()` sets from Docker's
  `/.dockerenv` or Podman's `/run/.containerenv` (marker files, never cgroup
  parsing), in `bridge doctor` (the console shares its report) and in init's
  preflight (`initPreflightDeps`), so the two cannot grade one container
  differently. Unlike `Managed` it skips nothing else: a container's
  operator has a shell (`docker exec`), so `log-file-size` and every other
  check still speak to them. The console's no-log-file hint asks the same
  helper. **Don't detect a container from cgroups or an env var only our
  image sets, don't widen the skip past these two checks, and don't set the
  verdict in one report's inputs and not the other's, and don't skip a
  check that passes.** Pinned by
  `TestInContainerSkipsTheHostOperatorChecks`,
  `TestInContainerKeepsTheChecksItsOperatorCanActOn` and
  `TestDoctorAndInitCarryTheContainerVerdict`.
- **The console must SEND only what it SHOWED.** The settings Save payload is
  an explicit allowlist naming every field, `hideManagedSettings` sets `hidden`
  on the enclosing `.field` rather than removing the input, and a hidden input
  is still in `FormData` — so every managed field was supplied on every save,
  and the PATCH is refused WHOLE when any managed field is supplied. Renaming
  the library on a managed bridge failed with nineteen field names the operator
  never touched. `dropUnofferedFields` drops any key whose control is absent or
  inside something hidden; `dlnaEnabled` had done this for its own disabled-
  checkbox case since PR #342 and it was never generalised. No Go test can see
  this — the payload is built in JS — so it took driving the real form. The
  feature trays sent managed fields too, until 2026-09-28 (the tray bullet
  under **Admin console and the web player**).
- **Hiding a settings field leaves its heading and its prose behind.**
  Sections are flat siblings, so `collapseEmptySettingsSections()` hides a
  heading only when its section had a `.field` and every one is now hidden,
  and hides a jump link whose pane has nothing left. Verify this class of
  change IN A BROWSER — the Go suite cannot see it, and dropping the restart
  button server-side left two unguarded `restartBtn.hidden` writes that turn
  a successful save into "Save failed".
- **Never split a config field's halves.** Either EVERY consumer reads it live
  or every consumer takes it at boot. Hot-applying a cheap struct field while
  reporting `restart` makes `/v1/health` advertise a capability in the same
  breath the settings response calls the change pending. This is why there is no
  `partial` status — the rule removes the case instead of naming it. The
  field → apply-semantics matrix is **`ops/settings-apply-semantics.md`**, and a
  test drives the real handler for every row in it. **That test checks the
  REPORT, and no consumer**: under `upscaleEnabled`'s `live` row the console's
  size projection was taken at boot until 2026-09-28, and four consumers read
  the flag without its sox half (the construction-time bullet under The CLI
  and the serve wiring, and the one after it).
- **…and a DEFAULT splits the halves too, when a writer can store what `Load`
  would replace** (#1042). `applyDefaults` runs in `Load` alone, and before
  the `BRIDGE_*` overrides, so its `if c.LibraryName == ""` reached neither
  the settings PATCH nor an override: `PATCH {"libraryName":""}` answered
  `live`, the running bridge served `""` in `/v1/health` and in every pairing
  QR's `name=` (which the app refuses), and a restart served
  `DefaultLibraryName`. The default now lives in `Normalize`, which every
  writer runs before it saves, so no writer can store a name `Load` would
  serve differently. **Refuse a blank that would replace a name, default one
  that loses nothing**: the PATCH answers a blank name with 400 `validate`,
  because defaulting it would replace the operator's name with one nobody
  chose, while init treats a blank `--name` as none (the kept or host name,
  as `--name ""` and Enter at the prompt). **Trim with
  `config.TrimLibraryName`, never `strings.TrimSpace`**: the app's pairing
  parser refuses a `name=` that its `.whitespacesAndNewlines` trim changes,
  and that set is `unicode.IsSpace` plus U+200B ZERO WIDTH SPACE (every
  scalar enumerated on both sides). Don't move the default back into
  `applyDefaults`, and don't turn it into a `Validate` refusal: a
  `BRIDGE_LIBRARY_NAME` of spaces would then stop a bridge from starting
  after an update, over a display name.
- **…and every stored name is one a pairing code can CARRY** (#1046). The
  app also refuses a `name=` over 256 Characters ("Pairing code's name field
  is too long.") and one that is not UTF-8 (Foundation leaves the item with
  no value: "missing the name field"). The console took any length (257
  characters answered 200 `live`, and the next QR did not pair), and `bridge
  init --name $'Caf\xe9 Tunes'`, "Café" typed in a Latin-1 terminal, was
  saved as `!!binary` and served with `name=Caf%E9`. **The cap,
  `config.MaxLibraryNameLength`, is 256 RUNES**: a Character is one or more
  scalars, so that is never more than 256 Characters on any iOS version,
  whatever Unicode tables it segments by, and Go has no grapheme
  segmentation to count the app's way. It is conservative for multi-scalar
  Characters (37 family emoji, 259 scalars, are refused; the app counts 37).
  Don't count bytes: 256 `é` must pass. **Refuse what the operator types,
  repair what is already stored** (#1042's rule, one step on):
  `config.CheckLibraryName` refuses in the PATCH (400 `validate`, nothing
  written) and in init's `--name` (exit 2, before anything is written), and
  init's prompt asks again; `Normalize` repairs a config's or an override's
  name (`config.RepairLibraryName`: U+FFFD for bytes that are not UTF-8, cut
  to 256 runes and re-trimmed, with a WARN), because a `Load` refusal would
  stop a bridge from starting over a display name. init keeps and offers an
  install's name AS LOAD SERVES IT, or its prompt would offer a name it then
  refuses. Don't cut in the PATCH instead: that stores a name nobody typed.
  **Nor in the console**: the name box has NO `maxlength`. It counts UTF-16
  units, so it stopped names the handler takes (129 emoji), and it cut a
  paste without a word; the handler's 400 is what the page shows ("Save
  failed: libraryName: must be at most 256 characters, …"). Added in #1046's
  first round, removed on review (CodeRabbit).
- **A configured URL is named in every refusal and warning by its field,
  its scheme and its host, never its value** (backlog B54). #1074 gave the
  WARNINGS that rule (`urlOriginForLog`, under DLNA) and left the two
  REFUSALS that stop the bridge from starting: `normalizeBaseURL`'s and
  `Validate`'s harvest pin quoted the value (`got
  "ftp://user:password@mirror/ws/2"`), which serve prints, `bridge doctor`
  puts on its config-file line and the console's PATCH answers, and an
  enrich base may legitimately carry a mirror's password (net/http sends
  userinfo as Basic auth). `urlFieldForLog` is the one naming
  (`enrich.musicbrainzBaseURL (ftp://mirror.example)`), and the message says
  what the value may not carry, since it is not shown. **A URL with no host
  part renders as NOTHING, scheme included**: `user:password@host`, written
  without a scheme, parses with the USER NAME as its scheme, and #1074's own
  custom-endpoint warning quoted it twice (`(s3cret-pw:)`, `got
  "s3cret-pw"`). **Search for a leaked secret without regard to case**:
  `url.Parse` LOWERCASES a scheme, which is how #1074's test, searching for
  `s3cret-Pw`, passed over that leak.
  `TestNoStartupErrorCarriesAURLsCredentials` (every shape, through Load,
  Validate and the environment) and
  `TestAStartupRefusalNamesAURLWithoutItsCredential` (serve and doctor).
  The enricher's own request errors are covered too, by keeping the
  credential out of every request URL (the bullet on a configured base URL's
  user information under **Enrichment**, backlog B69): net/http's
  `*url.Error` names a request URL with its password masked (`user:***@`)
  and a token written as the user name whole. So are `autocert.domain`'s
  two warnings (the field, and its scheme and the host it is served as), a
  manual upstream's control URL, whose user information goes in a header,
  and the manual poller's Debug lines (backlog B66: the `autocert.domain`
  bullet under **The wire contract**, and the manual-URL one under **DLNA,
  UPnP and discovery**).
- **When a change cannot take effect, say so** — but only when the outcome
  depended on THIS bridge's runtime state (a Tailscale mode's transition;
  applied-but-inert because a toolchain is missing; no sweeper wired, which
  only a harness is since #781). NOT for "listeners bind once", which is true
  everywhere; twenty near-identical strings is how the two that carry
  information get skipped. The verdict is computed inside the update closure,
  never from a static table, because a table cannot see this bridge's wiring.
- **A cadence provider needs a rearm, or `live` is a lie.** Every loop reads a
  `func() time.Duration` before each wait (a timer per iteration — a ticker
  cannot change period), and the rearm fires only on an actual change. A zero
  interval PARKS a loop rather than ending it; start the ticker unconditionally
  or "disabled" becomes terminal for the process.
- **…and a loop whose cadence is 0 parks BEFORE its boot pass: starting it
  unconditionally must not run it once** (2026-10-02, backlog B209).
  `runSweepLoop` swept after its settle delay and read the interval after, and
  #769 started the backup ticker on every bridge so that 0 → N had a loop to
  notice, so `backup.intervalHours: 0` wrote a snapshot and pruned the backups
  to `backup.keep` at every boot (measured through the real serve: the
  snapshot on disk 0.15 s into the test), while the config doc, runServe's
  comment and the settings matrix all said 0 parks it; the matrix test
  passed, since it checks the REPORT. The loop reads the interval first and
  takes the boot pass only when it is on; a nudge from the settle window is
  drained only by a boot pass that covers it, and served otherwise. **Each
  pass is told whether it is the boot pass** (`sweep func(boot bool)`): the
  backup ticker's startup snapshot skips when one exists within 24 h, and a
  flag of its own ("the first call") took the first scheduled pass after a
  dormant boot and a 0 → N rearm for the startup one. **A dormant cadence is
  off, not "a loop with no tick"**: `TestSweepLoopDormantIntervalIsResumable`
  asserted the boot pass at 0 ("want 1 (the initial one only)"), and the
  auto-optimize nudge test used an interval of 0 to mean nudge-only, a shape
  no production cadence has (only the backup ticker's can be 0: analysis and
  auto-optimize follow `scanIntervalSec`, 1 s or more, and fingerprinting and
  smart mixes read 0 as their default); a test that wants no tick uses an
  hour. Pinned by `TestServeWithBackupsOffTakesNoSnapshotAndFollowsItsCadenceLive`
  (the consumer, through the PATCH, not the report),
  `TestABackupTickerWhoseCadenceIsOffTakesNoPassUntilItIsSwitchedOn` and
  `TestSweepLoopDormantServesANudgeFromTheSettleWindow`.
- **Config is copy-on-write behind an atomic pointer.** Readers call `Load()`
  per request; writers clone → mutate → validate → save → hot-apply → `Store()`.
  **Never mutate `Load()`'s result in place** — even under a mutex, concurrent
  readers race. Admin mutations must re-`Load()` and clone INSIDE `Server.mu`,
  or a settings PATCH committing between the load and the lock is silently
  reverted.
- **`Validate()` is a pure shape check; accessibility is
  `CheckLibraryRootsAccessible`, called only by `serve`.** Putting `os.Stat` back
  in `Validate` breaks `bridge update` on any host where the daemon user and the
  binary owner differ (the public-mode VPS layout). Loopback installs fail fast
  on an inaccessible root; public installs warn and continue. An output-only
  bridge (`output.mpd: on`) is a loopback install with no library root:
  Validate allows that and does not dial MPD to decide, and `auto` still
  requires a root. **Don't treat `auto` as on inside Validate.** Nor does
  `output.player` ever refuse a config: an invalid block turns the player
  off with a logged reason (the first bullet under **Player output
  binding** in `output.md`).
- **Cadence ceilings are unit-appropriate and ports admit 0.**
  `time.Duration(n)*time.Second` overflows negative past ~9.2e9 and
  `time.NewTicker` PANICS, crashing `bridge serve` at startup; a single seconds
  cap applied to an hours field still overflows. Port `0` is the documented
  OS-picks-an-ephemeral-port mode used by every `:0` test fixture.
- **Env overrides are DERIVED from the Config struct.** Only `libraryRoots` uses
  the OS path separator; everything else is comma-separated, because
  `customEndpoints` holds URLs and splitting `https://host:7788` on `:` yields
  three fragments that then vanish in validation. Two legacy enrich base-URL
  names are kept as aliases — losing them sends an Atlas-configured bridge back
  to public MusicBrainz at the self-hosted pace.
- **`Uninstall` refuses a system-level install, like `Stop`/`Start`/`Restart` —
  and the four read ONE predicate.** The condition was spelled out in the first
  three and missing from the fourth, and the two POSIX uninstallers touch only
  the fixed USER-level path (`~/Library/LaunchAgents`,
  `~/.config/systemd/user`) and treat a missing file as success. So against a
  sudo install `Uninstall` returned `(userPath, nil)`, the menu printed "service
  uninstalled." with the LaunchDaemon still registered and running, and the same
  flow then offered `os.RemoveAll(cfgDir)` — config, data, certs and TOKENS —
  out from under a live bridge. The Windows arm already reasoned about exactly
  this ("a zombie service reported as a clean uninstall"); the POSIX arms did
  not. The menu now skips the wipe after a refused uninstall and SAYS SO, since
  a silently-skipped step reads as the menu being finished.
  **`NeedsRootFor` is exported so the decision is drivable** — `installedKindForOS`
  probes absolute paths only root can create, so a behavioural test of the
  wiring passes on CI whether the call is there or not (verified: the control
  stayed green). The wiring is pinned structurally by AST across all four entry
  points; the classification is pinned by table.
- **`POST /api/restart` must invoke the same cancellation closure as
  SIGINT/SIGTERM**, never `os.Exit(0)` — that is what honours the `bgScans`
  WaitGroup (SQLite corruption), cleans up in-flight jobs, and flushes the auth
  store's debounce buffer. Same rule for the updater's auto-install restart.
- **…and a restart request exits with `supervision.RestartExitCode` (75),
  a stop with 0: an exit 0 is what a supervisor leaves stopped** (2026-09-29,
  backlog B201). The console's Restart, "Install & restart" and the
  auto-installer's restart all exited 0, and the LaunchAgent `bridge init`
  writes relaunches an unsuccessful exit only (KeepAlive
  {SuccessfulExit: false}), while the Windows service had no recovery
  actions: measured under a real LaunchAgent on the dev Mac and a real SCM
  service on nomos, `restarting: true`, then exit 0 and nothing listening,
  for good. `serveRestart.request` marks the stop and calls runServe's own
  cancel (so the whole shutdown runs, the rule above), and runServe's
  second defer turns a clean exit after a request into 75 (EX_TEMPFAIL):
  launchd relaunched the probe with `last exit code = 75: EX_TEMPFAIL`,
  and a SIGTERM (`launchctl kill`) still exited 0 and stayed down.
  `admin.Deps.Restart` and `AutoInstallRestart` are both
  `restart.request`, never a bare `cancel`
  (`TestEveryRestartInServeGoesThroughTheRestartRequest` reads the wiring;
  `TestARestartRequestedFromTheConsoleExitsToBeRestarted` drives the
  console's). **Windows needs two more parts, both load-bearing**: the
  service's recovery actions (restart after 2 s, 2 s, then 30 s, reset
  after an hour) with `FailureActionsOnNonCrashFailures`, set by the
  installer and, for a service installed before v0.2.1, by the running
  service itself as it starts (`packaging.EnsureServiceRecovery` with its
  own name, args[0]; it leaves recovery an operator configured alone), since
  a console update replaces the binary and never re-installs; and the
  service handler returns the exit code WITHOUT reporting Stopped first,
  because the SCM takes the exit code from the first Stopped status: the
  old handler's early Stopped (exit 0) made a failure read as a clean stop
  (measured on nomos with recovery set: the early-Stopped build stayed down
  with exit code 0; the fixed one logged event 7024, "service-specific
  error 75", then 7031, "corrective action … Restart the service", and was
  back in 4 s; an SCM Stop stays stopped). systemd's Restart=always restarts
  either (its journal names this one 75/TEMPFAIL), and Docker's
  `on-failure` policy, which restarts a non-zero exit, now relaunches it
  too (docs/docker.md). **And `bridge start` kickstarts a
  loaded agent**: `launchctl bootstrap` of one answers "Bootstrap failed:
  5: Input/output error" on macOS 27, which `startForOS` did not swallow,
  so `bridge start` over the stopped agent failed; a kickstart starts a
  loaded agent and leaves a running one alone (both measured).
  **Release note**: a macOS or Windows service install that updates from
  v0.2.0 through the console runs v0.2.0's restart, which exits 0, so it
  comes back down: run `bridge restart` once after that update.
- **Atomic writes: stage, then rename with retry.** `RenameWithRetry` absorbs
  the Windows AV scan-on-close window; the deferred `Close` must be registered
  AFTER the deferred `Remove` (LIFO — Windows won't unlink an open file).
  `GenerateWithOptions` stages BOTH cert and key before renaming either, so a
  failure leaves the existing pair intact; **never pre-delete**, and never run an
  orphan-cleanup `Remove` on a file already committed. Each site keeps its OWN
  `Chmod` / `Sync` / parent-dir fsync — **don't collapse them into a shared
  `WriteBytes`**, which silently drops e.g. the auth store's belt-and-braces
  `Chmod(0o600)`.
- **`atomicwrite.WriteBytesDurable` is the strict writer: nil means the
  file's bytes and every directory entry naming them were fsynced, and a
  failed directory fsync is the write's error, never tolerated as
  `WriteBytes` tolerates it once its rename has landed** (B470 PR 4b1; its
  one caller is the player's ID and token reservation, the "No ID and no
  token is handed out twice" bullet in `player.md`). It refuses a platform
  with no directory barrier before it writes anything
  (`ErrNoDirectoryBarrier`: Windows, whose `fsutil.SyncParentDir` is a
  no-op that answers nil), syncs each directory it creates in its parent
  before the file, and takes a race winner's equal bytes only once it has
  synced them and their entry itself. A directory it created whose entry
  it could not sync stays where it is, and the process keeps it: every
  later strict write below it runs that barrier again before it reports
  success, until one succeeds. Round 4 removed such a directory instead,
  which raced every other writer: it deleted a directory one made between
  the look for what was missing and the creation, and a regular file one
  put at the path, and could not remove one another had filled, which the
  next write then took for a directory that was always there.
  `ProbeDurable` asks the same of a directory without writing: not a
  directory is `ENOTDIR`, one it cannot stat or open is that error, and
  only a barrier that ran and failed is marked `ErrBarrierFailed`, the one
  case another filesystem mends. **Don't count a write durable without its
  directory barrier, don't delete anything to recover durability, and
  don't advise moving to another filesystem for an error not marked
  `ErrBarrierFailed`.**
  `TestWriteBytesDurable_RefusesAPlatformWithNoDirectoryBarrier`,
  `TestWriteBytesDurable_RetriesTheBarrierOfADirectoryItCreated`,
  `TestWriteBytesDurable_RemovesNothingAnotherWriterMade`,
  `TestWriteBytesDurable_RaceWinnerStillNeedsTheBarrier`,
  `TestBarrierFailuresAreMarked` and
  `TestProbeDurable_AnUnreadableDirectoryIsNoBarrierFailure` pin it.
- **`auth.Store.FlushLastUsed` reloads before persisting** — the shutdown flush
  rewrote `tokens.json` from a stale in-memory slice and deleted a sibling
  `bridge pair` mint. A reload failure ABORTS the flush.
- **…and a reload before the staging is not enough: every `tokens.json` write
  re-reads the file just before its rename** (#1043), adminauth's #1039 rule
  (under Auth, pairing, TLS) on the bearer-token store. All seven writers (the
  debounced Validate and RecordClientVersion writes, the shutdown flush, and
  Mint, Revoke, Rotate, SetExpiry) reloaded, then staged (a temp file, a write,
  an fsync: a median of 0.6 to 0.7 ms on ext4, 2.3 to 3.8 ms on APFS) and
  renamed with no further check. A `bridge pair` or
  `bridge token revoke|rotate|expire` committing inside the staging was renamed
  over: the paired device's token vanished (401 from then on) or the revoked one
  came back, on disk and in the running bridge. Measured with two processes and
  no test seams, against a writer every 20 ms: 24 of 200 mints lost and 30 of
  200 revokes undone on APFS (16 and 11 on ext4), and 1 and 2 after the fix (2
  and 1). `commitLocked` builds from the last read, stages, compares the file's
  BYTES with what that read or this process's last write saw (`Store.raw`), and
  on a change reloads (whose per-token merge keeps unwritten LastUsedAt and
  client-version bumps), rebuilds and restages, up to three attempts. A re-read
  or reload that fails ends the write with nothing written. **Bytes, not
  mtime+size**: a write is rare enough to afford the read, and a same-size
  sibling write in the same mtime tick passes a stat. A write adopts its list
  only once its rename lands, and a Rotate or SetExpiry of a token revoked
  meanwhile answers ErrNotFound rather than writing it back. **What a write
  records as its file is the STAGED file's stat**, after its Close and before
  the rename: RenameWithRetry fsyncs the directory after renaming (0.5 ms ext4,
  2.8 ms APFS), and a stat of the path after that took a sibling's commit in the
  fsync for this process's own, so Validate refused a device paired there until
  the next write. What remains is the rename itself (a median of 50 µs on ext4
  and 170 µs on APFS from the re-read to the rename's return; 2 of 1,600 mints
  lost against a 10 Hz writer) and, on Windows, its retries. A kernel lock would
  close it, as for adminauth.json, and is not taken.
- **…and a debounced write that FAILS starts the next window too, and a store
  the running bridge cannot read is reported once each way** (#1047).
  Validate's and RecordClientVersion's debounced write started the next 30 s
  window only when it LANDED (`writeLocked` stamped `lastUsedFlush`), so while
  it could not land every request past the window re-entered the write: 10
  requests gave 10 ERROR lines with tokens.json unreadable (a `sudo bridge
  pair` beside a service install leaves it root's 0600), damaged, or on a full
  disk, and a write failing at its commit paid its staging per request under
  `s.mu`, which every authenticated request takes. With the rename refusing
  (RenameWithRetry's 750 ms of retries), 40 concurrent requests took 30.4 s,
  the worst 6.8 s; with the fix 0.76 s and one ERROR. **`flushDueLocked(now)`
  is the one gate, and it starts the window at the ATTEMPT**, adminauth's #1039
  rule; never gate a debounced write on the interval without starting the
  window in the same step. A failed reload still ABORTS the write, the
  observations stay in memory for the attempt a window later or the shutdown
  flush, and `FlushLastUsed` asks nothing of the window. The write side still
  logs once per window: that bounds the flood rather than silencing it, and it
  is the only report of a file made unreadable IN PLACE (a chmod changes no
  mtime or size, so `reloadIfStale` has nothing to reload). **While
  `reloadIfStale` cannot read the file, devices are checked against the tokens
  last read, deliberately**: a device `sudo bridge pair` paired is refused and
  one `sudo bridge token revoke` removed is still accepted. Fail-closed was
  rejected: the content is unknown, so the only refusal left is every paired
  device, over a permissions mistake. What was missing was the SAYING: a 401 is
  not logged, and the only other line was the write side's, about a
  timestamp, from a device the bridge already knew, so a first `sudo bridge
  pair` on a fresh install 401'd in silence. `noteReadLocked` logs one Warn
  when the store first cannot be read (the path, the error, and on POSIX for a
  permission error the uid and the chown remedy) and one Info when a request
  can read it again, with no restart. Both give the token count the bridge
  answers from: `tokens=0` after the file is deleted to mend it, which unpairs
  every device, where "readable again" alone read as all clear. **A test that
  asserts only the FILE can no longer see the abort**, since #1043's re-read
  before the commit refuses the same write: the skip-persist tests passed with
  the abort removed, and count stagings now. The cause, a CLI run as root
  re-owning the file, was the runbook's "always as the service user" rule
  alone until the next bullet fixed it for this file.
- **A CLI write run as root keeps the owner of the file it replaces**
  (#1048). `sudo bridge pair`, `sudo bridge admin reset-password`,
  `sign-out-everywhere` or `login-link`, `sudo bridge cert rotate`, `sudo
  bridge update` and a `sudo bridge init --force` rewrite beside a service
  install staged their file as root, 0600, and the bridge running as the
  service user could no longer read it: #1044's 503 on every console
  request, #1047's stale token list, and a restart that could not open the
  store or load its key. Measured with the real CLI as root over an install
  uid 4242 owns: exactly six files came back root's (`bridge.yaml`,
  `tokens.json`, `adminauth.json`, `adminauth-tickets.json`, `server.crt`,
  `server.key`). `fsutil.KeepOwner` runs on each staged file while it is
  open, before the rename, as one more of the site's own steps (the Atomic
  writes rule: no shared writer), and does something only when the process
  is root: it gives the file the owner of the ENTRY it replaces (`os.Lstat`,
  so a symlink's own owner, never its target's), or of the directory for a
  new file, which every login ticket's file is since the shared tickets file
  became one file per ticket (2026-09-28, under Auth, pairing, TLS). The
  updater's `update-state.json` takes it too. **Keep, not
  refuse**: a refusal keyed on who owns the data dir turns away a setup that
  works (a container run as root over a bind mount another uid owns, whose
  files the root service created), while the replaced file's own owner is
  the evidence of who reads it. **A chown that fails abandons the write**
  (root_squash NFS gives root's files to nobody), so the service keeps a
  file it can read. The job and database CLIs (`scan`, `upscale` /
  `optimize` / `render` / `analyze`, `artwork`, `backup` / `restore`,
  `manifest`, an offline `library` change), which CREATE directories and
  sidecars rather than replace one file, were this bullet's "not covered"
  list until 2026-09-28; the next bullet covers them. Pinned per writer
  through `fsutil.SimulateRootForTest` on every host, and as root by
  `TestKeepOwnerAsRoot` and `TestCLIRunAsRootKeepsTheInstallOwner`, which CI
  skips and dido's container runs. With the six writers reverted, the
  end-to-end test names exactly those six files.
- **…and a job or database CLI run as root gives every entry it CREATES
  the install's owner** (2026-09-28, backlog B17). Measured the same way
  (`TestJobCLIsRunAsRootKeepTheInstallOwner`, as root with sox and ffmpeg
  on dido), 51 entries came back root's on main: every directory and file
  `scan` (the artwork cache, 0700), `upscale` / `optimize` / `render` (the
  variants tree and each rendition, `--force` ones included), `analyze`
  (the waveform tree, 0700, and each curve, 0600), `backup` (a snapshot the
  service's own prune cannot remove) and `variants move` create; the five
  files `restore` replaces (config, store, token file, TLS pair, root's
  0600: the service does not start); a store a root CLI creates; and the
  render scratch `1-bit-bridge-render`, root's 0700 in the shared temp dir,
  where every later DSD render of the service failed. **SQLite's `-wal` and
  `-shm` were never part of it**: its unix VFS gives both the database
  file's owner whenever it opens them as root (`robustFchown`, carried in
  modernc's transpile), measured with the store open; the one SQLite hole
  is a database root CREATES. **A new entry takes the owner of the
  directory it is created in**, #1048's rule for a file with nothing to
  replace. `fsutil.MkdirAll` / `Mkdir` make each directory through an
  `os.Root` on its parent and read the owner through that descriptor, so a
  directory only ever goes to the owner of the directory actually holding
  it, who could have made it anyway, never to the owner of a path looked
  up beforehand; one it cannot give away (root_squash) is removed again.
  Files the bridge stages take `fsutil.KeepOwner` (a cover, a curve, a
  snapshot's copies and manifest, a restored file). **A file another writer
  creates is precreated** (`fsutil.Precreate`: `O_EXCL`, the owner looked
  up BEFORE the file exists; a render's sidecar and scratch since B211 by
  `transcode.createOutput`, `O_EXCL` then `KeepOwner`, which creates them
  for every user so a folder that refuses them fails by type, the
  output-fault bullet under **Job pools**): sox fills its output with `O_TRUNC`, SQLite
  opens an existing empty database, `VACUUM INTO` writes into an empty
  file, and each keeps the owner (all measured). **Two directories take a
  reference's owner, because there the parent's says nothing about who uses
  them**: the render scratch, when created in a directory anyone may create
  entries in (other-writable and -searchable, as /tmp is), takes the
  variants directory's (`fsutil.MkdirAllShared`; only there, since the temp
  dir comes from the config the service user can write), and a `variants
  move --to` directory takes the owner of the variants directory it
  replaces, as `mv` keeps it (`fsutil.MkdirAllLike`; the command line names
  that path). A cross-device copy in the move keeps its source's owner.
  `TestJobWritersKeepTheInstallOwner` sweeps the writers
  (`internal/{atomicwrite,manifest,transcode,analyze,backup}`,
  `cmd/bridge/variants.go`) and fails on `os.MkdirAll` / `Mkdir` /
  `MkdirTemp` / `WriteFile` / `Create`, or on `os.CreateTemp` /
  `os.OpenFile(O_CREATE)` in a function with no `fsutil.KeepOwner`: **a
  new writer there makes its directories through fsutil and gives its files
  an owner**. What a child process creates the sweep cannot see; that is
  `Precreate`'s, and the root test pins it. **Not covered**: `bridge tsnet
  auth` (internal/tsnet makes `<dataDir>/tailscale` with `os.MkdirAll`,
  `assertSecureDir` then requires it to be the RUNNING uid's, and the tsnet
  library writes its state there: a root run over the service's state
  refuses, and one on a fresh install leaves a state the service's node
  refuses; read from the code, not measured). **Root's by the rule**: a
  configured `variantsDir` or `tempDir` a root CLI creates under a parent
  root owns that is not shared, where the service's own attempt would fail
  too.
- **`logging.Component` resolves `slog.Default()` at LOG time, not construction.**
  Package-level `var logger = logging.Component(...)` runs during package init,
  before `main()` calls `logging.Init()` — a captured-handler shape would lock
  every logger to the pre-Init handler forever and Windows service log redirects
  would never reach them. No `log.Printf` anywhere in `internal/`; that is also
  what makes the CodeQL log-injection dismissal sound.
- **Use SEPARATE `http.Client`s for a probe and the mutation it gates.**
  `Client.Timeout` caps the whole request regardless of the request context, so
  reusing a 200ms probe client for a 30s mutation fails at 200ms. Same class: the
  updater's download client must have `Timeout: 0` and be bounded by a ctx
  instead, or multi-MiB archives die on slow links, permanently.
- **`bridge doctor --json --fix` writes fix progress to stderr** so stdout stays
  valid JSON. `--fix` only does mkdir-class remediations; anything destructive or
  security-relevant stays operator-in-the-loop.
- **Truncate to a UTF-8 boundary with `trimPartialTrailingRune`, not a
  validate-the-whole-string loop.** The loop shape is safe only for input
  guaranteed valid except at the cut; on localized CLI output with interior
  invalid bytes it discards everything after the first bad byte, at O(N²).
- **The Docker image runs as non-root and chowns `/data` in the same layer that
  creates the user** — otherwise `WORKDIR`/`VOLUME` create it root-owned and the
  first-run cert mint fails. Env overrides apply between defaults and path
  resolution so relative paths keep their config-dir semantics.
- **`ReapOrphans`-style directory reapers must refuse an empty root, BEFORE
  anything resolves it — and not because `os.ReadDir("")` reads the working
  directory, which is false.** It fails with ENOENT on every platform, and
  `filepath.WalkDir("")` visits `""` with an lstat ENOENT (measured with
  go1.26.6 on macOS, Linux and Windows; this bullet said otherwise until
  #1031). The working directory is reached through what an empty root
  BECOMES: `filepath.Clean("")` is `"."`, `filepath.Abs("")` is the working
  directory, `filepath.EvalSymlinks("")` answers `"."`, and
  `filepath.Join("", name)` is relative to it, which let `os.RemoveAll`
  delete the working directory's `sub/` in the probe. The EvalSymlinks case
  is why `integrity.TakeSidecarInventory`'s and
  `TreeHoldsVariantSidecars`' refusals are load-bearing: both resolve the
  root before walking. Everywhere else the refusal keeps a future
  resolution from reaching the working directory, and makes an empty root
  an explicit answer rather than one that rests on ReadDir's error (in
  `backup.ReapOrphans`, an error the caller reports instead of a silent
  no-op). **A test of such a refusal must plant something in the working
  directory** (`t.Chdir` into a temp dir) and require it untouched, or it
  cannot see the change that matters, a root resolved before the listing.
  The updater's and the orphan sweeper's planted nothing and passed with the
  refusal deleted, and backup's had no test.
- **The retention reap fails closed on an empty live-token set**, and the two
  empty forms are NOT interchangeable: `nil` deletes zero rows while
  `[]string{}` deletes EVERY row — and the caller builds the dangerous spelling.
  The rest of the retention and compaction rules now live under **### Database
  compaction and retention** — including the CEILING those windows also need,
  which this list did not have and which is the more dangerous half.
- **The binary swap keeps `dst` present throughout** (`Remove(bak)` →
  `Link(dst,bak)` → `Rename(new,dst)`), with the old two-rename path kept only
  as the EXDEV fallback: a power loss between two renames is permanently
  unbootable and rollback-on-boot cannot help, because the missing file IS the
  bridge. On Windows a stop-timeout must best-effort `Start()` again.
- **The new bytes are STAGED beside `dst` before anything is vacated, so the
  no-file window is two adjacent renames and never encloses a copy.** Both
  fallback shapes vacated `dst` first and then called `placeNewBinary`, which
  falls back to a ~30 MiB cross-volume copy plus an fsync on EXDEV /
  `ERROR_NOT_SAME_DEVICE` — so the gap both files described as "the tiny no-file
  window between the two renames" was the duration of a full transfer. **The
  POSIX hardlink path was never affected** (dst keeps resolving through its own
  dentry for the whole copy); the two that were are `swapBinaryViaRename` and
  **Windows, where the hardlink trick cannot apply at all and this is the ONLY
  path** — on the very host `placeNewBinaryWindows`' docblock names ("bridge.exe
  on D: and the data dir under %LOCALAPPDATA% on C:"), every update spent seconds
  with no `bridge.exe` on disk. The in-process restore cannot cover a power loss
  there. Staging tries the cheap same-volume move FIRST, so an ordinary install
  still pays a rename rather than a copy. `swap_test.go` exercised each fallback
  alone and never composed them, which is why nothing saw it — the pin now walks
  all four combinations and asserts the ORDER (copy before vacate), because
  "dst is never absent" is false by construction for the two-rename commit.
- **A swap PRESERVES the mode `dst` already has; it does not impose one.** The
  rename path inherited the extractor's `O_CREATE 0o755`, which IS umask-masked,
  while the copy path chmod'd an unmasked `0o755` — under a comment asserting the
  two matched. Measured under `UMask=0027`, which this repo's own deployment
  runbook prescribes: the same-volume path installed **0750** and the
  cross-volume path **0600**. The service user still execs it, so the bridge
  runs and nothing looks wrong; every other account on the host gets `EACCES` on
  a binary that worked yesterday, with no log line. The divergence is invisible
  at umask 0, so the test sets one.
- **An install of a target already staged on disk is REFUSED, and the refusal
  reads the PERSISTED marker.** `swapBinary`'s EEXIST retry does
  `os.Remove(bak)` before re-linking, so a SECOND install of the same version
  destroys the operator's rollback target and replaces it with a copy of what is
  already live — while `canRollback()` keeps reporting true, because it only
  stats for the file's existence. Measured, not argued: with the guard removed
  the test's `.bak` goes from `0.1.0` to `0.2.0`. Reachable through the ORDINARY
  console flow — `apiUpdatesInstall` does **not** restart (restart is a separate
  operator action), and `u.status.CurrentVersion` is written ONCE at
  construction (`Install` only decorates the local copy it returns), so
  `UpdateAvailable` stays true for the process lifetime and the console keeps
  inviting the click. Three terms, each load-bearing: **`Status=="installing"`
  AND `SwapStarted`** (an armed-but-unswapped marker mutated nothing — the
  Windows SCM-stop window — and `DecideBootAction` reads it as
  `BootClearNotSwapped`); **the SAME target**, because refusing a newer release
  would strand the host on a version it has not booted; and **within
  `recencyWindow`**, the same constant `DecideBootAction` uses for
  `BootClearAbandoned`, so the refusal expires exactly when boot would clear the
  marker and the two cannot disagree about whether it is live. **Marker, not the
  in-memory `pendingRestart` flag** — the flag records THAT a swap landed and
  not WHICH version, so refusing on it blocked a legitimate newer install (caught
  by the positive control), and `bridge update` is a separate PROCESS that sees
  no `atomic.Bool` anyway. `Install` now sets `pendingRestart` for EVERY path;
  it used to be set by `maybeAutoInstall` after the fact, so the admin and CLI
  installs — the two that never restart — left it false. An unreadable marker is
  NOT a refusal: failing closed there would block every install on the host.
- **A Windows rollback moves the running exe aside before it renames
  `.bak` into place** (2026-10-09, backlog B202). `RollbackBinary` on
  Windows was one `os.Rename` of the backup onto `dst`. Both callers
  run in this process (`Updater.Rollback`, `maybeRollbackOnBoot`), and
  `stopServiceIfRunning` returns without stopping when this process is
  the service, so that rename replaces the mapped image. Windows
  answers "Access is denied". `swapBinary` already vacates a running
  exe first (a rename of the image succeeds; a rename onto it does
  not). The rollback does the same, then renames `.bak` onto the
  vacated path, and renames the aside file back onto `dst` if that
  rename fails. The aside file is the image this process still maps.
  Windows will not delete it. `swapBinary` leaves its leftover on disk
  as `.bak`, which is the rollback target. The rollback leftover is
  the build being replaced, so it is not named `.bak`: `canRollback`
  would stay true and the next rollback would restore the broken
  build. A best-effort remove drops an earlier leftover when nothing
  maps it; a refusal leaves `bridge.exe.rollback-<n>` beside the
  binary, and the next vacate uses a new name so it never replaces
  that leftover. A missing `dst` stays one rename of the backup onto
  that path. Unix `RollbackBinary` is still one rename over the live
  path. (`TestRollbackBinaryReplacesARunningExe`,
  `TestRollbackReplaceVacatesARunningPathBeforeRenamingBak`,
  `TestRollbackReplaceRestoresDstWhenTheSecondRenameFails`.)
- **Booklet GC is skipped while a scan is in flight** — mid-rescan the release
  universe is transiently partial, so GC deletes every filesystem album's
  booklets and re-fetches them next cycle. An empty universe is a deliberate
  no-op everywhere it appears.
- **`PrunePlaylistCoversExcept` is deliberately unwired** — smart-mix retirement
  is reversible and playlist deletion is a revivable tombstone, so either
  trigger silently destroys operator-uploaded content to reclaim a JPEG. An
  AST-based guard fails if a production caller appears.
