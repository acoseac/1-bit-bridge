## 2026-11-22 — B989: the console on the home network, an explicit opt-in with no login

Branch `feat/b989-lan-console`, from main at `ab898b41` (#1552's merge), with
`design/b989-lan-console` at `b809a246` merged first (`6cebcd59`), so the design,
`ops/plan-b989-lan-console-design.md`, ships with the code. What it builds is that plan's; where
the implementation's brief added to it (I1 to I13 below), the brief's word decided. The rules
are `.claude/rules/auth-tls.md`'s B989 bullet and `admin-console.md`'s; CLAUDE.md's line on the
loopback console and its long form in `ops/History-Cross-Repo.md` name the posture.

Every run below used `GOTOOLCHAIN=go1.27.3` and `GOPROXY=off`, on Linux amd64 (16 CPUs,
kernel 7.0), with `TMPDIR` below the checkout's `.tmp/`; every multi-package run went through
the shared build slot.

### The commits

- `b84a628c`: the tests first, with the names-only shim of the plan's 9.2.
- `24fe27e1`: the change.
- `4a401ecc`, `5e4bab0a`, `c7e95c97`: tests made able to fail for their own reasons, each
  found by a negative control (below).
- `1140d3f4`: the routed client's CI job, `console-lan-routed`, and its site in the build-site
  census.
- `7a5a3e71`: this record, with the rules, the docs and PROTOCOL.md's sentence.
- `39ff5c25`: the source guards' findings in the new tests (two import keepers, a doc comment
  on the wrong declaration).
- `6f38a0cd`: the changed-config check's structural test through init's save seam (the gate's
  red, below).
- the runs, in this section's last part.

### Red first

At the tests-first commit (`b84a628c`'s tree, each package's tests run with `-v`), 71 of the
plan's tests failed, 10 passed and 1 skipped; the macOS and Windows hold tests do not build on
Linux. The 10 that passed are the plan's 9.4 baseline controls (`TestALANAddressTakesEachPrivateClass`,
`TestNoLANAddressWritesNoKey`, `TestTheLANAddressIsSetByItsVariable`,
`TestThePairingCopyFallsBackWithoutTheClipboard`, `TestALANSocketThatDoesNotBindIsNoFailedStart`,
`TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback`,
`TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket`,
`TestOnlyTheBindAndItsChecksReadTheLANAddress`, `TestTheProductionSeamsAreConsolelansAndLans`)
and the composition test, whose snapshot reader the shim gave its new signature. The routed
client's test skipped (no root, no `BRIDGE_QUALIFY_ROUTED`). Each behavioural red of 9.3 failed
on the assertion its row names, read from the logs: the shim's check took every refused value,
`ConsoleLANClass` answered false, `LANKeyOf` absent; the handler tests' LAN rows were refused
by main's source check; the state tests read the console's state as off, where the plan names
"no state"; serve bound nothing for the LAN; the doctor printed no line.

Three tests were reworked before that commit so that main's answer was the plan's reason and
not a name the shim supplied: the Host and spelling tables ask `/api/stats`, which main serves,
so their loopback columns are the plan's green controls at main (0 failures there); the Origin
table reads the boundary's three refusal shapes (the Host's 421, the source's `admin refused:`
403, the Origin's `cross_origin`), so at main its LAN columns fail on their passed rows alone
(36), and the refused rows are green; the hold, reading and no-network tests read the state the
console reports, which needs no plan to have opened. The Host table's LAN column fails at main
on its refused rows as well as its accepted ones (53 and 12): main refuses every LAN source with
its 403 before any Host check, where the socket answers a foreign Host 421.

After the change every test of the plan passed; the routed client's skipped, and the upload's
and the note's node halves skipped, node not being installed on this host.

### The controls

80 mutations, each of a copy (`go test -overlay`), or, for a test that reads its file itself
(a source guard, `app.js`, the config example), of the tracked file with its bytes put back and
checked by SHA-256 after the run; each turned its named tests red, and the tree read clean after
every batch. They are the plan's 10.3 one by one (with 9, 19, 25, 29, 31, 32, 34, 37 and 38
taken apart into their halves, and 34 one part of the policy at a time, its structural half by a
part no case names), its 9.4 baseline controls (a block left out of the class, the variable's
derivation, the copy's fallback, the prefix of the new reader) and after-only controls (the LAN
bind written into the loopback hint's address, a failure leaving its connections open, the loop
joined after the shutdown, the QR built from the request's Host), and the brief's additions: an
alias left unresolved (I1), the retirement's close left out (I2), a missing row let go, an
unnumbered adapter's row closing the console and a missing row closing it for good (I5), the
framing left off and set on admitted requests alone (I6), a window of its own for the loopback
shutdown (I7), Tailscale left to the prefix (I9), the `.local` name offered with the responder
off (I10), and the composition snapshot waited for on its flight (I11).

Four controls first stayed green or would not run, and each was a test that could not fail for
its own reason, fixed before it was run again:
- the retirement's wait is bounded from its start, and the event stream test held its
  retirement two seconds at its mark under a two-second bound, so its blocked-write case could
  read a wait ended at its bound; its plan waits ten seconds now (`24fe27e1`);
- the shutdown test held a handler on the LAN socket alone, so a second window for the loopback
  socket's shutdown cost nothing it could see; it holds one on each socket and asks, at the
  stop's retirement, that the loop is joined and the loopback socket still takes connections
  (`4a401ecc`);
- the parked admission was let go as soon as the loop took its tick, so a fence whose check and
  count were two sections usually let the retirement in after the count; the test asks that the
  plan's mutex is held while the admission is parked, which a fence of two sections never holds
  (`5e4bab0a`, I4), and its steps answer on the case's goroutine, which a case that failed while
  its retirement was held had panicked on;
- the fallback test never read the menu's line or bridge doctor's, which the plan's row names
  (`c7e95c97`).

Not run here: the hold made a no-op on macOS (`TestTheConsolesHoldScopesItsRepliesOnMacOS`, the
`macos-latest` leg) and on the routed client (`console-lan-routed`, as root), and the node halves
of the upload's, the note's and the rotation's tests, where node is installed. The Linux hold
made a no-op was run: `TestTheConsolesHoldRefusesASYNFromAnotherInterface` went red.

### The brief's additions

- I1: `config.LANKeyOf` decodes the file into a struct whose one field is a `yaml.Node`, as the
  load decodes it, so a merge key is taken as the load takes it, and follows an `AliasNode`'s
  chain; `TestAnAliasedLANAddressLoadsAndKeepsItsSource` holds an aliased address alone and under
  the variable.
- I2, I8, I11: one rule for a stream. The retirement cancels the policy's context in its
  section, the snapshot readers wait for their shared flight on the request's own context
  (`getCompositionSnapshot`, `getEnrichmentBreakdownPart`, `getEnrichmentMetaSnapshot`, through
  `singleflight.DoChan`), and the server's `Close` ends a stream blocked in a write. The test's
  three cases end at the section (on the context, on the snapshot) or at the close (the write),
  each red under its own control.
- I3: `prefixOf`'s own cases stay the baseline the tests-first commit keeps; the synthetic masks
  through the injected reader are the change's (`read_prefix_test.go`).
- I4: the seam sits inside the fence's one critical section; the parked admission proves it by
  the mutex it holds.
- I5 and I13: the plan's 9.3 test decides. Every row of the family's table is judged; every
  interface the listing shows holding an address of the family, the address's own above all,
  needs a row of its own; a missing row is a reading that is not whole (`interfaces_unreadable`,
  asked again at the next reading, never closed for good); a row of an adapter that holds no such
  address is judged and closes nothing else.
- I6: the fence sets `X-Frame-Options: DENY` and `frame-ancestors 'none'` on every answer of
  the socket, its 503 and the source's 403 included.
- I7: the stop's one deadline bounds the retirement's wait and the loopback server's shutdown.
- I9: the source rule refuses Tailscale's IPv4 and IPv6 ranges before the class and the prefix.
- I10: serve hands the plan the `.local` name only while the bridge's responder runs.
- I12: the commits above.

### Measured on this host

The hold on Linux 7.0, unprivileged: `SO_BINDTOIFINDEX` set and read back, and a loopback dial
to `127.0.0.1` held to another index refused (`TestTheConsolesHoldRefusesASYNFromAnotherInterface`).

### Pending CI

**To be filled once the branch has run in CI**, which no host of this session could give: the
Windows runner's interface tables and a reading's cost (`TestTheStrongHostModelIsReadFromTheStack`'s
log), the macOS leg's hold (`TestTheConsolesHoldScopesItsRepliesOnMacOS`), the routed client
(`console-lan-routed`'s log), and each platform's bind errnos where CI shows them.

### The owner's gates

- Windows: a routed SYN to the address from another interface, measured on a real machine.
- Linux needs 5.7 for an unprivileged hold, or `CAP_NET_RAW`.
- macOS holds IPv4 alone, so a ULA is refused there.

### Runs

- The gate, `make fmt vet test build-all` in one build-slot session (`-p 4`, `-race`,
  `-timeout 45m`). Its first run, at `39ff5c25`, failed after 2,128 s on one test,
  `TestTheChangedConfigCheckIsTheStepBeforeSave` (cmd/bridge, 1,789 s; every other package
  passed): the structural test of init's save found initCmd calling `initSave`, the seam the
  rollback warning's test observes the save by, where it looked for `saveInitConfig`. `6f38a0cd`
  holds the test to the seam (initCmd saves once, through `initSave`, which makes no call but
  `initHost.save` or `saveInitConfig`, production's `initHost.save` being `saveInitConfig`) and
  corrects its long form; a direct call past the seam, a step inside it and another production
  save each turned it red. The second run, at `6f38a0cd`, passed in 2,176 s: the race suite
  (cmd/bridge 1,826 s, manifest 1,681 s, admin 1,356 s, backup 678 s), the build-site census
  and build-all's six targets. It answered 38 small packages, unchanged since the first run,
  from that run's test cache; those 38 then passed fresh (`-count=1 -race`, 44 s in the slot).
- Vets with the release tags for darwin/arm64, windows/amd64, linux/386 and linux/arm: clean.
- `make docs-check`'s seven steps, one at a time: clean.
- cmd/bridge's source guards (the cited test names, the docblocks, the import keepers, the
  drains, the census): clean once `39ff5c25` fixed their findings.
- `scripts/qualification-version -base ab898b41 -head HEAD`: OK.
- The routed client's test was not run here, with no root; CI's `console-lan-routed` runs it.
- PROTOCOL.md's one changed sentence, the install endpoint's trust boundary, leaves its
  SHA-256 `e8be1877cadf31d4d8a02608d9c5b5f6cb4fd26757abc705a4ea3a244317e26e`, which the iOS
  app's mirror copies; no wire change, `ProtocolVersion` unchanged.

### Round 2

The second round's brief: the home-network socket's lifecycle, closed on every way out. Main
had moved to `9b66c2af` (B990, #1553, which changed PROTOCOL.md too); it was merged first, with
a merge commit (`c62d68c9`), clean, PROTOCOL.md merged by git. The rule is `cli-serve.md`'s
bullet on serve's ways out; `auth-tls.md`'s B989 bullet stands as written.

#### The commits

- `c62d68c9`: main merged.
- `dd950897`: the tests first, with a names-only shim: the startup's split
  (`PrepareConsoleLAN`, `BeginConsoleLAN`, `AbortConsoleLAN`), a seam at a retirement's end,
  the stats reads' seam, the new reason code, the snapshot reads' context parameters (ignored
  there), the tailnet status read on a context (`Status` there) and serve's mDNS start seam.
- `2a128d95`: the change.
- `476d38ce`: the loop's wait for the first attempt, tested.
- `ada7bc1d`: a tick's own rebind asks the retirement count no more.
- this record and the rule, then the runs, in this section's last part.

#### What the round changed

- A loopback bind that fails (`Serve`), a handed console listener serve refuses, and the
  loopback socket's failure after its start retire the home-network socket through one startup
  abort, `admin.Server.AbortConsoleLAN`, within the console's one shutdown window (items 1 and
  N4); runServe takes no defer for it.
- A retirement counts itself under way (`consoleLAN.retiring`, under the plan's mutex) from its
  first section until its wait for what it admitted has ended. A tick that finds no socket
  binds nothing while one runs (item 2), and the stop joins every retirement under way within
  its one deadline, so it cannot finish before a failure's retirement has closed its
  connections and ended its bounded wait (N1). A tick that has just retired its own socket
  rebinds on its fresh reading as before: the count's one reader is the tick that finds none.
- The retirement's first section stores the waiting state with its reason, so no surface reads
  listening while nothing is served: `reopening` for a reading that changed (a new code),
  `stopped_serving` for a failure, `stopping` for the stop (item 3). A failure's retirement
  stores nothing after it, so a retry's newer reason is never replaced (N3, its second option).
- The plan is opened before the console's goroutine (`PrepareConsoleLAN`) and its first attempt
  made once the mDNS responder's start has answered (`BeginConsoleLAN`, after the lifecycle's
  `Set`), the `Running()` gate on the name kept (N2). The loop takes no tick before the first
  attempt (`consoleLAN.first`); between the two the state reads waiting, `starting` (a new
  code), so no surface words an empty reason; a first attempt once the stop has begun makes
  none. The banner's home-network line now follows the mDNS line.
- The stats reads (`getStatsSnapshot`), the sources' cold read (`trackSourceCounts`) and the
  tailnet's status (`getTailscaleSnapshot`) wait on the request's context, bounded as before
  (`snapshotDBTimeout`, `adminStatusTimeout`): serve's provider gains `StatusContext` beside
  `Status`, which the health path keeps calling, and no status read calls `RefreshNow` (N6).
  `handlers_events.go`'s comment says which snapshots wait on the stream's context and which
  read memory, the box's interfaces or the updater's state.
- `address_on_two_interfaces` is worded for both cases, held more than once on this box, on two
  interfaces or twice on one, and its hint says to keep the address once, on one interface
  (item 4d).
- The Windows table through the listener (4a) and the private class behind a prefix wider than
  it (4b) were behaviour already: their tests pin it, red under their controls alone.

#### The mDNS check (item 4e)

Checked first, on the merged tree: serve made the plan's first attempt before it started the
responder, so with mDNS on the first reading held no `.local` name, the responder then answered
one, and a clean start's first tick found the name changed and retired the socket. The
tests-first commit's three starts show it: with a responder that starts, two binds and the
line `console: the home-network socket is retired` with `cause=name_changed`; with mDNS off and
with a responder that fails to start, one bind and no name, before the change as after it.

#### Red first

At `dd950897`, each test run with `-v`, every red on its own assertion:

- `TestALoopbackBindThatFailsRetiresTheLANSocket`: Serve returned its bind's failure with the
  socket still listening.
- `TestAHandedConsoleListenerThatIsRefusedRetiresTheLANSocket`: the socket took connections
  after serve exited 1.
- `TestNoSocketBindsWhileAFailuresRetirementIsUnderWay`: two sockets bound while the failure's
  retirement was held.
- `TestTheStateSaysNotListeningFromTheRetirementsFirstSection`: listening while each of its
  three retirements ran.
- `TestARetryAfterAFailuresRetirementKeepsItsOwnReason`: `stopped_serving` stored over the
  retry's `address_not_on_this_box`.
- `TestAShutdownJoinsAFailuresRetirementUnderWay`: the stop finished while the failure's
  retirement was held, its connections open.
- `TestEachSnapshotReadEndsWithItsRequestsContext`: in each of its six cases the held read ended
  on no context, and the request did not end at the retirement's section.
- `TestTheTailnetTilesStatusEndsWithItsCallersContext`: the read returned after 5.0 s, its own
  bound.
- `TestTheConsolesFirstReadingFollowsTheResponderStart`: the responder that starts, as above.
- `TestAnAddressHeldTwiceOnOneInterfaceIsSaidSo`: the words said "is on more than one
  interface".

Green there by design: the Windows table's and the private class's tests (4a, 4b), N2's starts
with mDNS off and with a responder that fails, and the failure test, which waits for its
retirement's end since this round (N5). Added with the change, red under their controls:
`TestAPlanBetweenItsOpeningAndItsFirstAttemptSaysTheBridgeIsStarting` and
`TestThePlansLoopTakesNoTickBeforeItsFirstAttempt`.

#### The controls

Twenty mutations, each of a copy (`go test -overlay`), each red on its named tests for its own
reason, the tree clean after: no startup abort in Serve's failed bind (R2-01) or in the handed
listener's refusal (R2-02); a tick that finds no socket binding while a retirement runs
(R2-03); no state stored in the retirement's section, all three ways out red (R2-04); the
failure's late store put back (R2-05); the stop joining no retirement (R2-06); a retirement
counted out before its wait (R2-07); serve's first attempt before the responder's start, the
starting responder's case red (R2-08); the loop taking ticks before the first attempt (R2-09);
the stats reads, the sources' cold read and the tailnet's status each on a context of their
own, their two cases red each (R2-10 to R2-12), and serve's status read on one (R2-13); a
Windows table whose read fails keeping the socket (R2-14); a missing row closing the console
for good, red at the whole reading after it (R2-15; a first mutation, the hold dropped, bound
with no hold and went red one phase early, so it was replaced); the class check left out, the
four rows of a prefix wider than its class red (R2-16); the two-interface words as before
(R2-17); no starting state (R2-18); a first attempt after the stop, which bound and closed a
socket (R2-19); and the abort's own doubled window (R2-20).

#### Where the round went past the brief's lines

- N3 took its second option: the late store dropped, item 3's early store saying the failure.
- `reopening` and `starting` are new reason codes of `GET /api/console`, the console's alone:
  no `/v1` field changed, PROTOCOL.md and `ProtocolVersion` unchanged.
- The tick's guard is the brief's one, on the tick that finds no socket; a second, after a
  tick's own retirement, was dropped as past the lines named (`ada7bc1d`).
- `StatusContext` is an optional interface of the console's, so the API's provider and its tests
  are untouched.
- The banner's home-network line moved after the mDNS line, the first attempt's place.

#### Runs

On `9a8dabdd`, with `GOTOOLCHAIN=go1.27.3` and `GOPROXY=off`, `TMPDIR` below `.tmp/`:

- The gate, `make fmt vet test build-all` in one build-slot session, after 1,240 s waiting for
  the slot: passed in 2,115 s (`-p 4`, `-race`, `-timeout 45m`; cmd/bridge 1,790 s, manifest
  1,624 s, admin 1,319 s, backup 660 s), the build-site census and build-all's six targets
  with it, the tree clean after its fmt. It answered 36 small packages from its test cache;
  those 36 then passed fresh (`-count=1 -race`, 45 s in the slot).
- internal/admin's 45 console-LAN tests (the four `console_lan_*_test.go` files), one race
  binary, `-race -count=50` in four lanes of `GOMAXPROCS=2` on eight of the host's sixteen
  CPUs: every run passed, no race reported, 911 s.
- Vets with the release tags for darwin/arm64, windows/amd64, linux/386 and linux/arm: clean.
- `make docs-check`'s seven steps, one at a time: clean, before and after this section.
- cmd/bridge's source guards (the cited test names, the docblocks, the import keepers, the
  drains, the servers' error logs): clean.
- `scripts/qualification-version -base 9b66c2af -head HEAD`: OK.

PROTOCOL.md's SHA-256 after the merge, for the iOS app's mirror:
`b3198929371b3041340a168f19f955cefe9786f159120f396f70e42e91e60c7d`.

### Round 3

The third round's brief: the endpoint list ends with its request, and two tests made exact.
Of round 2's review findings one was confirmed by line and two test points were tightened; the
other four were refuted, and their reasons are not repeated here.

#### The commits

- `a7c56541`: the endpoint list's tests first.
- `8a87c6cd`: the change.
- `37bd6771`: two tests made exact.
- this record, then the runs, in this section's last part.

#### The endpoint list (item 1)

GET /api/endpoints and the event stream read the endpoint list through `endpointsWithin`. Its
provider (`Deps.Endpoints`, serve's `api.Server.ReachableEndpoints`) takes no context, and its
Tailscale status read waits under the API's lock (`cachedTailscaleStatus` holds
`tailscaleStatusMu` across the provider's call) and on the embedded node up to
`adminStatusTimeout`; so the provider runs on a goroutine of its own and to its end, while the
request waits for it on its own context and returns at once when that ends, behind another
caller's read as in front of one. The API's cache is left as it is: its shared read takes no
request's context, so a console wait that ends can neither cut it short nor leave an empty status
cached, and its other callers (`/v1/health`, the pairing QR's alternates, the player's hosts)
read it as before. A panic in the provider is raised again on the waiting caller's goroutine, as a
direct call raised it, and logged with its stack once its request had ended. The REST route
answers nothing once its request has ended; the stream ends there.

#### Red first

At `a7c56541`, on round 2's code: the snapshot test's two endpoint-list cases, the stream's and
GET /api/endpoints', red on "the request did not end at the retirement's section", their provider
still held; the shared-read test red on "request 2: the console's wait for its endpoint list did
not end with its request's context". The endpoint cases have their own expectation (their
provider takes no context, so it stays held while the request ends), so their control fails on
the request's end alone.

#### Two tests made exact (item 2)

- The shutdown's join is two cases. With the handler let go before the stop's deadline, the stop
  finishes before that deadline with the retirement's count back to zero and its connections
  closed. With the handler held past it, the stop finishes within its one window and the
  retirement ends just after, its bounded wait said once; the count at the stop's return is no
  assertion there, the stop's wait being bounded by its deadline by design.
- The refused handed listener's test reads that listener's address only once the box's stand-in
  is bound (`boundFirstListener`), so the refusal always follows the bind, and a precondition
  asks for that one stand-in.

#### The controls

Seven mutations of copies (`go test -overlay`), each red for its own reason, as predicted in
writing before the run: the endpoint list read with no context wait (the two endpoint cases on
the request's end alone, the shared-read test on its first wait); the API's cache keeping an
empty status (`/v1/health`'s list without the tailnet's name); the API's cache keeping nothing
(the status asked twice); the stop joining no retirement (both shutdown cases at the stop's
first 300 ms); a retirement's wait not ended by the stop's deadline, and a retirement counted
out before its wait (the deadline case alone, the retirement not ended once the deadline had
passed); the handed listener's refusal with no startup abort (the socket taking connections
after serve's way out).

#### Runs

On `14f9d028`, with `GOTOOLCHAIN=go1.27.3` and `GOPROXY=off`, `TMPDIR` below `.tmp/`:

- The gate, `make fmt vet test build-all` in one build-slot session, after 2,005 s waiting for
  the slot: passed in 2,112 s (cmd/bridge 1,792 s, manifest 1,616 s, admin 1,317 s, backup 660 s,
  api 154 s), the build-site census and build-all's six targets with it, the tree clean after
  its fmt. It answered 37 small packages, unchanged this round, from its test cache; a fresh
  run of them queued behind another worker's slot for 57 minutes and was stopped before it
  began, inside this round's time, so those 37 stand on the cached results.
- internal/admin's console-LAN, endpoint-list and event-stream tests (53: the four
  `console_lan_*_test.go` files, the external `console_lan_endpoints_test.go` and
  `handlers_events_test.go`), one race binary, `-race -count=50` in three lanes of
  `GOMAXPROCS=2` on eight of the host's sixteen CPUs, beside another worker's gate: every run
  passed, no race reported, 1,657 s.
- Each new or changed test `-count=20` before its controls, the admin ones under `-race`: passed.
- Vets with the release tags for darwin/arm64, windows/amd64, linux/386 and linux/arm: clean.
- `make docs-check`'s seven steps, one at a time: clean, before and after this section.
- cmd/bridge's source guards (the cited test names, the docblocks, the import keepers, the
  drains, the servers' error logs): clean.
- `scripts/qualification-version -base 9b66c2af -head HEAD`: OK.

PROTOCOL.md's SHA-256 did not move this round:
`b3198929371b3041340a168f19f955cefe9786f159120f396f70e42e91e60c7d`.

### Round 4

CI on round 3's head failed three tests, in two cases; nothing else failed.

- The upload page's two node drivers assigned `globalThis.crypto`, which Node 24 defines with a
  getter alone, so the assignment threw in the module's strict mode before the shipped functions
  ran (macOS, Windows and the race leg; this host has no node, so the drivers' node halves skip
  here and CI runs them). Each driver now defines every global it sets
  (`Object.defineProperty`, writable and configurable): `crypto`, and in the note's driver
  `document` and `isSecureContext` too. Read here, not run.
- The refused handed listener's test went through the startup helpers, which read an exit that
  follows the banner as a failed start; serve prints its banner before it reads the refusal, and
  on macOS the exit came first to the helper's look. The test takes a path of its own now: serve
  on a goroutine it drains (`drainServeOnCleanup`), its return waited for and its exit code read
  once, then the refusal's line, the one stand-in bound before it, and its listener's close. The
  helpers and every other test are unchanged.

`9b98c15f` holds both. The control, `AbortConsoleLAN()` taken out of the refusal's branch, went
red at the listener check alone (the socket taking connections after serve's way out), as
predicted. Runs on `9b98c15f`: the refused listener's test `-count=20`, passed; the two page
tests, their structural halves passed and their node halves skipped; the release-tag vets for
darwin/arm64, windows/amd64, linux/386 and linux/arm, clean; and the gate, `make fmt vet test
build-all` in one build-slot session taken at once, passed in 2,097 s (cmd/bridge 1,781 s,
manifest 1,617 s, admin 1,309 s, backup 662 s), 38 small packages answered from its test cache.
The macOS order and the node halves are CI's to verify.

### Round 5

CI on round 4's head failed one test on windows-latest alone, `TestTheStrongHostModelIsReadFromTheStack`,
as it had at every gate run since round 1's runs commit; round 4's two fixes passed.

The cause is production's, on every Windows bridge with two or more interfaces. `HostModel` stepped
through `GetIpInterfaceTable`'s rows by indexing x/sys's `MibIpInterfaceRow`, which declares
`TransmitOffload` and `ReceiveOffload` (natively `NL_INTERFACE_OFFLOAD_ROD`, one byte of one-bit
fields each) as `uint32`: measured for windows/amd64 and windows/arm64 against x/sys v0.50.0, its
row is 176 bytes, its fields below `TransmitOffload` (offset 164) at the native offsets, where the
native row is 168. Row k was read 8×k bytes past its start, so from the second row on the console
read another row's bytes: the runner's rows read the indexes 1, then 0 and 200 (the native row's
`InterfaceIdentifier` and `MinRouterAdvertisementInterval`), the addressed interface had no row, and
the console decided `interfaces_unreadable`; a row read from another's bytes could as well have
carried another interface's flags.

`parseInterfaceTable` (`internal/consolelan/table.go`, no build tag) reads the table's bytes at the
native layout, `NumEntries` at 0 and rows of 168 bytes from 8, a row's index at 16 and its
forwarding, weak host send and weak host receive bytes at 41, 42 and 43, non-zero true, and refuses
a buffer short of the rows its `NumEntries` says as `host_model_unreadable`. `HostModel` hands it
the table's bytes before `FreeMibTable`. `hostModelEntry` keeps x/sys's row: `GetIpInterfaceEntry`
writes the native 168 bytes into its 176, and the fields it reads lie below offset 164.

The platform-independent test builds a three-row table at the native layout, its offsets numbers of
its own, each row one flag alone (one of them `0x80`), every other byte a filler, and asks each
row's index and flags and a buffer one byte short. The live test, where CI is set, now fails an
interface whose addresses do not read and one holding an address of the family with no row in the
table, asked of the rows' indexes directly.

`a0d01a5c` holds it. The controls, each a copy, went red for their own reasons as predicted before
the run: the rows stepped at 176 with the length check left at 168, at the second row's index (the
filler's `0xA5A5A5A5` where 14 was due); `WeakHostSend` read at 43, at the first row's flags.

Runs on `a0d01a5c` and this record: `internal/consolelan` `-race -count=50`, passed in 1.1 s; vets for
windows/amd64 and windows/arm64 of `internal/consolelan` and cmd/bridge, and the release-tag vets for
darwin/arm64, windows/amd64, windows/arm64, linux/386 and linux/arm, clean; `make docs-check`'s seven
steps, the source guards and `scripts/qualification-version -base 9b66c2af -head HEAD`, clean; and the
gate, in its own commit below. The live Windows reading runs only on CI's windows-latest leg, which
verifies it. PROTOCOL.md's SHA-256 did not move:
`b3198929371b3041340a168f19f955cefe9786f159120f396f70e42e91e60c7d`.

The gate on `9b48b78a`, `make fmt vet test build-all` in one build-slot session taken at once, passed
in 2,111 s (cmd/bridge 1,779 s, manifest 1,619 s, admin 1,310 s, backup 661 s), the build-site
census and build-all's six targets with it, 37 small packages answered from its test cache.
