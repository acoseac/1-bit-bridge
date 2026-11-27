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

### Round 8: the notes made short

The operator chose short notes on 2026-11-27. The notes now have three parts:
"What's new in 0.2.4", one plain sentence per change with its backlog ids, 34
lines, the dependency line and the B662 line as `0a3717f1` left them; "Before
you upgrade or roll back", only what keeps an operator from a wrong step, each
item read against `29df3a4a`, or `v0.2.3` for what an earlier version does;
and one closing line, to the documentation and this log. The example config,
the five stubs and the code are as round 7 left them.

Moved into "Before you upgrade or roll back" from round 7's long lines:

- under Warnings, the test tone's level (B601), the console on the home
  network, with public mode's refusal of the address (B989), and Delete's
  reach across editions (B1023, B1036);
- under Snapshots and restore, `--verify` on a snapshot from before 0.2.4 and
  outside the backups folder, restore's lock and the storage it refuses
  (B653, B826), and what a restore replaces;
- under "Also before you update", `bridge backup`'s exit status and its
  `--keep 0` before the update (B653, B1035), storage with file locks (B653),
  the data directory's identity files (B727, B741), the IDNA forms at load,
  a custom endpoint's drop included (B744), `bridge update`'s reading of the
  update switch (B761), an upload into a root with nothing indexed yet
  (B776), an upload another tab lists as interrupted (B797) and a tab left
  open (B1029, B1037);
- under the Tailscale item, the container's recreate and the new process a
  change of the uploads key takes, the menu's Start now among what does not
  give one (B749);
- the doctor's bound, which starts once `bridge.yaml` is read (B650).

The operator's decisions of `0a3717f1`: the preamble's sentence, the
dependency line and the B662 line stand as they were, and the Hugo 2 line
stays out. The API line keeps its heading's claim (the bridge publishes the
bound DAC's choice, and an API changes it), the protocol's version and "No
released app uses the change route yet"; its field names, its feature flag,
its modes, its refusal's words, the fields no longer sent and "while playback
is stopped" leave it, the last for section 2. The line on the other new keys
becomes the five keys named on the way back, with "check that none is left";
its sentence on what a switch turned back on removes leaves the notes.

Corrections to round 7's text, each read against the code:

- A restore replaces the files its snapshot captured and no other, and a
  snapshot taken before the first pairing holds no token store, so a device
  paired after it stays paired (`v0.2.3`'s `internal/backup/backup.go`, 140
  to 152, 252 to 262 and 514; at `29df3a4a` the same loop, 784 to 796).
- No sentence says that an automatic rollback is not made: the notes name the
  console's Roll back and the crash rollback, which 0.2.4's first start rules
  out (`RollbackRefusal`, `CrashRollback`).
- The DoP case under `auto` is what `auto` can do, its check worded "passes
  the DoP gain check", a check of every level control on the card but the
  profile's analogue ones (`internal/player/policy/gain.go`, 250 to 270), and
  claims no DoP confirmation from a record that approved no DoP rate
  (`internal/player/dsdstate/migrate.go`, 281 to 285) and none that outranks
  the catalog's refusal of a rate (`internal/player/policy/policy.go`, 548 to
  559).
- The operator sets a choice only through the PUT, and the migration writes
  one too: the two sentences no longer contradict each other.
- A state file that does not read has two ways out: stop serve, move the file
  aside and start serve, so that no write of the player's DSD state comes
  between the move and the start; or the PUT with playback stopped, the file
  moved aside first where the log says it stays where it is (serve's four
  lines, `cmd/bridge/serve_player_dsd.go`, 234 to 251; the copy's name,
  `internal/player/dsdstate/store.go`, 229 to 234).
- After either way back, a later 0.2.4 goes on from the state the first one
  left, and migrates the keys only where no migration's mark is there
  (`playerDSD.steps`, `serve_player_dsd.go`, 357 to 366).
- The five keys are named, with "check that none is left", which needs no
  account of which keys a console switch removes.
- A `false` written into Compose's `.env` takes `docker compose up -d` as a
  `true` does, since `restart` keeps the old environment.
- 0.2.3 serves the WavPack and Monkey's Audio rows to every app, with no
  "until its scans drop them".
- The probation line gives no count of starts; the empty Tailscale state is
  the empty file alone; a newly made certificate carries the IDNA form; an
  upload into a root with nothing indexed is not refused for its volume,
  where round 7 said it takes the write; and the cases of a Settings save's
  refusal, the update switch's reach over an install under way, the rows
  sent again and the node's name over a state that does not read leave the
  notes with the detail they sat in.

One proposed correction did not hold against the code: a record refused as
another model's gives the bound DAC its mode with nothing approved and keeps
the record waiting for its own model (`migrate.go`, 204 to 216), as the notes
and the example both say.

Left for a later round, since this round's budget keeps the example as round
7 left it, in `config/bridge.yaml.example`: "only PUT /v1/outputs/{id}/dsd
sets" (77 to 79) beside the migration's choice (104 to 106); a DoP
confirmation from any record taken (106 to 109), where one needs an approved
DoP rate; a DoP rate "confirmed on the DAC's firmware" (117 to 120), which
does not outrank the catalog's refusal; "an update's automatic rollback to
one is not made" and its twins (5, 207, 264 and 286); "one a BRIDGE_ variable
overrides, stays until it is deleted" (289 to 290), where a key whose
override holds the switch off goes when the switch is turned on
(`filedSwitch`, `internal/config/runtime.go`, 466 to 475); and "or turn the
checks on in the console" (323 to 324), which removes only a key that turns
them off. The example is one of this change's files, which round 7's
"Outside this change's files" says otherwise of its `update` block.

### Runs (round 8)

On go1.27.3 with `GOPROXY=off`: `make docs-check` clean, and the citation
guard (`TestEveryCitedTestNameExists`) passed. No code, test or config value
changed.

### Round 9: wording fixes

Twenty-three fixes, each read against the code again as it was written, 4, 5,
7, 10, 14, 19 and 20 in a corrected wording; all are in the notes but 3, which
is in the example.

1. The player's line: under its default `auto`; DSD512, mono or multichannel
   DSD and DST-compressed DFF refused; a converted rate not bit-exact
   (`internal/player/playback/dsd_policy.go` 328 to 333,
   `internal/player/decode/dff.go` 124 and 233, `internal/player/dsdconv/doc.go`
   5).
2. To keep DSD off: check the log for "the player starts without it"
   (`cmd/bridge/serve_player_dsd.go` 310 and 317).
3. The example's player paragraph, which the notes point to, at its three
   places: the migration writes a choice too; a DoP confirmation only where
   the record approved a DoP rate (`internal/player/dsdstate/migrate.go` 281
   to 285); and a DoP rate the profile database refuses is neither heard nor
   confirmed unless a listener answered clean there
   (`internal/player/policy/policy.go` 548 to 559).
4. A restart takes a fresh install's defaults, and migrates the legacy keys
   again, only while `player-dsd.json` is absent (`dsdstate.Open`,
   `playerDSD.steps`).
5. The probation line: the crash rollback to 0.2.3 is disabled once 0.2.4
   begins the database migration (`updater.CrashRollback`).
6. A Settings save keeps the changes it finds, which apply at the next start.
7. 0.2.3 drops the WavPack and Monkey's Audio rows after
   `scanner.deleteAfterMissingScans` scans that count them missing, 3 by
   default (`internal/config/config.go` at `v0.2.3`, 866 to 885 and 2755), and
   a root its scanner protects as empty keeps them: a subfolder its walk
   enters is library content and a `.wv` or `.ape` file is not
   (`internal/manifest/scanner.go` at `v0.2.3`, 1061, 3449, 3508 and 3958 to
   3966). The next sentence names 0.2.3 in place of its pronoun.
8. The snapshot path is for a snapshot that holds `bridge.db`.
9. Retention: once a newer snapshot's database has verified
   (`keptByRecovery`).
10. A restore brings back a captured console account with its sessions
    signed out (`adminauth.SignedOutFile`, at `29df3a4a` and `v0.2.3` alike)
    and the captured DAC choices (`dsdstate.ForRestore`), and a token in the
    snapshot that was revoked since works again.
11. A `compose.yaml` kept from 0.2.3 has no line for the uploads variable
    (`compose.yaml` 62 at `29df3a4a`, none at `v0.2.3`).
12. A stop that gave up on the node's start takes a new process too, and the
    menu's Start now and the start its Setup offers run in the menu's own
    process (`internal/tsnet/uploads.go` 100 to 131; `cmd/bridge/menu.go` 361
    to 364 and 407 to 414).
13. `TS_NETSTACK_ENABLE_RACK=true` brings back the stalls after a burst loss
    that 1.104.5 turned RACK off for (`wgengine/netstack/netstack.go` in
    `tailscale.com@v1.104.5`, 146 to 152 and 412 to 417).
14. A refused data directory: check the volume is mounted at `dataDir`, then
    follow the refusal's words, which name the record to remove only for a
    data directory removed on purpose (`cmd/bridge/serve_data_dir_record.go`
    394 to 406 and 476 to 500).
15. B903AUD's line: while a playable entry follows the track heard, the word
    "playable" added, since entries already refused leave the selection
    where it was (`player.md`'s B903AUD bullet).
16. The console shows each snapshot's verdict, `bridge doctor` the newest's
    and the newest verified's (`internal/doctor/backups.go` 15 to 31,
    `internal/admin/handlers_backups.go` 66 to 77).
17. The layout warning goes by the path as written (`config.LayoutOf`).
18. A library rename no longer renames a registered node whose state reads.
19. The format gate filters the catalog's rows; `/v1/list` and the other
    filesystem views are not gated (`internal/api/formats.go` 3 to 11).
20. The Christmas floor counts the tracks the request sees (PROTOCOL.md,
    "Client formats").
21. `adminLANAddress` opens the console once its listener opens.
22. Delete leaves an SACD image's and an upstream server's tracks alone, and
    its confirmation names each edition
    (`internal/admin/static/player/views.js` 420 to 455).
23. The PUT's request, with `dacToken`, is in PROTOCOL.md.

Nothing changed for a host name that isn't ASCII on an existing certificate:
that claim did not hold.

### Runs (round 9)

On go1.27.3 with `GOPROXY=off`: `make docs-check` clean; the example's tests
(`TestLoadShippedExample`, `TestTheShippedExampleLeavesThePlayersDSDToItsState`,
`TestTheConfigExampleSaysWhatTheKeyCostsTheRollback`) and the citation guard
(`TestEveryCitedTestNameExists`) passed. No code or test changed; the example
changed in its comments alone.

### Round 10: the DSD section at main `98efe691`

Main `98efe691` (#1579, B1042, B1043 and B1045's fix, the squash of one
parent, `29df3a4a`) touches none of this change's files. Every sentence below
was read against it, `git show 98efe691:<file>`, and against `v0.2.3` where it
says what 0.2.3 does. The merge of main into this branch was not made in
this round: the session could not run `git merge`.

1. Section 2 opens with the environment's rule. A `BRIDGE_*` variable set to
   a value the running version accepts overrides its key at every start, at
   main (`internal/config/config.go` 3211, `env.go` 252) and at `v0.2.3`
   (`config.go` 3027, `env.go` 235). Both skip a variable whose value is
   empty before its kind is read. A value of spaces alone is not skipped: it
   clears a list, sets a string to the spaces and, at main, reads as off for
   an opt-out, so the notes say "empty" alone. 0.2.3 reads a boolean by
   `strconv.ParseBool` and ignores what it refuses, so
   `BRIDGE_UPDATE_AUTO_INSTALL=off` leaves the file's value (`v0.2.3`
   `env.go` 277 to 283). The Settings save's line (`runtime.go` 496 to 523,
   `outside.go` 58) and the way back's `update.autoInstall` sentence point
   to it; its pointer in the snapshot path follows.
2. "DSD after an upgrade", three paragraphs and the recovery paragraph.
   - The opt-out's check, by `playerDSD.say` (`cmd/bridge/serve_player_dsd.go`
     at main, 305 to 373). Three lines hold the opt-out with `defaultMode=off`:
     the Info line of a step saved, the Warn lines of a write that failed
     (the 1 MiB one among them) and of one that landed not durable. "The
     player starts without it" is a migration refused before its write.
     `steps` (383 to 394) migrates again only with no marker and a legacy key
     set, and a later write of the run can save a held step (the player
     rules' B1042 line).
   - What 0.2.4 keeps: B1043's rule (`internal/player/policy/policy.go` 642,
     654, 708 to 714), B1045's (565; `choice.go` 172 and 181), and an explicit
     `native` or `dop` with nothing approved, which never converts and
     refuses a rate needing a listener's answer or a confirmation (659 to
     688).
   - The outcome check: `internal/api/outputs_bridge.go` 53 and 416,
     `cmd/bridge/serve_player_output.go` 174, `serve_player_console.go` 558
     to 569, `internal/api/outputs_dsd_choice.go` 54, 217 and 262,
     `internal/player/playback/dsd_choice.go` 601, and `waitingFor`
     (`serve_player_dsd.go` 372 at main). Only the PUT's route calls
     `ChooseDSD` (`outputs_dsd_choice.go` 210, `serve_player_output_dsd.go`
     77); the migration (`dsdstate.Migrate`) and a restore
     (`dsdstate.WriteRestored`) write choices too.
   - The recovery paragraph drops "the default `auto`".
3. The example's player paragraph: the choice's writers; the record refused
   as another model's, kept for its model and taken when a DAC it reads for
   is bound, unless a choice of its own is there (`migrate.go` at main, 224
   to 257; `record.go` 93 to 108); the "As they behave today" sentences
   replaced by B1042's, B1043's and B1045's behaviour; and the `auto` rule,
   known and unknown DACs apart (`policy.go` 541 to 589 and 625 to 658).
4. The narrow fixes: the backups' verdicts (`internal/doctor/backups.go` 98
   and 137, `internal/backup/status.go` 43 to 46); the data directory's
   record, which protects once written (`cmd/bridge/serve_data_dir_record.go`
   577 and 761 to 764); a later 0.2.4 over the file the first one left
   (`dsdstate.Open`, `Defaults`, `Refusing`, `steps`; a marker can be
   `keys`, `state_unreadable` or `schema_1`); the player's line's backlog
   ids; the restore's `player-dsd.json`; and the candidate: `98efe691`, 151
   merges since `v0.2.3`, the one tag in that range, so `git describe` reads
   `v0.2.3-151-g98efe691`.

### Runs (round 10)

On go1.27.3 with `GOPROXY=off`, on this branch at `a9fd3114` with this
round's edits: `make docs-check` clean; the example's tests
(`TestLoadShippedExample`, `TestTheShippedExampleLeavesThePlayersDSDToItsState`,
`TestTheConfigExampleSaysWhatTheKeyCostsTheRollback`) and the citation guard
(`TestEveryCitedTestNameExists`) passed. No code or test changed.
