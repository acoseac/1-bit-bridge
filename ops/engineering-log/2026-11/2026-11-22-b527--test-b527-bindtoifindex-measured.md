## 2026-11-22 — B527: the OpenHome surface's interface restriction, measured on a Linux test box

Branch `test/b527-bindtoifindex-measured`, from main `18519baa` (#1545's merge, one commit after
the brief's `f899d811`; that commit touches none of the lines the brief cites, each of which held
at the base). Two commits: `fcab2e9d`, the loopback test; and this record, with the measurement in
the restriction's long form (`ops/History-rules-openhome.md`, in place of "Not measured from this
session: no Linux host was reached.") and Linux's clause in its line (`.claude/rules/openhome.md`).
No production code changes. Every Go run: Go 1.27.3, the tags
`ts_omit_webclient,ts_omit_debugeventbus`.

### The box

A Linux test box: kernel 7.0.0-38-generic, Ubuntu 26.04.1 LTS. Every case ran as an unprivileged
user: uid 1000; `/proc/self/status`'s CapEff, CapPrm and CapAmb `0000000000000000`, which
`capsh --decode=0000000000000000` answers `0x0000000000000000=`, naming no capability, so neither
CAP_NET_RAW nor CAP_NET_ADMIN. No sudo, and no route, sysctl or firewall rule changed.

### The probe

Never committed. The LAN cases and the address's choice ran as a `main` package in the ignored
`.tmp/` (`go build -tags … -o …`, then `probe -mode lan -n 50`), through the exported
`openhome.Listen`, which restricts through `restrictToInterface`, and `lan.Look(lan.HostInterfaces)`;
the loopback-only cases (d) and (e) through a temporary same-package `_test.go` of three tests
(`go test -run … -count=1 -v ./internal/player/openhome/`), deleted before the first commit. 50
runs a case. A dial expected to be refused was bounded at 200 ms, one expected to be taken at 2 s;
every dial was answered within 0.11 ms, a refusal by a reset, and none timed out. Every outcome
but the controls' was predicted in writing before the first run, and each held.

`lan.Look` answered with no error and served two addresses: an up, non-loopback interface with an
RFC 1918 /24 address, and another with an RFC 1918 /30 address. On each, an unrestricted listener
took a dial from this host before any case used it.

| Case | The socket | The dial | On each address |
|---|---|---|---|
| (a) | the served address, restricted to its interface's index | from this host, 200 ms | taken, 50 of 50, its source the address itself |
| (a'), exploratory | the same, restricted to loopback's index | the same | refused, 50 of 50 |
| (a''), exploratory | the same, restricted to an absent index, `1 << 30` | the same | refused, 50 of 50 |
| (b) | the served address, unrestricted (`net.Listen`) | from this host, 2 s | taken, 50 of 50 |
| (c) | `127.0.0.1:0`, restricted to the served interface's index | loopback, 200 ms | refused, ECONNREFUSED, 50 of 50 |
| (c'), (c)'s control | `127.0.0.1:0`, restricted to loopback's index | loopback, 2 s | taken, 50 of 50 |

Every restricted socket's restriction answered `SO_BINDTOIFINDEX`, and its index read back as the
one asked. (a), as the probe printed it for the /24 address (the /30's was the same, 30 to 68 µs):

```text
(a) Listen on the address restricted to its own interface's index, dialled from this host
  runs=50 dial bound=200ms
  restriction answers: {"SO_BINDTOIFINDEX"=50}
  index read back: {"the served interface's"=50}
  outcomes: {"accepted"=50}
  source of the accepted connections: {"the served address itself"=50}
  dial answered in: min 30.898µs, median 37.37µs, p95 82.876µs, max 88.737µs
```

(d), on `127.0.0.1:0` with the absent index: on a raw socket, the setsockopt, through production's
`restrictToInterface`, answered `SO_BINDTOIFINDEX` and the index read back as set; `bind` and
`listen` (raw `x/sys/unix` calls) each answered nil; and every loopback dial was refused, 50 of 50,
in 28 to 92 µs. Through `Listen`, the same: `SO_BINDTOIFINDEX`, the index read back, every
loopback dial refused (50 of 50, 25 to 92 µs), where restricted to loopback's own index the socket
took every dial (50 of 50). So this kernel's setter stored an index no interface has without asking
whether one does, and the socket then listened and was handed nothing.

(e): `restrictToInterface` called again on a socket `Listen` had restricted to loopback's index
answered `none: the kernel refused SO_BINDTOIFINDEX (operation not permitted)`, EPERM's words, for
the absent index and for loopback's own (50 of 50 each), and `none: the interface's index is not
known` for 0, which asks the kernel nothing; the index read back stayed loopback's, and the socket
kept taking loopback dials (50 of 50). This measures the refusal's string alone: the socket stays
restricted to its first index.

The refused path, "refused, the socket listens unrestricted", needs a kernel that refuses a fresh
socket. Tried: a seccomp filter on the probe's own process (`no_new_privs`, then
`seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC)`), answering EPERM to
`setsockopt(SOL_SOCKET, SO_BINDTOIFINDEX)` and allowing everything else, the answer a kernel
before 5.7 gives an unprivileged process. Under it (`probe -mode seccomp -n 50`), `Listen` on
`127.0.0.1:0` answered `none: the kernel refused SO_BINDTOIFINDEX (operation not permitted)` (50
of 50), the index read back 0, and a loopback dial was taken whether loopback's own index or the
absent one was asked (50 of 50 each), where restricted to the absent index it would have been
refused: production's path listens unrestricted and says why. A kernel before 5.7 was not run.

### What (a) shows

The brief's premise was that a dial from this host to the served address arrives on the loopback interface, so that
the restriction would keep the box's own processes from the socket. On this kernel it does not.
Restricted to the address's own interface, the socket took every such dial; restricted to
loopback's index, it refused every one. So the kernel counts the box's own connection to its
address as arriving on that address's interface, not on the loopback interface. The brief names a restricted socket
that takes a connection arriving on another interface as a contradiction to stop at and report, so
this part stops here: the rule's line and its long form say what was measured, nothing else in
them or in the code changed for it, and any rewording of the long form's "a listening socket bound
to a device is handed only a SYN that arrived on it" or of `restrictToInterface`'s comment is a
later, reviewed change. Admission does
what the restriction does not: such a connection's source is the address itself, which admission
refuses (`source_own_address`), as it refuses a loopback source (`source_loopback`;
`TestAdmissionRefusesAtAcceptAndCountsNothing`).

The mechanism, read from upstream's `net/ipv4/route.c` at its tag `v7.0` (upstream's source, not
this box's build): for a local destination, `ip_route_output_key_hash_rcu` sets
`orig_oif = FIB_RES_OIF(*res)` under the comment "make sure orig_oif points to fib result device
even though packet rx/tx happens over loopback or l3mdev" (lines 2860 to 2863), and
`__mkroute_output` stores it, `rth->rt_iif = orig_oif` (line 2660); `rt_dst_alloc` starts every
route at `rt_iif = 0` (line 1659), which an input route keeps. The box's own kernel headers
(7.0.0-38) give the rest: `__inet_lookup_skb` (`include/net/inet_hashtables.h`) looks the listener
up by `inet_iif(skb)`; `inet_iif` (`include/net/route.h`) answers `rt->rt_iif` where it is set and
`skb->skb_iif` otherwise; and `inet_bound_dev_eq` (`include/net/inet_sock.h`) matches a bound
socket only where its index is that one or the l3mdev's. A packet looped back keeps its output
route, so the box's own SYN to its LAN address matches the address's interface; a SYN that
arrives on an interface matches that interface. The last is upstream's reading, not a measurement.

### The real surface, (f)

`bridge` built from the base (`go build -tags … -o …`); `bridge init --yes --no-service --dir
<a directory under .tmp/> --library <an empty directory> --name … --listen-address 127.0.0.1:<port>
--admin-address 127.0.0.1:<port>`; then the config given `output.player.enabled` and
`output.player.openhome` on, a port of its own for the surface and every other listener, and mDNS,
DLNA, MPD, the update check, enrichment and Tailscale off; then `bridge serve --config …` for about
ten seconds, stopped by SIGTERM (exit 0), and the directory deleted. The player did not start:
`player: not started`, its reason `output.player.device binds no DAC: bridge output dac bind
<card> --binding serial|uniqueModel|port binds one, and the player starts when bridge serve next
starts`; so no surface listened and no line said `player: the OpenHome surface listens on an
address`. The player starts only with a binding (`planServePlayer`), which `bridge output dac
bind` makes for a USB card alone, and the box has none: `bridge output dac inspect` listed three
cards, each `identity_unsupported: a pci card, not USB: it has no identity a binding names`. (f)
is skipped for that, as the brief said.

### Unmeasured

- A SYN that arrives on another interface (a VPN's, a container bridge's) for the surface's LAN
  address: it needs a second interface carrying traffic to the box, which only a privileged
  process makes (a network namespace's interfaces carry none of the box's own traffic, and a veth
  to the box's namespace needs CAP_NET_ADMIN there). Upstream's source says such a SYN is matched
  by the interface it arrived on; it was not measured.
- A kernel before 5.7 refusing a fresh socket: the seccomp filter stood in for its refusal, and
  production's path under it was measured; that kernel itself was not run.
- The real surface's start-up line, (f) above.
- CI: the openhome package runs in CI's `test -race (rest)` shard, whose runner
  (`.github/scripts/test-shard.sh`) runs `go test` without `-v` and prints no test's outcome, so
  whether the new test runs or skips there is unknown until a log shows it.

### The test

`TestALoopbackSocketRestrictedToAnotherIndexRefusesALoopbackConnection`, in
`internal/player/openhome/restrict_measured_linux_test.go` (`//go:build linux`), committed because
(d) refused in every run: it finds the loopback interface alone (`loopbackInterface`), calls
production's `Listen` on `127.0.0.1:0` with the loopback interface's own index and takes a loopback
dial (2 s), its premise, then with `1 << 30` and requires the dial (200 ms) refused,
`errors.Is(err, syscall.ECONNREFUSED)`. It skips only where `Listen`'s answer starts `none: `, and
fails on any answer but that and `SO_BINDTOIFINDEX`. Screened before its control: `-race
-count=200`, 200 passes, no skip.

### Red first

Predicted in writing; the mutation, uncommitted on `fcab2e9d`: `restrictToInterface` returns
`SO_BINDTOIFINDEX` right after its index check, never calling setsockopt. Each test run alone
(`-run '^…$' -count=1 -v`):

- the new test, red at its refusal assertion, as predicted:
  `restrict_measured_linux_test.go:80: restricted to an index no interface has, the socket took a
  loopback connection`;
- `TestEachSocketIsRestrictedToItsInterfaceWhereThePlatformLets`, red too, as expected, since it
  reads the index back: `restrict_test.go:57: the socket is bound to interface 0, want` the loopback interface's index.

Restored with `git checkout -- internal/player/openhome/restrict_linux.go`, its SHA-256 the one
taken before the mutation, `git status` clean; then the package green under `-race` (122 top-level
tests passed, none skipped, 36.6 s).

### Runs

Before this record's commit, on `fcab2e9d`'s code: the openhome package with `-race -count=20
-timeout 60m` passed in 720.9 s (2,440 top-level results, every one a pass, none skipped). The
gate's runs on this record's commit are in the report.
