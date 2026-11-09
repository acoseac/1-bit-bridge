# The embedded node's diagnostic-log uploads, from a production binary

`evidence.py` is backlog B749's evidence that a bridge in `tsnet` mode uploads
Tailscale's diagnostic logs only where `tailscale.uploadDiagnosticLogs: true`
allows it. Go's test cases cannot show this: upstream's logger is never
started inside a test binary. The script runs a real bridge binary, a non-test
build such as the one `make build` writes to `bin/bridge` or a release
archive's, as an operator runs it, and nothing it does can reach Tailscale, a
relay or any other host:

- Each run's HTTP and HTTPS go through a proxy on 127.0.0.1 that records every
  request's target (a CONNECT's host and port) and refuses every one; it opens
  no connection of its own.
- The bridge runs confined (Linux on x86_64 or aarch64): Landlock lets it
  connect TCP sockets to the proxy's port and to no other, and a seccomp
  filter refuses it every IPv4 and IPv6 socket that is not plain TCP, so no
  name lookup, STUN probe or direct dial leaves the host. The script checks
  the confinement from inside before it starts any bridge. An unprivileged
  network namespace (`unshare -rn`) would do the same with less code; on hosts
  whose AppArmor restricts unprivileged user namespaces
  (`kernel.apparmor_restrict_unprivileged_userns=1`, Ubuntu's default) it is
  refused, and a user unit's `PrivateNetwork=yes` is not applied there either.
- `TS_DEBUG_ALWAYS_USE_DERP=1`, an upstream debug setting, keeps the node from
  binding UDP, which the confinement refuses, so it starts as on a host whose
  UDP is blocked; `GODEBUG=netdns=go` keeps name lookups in Go's resolver.

Each run is a throwaway install under `--work` (its own home, config, library
folder and data dir) in one of three arms: the key true; the key absent; the
key true with `TS_NO_LOGS_NO_SUPPORT=true` in the bridge's environment. The
verdict rests on the CONNECTs to `log.tailscale.com:443` (upstream's
`logtail.DefaultHost`) alone; the coordination server's are expected in every
run. Every run of the first arm must ask for the log host within the window,
no run of the other two may, and the window must be at least five times the
slowest first request of the first arm. The node's name must be
`1-bit-bridge` at every start, the second start of a node that registered
nothing included, never the library's name.

```sh
python3 -I scripts/tsnet-uploads/evidence.py --bridge bin/bridge --work /tmp/b749-evidence
```

It prints a table of each run's targets and the checks, and saves
`evidence.json` in the work dir; the exit status is 0 when every check held.
The record of its runs is in `ops/engineering-log.md` (2026-11-09, B749).
