## 2026-11-21 — Bridge lows 15: the embedded node's two open follow-ups (B829, B830)

The two follow-ups the embedded Tailscale node's B749 work left open, on `fix/bridge-lows15` from
main `af8f9a72`: B829, a registered node's name read under the keys upstream itself writes, and
B830, a consenting node's log uploads outliving a stop that gave up on its start. Both premises were
read again at that base and in tailscale.com v1.104.5, the version `go.mod` pins (the backlog
entries were written against v1.104.2), and both held. B829 adds a test of code that already did
what it says, so it has no red; B830's tests were committed before its fix with the names they need
and today's behaviour, and run red at that commit. The rule changed is `cli-serve.md`'s tsnet entry,
condensed to its bold title, its Don'ts and its tests (its whole line, edited, is its History twin
in `ops/History-rules-cli-serve.md`'s B749 section), with the long form in the same commit.

### B829: the keys upstream writes (commit 19bb8e63)

`TestTheStateUpstreamWritesForARegisteredNodeReadsAsRegistered` (`internal/tsnet`) registers a node
with upstream's own `tsnet.Server`, built as `startUnlocked` builds the bridge's (its state dir of
its own, upstream's file store, not ephemeral) but for its control server: an in-process
`tstest/integration/testcontrol.Server` on an `httptest` listener on loopback, which registers any
node with no auth (testcontrol.go, 973 to 1136 at v1.104.5). It waits on the node's backend through
its local client (`WatchIPNBus` with `NotifyInitialState`) until its state is Starting or Running,
every wait on a context that ends at the test binary's deadline less 30 s, or less half of what is
left when that is less; closes the node; and asks `readNodeState`, which answers registered, with
the hostname the server was given. A start of the bridge's over that state then keeps the name.
Upstream wrote `_current-profile`, `_machinekey`, `_profiles` and `profile-<id>` on Linux; CI's
windows-latest and macos-latest jobs run the case there, which is the Windows check B829 asked for.

- **Starting, not Running.** The case gives the control server a DERP map with no region, so the
  node dials no relay and sends no STUN probe, and ipnlocal moves a node from Starting to Running
  only once a relay or a peer is live (ipn/ipnlocal/local.go, 6978 to 6983). It registers all the
  same: the status that carries the netmap carries the persist with the node's ID
  (control/controlclient/map.go, 78 and 237 to 240; auto.go, 732 and 740), and
  `setControlClientStatusLocked` writes the profile (local.go, 2006) before it moves the state
  machine (2161). upstream's own in-process DERP and STUN servers (`tstest/integration`'s
  `RunDERPAndSTUN`) need modules the build's module graph does not hold, so the case uses none.
- **Nothing beyond loopback.** Upstream's `startLogger` returns at once in a test binary
  (tsnet/tsnet.go, 1101 to 1103), so no log transport is built. The portmapper would probe the LAN's
  gateway: its knob is registered (net/portmapper/portmapper.go, 45 and 87 to 95), so the case sets
  `TS_DISABLE_PORTMAPPER` through `envknob.SetenvForTest`, which restores it. An auth key of the
  case's own keeps an auth key or an OAuth secret the environment may hold out of the start
  (feature/oauthkey/oauthkey.go, 38 to 41). Under
  `strace -f -qq -e trace=connect,sendto,sendmsg,sendmmsg,bind` the test binary connected and sent
  only to loopback, unix and netlink sockets; the same case built without the knob
  (`go test -c -overlay`) sent four NAT-PMP and PCP probes to the LAN's gateway on port 5351. One
  upstream read the case cannot turn off: on a Linux host whose DMI names a Hyper-V or an old GCE
  machine, `hostinfo` asks the link-local metadata address once per process, with a 2 s bound
  (util/cloudenv/cloudenv.go, 133 to 237), as the production binary does; the case reads nothing of
  its answer. The host these runs were made on is not such a machine, so the audit saw none.
- **Runs.** `go test -race -count=20 -run '^TestTheStateUpstreamWritesForARegisteredNodeReadsAsRegistered$'
  ./internal/tsnet/`: 20 passes in 4.9 s, and the whole package under `-race` beside it. Two negative
  controls by `go test -overlay`, each predicted before it ran, went red at the case's
  `readNodeState` assertion and nowhere else: the current profile read under
  `ipn.ServerModeStartKey`, the key a start that is not OS-neutral would take (`registered:false`),
  and a registered node's name taken as `DefaultHostname` (`1-bit-bridge`, want `B829 Node`).

### B830: the uploads of a start a stop gave up on (commits bc3c4457, 2cada562)

What upstream does, at v1.104.5. `tsnet.Server.Start` runs `start` once (tsnet/tsnet.go, 565 to 569
and 718 to 723), whose first step after the state dir is `startLogger` (884; 1100 to 1138), before
the network monitor, the engine, the store and the backend, none of it on a context. The logtail
transport is chosen as it is built (logpolicy/logpolicy.go, 894 to 897), so the variable set later
does not stop it. serve's `tsnetFront.stop` waits for the goroutine that starts the node until its
grace runs out, then closes the wrapper, whose Close cancels a start it has not published and
returns. A start that allowed the uploads and stalled past `startLogger` could therefore upload past
serve's stop, and while a later node whose config withdrew the consent runs: never an upload without
consent, but one that outlived the withdrawal.

What the bridge does now.

- **The record and the decision live in `internal/tsnet`.** The start wrapper records when its call
  to upstream's start returns (`startUpstream`). `tsnet.Server.AbandonStart` acts only where the
  config allows the uploads and a Start is in flight whose call to upstream's start has not returned,
  one not yet made included (a start held before it, in a hung read of the state dir, would still
  make it). `tsnetFront.stop` calls it where its wait for the goroutine has run out, before it closes
  the node, and decides nothing of it: `f.done` closes once the whole goroutine and its HTTP/3 Serves
  have ended, so the wait can also run out on a listener or an HTTP/3 teardown after a start that
  completed, which the node then leaves alone.
- **What it turns off.** As upstream's own setter does for the coordination server's DisableLogTail
  message (control/controlclient/direct.go, 1403 to 1406): `logtail.Disable` (logtail/logtail.go, 735
  to 738), which the uploader asks before each upload (532) and each write before it buffers (768),
  and `envknob.SetNoLogsNoSupport` (envknob/envknob.go, 499 to 501), which also keeps off the network
  flow logs a tailnet can ask for (feature/netlog/netlog.go, 76). After such a stop no new upload
  starts; one already past the uploader's check finishes through `httpc.Do` (logtail.go, 671),
  bounded by its 45 seconds (634 to 636). Nothing here cancels or joins that upload, so the rule, the
  status words and the line serve prints claim no more than that.
- **The narrower way, read and left.** `(*logtail.Logger).SetEnabled` (logtail.go, 740 to 747) stops
  one logger, but the node keeps its logger in an unexported field (tsnet.go, 355) that `start`
  writes, reachable only through `LogtailWriter` (761 to 767), whose read of the field races the
  stalled start that is writing it.
- **The latch, read live.** The abandonment is a flag of its own in the policy's record
  (`policyRecord.abandoned`, which `ForgetUploadPolicyForTest` forgets), never `LatchedOff`. Every
  node's `Uploads()` reads it live (`AbandonedOff`, in `Off()`): the stopped node, one built before it
  and every later one, and every later `NewServer` that allows the uploads sets the variable again,
  over a false written since, so none reads `Allowed` with nothing held off. `HeldOff()` says it after
  the launch environment and this process's latch and before anything else in the process, in words
  that read both on the console's tile and after "tailscale.uploadDiagnosticLogs is true, but " in
  `LogsRequiredError.Error()`, ending with the relaunch words. `internal/admin` passes `HeldOff()`'s
  words through unchanged (`TailscaleStatus.UploadsHeldOff`, set by cmd/bridge's `tsnetAdminStatus`);
  only its doc comment, and the Settings page's help, which counted three reasons, changed.
- **The seam.** `logtail.Disable` lasts the process. It is called through `disableLogtail`, which
  `tsnet.SetLogtailDisableForTest` replaces; each suite's TestMain (`internal/tsnet`'s, cmd/bridge's)
  puts a counting stand-in in place for the whole binary and fails it on a call no case stood in for,
  so no test process turns the switch for real.

Red first. At bc3c4457 (the tests, `AbandonStart` doing nothing, `AbandonedOff` set by nothing, stop
handing the node nothing), run with HEAD's bytes overlaid on the two files the fix's comments had
touched: every row of `TestAStopThatGivesUpOnAStartThatAllowedTheUploadsTurnsThemOff` failed ("turned
logtail's switch 0 time(s), want once", the variable unset after the stop, the stalled and the earlier
node's status without `AbandonedOff`, no line, the later node's status and words empty);
`TestAStopThatRunsOutOfGraceHandsTheNodeItsStart` and `TestServeGivesUpOnAWedgedTsnetStartAfterTheGrace`
failed ("handed the node the start it gave up on 0 time(s)"), and
`TestAServeWhoseTsnetStartStalledPastItsStopTurnsTheUploadsOff` failed through a real serve and a real
node (no line, the switch turned 0 times, the later node starting over the false written, the tile's
line empty). `TestAStopThatFindsNoStalledStartWhichAllowedTheUploadsTurnsNothingOff`,
`TestAStopWhoseGoroutineEndedInTimeHandsTheNodeNoStart` and `TestServeStillReportsATailnetHTTP3BindThatFails`
passed there, as the zero side does; their controls below are what shows them.

Negative controls on 2cada562, by `go test -overlay`, each predicted before it ran
(`.tmp` scratch, not kept), each red where predicted and nowhere else:

| Mutation | Red |
|---|---|
| the consent check dropped | the zero test's row for a held start the config never allowed |
| `!s.upstreamReturned` dropped | its row for a start whose upstream start returned, still coming up |
| `s.starting` dropped | its row for no start made |
| the return recorded as not returned | its row for a start whose upstream start returned |
| stop hands the node the start on every stop | `TestAStopWhoseGoroutineEndedInTimeHandsTheNodeNoStart`, `TestServeStillReportsATailnetHTTP3BindThatFails` |
| stop hands it over after closing the node | `TestAStopThatRunsOutOfGraceHandsTheNodeItsStart`, `TestServeGivesUpOnAWedgedTsnetStartAfterTheGrace` (the serve test through a real node stays green: the real start is still unreturned after Close) |
| `Uploads()` not reading the record | every row of the positive test |
| no policy branch for the abandonment | its rows inherited unset, false and a false written since; the serve test |
| the variable not set by the abandonment | the same three rows |
| the abandonment said before the environment's reason | the rows inherited true and after a node built without the setting |
| `Off()` without `AbandonedOff` | the rows inherited unset, false and a false written since |
| the case's stand-in not put in place | every row, and the binary's TestMain: "the suite turned logtail's switch 5 time(s) with no stand-in of a case's own in place" |

Runs before the gate: `go test -race -count=20 -timeout 20m ./internal/tsnet/` (49 s), and the touched
cmd/bridge tsnet tests at `-race -count=20` (the two stop tests, the wedged and the ordinary
shutdown, the serve case through a real node, the tile and same-process tests, and the two tests of
a held start's join). The gate's runs are made on this entry's own commit, so the PR records them.

### The records

`cli-serve.md`'s tsnet entry held 5,719 bytes of a file at 99,825 of its 100,000, and
`move_entry.py --largest` offered one candidate, which it refuses (a 729-character Don't sentence).
The entry's line is now its bold title (which gains the stop), its date (which names B829's check and
B830), its bolded Don't sentence (which gains the stop's Don'ts), its tests and its evidence, 3,176
bytes; its whole line, edited, is its History twin, where the long form gains *The abandoned start*
and B829's check, and B828's section no longer names B829 as open. The sentences that said B830 stays
open (`internal/tsnet/uploads.go`, `internal/config/config.go`, the console's Settings help, the
README, `ops/settings-apply-semantics.md` and the v0.2.4 release notes draft) now say the stop.
