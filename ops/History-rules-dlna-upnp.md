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

**…and the parser of each datagram's control data holds whatever it is
handed, on every platform: `parseDatagramMeta` reports x/net's answer
wherever `ipv4.ControlMessage.Parse` returns, and neither a destination nor
an arrival wherever that parse panics, whatever else the buffer holds**
(2026-10-31, backlog B610). Windows reads its own `parseWSAPktinfo`, which
needs no guard. The nightly fuzz job failed eight nights running, every night
the target had existed, from run 40077306390 (2026-10-24; B470 PR 5a1 brought
the target at 22:37 UTC the evening before) to 41190127175 (2026-10-31), each
time `FuzzParseDatagramMeta` within a second:
x/net's `ipv4.(*ControlMessage).Parse` called a nil function
(`ipv4/control.go:90`) from `parseDatagramMeta` on a 24-byte input.

x/net's parse alone does not hold. It keeps a table of the IPPROTO_IP
messages it reads, each platform's own, and compares each message's type and
length with the entries in turn (TTL, destination, interface, packet info).
An entry a platform leaves unset has a name and a length of 0 and no parse
function, so a message of type 0, of any length, matches the first unset
entry and calls the function it lacks. At v0.61.0 Linux's table
(`ipv4/sys_linux.go`) sets only its TTL and packet-info entries; FreeBSD's,
NetBSD's, OpenBSD's, DragonFly's and AIX's leave the packet-info entry unset,
Solaris' the destination and interface entries, and z/OS's all but packet
info. Only Darwin's sets all four, and there a message of type 0 is passed
over like any type x/net does not read. x/net's IPv6 parse consults four
entries, which Linux's IPv6 table sets (z/OS's alone leaves one unset), and
the bridge parses no IPv6 control data. (The hotplug watcher's netlink
credentials go through x/sys, whose `ParseUnixCredentials` does not check
its payload's length either; that parse is the player's, and its record is
B610's in `ops/engineering-log.md`. Backlog B616 guards it since 2026-11-02,
the hotplug source's rule in `player.md`.)

**x/net v0.62.0 passes such an entry over** (2026-11-02, backlog B615). Its
`ipv4` and `ipv6` `ControlMessage.Parse` open every case of their switch with
`ctlOpts[X].name > 0 &&` (upstream's fix, read in the module's own source),
so a message of type 0 is passed over like any type x/net does not read, on
every platform, and every entry a v0.62.0 table sets has its parse function.
No input known reaches the recover now: it stays, as defence in depth, for a
release that panics on control data again, through an entry set with no
parse function or anything else.

**A network peer cannot supply the ancillary bytes.** They are recvmsg's
out-of-band data, which only the kernel writes, and every platform numbers
its IPPROTO_IP options from 1 (Linux's IP_TOS, the others' IP_OPTIONS), so no
kernel writes a message of type 0. The parser is made to hold all the same
because the SSDP read loop (`SSDPSubsystem.readLoop`) runs on a goroutine with
no recovery: a panic there ends the process, the whole of `bridge serve`.

**The policy is the orchestrator's decision.** On every input x/net's parse
handles without panicking, the metadata is what it was, x/net's own
semantics, each checked against its code: a message at another level, or of
an IPPROTO_IP type x/net does not read there, is passed over; an IP_PKTINFO
shorter than its struct is passed over, and one before it stands; a header
that does not parse (a length of 0, below a header's or past the buffer)
makes the whole buffer report neither, wherever it sits; of two IP_PKTINFO
messages the later wins, its index of 0 included; a tail shorter than a
header is ignored; and the index is in_pktinfo's 32 bits, which Linux reads
signed and macOS unsigned, so 0x80000000 and up report no index on either.
On an input that makes x/net's parse panic the metadata is neither, so up to
x/net v0.61.0 an IP_PKTINFO before or after a message of type 0 was not
reported on Linux; since v0.62.0 x/net passes that message over, and the
IP_PKTINFO beside it is reported, as Darwin's table always had it (B615).

**A recover scoped to the one call meets the policy on every platform**
(`parseControlMessage`, in `ssdp_dst_other.go`, which every platform but
Windows builds), whatever else a table leaves unset, and what it hides is
the intended effect: x/net panicking on control data no kernel wrote.
**Declined**: a file of Linux's own, which leaves the BSDs, Solaris, AIX and
z/OS panicking; a filter that drops type-0 messages before x/net, which would
report an IP_PKTINFO beside one and would need a copy of each platform's
unexported table; and decoding in_pktinfo by hand, which would replace x/net's
reading of Darwin's and the BSDs' separate IP_RECVDSTADDR and IP_RECVIF
messages with one of the bridge's.

**Don't call x/net's parse outside that recover, widen the recover past the
one call, trade it for anything narrower (a filter of type-0 messages, a fix
of one platform's), or drop it because x/net v0.62.0 no longer panics on a
message of type 0: it is the defence in depth for whatever else panics
there.** `TestParseDatagramMetaAnswersEveryShapeOfControlData` gives every
shape its metadata, laid out for the platform it runs on with x/sys/unix,
and fails on any shape x/net's parse panics on. Since B615 that is none: the
rows of type 0 are `portableControlData`'s, x/net passing them over on Linux
as on macOS, and `ssdp_dst_darwin_test.go` keeps macOS's IP_RECVDSTADDR and
IP_RECVIF messages; until then Linux's rows of type 0, in
`ssdp_dst_linux_test.go`, said x/net panicked on them.
`FuzzParseDatagramMetaIsXNetsParse` holds the parser, on any bytes, to the
parser before B610 run under a recover of the test's own.
`TestTheDatagramMetaSeedsAreLinuxsControlData` pins the committed crasher and
three native seeds byte for byte, by the name the fuzz engine gives them and
by what each decodes to. With the recover removed at x/net v0.61.0,
`TestParseDatagramMetaHoldsAnIPMessageOfTypeZero`, both seeds of type 0 and
each new test panicked at `control.go:90`, each run alone, and
`TestParseDatagramMetaReadsThisPlatformsPktinfo`, unchanged, stayed green; at
v0.62.0 every one of them stays green with the recover removed (B615), which
is what no input reaching it means.

**…and both announce and serve only the private (RFC 1918, a ULA outside
Tailscale's), link-local and loopback addresses, whichever order an interface
lists them in; a public address only as the IP literal `dlna.listenAddress`
names, the operator's opt-in, that address alone** (2026-11-02, backlog B687,
security; Codex's and Argon's notes on the brief were binding). In LAN mode
with the wildcard listener, B48 kept a tailnet peer out and still exposed the
library on a public IPv4 address: an interface that carries a private address
is eligible as a whole (`IsLANEligibleInterface`), the wiring announced each
member's first IPv4 address whatever its class (`firstIPv4OnInterface`, and
the fallback ServerURL alike), `servedAddrs` served a member's address the
server advertised whatever its kind, and the Host check took any address of
the host (`isInterfaceAddr`). On a LAN interface that listed a public IPv4
address before its private one, every NOTIFY and every search's answer named
the public address, the listener answered a connection there whatever Host it
named (1,566 bytes of description), and a Browse over a private connection
naming the public address got the library's URLs on it: whoever could route
to that address browsed and streamed the library, unauthenticated. With the
private address first, only the Host link was open.

**The policy**, of an address of this host:

| class | wildcard `dlna.listenAddress` | pinned literal |
|---|---|---|
| loopback | served, a Host; never announced | the pin is served (loopback) |
| RFC 1918 IPv4, 169.254/16 | a member's first is announced; served on a member; a Host | served if a member holds it (warns and starts if not) |
| ULA outside Tailscale's, fe80 (zoned to its member) | served on a member, a Host; never announced (SSDP is IPv4) | as the row above |
| public, IPv4 or IPv6 (global unicast, not private) | neither announced nor served nor a Host, on whatever interface; left out, said once a start | **the opt-in**: announced, served while an interface holds it, a Host, that address alone, its advertiser on the interface that holds it, eligible or not |
| 100.64/10 (CGNAT, Tailscale's IPv4), Tailscale's ULA | refused | announced as written, served by no one: warns at Start and starts |

A host name never opts in: what it resolves to is served only by its class.
Every spelling of the unspecified address is a wildcard (`::0`,
`0:0:0:0:0:0:0:0` and `::ffff:0.0.0.0` read as pins until B687 and were
announced as `http://[::0]:7790`). A host whose only network is public
(its interface public plus fe80 is not LAN-eligible) does not start with the
wildcard, as before, and is served only through the opt-in. An interface
eligible through a ULA whose one IPv4 address is public gets no LOCATION, and
with no other DLNA does not start (the fallback ServerURL never takes a public
address). Where every SSDP endpoint fails, Start fails and DLNA is off, the
pin included, as before.

**Where it is enforced**, each guard pinned on its own (the controls in the
log's record): the wiring's selection (`firstLANIPv4OnInterface`,
`dlna.IsLANClassAddress`, reading every address so one left out after the
chosen one is still said; `publicLeftOutNote`, Info, once per start, naming no
address, from the advertise set and the fallback alike); `servedSnapshotOf`
(no advertised arm; the pin while `holder` has it), which the listener's
accept, `AcceptsOn` and the MediaServer's unicast M-SEARCH admission all read;
the Host check (`mayName`: a literal of those classes or the pin, before
ownership), so a wildcard ContentDirectory, which builds `<res>` and
albumArtURI from the Host, builds no URL on an unpinned public address;
a specifically bound listener captures its actual address and port before
serving and builds both URLs on that authority, regardless of the accepted
Host (B687 round 3). Private, loopback and public binds use their address,
with IPv6 bracketed and zones URL-escaped; a pinned name keeps its name, as
the SSDP LOCATION announces it, on the bound port
(`TestTheMediaServerServesAPinnedNameOnlyOnItsBoundAddress`). The capture survives Stop
clearing `accepting`, so in-flight responses keep the same URLs. Empty Host
and localhost still pass the unchanged Host check; only a wildcard uses
ServerURL for an empty Host and the Host itself for localhost. The regression
`TestContentDirectoryURLsFollowTheBoundListener` exercises Start with stand-in
TCP and SSDP transports, then Browse and Search for file and artwork URLs;
NewServer
(`checkAdvertisedHost`): a malformed or hostless ServerURL or LOCATION is
refused; a literal of another class is refused unless it is the literal
`dlna.listenAddress` names; a hostname is taken only when it is the
configured listen name, so a pin to the tailnet's address still warns and
starts (B48's "don't make it refuse"); Start: a listener not bound to the
pinned public address, or a named address handed a wildcard listener, is
refused, and a pin
that is served is said once at Warn, naming no address ("anyone who can reach
that address can browse and stream the library without signing in"). The
refusal line now says the listener answers on "the private and link-local
addresses of the LAN-eligible interfaces"; it still names the host's own
refused address, as B604 allows. The Host check reads a boot test's host when
the configuration stands one in (`hostAddrs`, `ServerConfig.Interfaces`), and
the wiring lists the host once and runs its real pickers over that listing
(`PickLANEligibleInterfaceOf`, `PickAllLANEligibleInterfacesOf`), so
`dlnaBootNetwork` lost its `Pick` and `Gather` overrides: no test stands an
approved answer in for the selection.

**Decided, and declined.** The opt-in is an IP literal only (a name's
resolution is the resolver's to change), globally routable only: 100.64/10 is
shared space, not public (RFC 6598), and holds Tailscale's IPv4 range, which no
address-based check tells from an ISP's, so it is never the opt-in, and a pin
to it on a member, served until B687 as an advertised address, now serves no
one. Rejecting a mixed interface whole was declined: the shared pickers drive
mDNS and the renderer and upstream discovery clients too, and eligibility is
per interface while serving is per address. The OpenHome surface already
served IPv4 addresses of the LAN classes alone (`internal/player/lan`), and no
DLNA setting reaches it; its Insert takes `AcceptsOn`, which follows the
listener's served set, the pin included. **Residuals**, unchanged: a listener
that cannot be armed answers a unicast M-SEARCH unchecked, with its one Warn;
Docker's DNAT, a subnet router, a port forward or a proxy can carry anyone to
a private address; a VPN numbered in RFC 1918 or ULA space is a member; IPv6
on a SLAAC-only LAN is served on fe80 alone (no ULA), and the MediaServer
announces IPv4 addresses only for wildcard listeners. A pinned IPv6 literal
is bracketed in LOCATION over the IPv4 SSDP transport; the description's
relative service URLs resolve against that IPv6 authority.

**Don't serve or announce a public address but the literal `dlna.listenAddress`
names, take a name, a wildcard, 100.64/10 or Tailscale's ULA as the opt-in,
accept a Host literal of a class the listener does not serve, choose an
announced address by its place in the interface's list, fall back to a public
one, reject a whole mixed interface, or name an address in either new line.**
`TestTheMediaServerServesAMixedInterfaceOnItsPrivateAddressAlone` (both
orders, through the real wiring: NOTIFY, AcceptsOn, Browse and the file on the
private address, no byte on the public one whatever its Host, the M-SEARCH to
it refused), `TestTheMediaServerAnswersNoRequestNamingItsPublicAddress`,
`TestTheMediaServerFallsBackToNoPublicAddress`,
`TestTheMediaServerServesThePublicAddressItsListenAddressNames` (a public-only
host, beside a private address, on an interface of its own),
`TestTheMediaServerTakesNoPublicAddressItsListenAddressDoesNotName`,
`TestTheMediaServerRefusesAPinNoInterfaceHolds`,
`TestTheMediaServerServesAPrivateOnlyHostAsBefore`,
`TestTheMediaServerServesAULAAndNotAGlobalIPv6Address`,
`TestTheMediaServerSaysOnceAndWithoutTheAddressThatItLeftOutAPublicAddress`,
`TestTheMediaServerTakesEverySpellingOfTheWildcardAsOne`,
`Test_ownHostOnly_RefusesAnAddressOfThisHostItDoesNotServe`,
`Test_ownHostOnly_TakesThePinnedPublicAddressAlone`,
`TestNewServerRefusesToAdvertiseAnAddressItWouldNotServe`,
`TestStartRefusesAListenerNotBoundToThePin`,
`TestStartRefusesANamedListenAddressOnAWildcardListener` and
`TestTheAddressClassesAndTheOptIn` pin it, with the B687 rows of
`TestServedAddrsAreTheLANAddressesOfTheMulticastSet` (its
`a_members_public_address` row was the advertised exception, served) and
`TestAcceptsOnIsTheListenersServedSet`. The record is in
`ops/engineering-log.md`, "2026-11-02 — B687".

**The ContentDirectory offers no WavPack or Monkey's Audio track, an upstream
such item keeps its format's extension and no codec, and a `.wvc` is no item**
(2026-11-06, backlog B489). Few renderers play either format, the DLNA profiles
have no entry for them, and a renderer cannot declare a format the way an app
does (`X-Bridge-Formats`, `wire.md`), so the adapter's rebuild
(`manifestLibraryAdapter` in `dlna_wiring.go`, its `view` field) reads the
served set in the view that declares nothing (`ListServedTracks(ctx, nil,
formatgate.Set{})`), pinned through the real MediaServer wiring. The loopback
track route, which the MPD beside the bridge fetches from, reads every format
(`formatgate.All()`) and serves the file as it is, MPD decoding it
(`output_loopback_test.go` fetches a `.wv` there). **Upstream**: the walk takes
a `.wvc` for no track (`isWavPackCorrection`: its URL's extension, or a
protocolInfo naming `wavpack-correction`), and names a WavPack or Monkey's
Audio item by its protocolInfo's extension whatever its URL's (`audio/x-wavpack`
and `audio/wavpack` are `wv`; `audio/x-ape`, `audio/ape` and
`audio/x-monkeys-audio` are `ape`), since the client gate holds a routed row by
its path alone; and the ingest gives an upstream row of either no codec
(`codecFromExtension`): it parsed no header, and `WAVPACK` or `APE` would say
lossless where a hybrid `.wv` may be lossy. An item with neither an audio class
nor an audio protocolInfo is still not admitted by a `.wv` URL alone
(`isKnownAudioExt` names neither format). **Don't let the adapter read a
declared view, or give an upstream row a codec its header was never read for.**
`TestTheContentDirectoryOffersNoWavPackOrMonkeysAudio`,
`TestAnUpstreamWavPackOrMonkeysAudioItemCarriesItsExtension` and
`TestCodecFromExtension_KnownAndUnknown` pin it.

- **Eligibility is the allowlist; SELECTION prefers a real LAN among what it
  admits, and the two stay separate** (2026-09-27). `PickLANEligibleInterface`
  returned the FIRST eligible interface, and macOS enumerates system utuns
  ahead of `en0`: on the dev Mac that was `utun0` (`UP,POINTOPOINT,RUNNING,
  MULTICAST`, one fe80 address), which the zero-config arm admits. The mDNS
  responder pinned itself there, its IPv4 listen failed without a word
  (hashicorp/mdns drops that error, and utun0 has no IPv4), and neither
  hashicorp/mdns's client on `en0` nor `dns-sd -B` found the bridge, while
  the NUC's bridge on the same LAN was found. The picker now ranks what
  eligibility admits (`lanPreference`): not point-to-point first, then a
  private IPv4, then any other usable address (a ULA, the opted-in tsnet
  interface), then link-local only, and enumeration order among equals, so
  a host whose first eligible interface already ranks best keeps it (dido
  measured: `enp1s0f0` and the same five-member set as before; three
  members since the next paragraph's rule and the Tailscale bullet above).
  `PickAllLANEligibleInterfaces` keeps its members and order but leaves out
  a point-to-point interface whose only addresses are link-local whenever
  anything else is eligible: six such utuns sat in the dev Mac's set, and
  the renderer and UPnP-upstream discovery clients start one SSDP client per
  member, while an IPv4 multicast send pinned to such a tunnel fails
  (`sendto: can't assign requested address`, hashicorp/mdns's client on
  utun0). **Don't fold the preference into `IsLANEligibleInterface`**:
  dropping every point-to-point interface, or the zero-config arm, from
  ELIGIBILITY takes away an opted-in tunnel and a direct-cable renderer,
  and the bullet above says why each arm is there. **Don't reduce the key
  to either half**: the flag alone misses Windows' Wintun adapter
  (WireGuard's, and Tailscale's when opted in; `IF_TYPE_PROP_VIRTUAL`,
  which Go gives no point-to-point flag) and a Mac's `bridge0` holding a
  self-assigned address, and the class alone ties a WireGuard tunnel's
  private 10.x with `en0` and lets enumeration order pick the tunnel (each
  half turns rows red that the other leaves green). One case moves the
  other way: a host whose LAN is zero-config (169.254 / fe80 only) beside a
  private-IPv4 bridge (`docker0`, a VM's) now binds the bridge, where the
  first eligible used to win if it came first. The tables drive
  `pickLANInterface` and `pickAllLANInterfaces`, the seam both exported
  pickers call (`TestPickLANInterfacePrefersANonTunnelWithAPrivateIPv4`,
  `TestPickAllLANInterfacesDropsALinkLocalOnlyTunnel`), and
  `TestTheExportedPickersRunTheSelection` compares the exported pair with it
  on the host, which can fail only where the first eligible interface does
  not rank best (the dev Mac, not a typical Linux runner).

- **Each SSDP advertiser hears only the multicast M-SEARCHes that arrive on
  its own interface, and announces from that interface's address** (2026-09-29,
  backlog B71). The DLNA server runs one SSDP endpoint per LAN interface,
  each with that interface's LOCATION (#328), on one listener per interface
  that every device of the SSDP subsystem shares (B470 PR 5a1, the
  subsystem's bullet below), and by the eligibility rule a Linux
  host running Docker is such a host: `docker0` and every user bridge carry
  a private IPv4.
  **The listener**: `net.ListenMulticastUDP` binds the wildcard address
  (net's `listenDatagram` rewrites the group before the bind), and Linux's
  default `IP_MULTICAST_ALL = 1` delivers the group's datagrams to it from
  every interface where any socket on the host joined, so every advertiser
  answered every interface's M-SEARCHes with its own LOCATION. Measured with
  the real binary in three network namespaces: each M-SEARCH from either
  subnet (6 of 6) got two answers, one naming the other subnet, the wrong
  one first in 3. macOS and Windows deliver per joined interface
  (measured). `listenSSDP` (ssdp_listen_linux.go) sets `IP_MULTICAST_ALL =
  0` in the `ListenConfig`'s Control, **before the bind**: from the bind on
  a wildcard socket takes other interfaces' group datagrams, so setting it
  after `ListenMulticastUDP` returns leaves a window. A kernel that refuses
  the option keeps the old listener with one Warn, never a refusal that
  takes DLNA down. **The sender is an unconnected socket, pinned before
  anything is sent, writing each NOTIFY to the group** (`openNotifySender`).
  `net.DialUDP` fixed its route and source address at the connect, along
  the group's route, and a pin set afterwards did not move the source (Linux,
  macOS) or even the route (Windows). Measured: the second interface's
  NOTIFYs came from the first interface's address, and a renderer with no
  route back received 0 of 5 (5 of 5 now); on Windows with Tailscale
  (home-pc's shape: Windows' SSDP service holds the group on every
  interface and Tailscale's route has metric 5) the connect took the
  Tailscale address and route, and a sender pinned to Ethernet put 0 of 4
  datagrams on the LAN (2 of 2 now). **And on a host with no route to the
  group (no default route) the connect failed ("network is unreachable"),
  no advertiser could start, and DLNA did not start at all**; an
  unconnected socket needs no route. **Don't connect it, and don't pin a
  socket after connecting it.** A single-interface host sees no change:
  its advertiser's one membership is where every search arrives, and its
  source is that interface's address either way. The discovery clients
  take nothing from `IP_MULTICAST_ALL` (an ephemeral port, no membership);
  the upstream client's MediaServer search is answered by the bridge's own
  advertisers, with both LOCATIONs before and one now (a looped-back
  search, measured), and only a configured server is ever walked. Pinned
  by `TestAnSSDPListenerHearsOnlyTheInterfaceItJoined` (Linux, and macOS
  where the host sends its datagram: GitHub's macOS runner answers `no
  route to host` for en0, and the test skips there),
  `TestAStartedAdvertisersListenerHearsOnlyItsOwnInterface` and
  `TestAListenerTheKernelWillNotConfineStillListensAndSaysSo` (Linux),
  `TestAStartedAdvertiserWritesItsNotifiesFromAnUnconnectedSocket` (all),
  `TestAnAdvertiserOnLoopbackNotifiesFromALoopbackAddress` (macOS, Windows:
  Linux gives a lo-pinned multicast another interface's address either way,
  127.0.0.1 being host-scoped), and
  `TestSSDPAdvertisersKeepToTheirOwnInterfaces`, the real server over veths
  in a network namespace of the test's own, with and without a route to the
  group (root, or a user namespace: dido, not CI). **Still open**: a unicast
  M-SEARCH to port 1900 reaches ONE listener's socket (Linux: the last
  bound; macOS, Windows: the first) and the MediaServer answers it with its
  endpoint on that listener, whichever served address it was sent to
  (backlog B119, which B470 PR 5a1 keeps exactly; since
  B48 one sent to an address the server does not serve is not answered at
  all, the bullet after the Tailscale one below); on Windows the
  advertiser hears no search sent from its own host (B120); and a pinned
  `dlna.listenAddress` advertises on the picker's interface, which need
  not hold that address (B121).

- **A test that starts an SSDP advertiser binds it to the loopback
  interface, and a discovery test client sends no M-SEARCH** (2026-09-28,
  backlog B38). Two `internal/dlna` tests bound the OS default, so one run of
  the package multicast about 310 NOTIFYs of a fake MediaServer with a
  loopback LOCATION to every device and bridge on the LAN (measured with a
  listener joined on en0, three runs; about 300 of them from
  `Test_SSDPAdvertiser_StartStopRaceFree`'s 1 ms advertise interval), and
  each package's `TestStopWaitsForInFlightFetch` sent a real M-SEARCH.
  `loopbackInterface(t)` pins the join, the sends and the listener to
  loopback and keeps the race and lifecycle coverage (both tests PASS, not
  skip, on macOS and in a Linux container); `newTestClient` and
  `newServerDiscoveryTestClient` install a `writeMSearch` that sends nothing.
  Measured after: 0 datagrams on en0 in three runs of each package, while a
  lo0 listener heard the advertisers' NOTIFYs; on Linux 0 on the Docker
  bridge, where main put 299. **A new test that starts an advertiser or a
  client does the same.** **The skip is decided by the PIN, not only by
  Start's error**: Start only warns when `SetMulticastInterface` fails, and
  sends on the OS default interface, so a test whose Start succeeded would
  multicast onto the LAN on such a host. `loopbackInterface` first pins a
  socket of its own with Start's own call (`pinMulticastInterface`) and
  skips where that fails (CodeRabbit on #1086). The fallback itself stays:
  a production advertiser whose pin fails still advertises. **Measure what
  leaves a Linux host from ANOTHER network namespace, or with tcpdump on
  the bridge, never with a listener
  in the same one**: Go binds a multicast listener to the WILDCARD address
  and the group's port (this said "the group address" until 2026-09-29),
  and with Linux's default `IP_MULTICAST_ALL` a socket joined on one
  interface also receives the group's datagrams arriving on any interface
  another socket joined (a listener on eth0 heard the loopback advertisers'
  NOTIFYs, and one on lo heard eth0's). A test's own listener keeps that
  default; the advertisers' listeners turn it off since backlog B71 (the
  next bullet). On the Windows runner the join and the pin both take on
  "Loopback Pseudo-Interface 1": the tests run there, and its log shows them
  starting with no pin warning. What left a Windows host was measured on
  2026-09-29 (B71, on a Windows 11 host with Tailscale, with a listener on
  the same LAN): nothing reached the LAN, because the advertiser's
  connected NOTIFY sender took the Tailscale route, which its pin did not
  move. The sender is unconnected since.

- **A host that, decoded, holds a Unicode control character or whitespace
  is malformed, and it is refused before any other kind** (2026-10-12,
  backlog B246; the app's twin is acoseac/1-bit#2042). `classifyHost` asks
  `hostIsMalformed` on the original host, before the trailing-dot trim and
  before `ParseAddr`. `unicode.IsControl` or `unicode.IsSpace` is the
  kind `hostMalformed`. The character is never stripped so the remainder
  can be classified: `127.0.0.1` plus U+0085 stays malformed, and a strip
  would call it this machine. Go's `url.Parse` already refuses an ASCII
  control or space in a host. What reaches the classifier is the non-ASCII
  set (U+0085, U+00A0, U+2028, U+3000). **An ASCII space inside an IPv6
  zone whose head parses as an address is kept**: Windows names the
  adapter `Wi-Fi 4`, and `TestParseDeviceDescription_KeepsNoServiceURLThatDoesNotParseBack`
  requires `http://[fe80::1%25Wi-Fi%204]:8080/d.xml`. A control or any
  other space in that zone is still malformed. A `%` whose head is not an
  IP is not a zone (the `%25` name). `hostKindAllowed` refuses the kind.
  `isFetchableURL` refuses it, so `fetchableLocation` reads such a
  LOCATION as absent. `resolveServiceURL` returns
  `errServiceURLMalformedHost` before `isFetchableURL` and again in the
  kind switch. `LocationPermittedBy` refuses it even when the packet came
  from loopback, ahead of the localhost arm that would otherwise keep a
  name it cannot parse. `OperatorChose` approves only this machine and
  link-local, so a malformed manual URL approves nothing.
  `callbackHostAllowed` already refuses one: it is not an address
  `ParseAddr` accepts. **Fullwidth digits (`１２７．０．０．１`) and a
  literal `%25` are not this kind.** IDNA maps the fullwidth host to
  `127.0.0.1`; `%25` decodes to a name holding `%`. Both stay elsewhere,
  and `refuseUnapprovedHostLocal` is the backstop that stops the dial.
  Do not drop that dial check because the string check now exists.
  (`TestAHostWithAControlOrASpaceIsMalformed`,
  `TestAMalformedHostIsNeverStrippedBeforeItIsClassified`,
  `TestAMalformedHostIsRefusedAtEverySiteThatJudgesOne`,
  `TestFullwidthDigitsAndAPercent25StayOnTheDialCheck`,
  `Test_callbackHostAllowed`.)

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
