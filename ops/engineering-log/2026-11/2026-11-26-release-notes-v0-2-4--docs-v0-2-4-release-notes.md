## 2026-11-26 — 0.2.4: the release notes, the example config's rollback warnings, and 1-bit.swiss in the docs

Branch `docs/v0.2.4-release-notes`, from main `29df3a4a` (B1036, #1577), the
release candidate, `v0.2.3-150-g29df3a4a`. Documentation and comments only: no
code, test, protocol or wire change.

### The release notes

`ops/release-notes-v0.2.4-draft.md` is the working copy the operator turns into
the release's notes (`ops/release-playbook.md`, section 4 step 5). It held three
lines; it now covers the 150 merges from v0.2.3 to `29df3a4a`, in five sections
and a sixth of upgrade notes, and keeps those three lines word for word.

- The lines for the merges through `a23aab63`, each read against its merge's
  engineering-log record and the code.
- The six merges after it: B1021 PR 3a (a list keeps the reader's place, and a
  name filter narrows it) and PR 3b (an index rail for every album sort, and a
  rail a finger drags), B1028 and B1029 (the Player page's DSD words and its
  transports table), B1035 (`bridge backup`'s retention) and B1036. B1036
  corrects the B1023 line, which said Delete acts on the edition shown: Delete
  still moves every edition's files, and its confirmation names them.
- Two items of the release review (backlog B1037): there is no automatic or
  console rollback from 0.2.4 to 0.2.3, and a console tab left open across the
  update needs a reload.
- The DSD carry-over line said the legacy DSD keys are read for the migration
  only. It now says they reach the player's DSD state only through it, and that
  serve warns of each one set, as B1028's record and the example say.
- The two lines on going back to an earlier version move to the upgrade notes,
  unchanged but for one cross-reference, and the B660 line points there.
- Five lines were marked (check) for the operator's decision, which `0a3717f1`
  applied.

### The upgrade notes, against the code

- The console's Roll back: `updater.RollbackRefusal` (`internal/updater/state.go`)
  refuses once a version other than the backup's has opened the data dir
  (`DataDirOpenedBy`), which serve's admission records at 0.2.4's first start.
- The crash rollback: `updater.CrashRollback` (`internal/updater/probation.go`)
  refuses once a migration of the store began (`StoreMigratedBy`). 0.2.4's
  ladder ends at 62 and 0.2.3's at 60.
- 0.2.3 opens the migrated store: its `migrate` (`internal/manifest/store.go` at
  v0.2.3) skips every step at or below the store's `user_version` and refuses no
  version above its own last step.
- The updater's `.bak` is removed at the start after the install's confirmation
  (the confirmation's line in `cmd/bridge/serve_probation.go`).
- 0.2.3's auto-installer holds back only a version its install marker records
  as rejected (`HeldBack`, `internal/updater/configcheck.go` at v0.2.3), which a
  binary swapped back by hand records nowhere, so with `update.autoInstall` on
  it can install 0.2.4 again.
- The open tab: 0.2.3's `renderPlayerOutputTransports` reads `t.approved.length`
  (`internal/admin/static/app.js` at v0.2.3), and B1029 renamed that field
  `rates` (`PlayerTransport`, `internal/admin/handlers_player_output.go`). The
  error lands in `loadPlayerOutput`'s catch, which says "Couldn't read the
  player" at each refresh.

### The example config

`config/bridge.yaml.example` warned about going back for `adminLANAddress` and
`update.checkEnabled` alone, and had no `tailscale` block. Its header now names
the five keys 0.2.4 adds that 0.2.3 refuses (`adminLANAddress`,
`enrich.enabled`, `enrich.deezerEnabled`, `update.checkEnabled`,
`tailscale.uploadDiagnosticLogs`), the enrich block says it of `enabled` and
`deezerEnabled`, and a new `tailscale` block describes `uploadDiagnosticLogs`
with its own warning. The five are the keys whose YAML tags v0.2.3's config
package lacks, by a diff of both trees' tags (`enrich.enabled` by
`EnrichConfig`'s fields, since another block has an `enabled`), and v0.2.3's
loader decodes with `KnownFields(true)`. An enrichment switch turned back on in
the console clears its key (`enrichSwitchValue`). The two existing warnings are
unchanged; `TestTheConfigExampleSaysWhatTheKeyCostsTheRollback` pins one.

### 1-bit.swiss

The project's website and support mail moved from 1-bit.app to 1-bit.swiss,
and 1-bit.app redirects every path. The website's links and the support
address now name 1-bit.swiss in `README.md`, `SECURITY.md`, the five redirect
stubs under `docs/`, `docs/release-process.md` and the installer comment in
`.goreleaser.yaml`, which has no homepage field. Each new URL was fetched first:
the five pages answer 200, `/install.sh` redirects to the latest release's
asset, and the features page holds the `public-mode` anchor `README.md` links
to.
Each stub also carries a `<noscript>` meta refresh to the same page, so a
client that follows the `<noscript>` refresh reaches the same page, without the
original `#` fragment.

Kept as they are: the infrastructure hosts under 1-bit.app (the demo bridge,
the cloud tenants, Atlas); dated history (the engineering log,
`ops/History-*.md`, the older release notes, and the v0.2.0 gate in
`docs/release-process.md`); code and its tests (the DLNA and OpenHome
manufacturer URL, `scripts/install.sh`'s usage text); and the agents'
instruction files and the operations files under `ops/`, outside this
change's list, which the redirect keeps working.

### Runs

On go1.27.3 with `GOPROXY=off`: `make docs-check` clean; the example's tests
(`TestLoadShippedExample`, `TestTheShippedExampleLeavesThePlayersDSDToItsState`,
`TestTheConfigExampleSaysWhatTheKeyCostsTheRollback`), the tests that read
`README.md`'s text (`TestTheReadmeNamesEveryPathSettingLoadResolves`,
`TestTheConsolesSavesSayAnOffThatGotNoAnswerMayBeLost`,
`TestEveryTextSaysTheWindowsServiceRefusesAnOffBehindAnotherProcesssSwap`) and
the citation guard (`TestEveryCitedTestNameExists`) passed.

### Round 7: every behavioural sentence against the code

Each sentence of the notes that says what the bridge does, the example's
comments this PR changed, its player DSD paragraph, its console-on-the-LAN and
Tailscale uploads comments, and the five stubs were read against `29df3a4a`,
and against `v0.2.3` where a sentence says what an earlier version does. The
round's findings are F1 to F20; its report holds the claim-to-code table.

- The notes' two lines on DSD after an upgrade are one block: the default
  mode; a DAC's own choice, which in 0.2.4 only `PUT /v1/outputs/{id}/dsd`
  sets, kept per model and maker (`dsdstate.BoundOf`, `State.ChoiceFor`, F5);
  what carries over and when (`internal/player/dsdstate/migrate.go`); two
  carry-overs as they behave today (F3: `cmd/bridge/serve_player.go` 516 and
  `serve_player_dsd.go`'s `say`; F4: `internal/player/policy/policy.go`'s
  `decide`, 619 to 687); and a state file that does not read, its two cases
  told apart by serve's log line (`openPlayerDSD`) and each given its way out
  (F1: `internal/player/dsdstate/store.go` 155 to 226, and `updateLocked`, 311
  to 357, where a file moved aside lets the next write land with no restart).
  The conversion paragraph follows the block, with its scope (F2:
  `internal/player/playback/dsd_policy.go` 327 to 336, `decode/dsd.go` 37,
  `decode/dff.go` 125 and 234).
- The example's player paragraph says how a choice is set (F5) and holds the
  key-by-key cases the notes point to, F3 and F4 among them.
- At `v0.2.3`, for the way back by the binary alone (F10): no package reads
  `player-dsd.json` or `data-dir-id.json`; `readDataDirRecord` decodes the
  record beside the config into its two fields with `json.Unmarshal` and
  `recordServeDataDir` writes it only where those two differ, so the identity
  0.2.4 added stays; `updater.LoadState` decodes the install marker with
  `json.Unmarshal`, and `bootHousekeepingWith`'s `BootInstallFailed` arm,
  seeing `DataDirOpenedBy`, puts no backup back and clears the install, so
  0.2.3 starts; its ladder ends at 60, and steps 61 (a column with a default)
  and 62 (a table of its own) leave its writes as they were; its scanner's
  `Ext` has no `.wv` or `.ape` and it has no format gate, so it serves the
  rows 0.2.4 indexed for them to every app until its deletion pass reaps
  them; and `EffectiveDSDMode` reads an unset `dsdMode` as off. 0.2.3's
  restore loads the live config strictly and opens the restored store with
  its own ladder, where 0.2.4's opens it to mint the epoch and migrates it
  (`mintRestoredEpoch`, `restoreMigrationHeld`); and retention keeps a
  snapshot from before 0.2.4 past `keep` only while no snapshot's database is
  verified (`keptByRecovery`).

### Not in the notes

Each checked against SECURITY.md's private-item rule: none is a vulnerability.

- The dependency line, the operator's, leaves out x/sync 0.24.0 to 0.25.0,
  x/sys 0.49.0 to 0.50.0, x/text 0.43.0 to 0.44.0 and modernc.org/libc 1.79.0
  to 1.80.1 (`git diff v0.2.3 29df3a4a -- go.mod`).
- `bridge.dsdMode` and `bridge.allowDoP` on `GET /v1/outputs` carry the bound
  DAC's choice (`off` and false while none is published), where 0.2.3 sent
  the player's settings; PROTOCOL.md says so.
- The early rollback, for a 0.2.4 that refuses its own settings at its first
  start, asks the backup's version and not its settings
  (`recoverRefusedInstall`), so a key 0.2.3 refuses that was written into
  `bridge.yaml` after the install's settings check would put 0.2.3 back over
  a file it refuses. "An update's automatic rollback ... is not made" holds
  for the crash rollback, which asks the backup's settings; after 0.2.4's
  first start its migration rules that rollback out anyway.
- A writer that takes no lock (a hand edit) and lands between a console
  save's last check and its rename can still be written over: the window the
  bullet on `bridge.yaml`'s lock in `.claude/rules/config-lifecycle.md` names.
- The Player page's Rates column lists the player's DSD set while the output
  is withheld or the DAC gone, where the row's Plays says no.
- `bridge restore` reads the manifest again under the snapshot folder's lock,
  after its prompt, with no retry; only its read before the prompt retries.
- After an unreadable state file is moved aside, a write the player makes
  before the choice (a probe's evidence, where one is owed) lands first and
  leaves the refusing state in the file, which a restart reads; the notes
  word the restart by whether a new `player-dsd.json` is there.
- A start of a new version that fails before its admission is not counted
  toward the crash rollback.
- The doctors check where `bridge.yaml` and the data directory lie only for a
  config that loads.
- An empty Tailscale state is a file empty or holding JSON's white space
  alone; a file of NUL bytes is not that case.

### Outside this change's files

Text that says otherwise than the code, in files this change does not touch:
`Migrate`'s doc in `internal/player/dsdstate/migrate.go` (with no record,
"each refuses until the app acknowledges", which B990 made gate nothing);
`formatgate.Gated`'s doc (the bridge "decodes nothing of" a gated file, while
the fingerprint sweeper hands one to fpcalc); `openhome.md`'s ProtocolInfo
bullet (FLAC, WAV, AIFF and ALAC, where the code adds DSF and DFF while the
player plays DSD); the Player page's hint in
`internal/admin/templates/output.html` (a choice "which the 1-bit app
changes"); and the example's `update` block ("turn the checks on in the
console", which removes only a key that turns them off).

### Runs (round 7)

On go1.27.3 with `GOPROXY=off`: `make docs-check` clean; the example's tests
(`TestLoadShippedExample`, `TestTheShippedExampleLeavesThePlayersDSDToItsState`,
`TestTheConfigExampleSaysWhatTheKeyCostsTheRollback`) and the citation guard
(`TestEveryCitedTestNameExists`) passed. No code, test or config value changed.
