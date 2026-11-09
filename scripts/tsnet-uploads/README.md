# The embedded node's diagnostic-log uploads, from a production binary

`evidence.py` is backlog B749's evidence that a bridge in `tsnet` mode uploads
Tailscale's diagnostic logs only where `tailscale.uploadDiagnosticLogs: true`
allows it. Go's test cases cannot show this: upstream's logger is never
started inside a test binary. The script runs a real bridge binary, a non-test
build such as the one `make build` writes to `bin/bridge` or a release
archive's, as an operator runs it, confined, and reads what it asks a local
proxy for:

- Each run's HTTP and HTTPS go through a proxy on 127.0.0.1 that records every
  request it receives (a CONNECT's host and port) and refuses every one; it
  opens no connection of its own. The verdict rests on that record.
- The bridge runs confined by port, not by address (Linux on x86_64 or
  aarch64). Landlock lets it connect TCP sockets only to the proxy's port
  number, at any address: Landlock's network rules name a port and never an
  address. A seccomp filter refuses it every IPv4 and IPv6 socket that is not
  plain TCP. So no name lookup, no STUN probe and no TCP connection to another
  port can be made; a connection to the proxy's port number at another address
  is not prevented, and would not be in the proxy's record. An unprivileged
  network namespace (`unshare -rn`), which would confine by address, is
  refused on hosts whose AppArmor restricts unprivileged user namespaces
  (`kernel.apparmor_restrict_unprivileged_userns=1`, Ubuntu's default), and a
  user unit's `PrivateNetwork=yes` is not applied there either.
- Before it starts any bridge, the script checks the confinement from inside
  and from outside. Inside, a TCP connection to another port on 127.0.0.1 and
  on ::1, each with a listener there, must be refused with Landlock's own
  EACCES, and every IPv4 and IPv6 socket that is not plain TCP with the
  filter's own EPERM; outside, the same connections must be made and the UDP
  sockets created. A copy of the script with either mechanism not enforced
  fails the check and runs no bridge. The proxy's port at ::1, where nothing
  listens, gets the kernel's refusal inside and never Landlock's: the
  confinement is by port, as stated.
- `TS_DEBUG_ALWAYS_USE_DERP=1`, an upstream debug setting, keeps the node from
  binding UDP, which the confinement refuses, so it starts as on a host whose
  UDP is blocked; `GODEBUG=netdns=go` keeps name lookups in Go's resolver.

Each run is a throwaway install made for it under `--work` (its own home,
config, library folder and data dir; a work dir that already holds one is
refused), in one of three arms: the key true; the key absent; the key true
with `TS_NO_LOGS_NO_SUPPORT=true` in the bridge's environment. The verdict
rests on the CONNECTs to `log.tailscale.com:443` (upstream's
`logtail.DefaultHost`) alone; the coordination server's are expected in every
run. Every run of the first arm must ask for the log host within the window,
no run of the other two may, and the window must be at least five times the
slowest first request of a positive control. The node's name must be
`1-bit-bridge` at every start, the second start of a node that registered
nothing included, never the library's name.

With `--previous`, an earlier bridge (the release before this change, or a
build of main from before it), the script also checks an upgraded install.
Its setup is not an arm, and has expectations of its own: the earlier bridge
runs once without the key on a fresh install, and must run its whole window,
ask for the log host, and leave a non-empty log buffer (`tailscaled.log*.txt`)
in the node's state dir. The earlier bridge then runs again on a copy of that
data dir, and must ask for the log host again: the positive control on that
state. The bridge under test runs on the data dir itself, without the key, for
the whole window, and must not ask for the log host.

```sh
python3 -I scripts/tsnet-uploads/evidence.py --bridge bin/bridge --previous /path/to/the/earlier/bridge --work /tmp/b749-evidence
```

It prints a table of each run's targets and the checks, and saves
`evidence.json` in the work dir; the exit status is 0 when every check held.
The record of its runs is in `ops/engineering-log.md` (2026-11-09, B749).
