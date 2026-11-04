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
  - "scripts/install.sh"
  - "scripts/stamp-install.sh"
  - "scripts/install-test/**"
---
# Config, settings and process lifecycle

The config and its live and boot halves, managed settings and controls, service install and restart, atomic writes and file ownership, the token store's writes, the self-updater's binary swap, and the one-line installer (`scripts/install.sh`).

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
  incident fail to start for every tenant at once. The warning names such
  an entry by its place in the list (`deployment.managedControls[1]`),
  beside the names this build knows, and never by what it holds, which can
  be anything (B505's round 4: it logged the entries as written;
  `UnknownManagedControls`,
  `TestServeNamesAManagedControlItDoesNotKnowByItsPlace`). The typo guard therefore
  lives in the program that WRITES the file (the conductor's
  `managed_controls_test.go`), pinned in both directions.
- **A managed setting can own other settings (`managedSettingImplies`): `playerEnabled` owns `playerOpenHome`**
  (2026-10-31, B612). The control plane that decides whether the bridge's own
  player runs decides whether it is exposed on the LAN for the 1-bit app to
  cast to, so `deployment.managedSettings` naming `playerEnabled` makes the
  Player page's OpenHome switch the control plane's too, hidden with the
  player's switch and refused by its route, with no second entry in the
  list. `IsManagedSettingOrImplied` (the write paths' predicate) and
  `EffectiveManagedSettings` (what the console is told) answer it beside the
  controls' `managedControlSettings`, through
  `SettingsImpliedByManagedSettings`; `IsManagedSetting` still answers what
  the operator named. `playerOpenHome` alone owns nothing else, so a control
  plane can own the exposure and leave the player's switch to the operator.
  **Don't imply a setting the other way, imply a control from a setting, or
  answer an implied setting in one predicate and not the other.**
  `TestAManagedPlayerSwitchOwnsTheOpenHomeSwitch` and
  `TestTheManagedPlayerSettingsHideAndRefuseTheOpenHomeSwitch` pin it.
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
  reverted. **`Store` closes `Changed()`**, the channel a consumer that
  follows a setting waits on (the player's hotplug watcher follows
  `output.player`, B470 PR 4a2): take the channel, then `Load`, then wait,
  so a Store in between still wakes it; an Update that stores nothing
  closes nothing. `TestChangedIsClosedByTheNextStore` pins it.
- **Every read-modify-write of `bridge.yaml` takes the exclusive lock beside the file the OS opens for its name (`config.LockFile`, `fsutil.ResolveParent`: the directory a `..` after a link resolves to, on Windows named by the handle the OS opens, junctions included (`fsutil.ResolveLinks`), and refused only as a link still, its own entry a name surrogate, or as one whose resolution moves when asked again, a OneDrive or container folder being a directory (`checkResolvedDir`); a link at the last element followed to its target, whose name the lock takes, `.actual.yaml.lock` for `alias.yaml` → `actual.yaml`; `config.Edit` for the whole read-modify-write), a lock file a root writer makes taking the config's owner (`fsutil.OpenLockFileLike`); the name is resolved again after every read a decision of the turn rests on, last: once the turn has made its own read, before each attempt at a rename after the byte comparison and a writer's own comparison in `SaveIf`, and at `FileLock.Check` after its read, and a name that opens another file than the turn locked refuses with nothing written (`*config.NameMovedError`; a CLI writer says to run it again, and nothing is retried on the file it opens now); it decides on what its turn found and saves a value taken from it (`FileLock.LoadFound`, `Found`), or only where its turn found no file, or after an if that returns on a comparison of the turn's bytes `comparedSaves` lists (init's, a restore's, the holder's), at the top of the function or inside its check of the turn against nil, a save after that check then made through the name it tests, assigned nothing since, and writes only through `Save(l)` / `SaveIf(l, …)` / `l.SaveBytes`, which stage beside the target and, before each attempt at the rename, refuse a file that is no longer the one the turn read (`*config.ChangedInTurnError`), a refusal ending the retries and no rename made once `saveWindow` has ended (`atomicwrite.RenameWithRetryIf`); the turn's reads are asked again only while Windows answers `ERROR_SHARING_VIOLATION` or `ERROR_ACCESS_DENIED`; the wait is bounded at `config.LockWait` (5 s), then a `*config.BusyError` that names the file and none of its values, or, where every try answered ENOLCK, a `*config.NoLockManagerError` naming the remedy; only a lock whose answer says the filesystem keeps none (EOPNOTSUPP, ENOTSUP, `errors.ErrUnsupported`, Windows' ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION) leaves the turn unlocked, said once, and any other failure is returned; a link to no file is refused before anything is written, a link into a directory this user cannot write names the remedy, and every entry resolves the config's relative paths against the directory `config.Load` takes from the name it was given, unmoved; serve's console save (`RuntimeConfig.Update`, `UpdateChecked`) takes its turn and compares before it runs its change, saves only into the file serve's boot read was of (`config.LoadFileAt`: the name resolved before each read, the file it resolved to read, the name resolved again after it, a read answered only when both resolutions agree, a name that never did refused, and one resolved at no side of any read booting with no file, every console save then refused before its turn until a restart, `*config.BootUnresolvedError`; `RecordFile` takes the file as it is, never `""`) and over the bytes the holder last read or wrote, refuses a file another writer changed, or a name repointed since, at its turn, its check or its save (`*config.ChangedOutsideError`), rather than merge, and a change of its own a late refusal would leave done asks the turn's comparison again just before it (`check`); what is left is the window between the turn's final check and the rename, a writer that takes no lock, and the time between a check and the work it authorizes** (2026-10-24, backlog B506, six rounds). **Don't save a copy read before the lock or decide on a read beside the turn, save a value not taken from the turn without an if that refuses on a comparison `comparedSaves` lists, accept a save past a nil-checked comparison through another name than the checked one or through that name assigned since the check, hold the lock across a prompt or a process's life, lock the config file itself or put the lock in the data dir, clean a config's name before it resolves or move the base of its relative paths, lock or rename beside a link rather than its target, follow a Windows link with `filepath.EvalSymlinks` over the whole name, keep a junction's own spelling or refuse a reparse point that is no link, write through a link to no file, read, compare or replace a file the config's name no longer opens, resolve the name before a read a decision rests on rather than after it, or retry on the file it opens now, stage a save anywhere but where its rename lands, make a root writer's lock file root's beside another owner's config, wait without the bound, take ENOLCK or any failure but a definitive no-locks answer for an unlocked turn, rename an attempt without the turn's comparison, retry past its refusal or rename once the save's window has ended, put a value in a refusal, give `Save` a path back, drop bind's `SaveIf` comparison, let a console save write over bytes it did not last read or write or into another file than serve's boot read was of, read the boot config through its name once it resolved it, take a resolution that failed for a file or compare `""` as one, resolve the holder's file anywhere but at that read, change anything besides the config before calling `Update` (do it in `fn`, after the comparison, asking `check` just before), or add a writer of `bridge.yaml` that `configWriters` does not name.** → History-rules-config-lifecycle.md
- **`Validate()` is a pure shape check; accessibility is
  `CheckLibraryRootsAccessible`, called only by `serve`.** Putting `os.Stat` back
  in `Validate` breaks `bridge update` on any host where the daemon user and the
  binary owner differ (the public-mode VPS layout). Loopback installs fail fast
  on an inaccessible root; public installs warn and continue. An output-only
  bridge (`output.mpd: on`) is a loopback install with no library root:
  Validate allows that and does not dial MPD to decide, and `auto` still
  requires a root. **Don't treat `auto` as on inside Validate.** Nor does
  `output.player` ever refuse a config: an invalid block turns the player
  off with a reason logged once each time it changes (the first two
  bullets under **Player output binding** in `player.md`, which has held
  that section since 2026-10-22).
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
- **A live backup retries Windows sharing/access failures while opening files replaced by rename, with atomicwrite’s bounded, cancellable backoff; persistent errors still refuse it.** **Only a reservation refused as data gets backup’s player reset advice; cancellation, an expired context and file-access errors never do. Exhausted sharing/access retries say the file was busy and to retry the backup.** → History-rules-config-lifecycle.md
- **Atomic writes: stage, then rename with retry.** `RenameWithRetry` absorbs
  the Windows AV scan-on-close window; the deferred `Close` must be registered
  AFTER the deferred `Remove` (LIFO — Windows won't unlink an open file).
  `GenerateWithOptions` stages BOTH cert and key before renaming either, so a
  failure leaves the existing pair intact; **never pre-delete**, and never run an
  orphan-cleanup `Remove` on a file already committed. Each site keeps its OWN
  `Chmod` / `Sync` / parent-dir fsync — **don't collapse them into a shared
  `WriteBytes`**, which silently drops e.g. the auth store's belt-and-braces
  `Chmod(0o600)`.
- **`atomicwrite.WriteBytesDurable(root, path, …)` is the strict writer: nil
  means the file's bytes and every directory entry between the file and
  `root` were fsynced by that write, and a failed directory fsync is the
  write's error, never tolerated as `WriteBytes` tolerates it once its
  rename has landed** (B470 PR 4b1; its one caller is the player's ID and
  token reservation, with the data directory as `root`: the "No ID and no
  token is handed out twice" bullet in `player.md`). It keeps nothing
  between writes: each syncs every entry from the file's directory up to
  `root`, whoever made those directories, so one an earlier write or an
  earlier run could not sync is synced by the next, across a restart too.
  Round 5 kept a process-local set of such directories, which a SIGKILL or a
  clean exit dropped while the directory sat in the kernel's cache, and
  round 4 removed them, which raced every other writer. **The path must lie
  plainly below `root`, so that one walk from the file's directory up to
  `root` reaches every entry** (round 7): a `..` element in `root` or the
  path is refused, since `filepath.Abs` cleans it lexically while a reader
  resolves the link before it (round 6 wrote and fsynced `root/ids` for
  `root/link/../ids`, which a reader opens below where the link points, so a
  restart read no reservation, with no crash at all), and so is a path whose
  last element names a directory, a trailing separator, `.` or `..`
  (`NamesDirectory`, `EISDIR`: round 7 wrote `root/ids/` as `root/ids`,
  which the engine's restart read as given and found stuck; Codex on round
  7); so is a symbolic link strictly below `root`, whether it leads out or
  stays inside, found by `Lstat` from `root` down; and so is any mount there
  or at the file itself, a bind mount of `root`'s own filesystem included,
  **told by the identity the platform gives each entry's mount, never by the
  device (`st_dev`) a stat reports**, whatever the path's spelling, on every
  write: on Linux the kernel's mount ID, from statx's `STATX_MNT_ID` (Linux
  5.8) or, where statx gives none (an older kernel, or a statx a seccomp
  policy refuses), the `mnt_id` `/proc/self/fdinfo` lists for an `O_PATH`
  descriptor (Linux 3.15; `mountIDFrom`), whose listing gives none only
  where it does not exist, with no `/proc`, any other failure to read it
  being the read's error (the Claude API on round 9: an EMFILE read as no
  identity); on macOS the mount's filesystem ID (statfs's `f_fsid`). Round
  7's probe matched the mount table by spelling, which a filesystem that
  folds case reaches a mount around, looked for a bind mount at New alone,
  and never looked at the file (Codex on round 7); round 8 compared devices,
  and overlayfs with its layers on different filesystems and xino off, the
  Raspberry Pi OS "Overlay File System" option, reports the overlay's device
  for a directory and a pseudo device per layer for every other entry, and
  btrfs a device per subvolume, each on one mount, so round 8 refused the
  reservation file there once it existed (the Claude API on round 8); an
  overlay whose upper layer is a tmpfs, that option's own, is refused since
  round 10 for another reason, its storage (below). The checks run before
  anything is created and again once the missing directories are, for a link
  another writer puts in place meanwhile. Round 6 walked the path as written
  and as it resolved instead, and missed a link reached through another link
  and synced outside `root` for a link out and back (Codex, the Claude API
  and Argon on round 6). `root` is the caller's to vouch for: it must exist
  (the writer never creates it), off an overlay its own entry is never
  synced, and a link at or above it, a data directory that is a link
  included, is the caller's too: `root` is resolved once, and every path the
  write opens, creates and fsyncs, and every name its errors give, is
  spelled from where it resolved. It refuses, before anything is created, a
  platform with no directory barrier (`ErrNoDirectoryBarrier`), a root that
  does not exist, and a path not plainly below `root` (`ErrNotUnderRoot`).
  Each directory is opened once and that descriptor fsynced (`syncDirAt`),
  so only an fsync's own failure is `ErrBarrierFailed`, an `*fs.PathError`
  naming the directory that would not sync: an open that fails (EACCES,
  EMFILE) is returned as it is. A rename that fails while the destination
  holds the same bytes takes them only from a regular file on its
  directory's own mount, by identity, asked as it takes them
  (`onItsDirectorysMount`): a file mounted there after the checks, or a
  symbolic link, leaves the rename's error, and a file or a directory the
  platform gives no mount identity for leaves `ErrMountsUnknown` beside it.
  **Once the write lands, by its rename or by that fallback, every directory
  from the file's up to `root` is synced again** (`durableChain`): staging
  the file on an overlay copies a directory found in the lower layer alone
  up into the upper one, creating its entry, and those above it up to
  `root`, after the syncs that ran before the file was staged, and overlayfs
  fsyncs none of the parents a copy-up creates (Codex on round 9: round 9
  then synced the file's directory alone). **On an overlay every directory
  above `root`, up to the root of its mount, is synced too, once the write
  lands and in the probe** (`copyUpsFrom`, `ancestorsOnMount`, by
  mount ID): a write below a root in the lower layer alone copies `root` up,
  and every directory above it up to the overlay's own root, and those
  entries are the copy-up's, which no caller could have synced; a crash that
  took them would take the reservation and keep the identity below (Codex on
  round 10, which round 10 left as an owner item; the orchestrator chose
  this fix). A directory there the service cannot open is named; only
  `fs.ErrPermission` advises read access for the service or leaving the
  overlay. ENOENT and other open errors retain their cause without that
  advice (`syncAbove`, B514). **A root on a bind of a
  directory inside an overlay, its mount's root field another than `/`, is
  refused** (`ErrOverlayBind`, also matching `ErrMediumUnknown`, wherever a
  writable overlay would be taken, naming
  the bind's root and the fix: a data directory on a filesystem that is not
  an overlay, or on the overlay's own mount): the copy-ups above it are out
  of that mount's reach: a crash risk even on persistent storage, not
  evidence that the medium loses data at reboot. A read-only overlay with
  no upper layer is taken only at depth 0, the root's own mount, a bind
  included, so the write itself fails EROFS. At greater depth it is
  `ErrMediumUnknown`: the upper path reaches a read-only overlay, which
  cannot be a writable overlay's upper layer (B514 round 2).
  An operator's systemd `ProtectSystem=strict` with
  `ReadWritePaths` on an overlay root makes such a bind of the data
  directory, as do a chroot and a container's bind of an overlay host's
  directory; our own unit adds no sandboxing, and an overlay root's own
  mount, Volumio's shape, is taken (Codex and the Claude API on round 11;
  the orchestrator's decision, which rejected syncing through another mount
  of the superblock, and syncfs, whose errors overlayfs reports only on
  recent kernels). **`root` must be on storage that keeps what is written to
  it through a reboot** (`ErrVolatile`; the orchestrator's decision on round
  9's review, to fail closed): on Linux tmpfs and ramfs, by statfs's
  `f_type`, an overlay mounted volatile, and an overlay whose upper layer is
  one of those, the Raspberry Pi OS "Overlay File System" option among them
  (the Claude API on round 9: every barrier answered there, and a reboot
  took the reservation and kept the identity, so IDs and tokens repeated
  with no crash at all). An overlay's upper layer is the one the mount table
  names on the current overlay's own mount-ID line (`mountLine`, `overlayLayer`),
  never a line picked by its mount point's spelling, the line split on the
  single ASCII space the kernel writes between fields and nowhere else
  (Codex on round 10: `strings.Fields` split a lowerdir at a U+00A0 the
  kernel leaves unescaped, and lost the upperdir and volatile after it),
  whole (six fields, the optional ones, the `-`, then exactly the type, the
  source and the super options, none empty but the source, which the kernel
  writes empty for a filesystem mounted with none: the Claude API on round
  11), and naming no upper layer only for an overlay that is read-only; and
  the path that names it is compared with the overlay's statfs totals,
  **read for up to three rounds where they differ** (`upperAsReached`); a
  path that reaches nothing is refused as one that cannot be read here,
  but on Volumio's evidence (the B603 bullet after this one).
  Agreement, including on the third upper read, is taken; moving overlay
  totals retain the existing check that rules out a tmpfs or ramfs path.
  Exhausting the retries is **inconclusive**, never proof of another
  filesystem: repeated A,B,A samples can come from legitimate XFS/ZFS.
  The upper path's own mount ID selects the line whose fstype is named
  (`upperMountEvidence`), never a line selected by spelling or device.
  Upper-path mount IDs follow the final symlink as statfs/stat do
  (`upperMountID`), for this evidence and nested overlay/FUSE lookups;
  the strict writer's mount-boundary reader keeps its no-follow semantics.
  Device numbers are detail only: a btrfs subvolume's `st_dev` can differ
  from mountinfo's superblock device without invalidating that line.
  This establishes the filesystem visible at that path only,
  not the overlay's backing across mount namespaces. A persistent mismatch
  stays `ErrMediumUnknown`, naming that evidence or why it was unavailable,
  with advice to retry after filesystem activity settles and to check the
  service's mount namespace. No missing evidence or mismatching memory
  upper is taken for durable storage. Likewise,
  where the table cannot be read or names the upper layer by a relative
  path, past eight overlays one over another, and on fuse-overlayfs, told by
  its type's subtype or, for a build that sets none, its source, whose upper
  layer the table does not show (the Claude API on rounds 10 and 11), the
  writer says it cannot tell (`ErrMediumUnknown`). **It refuses what it can
  tell is volatile and what it cannot read, and does not try to prove every
  stack persistent** (the orchestrator's scope on round 10's review, which
  rejected Codex's Highs 1 and 3): the data directory must be on persistent
  storage, and ext4 on a zram device or on a loop image kept in tmpfs, an
  NFS export written async, a FUSE filesystem other than fuse-overlayfs
  whose daemon writes to memory, an overlay nested in another's upper
  directory, whose copy-ups above the outer upper directory are not synced
  (the orchestrator on round 11), and a mount namespace that maps an
  overlay's upper path onto a filesystem with the same totals are stacks it
  cannot tell, the operator's to avoid. macOS takes every root, having
  neither tmpfs nor overlayfs. `ProbeDurable(root, path)` runs the same
  checks and barriers, without writing, on every directory a write would
  fsync that exists now. **Where the platform gives no mount identity, for
  `root` or for an entry below it, the write, the probe and the fallback say
  so (`ErrMountsUnknown`) rather than take two mounts for one** (Codex on
  round 8: where the kernel gave no mount IDs, round 8 compared no mount on
  a write, its probe read the mount table, matched by spelling, and its
  fallback took two unknown identities for one). `root`'s is asked for on
  every call, a file directly in `root` included, so that the probe refuses
  as its caller starts what every write would; the mount table is never read
  for a mount's identity, and only for an overlay's upper layer and root
  field and a FUSE mount's type and source, on each current mount's own
  ID line: the inner mount's for nested overlays and FUSE below an overlay.
  B514 also reads the visible upper path's own device and fstype evidence. On Linux that leaves a kernel older than 3.15, or one
  whose statx gives none with no `/proc` mounted, and elsewhere every
  platform but macOS and Windows, where the player does not run; Windows
  gives none and asks for none, refusing first (`ErrNoDirectoryBarrier`).
  **Don't count a write durable without every entry up to its root, don't
  keep in memory which entries are durable, don't delete anything to recover
  durability, don't sync above the root off an overlay, nor above the root
  of its mount on one, don't walk a link or a mount below the root or at the
  file, don't tell a mount by its spelling or its device, don't take two
  mounts the platform gives no identity for as one, don't clean a `..` or a
  trailing separator away, don't take a fallback's bytes from a file the
  directory's entry does not name, don't count a write durable before the
  chain is synced again once it lands, don't take a root on storage that
  does not outlive a reboot, or that cannot be told to, don't pick an
  overlay's line in the mount table by its spelling or split it on any white
  space but the ASCII space, don't take a bind of a directory inside an
  writable overlay, nor sync its copy-ups through another mount of the superblock,
  don't take a read-only overlay reached through a writable overlay's upper
  path, don't read an upper link's mount ID instead of its target's,
  don't reject mount-ID-selected evidence for a differing device,
  don't claim differing totals prove another filesystem, and don't advise
  moving to another filesystem for an error not marked `ErrBarrierFailed`,
  but for the two overlay copy-up refusals (a bind of a directory inside an
  overlay, a directory above the root its sync cannot open for lack of
  permission), where leaving
  the overlay removes that sync.**
  B514 evidence and refusal details → History-rules-config-lifecycle.md.
  `TestWriteBytesDurable_RefusesAPlatformWithNoDirectoryBarrier`,
  `TestWriteBytesDurable_RetriesTheBarrierOfADirectoryItCreated`,
  `TestWriteBytesDurable_AFreshWriterSyncsAnEntryAnEarlierRunLeft`,
  `TestWriteBytesDurable_RetriesThroughASymlinkedAlias`,
  `TestWriteBytesDurable_NeverSyncsAboveTheRoot`,
  `TestWriteBytesDurable_RemovesNothingAnotherWriterMade`,
  `TestWriteBytesDurable_RaceWinnerStillNeedsTheBarrier`,
  `TestWriteBytesDurable_RefusesADotDotElement`,
  `TestWriteBytesDurable_RefusesAPathThatNamesADirectory`,
  `TestWriteBytesDurable_RefusesALinkBelowTheRoot`,
  `TestWriteBytesDurable_RefusesALinkPutInPlaceWhileItWrites`,
  `TestWriteBytesDurable_TakesALinkAtOrAboveTheRoot`,
  `TestWriteBytesDurable_RefusesAMountBelowTheRoot`,
  `TestWriteBytesDurable_RefusesABindMountByItsMountID`,
  `TestWriteBytesDurable_RefusesABindMountWhateverItsSpelling`,
  `TestWriteBytesDurable_RefusesAMountedDestination`,
  `TestWriteBytesDurable_TakesTheFallbackOnlyFromAPlainFile`,
  `TestWriteBytesDurable_DecidesAMountByItsIDNotItsDevice`,
  `TestWriteBytesDurable_SaysSoWhereNoMountIDCanBeHad`,
  `TestWriteBytesDurable_SyncsTheChainAgainOnceTheWriteLands`,
  `TestWriteBytesDurable_SyncsWhatACopyUpCreated`,
  `TestWriteBytesDurable_RefusesARootThatDoesNotOutliveAReboot`,
  `TestWriteBytesDurable_RefusesAnOverlayWhoseUpperLayerIsTmpfs`,
  `TestWriteBytesDurable_SyncsUpToTheRootOfItsMountOnAnOverlay`,
  `TestWriteBytesDurable_RefusesABindOfAnOverlaySubtree`,
  `TestCopyUpsFromWalkOnlyOnAnOverlay`,
  `TestAncestorsOnMountStopAtTheRootOfTheirMount`,
  `TestMediumTakesTotalsThatMoveWithWhatIsWritten`,
  `TestProbeDurable_RefusesTmpfsThroughThePackagesOwnMediumCheck`,
  `TestOverlayLayerIsTheLineOfTheRootsMountID`,
  `TestMediumRefusesWhatDoesNotOutliveAReboot`, `TestMediumMagicsAreLinuxs`,
  `TestPlatformMediumReadsTheKernels`, `TestPlatformMountIDReadsTheKernels`,
  `TestProbeDurable_RefusesAMountPointTheKernelLists`, `TestParseMountInfo`,
  `TestPlatformMountTableReadsTheKernels`, `TestParseFdinfoMountID`,
  `TestMountIDFromFallsBackToFdinfoOnlyWhereStatxGivesNone`,
  `TestPlatformMountIDAsksFdinfoWhereStatxGivesNone`,
  `TestFdinfoMountIDSaysWhatFailedToRead`, `TestBarrierFailuresAreMarked`,
  `TestProbeDurable_SyncsTheDirectoryItOpened` and
  `TestProbeDurable_AnUnreadableDirectoryIsNoBarrierFailure` pin it.
- **On Volumio alone, the root overlay's upper layer that no path reaches is taken, on the box's own evidence, an explicitly trusted Volumio boot-layout assumption** (2026-10-30, backlog B603; `volumioUpper`, one inspection the writer and `bridge doctor` share, `atomicwrite.InspectMedium`). Volumio's initramfs mounts the boot disk's partition 3 at `/mnt/ext`, the root overlay's upper layer `/mnt/ext/dyn`, and nothing mounts it after the pivot, so statfs of the upper path answers ENOENT; the fallback takes it only at the root's own mount, on that first ENOENT, with every fact read in order and the refusal naming the first that fails: VOLUMIO_VERSION in `/etc/os-release`; the overlay read-write, mount and superblock, over `/mnt/static` with upperdir `/mnt/ext/dyn`, the overlay at `/`; `/imgpart`'s line, by its mount ID, partition 2 of a disk in sysfs, its `dev` that line's major:minor; the disk's partition 3, the directory `/sys/dev/block` resolves its own major:minor to, ext4 labelled `volumio_data` in udev's database; its ext4 superblock in `/sys/fs/ext4` and its major:minor on no mountinfo line. **Don't take an unreachable upper layer on any other box, at any depth but the root's own mount, or for any statfs failure but ENOENT; don't let it outrank a volatile overlay, a reachable memory upper, inconclusive totals or the bind refusal, and don't return nil where `taken()` decides; don't find the partition by its label (the owner's NVMe shares it), edit a device name as a string, or pick `/imgpart`'s line by its spelling; and don't take a partition 3 that no live ext4 superblock is, or that a mountinfo line lists.** → History-rules-config-lifecycle.md
- **`atomicwrite.InspectMedium` refuses, as the strict writer's `checkPath` does, a root that is there and is no directory, before it reads the root's mount: a regular file or any other entry but a directory, a link that leads to one, and a link at the root whose links lead to nothing, each a `*RootNotADirectoryError` matching `ErrRootNotADirectory`, never `fs.ErrNotExist`** (2026-11-02, backlog B606; bridge doctor's player-storage line fails them, cli-serve.md). **Don't inspect the mount of a root that is no directory, or answer a link to nothing at the root as a root not made yet.** `TestInspectMediumRefusesARootThatIsNotADirectory` pins it. → History-rules-config-lifecycle.md
- **`atomicwrite.ReplaceDurable` replaces an entry a rename cannot replace in
  one step, a directory included, without a failure emptying its name: after
  WriteBytesDurable's refusals it stages and fsyncs the new bytes beside
  path, renames the entry aside (a name beside path, checked) and the staged
  file into its place with no write and no sync between the two renames, puts
  the entry back and syncs again when the second rename is refused, and syncs
  the chain once both have landed; between the two renames the name is
  empty, so its caller keeps a second durable record** (B539; its one caller
  is the discard's `sealPlayerMarker`). **Don't sync between the two renames,
  stage after the entry moves, take an aside outside path's directory, or
  drop the put-back.** `TestReplaceDurable_StagesTheNewFileBeforeTheEntryMoves`,
  `TestReplaceDurable_ReplacesEveryKindOfEntry`,
  `TestReplaceDurable_LeavesTheEntryWhenAStepBeforeTheSecondRenameFails`,
  `TestReplaceDurable_SaysWhereTheEntryIsKeptWhenItCannotBePutBack`,
  `TestReplaceDurable_ABarrierThatFailsLeavesTheWriteNotDurable` and
  `TestReplaceDurable_RefusesBeforeItStages` pin it.
  → History-rules-config-lifecycle.md
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
- **…and a job or database CLI run as root gives every entry it CREATES the install's owner** (2026-09-28, backlog B17) → History-rules-config-lifecycle.md
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
- **…and a listing of the backups folder that `os.ReadDir` answers as not
  there, or as not a directory, asks Lstat before it answers** (2026-10-31,
  B604's fourth round). `os.ReadDir` opens with `O_DIRECTORY` and answers a
  regular file with ENOTDIR on every OS, and on Windows `syscall.ENOTDIR`
  is `Errno(ERROR_PATH_NOT_FOUND)`, which matches `os.ErrNotExist`, so a
  file where the folder goes listed as no backups on Windows alone: GET
  /api/backups answered 200 there and 500 elsewhere. `readBackupsDir`,
  which `List` and the orphan sweep share, takes only Lstat's not-exist for
  nothing there, and a folder it finds for one made since the listing;
  another Lstat error is the listing's. Anything else there, a link to
  nothing included, which listed nothing on every OS until then and through
  which a snapshot cannot make the folder either (`MkdirAll` answers
  EEXIST), fails the listing on every OS with one answer naming the folder,
  `errNotADirectory` (`open <folder>: not a directory`). **Don't take
  `os.ReadDir`'s not-exist for nothing there where a file can stand in the
  folder's place, and don't answer a path that is not a folder with
  `syscall.ENOTDIR`, which on Windows is an absence.**
  `TestWhatIsWhereTheBackupsFolderGoesListsAlikeOnEveryOS` pins it on every
  OS: its seam (`SetReadDirForTest`) lists as Windows does.
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
- **An install of a target already staged on disk is REFUSED, and the refusal reads the PERSISTED marker.** → History-rules-config-lifecycle.md
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
- **A candidate is checked against this bridge's settings before its swap, and an install whose version refuses them at its first start is rolled back before serve writes anything** (2026-11-02, backlog B613, five rounds): 0.2.2 refuses a fraction or a leading zero in a whole-number setting (B528, B533), 0.2.1's updater swapped without asking, and the new serve exited 2 at the load, before the boot rollback, under `Restart=always`. Install stages the verified candidate beside the binary and runs `<candidate> serve --validate-config --config <the settings the bridge loaded>` (`updater.CheckConfig`: 30 s, stdin empty, 64 KiB of each output): 0 swaps; 78 refuses, one `config refused: ` line a reason, naming the line and no path (`config.RefusedError.Reasons`), and the version is recorded as rejected with its reason and a digest of the settings (the file's bytes and every BRIDGE_ variable, on Windows whatever its name's case, `updater.ConfigDigest`), which holds it from the auto-installer while the digest is unchanged, a newer version and a manual install checking again; a 2 carrying the flag package's refusal of the flag is a candidate that predates the check, held from the auto-installer for every setting and installed by hand with a warning; for anything else (a timeout, a cancellation, a candidate that will not start, another code), nothing is swapped, installed or recorded. The check is serve's own load (`checkServeConfig`: the boot read, Load, `serveConfigRefusal`), which writes, opens and binds nothing, and never a root's reach (`CheckLibraryRootsAccessible` is the host's, and the user's who runs it). Serve, refusing its settings (a `config.RefusedError`, or `serveConfigRefusal`, every one of them before the data dir's lock), reads the data dir from the dataDir key, the default, BRIDGE_DATA_DIR and the settings' directory alone (`config.DataDirOf`), and puts the backup back only for a recent `installing` marker of this version over another, or its own record of one (`State.RollbackStarted`), swapped, under which no other version has opened the data dir, with the data dir's lock its own and the backup a regular file whose `version` names the marker's PreviousVersion (`updater.EarlyRollback`, `updater.ProbeVersion`, a version check and not a proof of the bytes); it records the hold durably before the rename, in the form the restored binary's boot keeps, Status "" beside the install it undoes (`updater.RollbackRecord`; 0.2.1's and 0.2.2's boot clears an install marker it cannot finish), so a start of this version that finds it with the backup not back finishes the rollback; a hold it cannot record leaves the install as it is, and a rename that fails puts the install marker back; then it exits 75 (`supervision.RestartExitCode`), which systemd, launchd and the SCM's recovery all restart; otherwise it refuses as before, saying why when the marker is this version's. Before its first write, serve records in an armed marker that it opened the data dir (`State.DataDirOpenedBy`, `updater.AdmitBoot`; a marker it cannot read is removed) and refuses to start when it cannot, and every store open with a migration step pending does the same before the step, whichever command opens the store (`updater.DisarmBeforeMigrating`, `manifest.OpenStore` refusing); BootInstallFailed leaves the backup where a version other than the backup's opened the data dir, and every clear at boot keeps a hold, and a veto of a rolled-back version's store migrations, recorded beside the install (`updater.ClearInstall`); and a directory barrier that fails is the marker's write's error (`SaveState`, `ClearStateDurably`), but where the filesystem cannot fsync a directory (the next bullet). **Don't swap a candidate whose check did not answer 0, take a failed or stopped check for a refusal or a 2 for "predates" without the flag package's words, record a refusal against no digest or a predates hold against one, change the flag, the codes or the prefix (every later release answers this updater), let the check write, open a store, bind or ask a root's reach, name a path in a reason, find the marker's data dir by anything but Load's rules for that key, restore without the lock, the marker's proof and the backup's version, or before the hold is recorded durably, restore after another version opened the data dir, migrate the store before recording that this version opened it, drop a recorded hold or veto at boot, ignore the marker's directory barrier, write the settings, or exit 0 after a restore.** `TestAnInstallWhoseCandidateRefusesTheSettingsSwapsNothing`, `TestAnAutomaticInstallHoldsARefusedVersionBackUntilTheSettingsChange`, `TestAnAutomaticInstallHoldsBackACandidateThatPredatesTheCheck`, `TestACheckThatFailsSwapsNothingAndRecordsNothing`, `TestCheckConfigReadsTheCandidatesAnswerByItsExitCode`, `TestTheSettingsCheckRefusesWhatServeRefuses`, `TestTheUpdaterReadsWhatThisBridgesSettingsCheckSays`, `TestDataDirOfIsTheDataDirLoadGives`, `TestServeRollsBackAnInstallWhoseVersionRefusesItsSettings`, `TestServeLeavesAnInstallItCannotProveAsItIs`, `TestServeDisarmsTheEarlyRollbackBeforeItsFirstStoreAccess`, `TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir`, `TestEveryInstallTheBridgeStartsNamesItsSettings`, `TestTheHoldIsRecordedBeforeTheBackupIsPutBack`, `TestServeLeavesTheBackupWhereItCannotRecordTheHold`, `TestAStartFinishesARollbackStoppedBeforeItsRename`, `TestACommandThatMigratesTheStoreDisarmsTheEarlyRollback`, `TestAStoreOpenThatMigratesRecordsItInAnArmedInstallMarker`, `TestTheBootKeepsARecordedHoldWhereItClearsAnInstall`, `TestSaveStateReportsADirectoryBarrierThatFails` and `TestTheSettingsDigestReadsTheBridgeVariablesAsThePlatformNamesThem` pin it. → History-rules-config-lifecycle.md
- **…and a marker's write is trusted only once its barrier has answered, one lock (`<dataDir>/update-state.lock`, `updater.LockState`) keeps the early rollback and every store open that migrates apart, a directory fsync the filesystem cannot do (EINVAL, ENOTSUP, EOPNOTSUPP) is taken as done, said once, and the boot reconciles an interrupted rollback** (2026-11-02, backlog B613's round 3). **Don't trust a marker a failed barrier left without the barrier again, take any other fsync answer, or an open or a close failing, for a filesystem that cannot fsync a directory, publish the marker through a rename that runs a barrier of its own, lock `update-state.json` or remove the lock file, take the lock before server.lock or the ladder or either while holding it, write the marker in a guarded step without it, migrate under a rollback of the running version under way, stamp an interrupted rollback installed before the store opens, read it as an install marker or a hold alone, restore from it at boot, keep a later spelling of a Windows variable, or open a store anywhere but `manifest.DefaultDBPath(dataDir)`.** `TestADisarmRunsTheBarrierAgainOverARecordItCouldNotMakeDurable`, `TestADisarmRunsTheBarrierAgainOverARemovalItCouldNotMakeDurable`, `TestServeRefusesUntilTheMarkersBarrierAnswers`, `TestTheMarkersBarrierTakesOnlyADirectorySyncTheFilesystemCannotDo`, `TestDarwinsFullSyncFallsBackToAnOrdinaryFsync`, `TestTheMarkersWriteRunsOnlyItsOwnBarrier`, `TestADisarmWithoutTheLockWritesNothing`, `TestAMigrationThatHoldsTheMarkersLockFirstStopsTheRollback`, `TestARollbackThatHoldsTheMarkersLockFirstStopsTheMigration`, `TestAMigrationRefusesWhileARollbackOfItsVersionIsUnderWay`, `TestATargetWhoseSettingsAreFixedConfirmsAnInterruptedRollback`, `TestARefusedStartFinishesAnInterruptedRollbackOnlyOnItsProof`, `TestAPatchedPreviousVersionKeepsTheHoldOfARollbackThatFinished`, `TestTheSettingsDigestKeepsTheVariableTheOSReads` and `TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase` pin it. → History-rules-config-lifecycle.md
- **…and the veto of a rolled-back version's store migrations (`State.MigrationVeto`) outlives the rollback's record, the boot's housekeeping holds the install marker's lock, that lock's wait ends with serve's stop, and an interrupted rollback is confirmed only on the install's own record** (2026-11-03, backlog B613's round 4). **Don't drop the veto at a clear or a refusal's record, lift it anywhere but at the vetoed version's own serve admission (never without the lock, but where the filesystem keeps none) or by a new install's fresh marker, let a store open of the vetoed version migrate with the lock or without, put a backup back before its veto is recorded, read, write or swap at boot without holding `update-state.lock` (a filesystem that keeps none, or a holder past the wait, leaves the marker and the binaries as they are), take a stop in that lock's wait for a lock that could not be taken, or confirm an interrupted rollback on less than the record its admission made.** `TestTheMigrationVetoOutlivesTheRollbacksHousekeeping`, `TestOnlyTheVetoedVersionsOwnStartLiftsTheVeto`, `TestANewInstallsMarkerCarriesNoVeto`, `TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots`, `TestAMigrationThatHoldsTheMarkersLockFirstStopsTheBootRollback`, `TestABootRollbackThatHoldsTheMarkersLockFirstStopsTheMigration`, `TestTheBootLeavesTheInstallWhereItCannotHoldTheMarkersLock`, `TestTheMarkersLockWaitEndsWithItsCallersContext`, `TestADisarmStoppedInItsWaitReturnsTheStop`, `TestAStopWhileServeWaitsForTheMarkersLockEndsTheStartThere`, `TestTheConfirmationNeedsTheInstallsOwnRecordOfTheDataDir` and `TestTheBootConfirmsAnInterruptedRollbackOnlyOnTheInstallsOwnRecord` pin it. → History-rules-config-lifecycle.md
- **An install marker's read and removal errors name no path in a refusal, log line or stderr; the refusal keeps its causes through `Unwrap`, and `DisarmResult.Unreadable` keeps the original read error for code** (2026-11-03, B613 round 5). **Render `Unreadable` through `fsutil.ErrorWithoutPath` only where it is printed or logged; never replace it with a string error.** `TestDisarmRefusalsKeepCausesWithoutPaths`, `TestDisarmUnreadableKeepsTheOriginalPathError`, `TestMigrationMarkerDiagnosticsCarryNoPaths` and `TestServeMarkerDiagnosticsCarryNoPaths` pin it. → History-rules-config-lifecycle.md
- **The updater's own install, rollback, hold and restamp hold the install marker's lock: `Updater.Install` (`bridge update`, the console's, the auto-installer's) takes `update-state.lock` only after its download, verification and settings check, decides again under it (`decideUnderLock`: a swap of that release waiting for its restart, whichever process made it; for the auto-installer, a hold recorded since its gate) and holds it through its marker writes and swap, a failed swap putting back the marker it read; the console's `Updater.Rollback(ctx, …)` holds it from its read through its record and rename, refuses where `DataDirOpenedBy` names a version other than the backup's (`RollbackRefusal`), and records durably before the rename the marker it read with the version held back and its migrations vetoed (`OperatorRollbackRecord`); the refused candidate's hold and the restamp write only under it; a lock that cannot be held (another holder past the 30 s wait, ENOLCK, a failure, a filesystem that keeps none) refuses before any write or swap, naming no path (`ErrMarkerLockUnavailable`; the console's 409s `marker-locked` and `rollback-refused`), and a stop in its wait returns the stop** (2026-11-03, backlog B692). **Don't hold the lock through a download, take it twice on one path or take server.lock or the ladder under it, write the marker or swap on these paths without it (a filesystem that keeps none included), skip the decision under it, act on a context that ended before an uncontended take, roll back over a store a newer version opened or before the veto is recorded, drop the marker's other fields at a rollback, let serve's stop take the lock, or let `scmStopWait` pass half its wait.** `TestAnInstallThatHoldsTheMarkersLockFirstKeepsTheMigrationWaiting`, `TestAMigrationThatHoldsTheMarkersLockFirstIsReadByTheInstallsDecision`, `TestASecondProcesssInstallDecidesAgainUnderTheMarkersLock`, `TestAnAutomaticInstallDecidesAgainOnAHoldRecordedWhileItDownloaded`, `TestAnInstallRefusedForTheMarkersLockSwapsNothing`, `TestAnInstallWhoseSwapFailsPutsTheMarkerBackAsItReadIt`, `TestTheHoldOfARefusedCandidateIsRecordedOnlyUnderTheMarkersLock`, `TestTheRestampRefusedForTheMarkersLockWritesNothing`, `TestARollbackThatHoldsTheMarkersLockFirstVetoesTheMigration`, `TestAMigrationThatHoldsTheMarkersLockFirstRefusesTheRollback`, `TestARollbackIsRefusedOnceTheVersionItRollsBackFromOpenedTheDataDir`, `TestARollbackRecordsItsHoldAndVetoBeforeThePutBackAndKeepsTheRest`, `TestARollbackRefusedForTheMarkersLockWritesNothing`, `TestAManualRollbackThatHoldsTheMarkersLockFirstLeavesTheBootNothingToDo`, `TestABootThatHoldsTheMarkersLockFirstLeavesTheManualRollbackNothingToPutBack`, `TestServeStopsWhileAnotherProcessHoldsTheMarkersLock` and `TestAnSCMStopFitsInsideTheMarkersLockWait` pin it. → History-rules-config-lifecycle.md
- **…and an operator's rollback stays under way until its rename has landed, and a stop in the updater's lock waits is a stop: the rollback's record carries `RollbackStarted` where the marker names the install, and `Rollback` clears it only after the rename, so a rollback the process's end stops between the two is finished by the version that runs next, by B613's rule for a rollback's record: on the target still in place, its commands' migrations refuse under the veto and its serve admission records `DataDirOpenedBy` and lifts the veto in one write, after which every rollback refuses and its housekeeping confirms the install (`BootRollbackOvertaken`); on the previous version, the backup back, its housekeeping ends the record as a rollback done (`BootRollbackFinished`); a stop while the restamp waits or writes requests no restart, and one while a hold's record waits is the install's answer, the context's error** (2026-11-03, backlog B692's round 2). **Don't clear the under-way mark before the rename has landed, lift a veto at the target's admission without recording that it opened the data dir, request a restart after a stop, or answer a stop with a refusal.** `TestARollbackStoppedBeforeItsRenameStaysAnInstallTheTargetsStartResolves`, `TestARollbackStoppedAfterItsRenameIsFinishedByTheVersionThatRuns`, `TestARollbackClearsItsRecordOnceItsRenameHasLanded`, `TestAManualRollbackStoppedBeforeItsRenameKeepsTheStoreFromASecondRollback`, `TestAManualRollbackStoppedAfterItsRenameIsFinishedByThePreviousVersionsBoot`, `TestAStopWhileTheRestampWaitsRequestsNoRestart` (its write's stop since round 3) and `TestAStopWhileTheHoldWaitsIsTheInstallsAnswer` pin it. → History-rules-config-lifecycle.md
- **The version installed is confirmed at its first boot however late it comes, and no clear at boot drops the record that a version other than the backup's opened the data dir while `.bak` may stand: `DecideBootAction` asks the version before the window (any other version: the window, then `SwapStarted`, then `BootInstallFailed`); `ClearInstall` keeps `DataDirOpenedBy` and `PreviousVersion` where `RollbackRefusal` refuses on them; a removal of `.bak` that fails leaves the marker for the next boot, and one that lands drops the record** (2026-11-03, backlog B705). **Don't read the window before the version, drop the record in a clear that leaves `.bak`, clear the marker after a removal that failed, or keep the record once `.bak` is gone.** `TestALateFirstBootOfTheInstalledVersionConfirmsTheInstall`, `TestAClearAtBootKeepsTheRecordARollbackRefusesOn`, `TestDeferredRestartRefreshesInstallRecency`, `TestALateFirstStartOfTheInstalledVersionCompletesItsInstall`, `TestAFailedRemovalOfTheBackupLeavesTheInstallForTheNextStart`, `TestAStartOfAnotherVersionKeepsTheRecordOfALateStartThatDied`, `TestTheBootKeepsARecordedHoldWhereItClearsAnInstall` and `TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir` pin it. → History-rules-config-lifecycle.md
- **The console's Updates panel offers Roll back only where the operator's rollback would run, from one read of the install marker per status: `CanRollback` is `.bak` present and no refusal from `updater.RollbackRefusalOn`, which `Rollback` asks itself of its own read (`RollbackRefusal`'s, or a fixed sentence for a marker that cannot be read), for `Status` and for an install's answer alike; where `.bak` is there and the rollback would refuse, Roll back stays hidden and a Rollback row beside Held update says why (`RollbackRefusal`, JSON `rollbackRefusal`), in the rollback's words and naming no path, at the first paint and in `renderUpdateTile`; with no `.bak` nothing is said, and the API keeps its 409** (2026-11-04, backlog B707). **Don't offer Roll back from `.bak` alone, decide it by any check but `RollbackRefusalOn`, read the marker twice for one status, take a marker that cannot be read for none, show a reason where there is no backup, or put an error's text or a path in it.** `TestTheUpdatesPanelOffersRollBackOnlyWhereTheRollbackWouldRun`, `TestTheConsoleOffersRollBackOnlyWhereTheRollbackWouldRun`, `TestTheRollbacksRefusalOnItsReadOfTheMarkerIsTheConsoles`, `TestTheSettingsPageSaysWhyRollBackIsNotOffered`, `TestSettingsUpdateRowsRenderOnceAndKeepDTAdjacency`, `TestTheUpdatesPanelScriptShowsWhyRollBackIsNotOffered` and `TestTheUpdatesPanelShowsRollBackOrWhyNot` (under node) pin it. → History-rules-config-lifecycle.md
- **Booklet GC is skipped while a scan is in flight** — mid-rescan the release
  universe is transiently partial, so GC deletes every filesystem album's
  booklets and re-fetches them next cycle. An empty universe is a deliberate
  no-op everywhere it appears.
- **`PrunePlaylistCoversExcept` is deliberately unwired** — smart-mix retirement
  is reversible and playlist deletion is a revivable tombstone, so either
  trigger silently destroys operator-uploaded content to reclaim a JPEG. An
  AST-based guard fails if a production caller appears.

- **No refusal, warning or log line of the config path prints a value that can hold a secret or text pasted where it does not belong** **Don't print a password, a token, a serial, a URL's user name, password, query or fragment, a value a setting refused, an entry of a list this build does not know, or a key, in a refusal, a warning or a log line, a path the operator configured excepted (`LibraryRootError` and init's directory errors name the path the operator must find and fix); and don't print yaml.v3's error without `ValueFreeYAMLError`.** → History-rules-config-lifecycle.md
- **A whole-number setting takes no value yaml.v3 would change on the way in: a fraction, an infinity, NaN or a float outside its range is refused as a word on that line is** (2026-10-24, backlog B528). yaml.v3 decodes a `!!float` into an int by dropping what follows the point, and 42 of the config's 44 whole-number settings loaded `2.75` as 2 (`limits.manifest.requestsPerMinute: 0.5` as 0, the limit off). Load's decode is `config.DecodeFile`: the `KnownFields` decode, then `CheckWholeNumbers`, a second decode into a shadow of the type (`shadowOf`, each integer an `integerCheck`) that yaml.v3 maps itself, merge keys and aliases included, refusing in `ValueFreeYAMLError`'s words for a float yaml.v3 refuses itself (`line N: cannot unmarshal a !!float value into int`). A float the integer holds exactly (`30.0`, `3e1`) loads as written, weighed by the value its text writes, read exactly as yaml.v3 reads it (`writtenInteger`), never the float64 `strconv.ParseFloat` rounds it to (`30.0000000000000001` loaded as 30, `requestsPerMinute: 1e-400` as 0). The player block's reader refuses through the same `exactInteger` and leaves the block unread (`player.md`'s binding bullet); bridge init's rewrite decodes through `DecodeFile` and asks `CheckWholeNumbers` of the block and each setting it recovers. **B533 also refuses an otherwise accepted leading-zero spelling: after dropping underscores and an optional sign, zero followed by a decimal digit; keep existing decode and B528 refusals first, decoder order and multiplicity, and the player's unread-block contract. Use one predicate in the shadow and player reader, never reinterpret as decimal, and pass only the exact fixed diagnostic through `ValueFreeYAMLError`. Don't decode the config but through `DecodeFile`, find the setting a value reaches with a resolver of your own, or quote the value in the refusal.** `TestAWholeNumberSettingTakesNoFraction`, `TestAWholeNumberSettingTakesWhatDecodesUnchanged` and `TestAWholeNumberOfThePlayerTakesNoFraction` pin B528; `TestLeadingZeroSettingsRefused`, `TestLeadingZeroDecoderReach`, `TestLeadingZeroBoundary`, `TestLeadingZeroAcceptedSpellings`, `TestLeadingZeroShadowKinds`, `TestLeadingZeroValueFreeDiagnostic`, `TestLeadingZeroPlayerBlock`, `TestLeadingZeroPlayerBoundary` and `TestLeadingZeroInitRecovery` pin B533. → History-rules-config-lifecycle.md
- **The one-line installer, `scripts/install.sh`, installs what `bridge init` installs and upgrades through `bridge update`** (2026-10-30, backlog B475; rounds 2 to 4, 2026-10-31): a release asset, stamped by `scripts/stamp-install.sh` in a before-hook, so every download is from its own release's tag (or `--version`'s, a new install only), never `latest`, by curl alone, held to HTTPS through every redirect (no wget: GNU's followed a redirect to plain HTTP) and to a speed floor, never a time cap (`--speed-limit 1024 --speed-time 60`: a transfer under 1 KiB/s for a minute is taken for stalled and ended, exit 28, which `--retry 3` retries, asking for the whole file again; one that keeps above the floor has no cap on its total time, no `--max-time`, since a slow but moving download, a Pi on Wi-Fi, must finish), the archive checked against its one exact `checksums.txt` line before anything is installed; the program in `~/.local/bin/bridge`, owned by the service's user, since the updater replaces it in place as that user; `bridge init` run as that user, root refused, and root asked for (sudo, else su, after saying why) only for `loginctl enable-linger`; the user's systemd manager asked what it has before anything changes, its bus found read-only, two ways, since they can differ: the unit as it has it loaded (`systemctl --user show`), which a restart runs, and its files on disk (`systemctl --user cat`, which reads the files, not what the manager loaded), a manager that cannot answer either refused; a new install only on a box with nothing of the bridge on it (no unit the manager has, no file left for one in any directory it reads units from, `systemd-analyze --user unit-paths`, since systemd reads no drop-in for a unit it has not found; no settings at the default path; nothing at `~/.local/bin/bridge`); an install only the per-user unit as init writes it and as the manager has it loaded (loaded, from the unit file in HOME, no daemon-reload pending, no environment of its own, named by property and file, never value; one ExecStart in its files), upgraded by its own program's `bridge update --yes --config`, which keeps the paired-device check, the `.bak` and the rollback marker, and the served version read back from `bridge status`, a release it could not read ending the run incomplete (exit 4); a BRIDGE_ variable this shell exports or the manager gives every service refused by its name, since the bridge takes one over bridge.yaml, `--config` or not; any other layout, and `--no-init` on an install, refused with nothing changed (exit 3); MPD detection left at `auto` and no output bound; the console loopback-only, pairing through an SSH tunnel with the console's own port or `bridge pair`; every printed command on a line of its own, each word quoted (`command_line`, `bridge_line`), with `--config` on pair, status, serve and update, the default settings file's too, since a bridge command without one reads `./bridge.yaml` first where it is run; the final line saying what lingering is; no music folder chosen without the user, and with no terminal and no `--library` the run refused before the download; the runs that leave the program without a service printing `bridge init`, never a second run; nothing reads the script's standard input; the last line one compound command, so a download cut short runs nothing. **Don't download over an install, or with anything but curl held to HTTPS and to a speed floor, with no cap on a moving download's total time; don't replace anything at `~/.local/bin/bridge` (a new install is for a box with nothing of the bridge on it, files left for its unit included); don't take an install the service manager runs from another file, with another ExecStart or with an environment of its own, take its files for what the manager has loaded (a daemon-reload pending is refused), or run where this shell or the manager has a BRIDGE_ variable; don't run `--no-init` on an install; don't install the program where its service's user cannot write, run init through sudo, ask GitHub for `latest`, choose a folder, enable or bind an output, or delete anything for `--uninstall`, which prints the steps; don't print a command unquoted, or without `--config`, the default settings file's included; don't report a release running that `bridge status` did not read, or a start at boot that lingering does not give; don't end the script with a bare `main "$@"`, whose prefix `main` runs; don't name a function after a word a shell keeps for itself (ksh93's alias `stop` ran `kill -s STOP` in place of the installer's `stop`).** `scripts/install-test` (`make installer-check`, CI's `installer` job) pins it, `TestATruncatedDownloadRunsNothing`, `TestAnInstalledBridgeIsUpgradedThroughBridgeUpdate`, `TestAnInstallWhoseServiceHasAnotherEnvironmentIsRefused`, `TestTheServiceManagerIsAskedWhatItRuns`, `TestAProgramAtTheTargetIsNeverReplaced`, `TestARunAfterOneThatDidNotFinish`, `TestThePrintedCommandsRunAsPrinted`, `TestAStalledDownloadIsAskedForAgain`, `TestASlowDownloadAboveTheFloorFinishes`, `TestFetchAsksCurlForTheShippedBounds` and `TestNoInstallerFunctionIsNamedAfterAShellWord` among its tests; `TestTheInstallerCallsTheBridgeWithFlagsItDefines`, `TestTheInstallerPrintsTheBridgesCommandsQuoted`, `TestTheInstallersFakeBridgeTakesOnlyRealFlags`, `TestTheInstallerReadsWhatBridgeVersionAndStatusPrint`, `TestTheInstallerMatchesWhatBridgeUpdatePrints` and `TestTheInstallerReadsTheUnitInitWrites` hold it to the real CLI and unit. → History-rules-config-lifecycle.md
- **…and the installer says what lets the bridge's own player open the DAC, and runs none of it** (2026-11-03, backlog B676): on a box with an ALSA playback node (`/dev/snd/pcmC*D*p`) whose group (`ls -ld`'s) neither every user nor the account (`id -nG USER`, never this process's groups) is given, on systemd a new install, an upgrade and `--no-init` print `usermod -aG <group> <user>` and `systemctl restart user@<uid>.service   # or reboot`, through sudo where the box has it, each word quoted (`root_line`, `command_line`), and a line on what the restart costs; an account in the group whose login is not gets the restart alone; without systemd, deferred set-up and `--no-init` get a new login instead of a manager restart; `--no-init` prints `init --no-service` and no lingering advice there, even with elogind; an unresolved user name (failed, empty or numeric `id -un`) withholds usermod and asks to add the account to the group by name; a node every user may open, root's group, no node, and `--uninstall` get nothing. **Don't run usermod or the restart, write a user or a group, print a placeholder or a `$USER` a root shell would expand, judge the account by this process's groups, or advise on a box with no playback node.** `TestTheInstallerSaysWhatLetsThePlayerOpenTheDAC` pins it. → History-rules-config-lifecycle.md
