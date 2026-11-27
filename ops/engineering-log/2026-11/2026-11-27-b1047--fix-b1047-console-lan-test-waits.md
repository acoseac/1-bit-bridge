## 2026-11-27 — B1047: B989's console-LAN serve tests wait for the home-network socket and the state serve publishes

### Why

In four days four of B989's console-LAN serve tests (`cmd/bridge/console_lan_serve_test.go`) failed Windows CI on PRs that touch no console-LAN code, each a different test: `TestServeOpensTheConsoleOnItsLANAddress` with "no LAN listener" (B1000, run 45166022500, 2026-11-23); `TestServeHoldsItsLANSocketThroughConsolelan` with its state still `waiting`/`starting` (runs 45680079438 and 45992972490); `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo` with "no LAN listener" at its `requireLANServes` (run 46003278454, attempt 1).

`startLANServe` returned once serve's banner named its API: `waitForListening` (`cmd/bridge/main_test.go` 197 to 232) looks every 25 ms (226) for the `https://` line and the fingerprint after "TLS fingerprint", which serve prints at `cmd/bridge/main.go` 7174 and 7213. Serve makes the home-network socket's first attempt after them, on its own goroutine (`BeginConsoleLAN`, 7264), and writes the home-network banner once that returns (7265 to 7267). A test that read the socket or the state at once could therefore read before the attempt or inside it: the stand-in box's client answers "no LAN listener" while the box has registered no listener (at e1f86c4a, `console_lan_serve_test.go` 120 to 124; the listen seam registers at 96 to 100), and `GET /api/console` answers the start's own `waiting`/`starting`, which `PrepareConsoleLAN` stores before the console's goroutine starts (`internal/admin/console_lan.go` 277 to 281). The loopback console can answer late as well: its goroutine starts before the banner (main.go 7102), but the handed listener only queues connections until `ServeListener` serves, and `lanServe.console` fails the test at a transport error. On this Mac the attempt had reached the listen seam by the time `startLANServe` returned in every red-first run below, so the window was the bind's last few statements; a loaded runner widened it. CI printed the failing state one line after "console listening on the home network", which fits a failure printed when its test ends, as `go test` prints one without `-v` (the Windows leg's run, `.github/workflows/gate.yml` 642), after the line the bind logged once it answered.

### The premise: the order is the product's contract

At e1f86c4a `consoleLAN.bind` (`internal/admin/console_lan.go` 511 to 587) publishes `listening` only after a successful bind with the policy installed. The listen (528) runs the hold's Control inside `ListenConfig.Listen`, and a failure returns there, with nothing stored. On success it builds the server and the fence that holds the policy (544 to 556), then, in one section of the plan's mutex, checks the stop (564 to 569), installs the policy the fence admits by (`current = p`, 570 to 572) and stores the state as listening (573); then it launches `http.Server.Serve` (576 to 583) and logs the listening line (584). A client that connects once the state reads listening waits on the bound socket until Serve accepts it, so no client of the product sees a state the socket does not keep, and the tests' requests on the LAN socket remain the proof that the console serves there. A failed attempt stores its waiting state under the mutex and says its WARN once it has let the mutex go (`setWaiting`, 351 to 367). An empty key's plan stores `off` in `PrepareConsoleLAN` itself (260 to 264), before the banner. No product change.

### The waits

Beside `startLANServe`:

- `waitLANState(t, s, what, want)` polls `GET /api/console` on the loopback socket with a request of its own each time (never `lanServe.console`, which fails at a transport error), tries a transport error or an answer that does not decode again, and returns the first state `want` holds for; a listening state holds only once the box holds a listener too (`box.count() > 0`). It gives up at `serveGiveUpTime(t)` (`cmd/bridge/serve_wait_test.go` 46 to 48), naming what it waited for, the last state it read and the last error it met. Since round 2 it ends at once at serve's exit too, its requests on a context that ends at both, and takes nothing it reads past either (Round 2, below).
- `lanListening`, and `lanWaitingFor(reason)` for a failed attempt, which names its reason: the start's own `waiting`/`starting` is the state at the start, so a wait for `waiting` alone would be answered before any attempt.
- `waitLANBanner(t, s)` waits for the first complete "Home network: " line, which serve writes after the state the attempt publishes; since round 2 through `waitLANStdout`, which waits as `waitLANState` does.

Each test that reads the socket, the state, the banner or the WARN after a start or a tick waits first for the state it then asserts, and round 1 changed no assertion:

- `TestServeHoldsItsLANSocketThroughConsolelan` waits for listening before it reads the hold's order, which reads `hold,control,bind` once the bind has answered, the order the seam's calls came in, and the state. Its second boot, whose hold refuses, waits for `waiting` with `restriction_refused` before it counts the box's listeners: the box holds none before the attempt either, so the count read at once proved nothing.
- `TestServeOpensTheConsoleOnItsLANAddress` waits for listening before its two clients and for the banner's line before its words, in each of its three rows (below).
- `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` waits for `waiting` with `address_not_on_this_box` and its WARN, one predicate, before it reads the state, and for the banner's line before the WARN's exact count and the banner's words (round 1 read the count before the banner: Round 2, below); after the tick, whose send is now bounded by `serveGiveUp` and, since round 2, ends at serve's exit, its 10 s poll is `waitLANState` for listening.
- `TestALANSocketThatDoesNotBindIsNoFailedStart` waits for `waiting` with `restriction_refused` before its stop. A stop that came first would leave `BeginConsoleLAN` nothing to attempt (`console_lan.go` 285 to 304), and the test would pass without the failure it is about.
- `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo`, `TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback`, `TestBridgeDoctorReadsTheRunningBridgesConsole` and `TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket` (whose library command runs "beside a LAN socket that listens") wait for listening.
- `TestUnsettingTheVariableMakesTheFilesAddressTheOneServed` waits for listening at each of its two boots. The second boot's asserted address and source are its plan's from the start; the wait is for the bind its docblock says it makes.
- `TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment`, both subtests, wait for listening. Their words are said of a bridge still starting too (`runningConsoleLANLine`, `internal/doctor/console_lan.go` 132 to 153: the source's words and this shell's variable), which is why the control below left them green; the wait gives the doctor a settled bridge and a console that answers within its one-second ask (`consoleLANAskBound`, `cmd/bridge/doctor_console_lan.go` 19), past which the doctor words a bridge that does not run.
- `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` waits for the fallback, `waiting` with `address_not_on_this_box`, before its probes.
- `TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne` asserts fields its plan holds from the start, and searches the answer, the console's doctor line, stdout, stderr and the log for the file's value. It waits for listening and for the banner first, so the bind's log line and the banner, written once the attempt has answered, are among what it searches (a value's absence asserted only where the lines that could carry it are there). Round 1 waited for the banner's first line, which a key in the file follows with a second write, and searched a state encoded again and the log's rendered lines; round 2 waits for the last line, searches the answer as sent and every string of the log, and requires the listening record (Round 2, below).
- Unchanged: `TestTheConsolesFirstReadingFollowsTheResponderStart`, whose ticks the loop takes only once the first attempt has run (`startLoop`, `console_lan.go` 812 to 818); `TestAHandedConsoleListenerThatIsRefusedRetiresTheLANSocket`, which waits for serve's return; and the two source tests. Round 1 left `TestAnEmptyKeyIsReportedAsCostingTheRollback` unchanged too, its `off` stored before the banner, but stored before the loopback socket answers as well, so its one read could meet a socket that does not answer yet: round 2 makes it wait (below).

### The gate

The serve box's listen seam holds the bind, outside the box's mutex, until the test lets it go (`listenGate`): without `afterBind` inside `seams.listen` before the box records the stand-in, so the state still says starting and the box holds no listener (`box.count() == 0`), or with `afterBind` after the box records the stand-in and before the listener returns, so the box's order reads `hold,control,bind` while the state still says starting. Neither holds a listening state with no box listener. The bind's answer, the state and the banner wait for the release. `startLANServe` launches serve through `launchServe` with `runServe`, as `serveWithOpts` does, so that it can register the release in a cleanup that runs before the drain (`launchServe`'s `beforeBanner`), whatever the boot did; `lanServe.stop` lets the gate go before it waits for serve's exit. A hold never keeps serve from its exit. Both waits let a held bind go once a read made while it held has not found what they wait for: the bind then answers after the test has read the state it keeps back, with no timer.

`TestServeOpensTheConsoleOnItsLANAddress` runs its case three ways: as serve binds, with the bind held before its registration, and held once recorded. The two held rows are the waits' regression guard: a wait that takes the first answer that decodes (NC1 below) is red in both, and one that asks for the box's listener alone (NC7) in the row held once the bind is recorded alone, so that row guards the state's half of the listening wait. Neither row exercises a listening state with no box listener, so neither guards the half that asks for the box's listener (NC9).

### Red first

Over 87717ccf, the gate committed and no wait, with three edits that were never committed: the gate set in each named test, a wait for the seam to reach it, and a log of what the test saw.

| Test | Gate | Failed at (e1f86c4a's lines) | Message |
|---|---|---|---|
| `TestServeHoldsItsLANSocketThroughConsolelan` | once the bind is recorded | the state, 271 to 273, past the order (`hold control bind`, one listener) | `state {State:waiting Address:192.168.88.20 URL: NameURL: Reason:starting Source:file FileKeyAtBoot:address FileAddressAtBoot:192.168.88.20 Network: Interface: Hold:}`, then "console listening on the home network" once the stop let the bind go |
| `TestServeOpensTheConsoleOnItsLANAddress` | before the registration | the inside client, 301 to 304 | `from inside: <nil> Get "http://192.168.88.20:53743/api/console": no LAN listener` |
| `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo` | before the registration | `requireLANServes`, 444 | `the LAN socket at 192.168.88.20: Get "http://192.168.88.20:53747/api/stats": no LAN listener` |

In all three the seam was already at its hold when `startLANServe` returned: `startLANServe` returns while the bind is held, which is CI's shape. With the waits (61dc16ae) and every `startLANServe` box held, the file's seventeen tests passed with the hold before the registration (25.8 s) and with the hold once the bind is recorded (29.0 s).

### Controls

The tests' bodies as at 87717ccf, with every `startLANServe` box held (a temporary default in `startLANServe`, not committed):

| Test | Held before its registration | Held once recorded |
|---|---|---|
| `TestServeHoldsItsLANSocketThroughConsolelan` | the order, `hold control` | the state, `waiting`/`starting` |
| `TestServeOpensTheConsoleOnItsLANAddress` | no LAN listener | the client's 10 s timeout, awaiting headers |
| `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` | the tick's bind, past the 10 s poll | the same |
| `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo` | no LAN listener | the client's timeout |
| `TestBridgeDoctorReadsTheRunningBridgesConsole` | the doctor's line for a bridge that is starting | the same |
| `TestUnsettingTheVariableMakesTheFilesAddressTheOneServed` | no LAN listener, at its first boot | the client's timeout |
| `TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback` | no LAN listener | the client's timeout |

Green in both: `TestALANSocketThatDoesNotBindIsNoFailedStart`, both subtests of `TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment`, `TestAnEmptyKeyIsReportedAsCostingTheRollback`, `TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne`, `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` and `TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket`. A gate holds a bind that succeeds, and their exposure is another: a stop or a probe before a failed attempt, words a starting bridge says too, lines not yet written.

The wait mutated over 61dc16ae, `TestServeOpensTheConsoleOnItsLANAddress` alone, the file put back after each:

| Mutation | The plain row | Held before its registration | Held once recorded |
|---|---|---|---|
| NC1: return at the first read that decodes, whatever it says | green | red, no LAN listener | red, the client's timeout |
| NC7: return once the box holds a listener, whatever the state | green | green: its listener is registered just before the state is stored, and the client waits on the bound socket | red, the client's timeout |
| NC9: the listening state without the box's listener | green | green | green |

NC9 is green as predicted: this box registers its listener before the seam returns and serve publishes after that, so neither placement can make that half of the wait red; it stays for a box that answers otherwise. The banner's and the WARN's waits have no red control: each comes one statement after the state on serve's goroutine (main.go 7264 to 7267; `console_lan.go` 362 to 366), which no seam holds, and the waits follow that order.

### Runs

Go 1.27.3, `GOPROXY=off`, the release tags (`ts_omit_webclient,ts_omit_debugeventbus`), every test run through the build lock.

- `go test -race -count=1` of the file's seventeen tests, named one by one: ok, 23.1 s; `-run 'Console|LAN'`, which selects 102 of the package's tests and misses five of the file's (`TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo`, `TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment`, `TestUnsettingTheVariableMakesTheFilesAddressTheOneServed`, `TestAnEmptyKeyIsReportedAsCostingTheRollback`, `TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback`), held rows included: ok, 118 s.
- `-count=20`: the three named tests and `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo`, ok, 163 s; the file's nine other serve tests, the eight that wait and `TestAnEmptyKeyIsReportedAsCostingTheRollback`, ok, 231 s.
- `TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers` and `TestEveryBackgroundGoroutineDrainsOnCleanup`: pass.
- `make fmt` changed no file; `make vet`, and `go vet ./cmd/bridge` for windows and for linux/386: clean.
- `make test`, once, at 61dc16ae: every package ok, 93 with tests and 15 without, in 33.5 minutes; `cmd/bridge` 1996.8 s, `internal/admin` 1610.3 s, `internal/manifest` 1487.6 s, `internal/backup` 604.8 s.
- `make docs-check`: clean. `TestEveryCitedTestNameExists` read this entry once staged: a test name planted in it turned the guard red, naming this file, and green again once taken out.

### What stays open

- `TestTheConsolesFirstReadingFollowsTheResponderStart` still bounds its wait for the mDNS line and its tick sends by a fixed 10 s, which the serve-wait rule does not allow (`.claude/rules/build-ci-tests.md`). It reads nothing of the socket before its ticks, so it is not exposed to this race, and it was left as it is.

### Round 2

The review of round 1 on #1586 found four P2s and three P3s, and this round folds exactly those. The brief's own review corrected how to fold them, and one of its claims did not hold: it said no seam holds the loopback console's start, so the empty key could have no red control, but `serveOpts.beforeAdminServe` (`cmd/bridge/main.go` 3221 to 3225, called at 7112 to 7114) holds serve just before `ServeListener` on the handed admin path, where the port accepts and the console does not answer yet (backlog B313), and C5 below holds it there.

#### The commits

- `03ab26da`: the waits, bounded and ended at serve's exit.
- `c5016c5a`: the empty key, the redaction, the WARN's count and the gate's docblock.
- This record: the stale bullet under "What stays open" gone, the gate's wording, the claims above that the round changed, and this section.

#### What the round changed

- **The waits are bounded.** At round 1 `waitLANState` sent each GET on `context.Background()`, so a GET in flight at the give-up ran on past it to `fetchAdminJSON`'s 5 s client timeout (`cmd/bridge/status.go` 104), and `waitLANBanner` took a line it found before it asked for the deadline. Each wait now asks at the top of each loop, and again before it takes what a read found, whether the give-up has passed (`lanWaitOver`: `!giveUp.IsZero() && time.Now().After(giveUp)`, the zero time being `serveGiveUpTime`'s where the binary has no deadline, `cmd/bridge/serve_wait_test.go` 44 to 48). `waitLANState`'s requests go on a context of the wait's, cancellable whether or not the binary has a deadline and given `WithDeadline` only where it has one (`lanWaitContext`), and the 10 ms pause between reads wakes at the give-up or at serve's exit (`lanWaitPause`). `waitLANBanner` waits through `waitLANStdout`, its contract, the first home-network line, unchanged.
- **The waits end at serve's exit.** `servedBridge` carries `exited`, which `launchServe` closes once serve has returned (`cmd/bridge/main_test.go`), so the serves `startLANServe` and `serveWithOpts` start both carry it. The exit cancels the wait's context, and with it a GET in flight; each wait asks `exited` at the top of each loop and again before it takes a matching result, and fails at once, naming the exit. It never reads `done`, so the exit code stays for `stopServed` or the drain to read and report. The fallback test's tick send has an `exited` arm beside `serveGiveUp`.
- **The empty key.** `TestAnEmptyKeyIsReportedAsCostingTheRollback` read `/api/console` once, at once after the start. The plan stores `off` in `PrepareConsoleLAN` (`internal/admin/console_lan.go` 260 to 264) before the console's goroutine starts (`main.go` 7099 and 7102), and the loopback socket answers only once that goroutine reaches `ServeListener` (7115), which nothing orders before the banner `startLANServe` waits for. The test now takes the console's first answer through `waitLANState`, whatever state it holds, and asserts `off` and `empty` on that answer, never waiting for `off`; and it clears `BRIDGE_ADMIN_LAN_ADDRESS`, so a variable the test's own environment sets cannot be the address served.
- **The redaction.** With a key in the file the banner writes its listening line and its rollback line in two writes (`cmd/bridge/serve_console_lan.go` 143 and 155), so the search could run between them. `TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne` now waits for the rollback line whole, `"Home network: " + consolelan.RollbackWords` and its newline, the banner's last line; searches `/api/console`'s answer as the console sent it (a `json.RawMessage`), where round 1 searched a `consolelan.State` encoded again, which drops any field the type lacks; requires the bind's listening record (`console listening on the home network`, `console_lan.go` 584), so the log's search is of the line that could carry the value; and searches the log through `Recorder.Carrying`, every string a handler could write (the B604 bullet in `.claude/rules/build-ci-tests.md`), where round 1 searched `All`'s rendered lines.
- **The WARN's count.** `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` read the count before its wait for the banner, and now reads it after: the attempt says its WARN once it has let the plan's mutex go (`setWaiting`, `console_lan.go` 357 to 366), and serve writes the banner once `BeginConsoleLAN` has returned (`main.go` 7264 to 7267), so the attempt's whole turn is over before the count.
- **The gate's words.** This record said each placement guards a half of the listening wait the other does not, and the docblock read the same way. They hold two points: without `afterBind`, inside `seams.listen` before the box records the stand-in, the state still `starting` and `box.count() == 0`; with it, after the box records the stand-in and before the listener returns. Neither holds a listening state with no box listener, which serve publishes only once the seam has returned, so neither exercises that half of the wait (NC9). The docblock and this record's sections on the gate say so.
- **The rules line.** It is in this PR, `.claude/rules/build-ci-tests.md` 163 (`88d6b477`); the bullet under "What stays open" that said it was not written, and its quoted copy, are gone.

#### Red first, and the controls

Each control was run at round 1's code (`88d6b477`) and again at this round's (C1 to C4 at `03ab26da`, C5 to C10 at `c5016c5a`), under `-race` with the release tags. C1 to C3 were tests in an untracked file beside the suite, removed after; C4 to C10 were edits over a committed tree, each put back with `git checkout` after its run. None is kept: each ends in a wait's own failure, which no test in the package can take for a pass unless the waits hand their failure back to a caller.

| Control | Round 1 | Round 2 |
|---|---|---|
| C1: a console that answers `waiting`/`starting` until 300 ms before the give-up, then holds each GET unanswered; `waitLANState` for listening, `-timeout 45s` | the wait ended 4.706 s past its give-up, when the held GET's own 5 s timeout let it go | the wait ended 3 ms past it, the held GET cancelled at the give-up |
| C2: the wait begins 50 ms past its give-up with a matching answer there, `-timeout 32s`: a state that decodes | `off` handed out 57 ms past the give-up | refused: "the test's deadline came first" |
| C2: the same with a home-network line already written | the line handed out 52 ms past the give-up | refused |
| C3: a stand-in serve prints the API banner, then exits with code 3 once the wait's first GET has reached a listener that holds it unanswered; `waitLANState`, `-timeout 45s` | polled to its deadline, 15.0 s; the held GET ended at its own timeout, 5.0 s after the exit | failed after 2 ms with its own exit diagnostic ("serve exited while the test waited"), the GET cancelled at the exit ("context canceled"); the drain reported code 3 |
| C3: the stand-in exits once the banner wait's first read has found no line (the box's gate, set held, let go by that read); `waitLANBanner` | polled to its deadline, 14.98 s | failed at once with its own exit diagnostic; the drain reported code 3 |
| C4: the fallback test stops serve and waits for its exit before the tick send, `-timeout 60s` | the send waited to its deadline, 30 s: "the plan's loop took no tick" | "serve exited before the plan's loop took the tick", at once |
| C5: `beforeAdminServe` holds the loopback console until 6 s after `startLANServe` returns; the empty-key test | its one read failed: `Client.Timeout exceeded while awaiting headers` | passed, its first answer read once the console served, in 7.3 s |
| C6: serve's stdout delays the rollback line 300 ms and writes a line carrying the file's value before it; the redaction test | passed | "stdout names the file's value" |
| C7: `/api/console` answers a field `consolelan.State` lacks, holding the file's value | passed | "the answer names the file's value" |
| C8: the bind logs no listening record | passed | "the bind logged no listening record, …" |
| C9: the listening record carries a value whose `String` is `redacted` and whose `MarshalText`, which slog's text handler writes, is the file's value | passed | "the log names the file's value" |
| C10: a failed attempt says its WARN twice, 300 ms apart | passed | "2 WARN lines, want one" |

C3's stand-in exits only once `launchServe` has returned and the wait has begun (its GET in flight, or its first read made), so `waitForListening`'s own exit check (`main_test.go` 222 to 225) is never what goes red. C4's first form stopped serve without waiting for its exit: the loop of a serve still stopping took the tick, and round 1 went red one step later, its `waitLANState` polling a console that was gone until the deadline; the form above waits for the exit first.

#### Runs

Go 1.27.3, `GOPROXY=off`, the release tags.

- The file's seventeen tests, named one by one, `-race -count=1`: ok, 22.4 s at `03ab26da` and 21.8 s at `c5016c5a`.
- `-count=20`: `TestServeOpensTheConsoleOnItsLANAddress` and the three tests this round edits, ok, 141.8 s; the file's nine other tests that call the waits, ok, 217.5 s.
- `TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists` (this entry staged), `TestNoBlankKeepers` and `TestEveryBackgroundGoroutineDrainsOnCleanup`: pass.
- `make fmt` changed no file; `make vet`, and `go vet ./cmd/bridge` for windows and for linux/386: clean.
- `make docs-check`: clean.
- `make test`, once, through the build lock: every package ok, 93 with tests and 15 without. Seven ran (`cmd/bridge` 1873.3 s, `internal/admin` 1510.0 s, `internal/manifest` 1437.1 s, `scripts/third-party-licenses` 301.3 s, `internal/adminauth` 49.0 s, `internal/player/dsdconv` 42.0 s, `internal/fsutil` 4.6 s); go's test cache answered the rest, for code this round leaves as it was.
