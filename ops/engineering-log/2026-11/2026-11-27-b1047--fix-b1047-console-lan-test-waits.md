## 2026-11-27 — B1047: B989's console-LAN serve tests wait for the home-network socket and the state serve publishes

### Why

In four days four of B989's console-LAN serve tests (`cmd/bridge/console_lan_serve_test.go`) failed Windows CI on PRs that touch no console-LAN code, each a different test: `TestServeOpensTheConsoleOnItsLANAddress` with "no LAN listener" (B1000, run 45166022500, 2026-11-23); `TestServeHoldsItsLANSocketThroughConsolelan` with its state still `waiting`/`starting` (runs 45680079438 and 45992972490); `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo` with "no LAN listener" at its `requireLANServes` (run 46003278454, attempt 1).

`startLANServe` returned once serve's banner named its API: `waitForListening` (`cmd/bridge/main_test.go` 197 to 232) looks every 25 ms (226) for the `https://` line and the fingerprint after "TLS fingerprint", which serve prints at `cmd/bridge/main.go` 7174 and 7213. Serve makes the home-network socket's first attempt after them, on its own goroutine (`BeginConsoleLAN`, 7264), and writes the home-network banner once that returns (7265 to 7267). A test that read the socket or the state at once could therefore read before the attempt or inside it: the stand-in box's client answers "no LAN listener" while the box has registered no listener (at e1f86c4a, `console_lan_serve_test.go` 120 to 124; the listen seam registers at 96 to 100), and `GET /api/console` answers the start's own `waiting`/`starting`, which `PrepareConsoleLAN` stores before the console's goroutine starts (`internal/admin/console_lan.go` 277 to 281). The loopback console can answer late as well: its goroutine starts before the banner (main.go 7102), but the handed listener only queues connections until `ServeListener` serves, and `lanServe.console` fails the test at a transport error. On this Mac the attempt had reached the listen seam by the time `startLANServe` returned in every red-first run below, so the window was the bind's last few statements; a loaded runner widened it. CI printed the failing state one line after "console listening on the home network", which fits a failure printed when its test ends, as `go test` prints one without `-v` (the Windows leg's run, `.github/workflows/gate.yml` 642), after the line the bind logged once it answered.

### The premise: the order is the product's contract

At e1f86c4a `consoleLAN.bind` (`internal/admin/console_lan.go` 511 to 587) publishes `listening` only after a successful bind with the policy installed. The listen (528) runs the hold's Control inside `ListenConfig.Listen`, and a failure returns there, with nothing stored. On success it builds the server and the fence that holds the policy (544 to 556), then, in one section of the plan's mutex, checks the stop (564 to 569), installs the policy the fence admits by (`current = p`, 570 to 572) and stores the state as listening (573); then it launches `http.Server.Serve` (576 to 583) and logs the listening line (584). A client that connects once the state reads listening waits on the bound socket until Serve accepts it, so no client of the product sees a state the socket does not keep, and the tests' requests on the LAN socket remain the proof that the console serves there. A failed attempt stores its waiting state under the mutex and says its WARN once it has let the mutex go (`setWaiting`, 351 to 367). An empty key's plan stores `off` in `PrepareConsoleLAN` itself (260 to 264), before the banner. No product change.

### The waits

Beside `startLANServe`:

- `waitLANState(t, s, what, want)` polls `GET /api/console` on the loopback socket with a request of its own each time (never `lanServe.console`, which fails at a transport error), tries a transport error or an answer that does not decode again, and returns the first state `want` holds for; a listening state holds only once the box holds a listener too (`box.count() > 0`). It gives up at `serveGiveUpTime(t)` (`cmd/bridge/serve_wait_test.go` 46 to 48), naming what it waited for, the last state it read and the last error it met.
- `lanListening`, and `lanWaitingFor(reason)` for a failed attempt, which names its reason: the start's own `waiting`/`starting` is the state at the start, so a wait for `waiting` alone would be answered before any attempt.
- `waitLANBanner(t, s)` waits for the first complete "Home network: " line, which serve writes after the state the attempt publishes.

Each test that reads the socket, the state, the banner or the WARN after a start or a tick waits first for the state it then asserts, and no assertion changed:

- `TestServeHoldsItsLANSocketThroughConsolelan` waits for listening before it reads the hold's order, which reads `hold,control,bind` once the bind has answered, the order the seam's calls came in, and the state. Its second boot, whose hold refuses, waits for `waiting` with `restriction_refused` before it counts the box's listeners: the box holds none before the attempt either, so the count read at once proved nothing.
- `TestServeOpensTheConsoleOnItsLANAddress` waits for listening before its two clients and for the banner's line before its words, in each of its three rows (below).
- `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` waits for `waiting` with `address_not_on_this_box` and its WARN, one predicate, before it reads the state and the WARN's exact count, and for the banner's line before its words; after the tick, whose send is now bounded by `serveGiveUp`, its 10 s poll is `waitLANState` for listening.
- `TestALANSocketThatDoesNotBindIsNoFailedStart` waits for `waiting` with `restriction_refused` before its stop. A stop that came first would leave `BeginConsoleLAN` nothing to attempt (`console_lan.go` 285 to 304), and the test would pass without the failure it is about.
- `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo`, `TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback`, `TestBridgeDoctorReadsTheRunningBridgesConsole` and `TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket` (whose library command runs "beside a LAN socket that listens") wait for listening.
- `TestUnsettingTheVariableMakesTheFilesAddressTheOneServed` waits for listening at each of its two boots. The second boot's asserted address and source are its plan's from the start; the wait is for the bind its docblock says it makes.
- `TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment`, both subtests, wait for listening. Their words are said of a bridge still starting too (`runningConsoleLANLine`, `internal/doctor/console_lan.go` 132 to 153: the source's words and this shell's variable), which is why the control below left them green; the wait gives the doctor a settled bridge and a console that answers within its one-second ask (`consoleLANAskBound`, `cmd/bridge/doctor_console_lan.go` 19), past which the doctor words a bridge that does not run.
- `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` waits for the fallback, `waiting` with `address_not_on_this_box`, before its probes.
- `TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne` asserts fields its plan holds from the start, and searches the answer, the console's doctor line, stdout, stderr and the log for the file's value. It waits for listening and for the banner's line first, so the bind's log line and the banner, written once the attempt has answered, are among what it searches (a value's absence asserted only where the lines that could carry it are there).
- Unchanged: `TestAnEmptyKeyIsReportedAsCostingTheRollback` (its `off` is stored before the banner); `TestTheConsolesFirstReadingFollowsTheResponderStart`, whose ticks the loop takes only once the first attempt has run (`startLoop`, `console_lan.go` 812 to 818); `TestAHandedConsoleListenerThatIsRefusedRetiresTheLANSocket`, which waits for serve's return; and the two source tests.

### The gate

The serve box's listen seam holds the bind, outside the box's mutex, until the test lets it go (`listenGate`): before the bind is registered, so the box holds no listener, or once it is recorded (`afterBind`), so the box's order reads `hold,control,bind` while the state still says starting. The bind's answer, the state and the banner wait for the release. `startLANServe` launches serve through `launchServe` with `runServe`, as `serveWithOpts` does, so that it can register the release in a cleanup that runs before the drain (`launchServe`'s `beforeBanner`), whatever the boot did; `lanServe.stop` lets the gate go before it waits for serve's exit. A hold never keeps serve from its exit. Both waits let a held bind go once a read made while it held has not found what they wait for: the bind then answers after the test has read the state it keeps back, with no timer.

`TestServeOpensTheConsoleOnItsLANAddress` runs its case three ways: as serve binds, with the bind held before its registration, and held once recorded. The two held rows are the waits' regression guard. Each placement is kept because each guards a half of the listening wait the other does not (NC7 below).

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

NC9 is green as predicted: this box registers its listener before the seam returns and serve publishes after that, so that half of the wait cannot be red with it; it stays for a box that answers otherwise. The banner's and the WARN's waits have no red control: each comes one statement after the state on serve's goroutine (main.go 7264 to 7267; `console_lan.go` 362 to 366), which no seam holds, and the waits follow that order.

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
- The rule's line was not written: this change could not edit `.claude/`. Its text, for `.claude/rules/build-ci-tests.md` as a continuation of the serve-wait rule (159), after its "…and checks first that the event can come where a seam stands in" line:

  > - **…and a console-LAN serve test waits for what it reads of the home-network socket: serve binds it and publishes its state on its own goroutine after the banner `startLANServe` waits for, and again at a tick, so `waitLANState` polls `GET /api/console` with requests of its own, a transport error tried again, until the state the test then asserts (listening only once the box holds a listener, a failed attempt's by its reason), and `waitLANBanner` waits for the banner's line** (2026-11-27, backlog B1047: four of B989's tests failed Windows CI four ways in four days, "no LAN listener" and a state still `waiting`/`starting`). **Don't read the socket, its state, the banner or the WARN at once after a start or a tick, wait for a state the start's own `waiting`/`starting` already answers, or bound the wait by anything but `serveGiveUpTime`.** `TestServeOpensTheConsoleOnItsLANAddress`'s rows that hold the bind (`listenGate`) pin it.
