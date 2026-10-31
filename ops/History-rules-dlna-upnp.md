# DLNA, UPnP and discovery — rule history

## DLNA, UPnP and discovery

**Every advertised device registers with serve's one SSDP subsystem (B470 PR
5a1).** Listeners are shared by (interface index, name, incarnation), reference
counted, and opened/closed outside the subsystem's lock. The MediaServer keeps
its startup endpoints and incarnation 0. An updating registration stages
additions before withdrawing removals, then announces additions and repeats
alive on retained endpoints after a removal. A failed addition is never
substituted; an empty update parks the registration. Dispatch selects one
endpoint and matched target set per registration, then takes that endpoint's
responder slot. A strict registration requires destination/arrival reporting,
multicast receive confinement and a pinned NOTIFY sender. Its source hook is
pure and non-blocking. No second SSDP stack belongs beside this one in serve.

**Close owns staged endpoints and withdrawals through their last socket
release, under its one deadline** (round 2). It stores the earlier of the
caller's deadline and five seconds from entry when it marks the subsystem
closed. Register and Update rollbacks take that deadline. Every NOTIFY sender
enters the subsystem's inventory before its first write, including a staged
endpoint that has never appeared in the dispatch table. Close snapshots the
handles under the lock when it begins, before waiting for in-flight operations.
At Close's deadline,
the subsystem closes those senders to interrupt writes; a sender acquired
after that point is closed before it can send. The in-flight wait is bounded
by the same deadline, followed by the interrupted operations' cleanup join.
A withdrawal removes its active endpoints from dispatch immediately but keeps
its handle listed until retirement finishes; Close serializes on its operation
lock even if another caller already began withdrawing it. The deadline watcher,
join workers and answer-cancellation callbacks are joined before return. Kernel
close/unwind and scheduling still take their ordinary execution time; no fresh
I/O budget follows the deadline. A concurrent second Close waits for the first.

**Don't unlist a withdrawal before retirement finishes or give a rollback a
fresh shutdown budget.** Cancellation stops answers and alive bursts; it does
not cancel byebye. Retirement joins normal sends before best-effort byebye;
expiry interrupts pending sends and releases everything, with
`context.DeadlineExceeded`. A datagram handed to the kernel cannot be recalled.
**Clamp every owned sender's installed and future write deadlines to Close's
under the same lock** (round 4). Reading Close's state before a separate
SetWriteDeadline left a window for a stale withdrawal deadline to overwrite
Close's. `ssdpOwnedSender` now owns deadline installation under the subsystem's
`mu`: every installation is capped at Close's deadline (zero, meaning no
limit, is capped too). Close clamps each owned sender's current deadline under
that same lock as it publishes its state; this changes a write already in
progress too. A sender acquired during Close gets the clamp before it can
write. Deadline installation is the only sender socket operation under this
lock: no write or close waits there. The watcher still closes senders at expiry
and joins cleanup. `TestCloseClampsAStaleWithdrawalDeadline` parks between the
withdrawal's read and install, begins Close with an expired deadline, and holds
Close before its watcher: the current deadline is already capped, the stale
install yields a socket timeout, no byebye is recorded, and Withdraw reports
`context.DeadlineExceeded`.

**Re-read before byebye only to skip and report expiry; decide Withdraw's final
budget result after release, atomically with handle removal, so Close owns
exactly the handles it saw** (round 4). The join and pre-write reads still decide
whether to skip a byebye and report expiry. They do not serialize deadline
installation. The former final check inside `retire`, before releasing senders
and listeners, missed expiry during release. Round 4 replaced it with the
check in `withdraw`; round 5 restores it after cleanup because Register's
rollback and Update's removal and rollback call `retire` directly.
**Every retirement checks expiry after sender/listener release and reader join
against the re-read deadline.** When the normal-send join did not already
expire, `retire` re-reads the earlier of its original deadline and Close's after
cleanup, and reports `context.DeadlineExceeded` if that budget has expired and
the result does not already carry it. A byebye socket timeout alone does not
match that context error. **Withdraw checks again atomically with handle
removal, adding a deadline error only if absent.** Its final check stays under
`s.mu` with removal, including time after `retire` returns and before removal.
Close snapshots that list under the same lock. A Close that saw the handle owns
the withdrawal through its release; one that did not cannot change its
completed result.
`TestRetirementReportsAByebyeTimeoutAsDeadlineExceeded` pins Update's removal
and Withdraw's single expiry report after a blocked byebye;
`TestRollbackReportsAByebyeTimeoutAsDeadlineExceeded` pins both Register and
Update rollbacks through the existing fake network and cancellation seam.
`TestUpdateRetirementBudgetIncludesListenerRelease` expires Update's original
budget or Close's shorter budget during listener release, after all byebyes.
`TestWithdrawalBudgetIncludesHandleRemoval` parks after retirement completes,
then starts Close with an expired deadline before the handle can be removed.
`TestWithdrawalBudgetIncludesListenerRelease` parks in `fakeListener.onClose`
after all byebyes, lets Close's short deadline expire, then releases it:
Withdraw reports `context.DeadlineExceeded`. The own-deadline variant also
reports expiry; release within budget returns nil, as does a withdrawal
completed before Close began. `TestWithdrawalReportsAnEarlierCloseDeadline`
keeps the earlier blocked-byebye coverage, and
`TestByebyeRefreshesTheCloseDeadlineBetweenWrites` pins pre-write skip/report
and shortened deadlines without relying on the watcher.
`TestCloseJoinsAWithdrawalAlreadySendingByeBye` parks a byebye, releases it or
lets Close's deadline abandon it, and checks socket cleanup at Close's return.
`TestCloseBudgetIncludesInflightWrites` parks Register and Update in initial
alive writes with a 100 ms Close budget (about two seconds before the fix).

**Strict replies use their selected endpoint's local address AND outgoing
interface, or send nothing and warn once. Don't use the MediaServer's routed
reply path for a strict registration.** A source bind alone does not constrain
the route on overlapping subnets or through a VPN. The strict socket binds the
endpoint's `Local` IPv4 address on an ephemeral port, never the :1900 listener;
it needs a valid non-unspecified IPv4 and a positive interface index.
**Refuse strict endpoints without a valid non-unspecified, unmapped IPv4 Local
and positive interface index before opening sockets** (round 3). Register and
Update call the same `checkStrictReplyAddress` as the reply factory; failed
endpoints carry `ErrSSDPStrictCapability`. A failed addition does not disturb
an endpoint retained by Update. Missing, unspecified and IPv4-mapped locals
previously announced successfully but could answer no search; IPv6 already
failed the generic endpoint check. The Register and Update tables in
`TestStrictRegisterRefusesAnUnusableReplyEndpointBeforeOpening` and
`TestStrictUpdateRefusesAnUnusableReplyEndpointBeforeOpening` pin the refusal,
zero new sockets, and the retained endpoint's replies. Linux
sends unconnected with `IP_PKTINFO` carrying both `ipi_ifindex` and
`ipi_spec_dst` on every packet. macOS sets `IP_BOUND_IF` before its first
unconnected send. Windows sets `IP_UNICAST_IF` (31), whose IPv4 index is in
network byte order, before its first unconnected send. These paths need no
privileged bind-to-device operation. Other platforms refuse strict replies.
An option failure closes the socket without sending; a per-packet selection
failure stops the burst. A registration warns once for a failed strict reply,
including send-time refusal, and never retries through the default route.
Normal socket closure at withdrawal is not a warning. **The warning names the
stage, the operation and the errno, never the error's text** (2026-10-30, B604,
the 0.2.2 logging audit). A send's `*net.OpError` names both ends of its
socket, the searching client and the endpoint's own address, and a bind's
names the endpoint's; the line carried `err.Error()` until then.
`noteReplyRefusal` takes the stage (`setup`, the reply's socket, or `send`) and
logs `replyFailure`'s reading: the operation as net and os name it (an
`*net.OpError`'s Op and an `*os.SyscallError`'s call, `write sendmsg`, `listen
bind`), `capability` for an `ErrSSDPStrictCapability` refusal with neither, or
`unclassified`; and the errno, `timeout` for a deadline, or `no errno`. Once per
registration and the `net.ErrClosed` suppression are unchanged.
`TestStrictReplyPinFailureSendsNothingAndLogsOnce` drives every shape: an error
with no errno, a refused bind, a capability refusal with and without an errno,
a refused send, a send past its deadline, a wrapped socket error and a closed
socket.

The MediaServer keeps `net.DialUDP("udp4", nil, searcher)` and its existing
destination admission, endpoint/LOCATION choice, headers, target order, MX
delay and bytes. The pre-extraction wire goldens remain unchanged. Strict
reply routing is pinned by `TestStrictReplyKeepsItsSelectedEndpoint` (a fake
overlapping route), `TestStrictReplyPinFailureSendsNothingAndLogsOnce`,
`TestHostStrictReplyBindsAndPinsBeforeSending`,
`TestLinuxStrictReplyCarriesItsInterfaceAndSource`, and the socket-control
refusal tests on macOS and Windows. Linux/Windows runtime belongs to CI;
cross-compilation alone does not establish behavior on a real multi-NIC host.
**Keep bind/pin ordering tests portable and expect strict write refusal outside
Linux/macOS/Windows** (round 3). The host-factory test replaces the pin to
observe its ordering, but uses the platform's real write function: it must
expect `ErrSSDPStrictCapability` and zero writes on unsupported platforms,
while still requiring socket closure.

**Normalize supported admission pointers before validation and refuse typed
nils and unknown implementations before opening anything.** Both value and
non-nil pointer forms of `SSDPStrictAdmission` and the package's served
admission are copied into their canonical values before storing. Validate the
source hook / served-address provider on those values. Interface embedding
can promote the private marker method into an unknown implementation; the
marker alone does not establish a supported policy. Dispatch and startup
capability checks must see the same policy. This is pinned by
`TestAdmissionFormsAreValidatedBeforeOpening` through Register, the fake
network, dispatch and sender-pin refusal.

## Strict aliases and cross-package transport (B470 PR 5a2b)

Strict multicast used to select firstOn and only then ask Source, losing a
searcher on the second alias's disjoint prefix. It now establishes every
metadata/capability proof first, then walks active endpoints on the receiving
listener and picks the first Source admits. Overlapping prefixes still produce
one target set. Unicast preserves exact Local and reported arrival index;
Source refusal there never falls through to another destination. Legacy
MediaServer selection, fallback and B119/B120/B121 are unchanged.
`TestStrictMulticastChoosesAnAdmittedAlias` failed first against the previous
dispatcher. FuzzSSDPDispatch adds a disjoint alias, reversed order, and a
completeness assertion beside its existing soundness/refusal properties.

`SSDPSubsystemOptions.Network` is an instance socket seam: explicit datagram
metadata/capabilities, sender/reply deadlines and Close; nil is the host
transport. `ssdptest` exposes the 5a1 fake socket model to Source and serve
tests, retaining packet ordering, source/interface selection, failures,
parking and socket counts. No global transport swap. `ServerConfig` can adopt
a test's held HTTP listener and inventory functions without changing normal
selection. Serve's shared deadline still reaches Close's existing ownership,
clamp and join machinery; this change does not weaken it or alter the goldens.

### Round 2: optional timing override

`configureNetwork`, called by the constructor, replaces replyDelay only when
Network.Delay is non-nil, matching IntervalFloor's positive-value guard.
Production still starts with nil replyDelay and `delay`'s random fallback;
this is preventive hardening, not a live timing defect.
`TestSSDPNetworkTimingOverridesPreserveDefaults` seeds a distinct default and
checks nil network, nil delay, an explicit override and the constructor's
wiring. The nil-delay case failed before the guard; removing it fails that
same case. No guard was added to the MediaServer's interfaces/addrsOf seams:
their readers already fall back to the host when nil.

### Round 3: a registration's normal-write barrier

`SSDPRegistration.SendBarrier` is optional and copied into the handle before
staging/initial alive. A zero-value SSDPSendBarrier permits normal writes;
Invalidate permanently ends that lifetime and joins writes under its read lock.
`sendNormal` holds that lock across both the cancellation/barrier check and the
actual alive or reply write. The caller installs the one-second burst deadline
before entry. Invalidation holds no surface/publication lock while waiting,
and overlapping normal writes have concurrent read locks, not serial bounds.
Byebye never enters the barrier; withdrawal still joins normal work before
sending it. No socket is closed merely by barrier invalidation. MediaServer
supplies no barrier and retains its context cancellation, packets and routing.
Do not reuse a barrier for a replacement registration.

`TestSSDPSendBarrierJoinsRepliesAndLeavesByebyeAndMediaServer` parks a reply
inside its write until release or its installed deadline, keeps its registration
context live and invalidates the barrier. It requires the write to have left
before invalidation returns and no recorded reply afterwards, then requires
later alive/reply attempts to send nothing. The same test withdraws and checks
every byebye, then sends alive and replies through the MediaServer sharing its
listener. The Source's HTTP-exit regression covers an initial alive before
Register returns the handle. Removing only sendNormal's barrier protection
makes both regressions fail behavioral assertions; compilation still passes.
Kernel-queued packets remain outside this guarantee.
