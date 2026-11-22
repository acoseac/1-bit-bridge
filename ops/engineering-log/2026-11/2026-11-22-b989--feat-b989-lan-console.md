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
- this record, with the rules, the docs and PROTOCOL.md's sentence.

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

The gate and the checks are recorded in the next section once they have run.
