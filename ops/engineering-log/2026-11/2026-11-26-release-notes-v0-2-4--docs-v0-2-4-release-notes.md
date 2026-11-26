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
- Five lines stay marked (check) for the operator's decision.

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
browser without JavaScript or a crawler that runs none is still sent on.

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
