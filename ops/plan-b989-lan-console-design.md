# B989: the console on the home network, an explicit opt-in with no login

Design and PR plan, 2026-11-22, for bridge 0.2.4 beside B634 (the console plays to the bridge's own
DAC). This round writes no code.

**Status.** For review, round 1. Binding: the owner's decision of 2026-11-22 (open the bridge's
console from another computer or phone on the home network, without a login, as an explicit
opt-in) and the brief's review items R1 to R10, each settled below and indexed in §12. One choice
departs from the shape the brief sketched, and §1 says why before anything else. Every current
behaviour cited is `path:line` at bridge main `9f98e0c1`.

No host, address or key path appears here. `A` is the private address an operator opens the
console on, `P` the console's port (`adminAddress`'s, 7789 by default), `A2` any other address,
and `<name>.local` the box's mDNS name.

**In one paragraph.** A loopback bridge gains one optional field, `adminLANAddress`: a private IP
address of the box (RFC 1918 IPv4, or an IPv6 unique local address outside Tailscale's range).
With it set, `bridge serve` serves the one console on a second socket bound to `A:P`, beside the
loopback socket, which works exactly as it does today. The second socket admits a connection only
from inside A's own network, answers only a request that names `A:P` or `<name>.local:P`, and
refuses a write that a page of another origin sends. It has no login and speaks plain HTTP, so
anyone on that network can watch and use the console. If A does not bind at start (DHCP not
answered yet, an interface down, the port taken), serve starts all the same with the console on
loopback, says so at WARN, and tries again every 10 seconds. The console's menu, `bridge doctor`
and the serve banner say which it is, from the socket's state, never from the config. Public
mode, the API port the app uses, the app itself and 1-bit Cloud are unchanged; public mode
refuses the field.

## 0. At a glance

| Today | Where | B989 |
|---|---|---|
| The console binds `adminAddress` alone, which loopback mode holds to a loopback host | `internal/config/config.go:4069`, `:4785-4814`; `internal/admin/admin.go:2226-2240` | `adminAddress` unchanged; `adminLANAddress` adds a second socket on `A:P` (§2, §3) |
| Every request must come from loopback | `internal/admin/middleware_auth.go:28-39`; `admin.go:2682-2696` | The loopback socket: unchanged. The LAN socket: a source inside A's on-link network and private class (§4.1) |
| A Host must be a loopback name or literal, any port or none, or empty | `admin.go:2719-2739` | The loopback socket: unchanged. The LAN socket: exactly `A:P` or `<name>.local:P` (§4.2) |
| An Origin must be a loopback name on the listener's port, in `csrfGuard` and in the SSE gate | `admin.go:2547-2605`; `internal/admin/handlers_events.go:63-68` | Per socket, through one function both gates call (§4.3) |
| The guards' address is one field, `boundAdminAddr`, which `ServeListener` sets | `admin.go:1813`, `:2248` | Each socket's own description, stamped on the requests it carries (§3.1) |
| An admin listener's error stops serve, and the API with it | `cmd/bridge/main.go:7386-7409` | Still so for the loopback socket; the LAN socket's never stops serve (§3.5) |
| A refusal's hint names loopback and an SSH tunnel | `admin.go:63-64`, `:2463-2476`, `:2755-2780` | Per socket (§4.4) |
| An upload without SubtleCrypto says "this browser did not expose SHA-256" at its end | `internal/admin/static/app.js:2970-2974` | Said before the upload and after it, naming the real cause (§6.3) |
| Nothing says where the console answers | none | A line at the foot of the console's menu, `GET /api/console`, a `console-lan` WARN in both doctors, a banner line (§5) |

## 1. Where this departs from the brief

The brief sketches a boolean opt-in (`adminLAN: true`) with `adminAddress` moved onto the private
address. This design keeps `adminAddress` as it is, loopback, and puts the private address in the
opt-in field itself: `adminLANAddress: <A>`. It is still one field, inside the budget, and every
rule the brief asks for holds. Why:

1. **R3's hazard does not arise.** Ten call sites in six files dial `adminAddress` as the bridge's
   local endpoint (§3.6). With `adminAddress` on a private address, each would have to learn a
   second, loopback endpoint, and one that did not would fail exactly when the LAN socket is
   down: `isAdminAlive` reads any dial error as "not running" (`cmd/bridge/tsnet.go:622-641`), so
   `bridge tsnet logout` would wipe the tailnet state under a live bridge, and `probeBridge` reads
   a refused connect as "not running" (`cmd/bridge/enrichment.go:756-785`), which lets
   `bridge enrichment retry` write the store behind it (`:396-432`). Kept loopback, `adminAddress`
   stays the one canonical local endpoint, and no consumer changes.
2. **R4's hazard does not arise.** A rewrite that keeps `adminAddress` and loses the opt-in can
   only drop the LAN socket, which fails closed. It can never write a private `adminAddress`
   without its opt-in, which loopback mode's Validate refuses.
3. **Off is one edit**: delete the line. With the brief's shape off is two edits, the boolean and
   the address back to loopback, and either alone leaves a config Validate refuses.
4. **Rollback is the same either way.** An older bridge refuses any key it does not know
   (`internal/config/wholenumbers.go:46`, `KnownFields(true)`), so neither shape loads on 0.2.3
   while the posture is on, and both write nothing while it is off (§2.5, item 6).

What it costs: the LAN socket's port is `adminAddress`'s rather than one of its own (§2.1), and the
field holds an address where the brief has a boolean. The brief's shape is the first alternative
in §8, and the owner's call in §11.

## 2. The posture's config

### 2.1 The field

`Config.AdminLANAddress`, `yaml:"adminLANAddress,omitempty"`, beside `AdminAddress`
(`config.go:52`). Empty, the default, is today's bridge. Set, it is an IP address alone, with no
port and no brackets, and the console listens on it at `adminAddress`'s port. Only serve's start
reads it to bind (§2.4).

### 2.2 Validation, and what each refusal says

Validate checks it after the `adminAddress` block (`config.go:4008-4071`), in this order, and the
first that applies is the refusal. Each names the field and why, never the value (R10): what is
refused can be a secret pasted onto the wrong line, and refusals reach the journal, `bridge
doctor`, the updater's settings check and the console (B505, `config.go:4746-4748`). One exported
function, `config.ValidateConsoleLANAddress(field, value, adminAddress string) error`, is
Validate's check and `bridge init`'s (§2.5, item 4).

| What is set | The refusal (`adminLANAddress: …`) |
|---|---|
| any value, in public mode | only a loopback install opens its console to the home network; public mode's console has a login and binds where adminAddress says, so leave adminLANAddress unset |
| something that is not an IP address alone: a name, a port, brackets, white space | give this box's home-network IP address alone, with no name, port or brackets: the console listens there on adminAddress's port |
| an address with a zone (`%eth0`) | an address with a zone is link-local, and a link-local address is not a home-network address |
| an IPv4-mapped IPv6 address | write an IPv4 address in its IPv4 form |
| `0.0.0.0` or `::` | 0.0.0.0 and :: listen on every network this box is on, the internet's included; give its address on the home network |
| a loopback address | a loopback address is this machine's alone, and the console answers there already (adminAddress) |
| a link-local address (169.254.0.0/16, fe80::/10) | a link-local address changes from link to link and is not a home-network address |
| 100.64.0.0/10 or fd7a:115c:a1e0::/48 | these are Tailscale's ranges (and the first is carrier-grade NAT's), not the home network's |
| any other address that is not private (public, multicast, reserved) | not a private address: give this box's address on the home network, in 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16, or an IPv6 unique local address (fc00::/7) |
| a valid address beside an `adminAddress` on port 0 | adminAddress names port 0, which the system picks at each start; give the console a fixed port, which a device on the home network can open |

The last row has a practical reason too: serve binds the LAN socket before the loopback socket has
bound (§3.2), so the port must be known from the config. Without the posture, port 0 is what it
is today, the ephemeral port every test fixture uses.

`config.ConsoleLANClass(a netip.Addr) (netip.Prefix, bool)` gives the private class an address
falls in (one of the three RFC 1918 blocks, or fc00::/7) and false for any other, Tailscale's
range included, so Validate and the source policy (§4.1) refuse the same addresses. It lives in
`internal/config/console_lan.go`, so Validate and the console (`internal/admin`, which imports
config and no player package) read one definition.

Validate does not ask whether this box holds A. It is a shape check, never a question about the
host (`.claude/rules/config-lifecycle.md`, the `Validate()` bullet), and an address the box does not
hold yet is runtime state (§3.2).

### 2.3 The environment variable

`BRIDGE_ADMIN_LAN_ADDRESS`, derived from the field as every leaf's variable is
(`internal/config/env.go:12-34`, held by `TestEveryConfigLeafIsEnvSettable`), with no exemption: a
variable someone sets is as explicit as the file. It takes the same checks. A console save writes
the file's own value for it, never the variable's (B761's rule, `config-lifecycle.md`).

### 2.4 Who reads it

Serve's boot snapshot, once, to build the bind's plan (§3.2); Validate; `bridge init`. No consumer
reads it live, so a change takes a restart, and every surface that says where the console answers
reads the socket's state, never the field (R9). A console save of other settings keeps a hand edit
of it, since B535's merge writes a leaf another writer changed as the file holds it
(`config-lifecycle.md`, the lock-and-merge bullet).

### 2.5 Every config path (R8)

1. **Validate**, as §2.2. `bridge serve --validate-config`, the updater's settings check
   (`internal/updater/configcheck.go:112`), runs it and binds nothing.
2. **`PATCH /api/settings`** takes no such field. `settingsPatch`
   (`internal/admin/handlers_api.go:2545`) gains none, the decoder ignores a key it has no field
   for (`:2729`), and no control offers it, so no visitor, on the LAN or on loopback, opens or
   closes the posture from the console: it is the file's, the variable's or `bridge init`'s.
   `adminAddress` stays what it is there (`:2874-2881`): held to loopback, applied at restart. A
   visitor who moves its port moves both sockets at the restart, and a LAN visitor who does so may
   lock themselves out of the LAN socket's old URL; nothing else changes. No managed-settings name
   is needed: a field the PATCH does not take is nothing a control plane must hide or refuse.
3. **The runtime config** holds it, and nothing reads it there (§2.4).
4. **`bridge init`.** `--admin-address` is unchanged: a loopback run's must name a loopback host
   (`cmd/bridge/init.go:141-161`). A new flag, `--admin-lan-address <A>`, writes the field, checked
   by `ValidateConsoleLANAddress` against the admin address the run writes, inside
   `initAddressFlagsError`, which may name the flag's value since the operator typed it on that
   command line (its docblock, `:136-140`). With `--public` the flag is refused before anything is
   written (exit 2), as the posture flags are. A run that writes the field prints the LAN URL and
   the no-login sentence (§5.4). init's prompts ask nothing about it: the posture is opted into by
   name.
5. **The init rewrite (R4).** `priorInstallFile` (`cmd/bridge/init_rewrite.go:125-147`) gains
   `AdminLANAddress`, read from the file like the rest (`TestPriorInstallTagsAreConfigs` holds its
   tag to Config's). The rule:
   - `--admin-lan-address` named: the run's value.
   - Otherwise it is kept exactly where the rewrite keeps the admin address (`addressesARewriteKeeps`,
     `:654-669`: a run that replaces the config, the same posture, neither `--listen-address` nor
     `--admin-address` named), when the kept value passes the check against that admin address;
     `printKept` (`:688-759`) then lists it.
   - Otherwise it is not kept, and one line says why: a rewrite to public mode ("public mode's
     console has a login: adminLANAddress is not kept"), a named address flag ("this run writes the
     console's addresses: add --admin-lan-address to open it on the home network again"), or a
     kept value the check refuses, named by its line and never its value, as `priorEnrich.readBase`
     names a refused base (`:266-290`).
   - A run that keeps the config (`--yes` without `--force`) leaves the file whole, the field
     included.
6. **The updater (B613, B761).** Empty marshals as no key (`omitempty`), so a config with the
   posture off is what 0.2.3 loads. With it on, 0.2.3 refuses the key, and the existing rules
   decide: an install's settings check is the candidate's own, which takes it; the crash rollback
   asks its backup to load the settings and puts none back that refuses them
   (`internal/updater/probation.go:198-229`); and the console's Roll back is refused once the
   installed version has opened the data dir (`RollbackRefusal`, `config-lifecycle.md`, the B692
   bullet). So with the posture on, the crash rollback of a 0.2.4 install still on probation is
   lost, recorded in the backup's words ("the backup, …, refuses this bridge's settings",
   `cmd/bridge/serve_crashed_install.go:230`), which the doctor's probation lines show
   (`cmd/bridge/doctor_install_probation.go`); removing the line restores it. There is no save to
   refuse here: B761's `ChecksOffRefused` (`internal/updater/checksoff.go:332-342`) guards a
   console save, and this field has none.
7. **Managed settings and 1-bit Cloud.** Public mode refuses the field (§2.2, first row), and every
   1-bit Cloud tenant is a public-mode bridge, so the posture never reaches one: a control plane
   writes no such key, and a tenant given the variable fails its load, which opens nothing.
   `managedSettingImplies` (`config.go:1460`) gains no entry.

## 3. The bind

### 3.1 Two sockets, one console

- One `Handler()` (`admin.go:1895-2176`) behind two `http.Server`s, both built by one constructor
  with today's settings (`admin.go:2266-2310`): handshakelog's `ErrorLog`, `ReadHeaderTimeout`
  10 s, `ReadTimeout` 30 s, `IdleTimeout` 120 s, `WriteTimeout` unset (`admin-console.md`, the
  `WriteTimeout` bullet).
- Each server's `BaseContext` returns the shutdown context carrying that socket's description: its
  kind (loopback or LAN) and its bound address and, for the LAN socket, the network it admits and
  the `.local` name it answers to. Every guard reads the description of the request's own socket.
  A request that carries none, as one through a test's own `httptest` server around `Handler()`
  does, is the loopback socket's, which is today's behaviour, and needs a loopback source anyway.
- `boundAdminAddr` becomes the loopback socket's bound address alone, which the LAN socket never
  writes (R2: a second `ServeListener` call would have overwritten it). The LAN socket's state is
  an atomic pointer that the posture's surfaces read (§5).
- `ServeListener(ctx, lis)` stays the loopback-only path every existing test takes.
  `ServeConsole(ctx, lis, plan)` is serve's, with a nil plan for no posture.

### 3.2 At start

- runServe builds the plan from the boot snapshot (loopback mode with the field set) and makes its
  first attempt synchronously, once the console exists and before its goroutine starts
  (`main.go:7043`), so the banner can say what happened.
- An attempt lists the interfaces (`lan.HostInterfaces`, `internal/player/lan/lan.go:52-73`, which
  reads each address with its on-link prefix, `:83-98`) and finds the interface that holds exactly
  A. None is `address_not_on_this_box`. One that is loopback or point-to-point is
  `address_on_a_tunnel`. A listing that fails is `interfaces_unreadable`, and nothing is bound,
  since there is no network to admit. Then it binds `A:P`: `EADDRNOTAVAIL` is
  `address_not_on_this_box` (the address left between the listing and the bind), `EADDRINUSE`
  is `port_in_use`, `EACCES` is `port_not_permitted`, anything else `bind_failed`.
- Bound: INFO `console listening on the home network`, with the URL and the network; the state is
  `listening`.
- Not bound: WARN `console: not listening on the home network`, with the reason's code and words
  and the address (Validate has taken it as a private IP by then, so it is no pasted secret), once
  per reason; the state is `waiting`, the console runs on loopback alone, and serve goes on. Nor
  is it a failed start for the install's probation: only the loopback socket's errors reach
  `adminErr` and `bindRefusalIsTheHosts` (`main.go:7386-7409`).

### 3.3 While it waits

Every 10 seconds the plan tries again: one listing, at most one bind, on serve's context, joined
before the console's servers shut down; a test drives it with an injected tick, never a sleep. A
reason that changes is said again, and a bind is said at INFO. Why a retry, not a WARN to restart:
the Linux service is a `systemd --user` unit (`internal/packaging/state.go:31`), and
`internal/player/lan/lan.go:2-5` records that on a Raspberry Pi the interface and DHCP often come
up after the service starts. Without a retry such a box would come up on loopback alone after most
boots.

### 3.4 An address that disappears while serve runs

A socket bound to an address the box no longer holds is expected to stay bound and report nothing
on Linux and macOS (not measured here: taking an address away needs a privilege, or a network
namespace of the test's own, and the PR's record says which it used, or that it could not):
nothing reaches it while the address is gone, and it serves again when the same address comes
back, as with a DHCP renewal. The state stays `listening`, since it is the socket's, and `bridge
doctor` adds that the box does not hold A now (§5.3). A box given another address keeps the
console off the home network until `adminLANAddress` names the new one and serve restarts, which
is why the config comment says to give the box a fixed address (a DHCP reservation). The design
does not depend on the socket staying quiet: where a platform fails it instead, its accept
erroring, §3.5 takes over.

### 3.5 A failure while serving, the loopback socket's failure, and shutdown

- The LAN server's `Serve` returning anything but `http.ErrServerClosed`: WARN
  (`stopped_serving`), close the LAN server and every connection it holds, so none outlives the
  state, then `waiting`, and §3.3 binds again.
- The loopback socket is unchanged: its bind or serve error is `adminErr`, which stops serve and
  the API (`main.go:7386-7409`); the LAN socket is closed on that way out.
- Shutdown: the retry loop stops and is joined first, so no bind follows the stop; then both
  servers shut down together under one 5-second window (the API's rule for its LAN and tailnet
  servers, CLAUDE.md); then the background-scan, catalog and backup drains run once, as today
  (`admin.go:2328-2338`).

### 3.6 One canonical local endpoint (R3)

`adminAddress`, defaulted and passed through `probeLoopbackAddr` (`cmd/bridge/netprobe.go:22-38`),
stays the local endpoint, because §1 keeps it loopback. The audit, by a search for `AdminAddress`
outside the tests at `9f98e0c1`:

| Consumer | Where | Change |
|---|---|---|
| `Server.Serve`, the loopback bind | `internal/admin/admin.go:2231` | none; the LAN socket is the plan's |
| the guards' fallbacks to the config | `admin.go:2488`, `:2588`, `:2619`, `:2640` | read the loopback socket's own bound address; a LAN request reads its socket's description |
| the settings payload and the PATCH | `handlers_api.go:807`, `:2404`, `:2874-2881` | the Settings hint alone (§5.1) |
| the handed-listener check | `cmd/bridge/main.go:3465-3470`, `:7050` | none |
| `bridge status` | `cmd/bridge/status.go:44-54` | none |
| the launcher's probe, and its start and restart waits | `cmd/bridge/menu.go:102-116`, `:665`, `:695`, `:717-729`, `:741-766` | none |
| the library commands' live API | `cmd/bridge/library.go:325-332` | none |
| `bridge enrichment misses` and `retry`, the write gate, the probe | `cmd/bridge/enrichment.go:170`, `:396`, `:645-726`, `:756-785` | none |
| `bridge tsnet logout`'s guard | `cmd/bridge/tsnet.go:622-641` | none |
| init's auto-start probe and its footer | `cmd/bridge/init.go:1237-1244`, `:1270-1284`, `:1441-1454` | the footer adds the LAN line (§5.4) |
| the doctor's port checks and init's preflight | `cmd/bridge/doctor.go:462-464`, `:1190-1201`; `init.go:887`, `:1728` | none: both sockets share the port |
| the OpenHome port clash | `internal/config/player.go:766` | none: the LAN socket's port is that port |
| the browser URL and the banner | `cmd/bridge/banner_url.go:102-148`; `menu.go:775-784` | loopback stays the URL a browser on this machine opens; the banner adds a line (§5.4) |
| the init rewrite | `init_rewrite.go:139`, `:341-343`, `:654-669`, `:720` | §2.5, item 5 |

The new field has three readers: Validate, serve's plan, and init. A structural test walks
`cmd/bridge` and `internal` and holds it to them, so no later probe dials the LAN address.

## 4. The guards

The layers keep their order (`admin.go:2158-2175`): the boundary (source, then Host), `csrfGuard`
(Content-Type, then Origin), the session (public mode alone), the mux. The LAN socket's rules apply
to every request it carries, whatever the live config says. The loopback socket's are today's.

### 4.1 The source policy (R1)

The LAN socket admits a connection whose peer address, unmapped and without a zone, lies inside
both A's on-link prefix (the mask A has on its interface, read at the bind) and A's private class
(§2.2). The box's own address inside that prefix, a process on this box dialling A, is admitted,
as the loopback socket admits it. Anything else is answered 403, `admin refused: this console's
home-network address answers only to devices on its own network`. The check sits in
`boundaryMiddleware`, where `loopbackOnly` sits for the loopback socket.

Why that set:

- **Binding a private address proves nothing about a caller.** Linux takes a connection to A that
  arrives on any interface (the weak host model), and a router, a VPN gateway or a tailnet subnet
  route can deliver one from another network. The console has no login to tell them apart, so the
  network decides.
- **Not "any private source".** A VPN or another VLAN numbered in private space is not the home
  network: private space alone is not enough (`internal/player/lan/lan.go:7-13`).
- **It is the answer the box already gives.** The OpenHome surface admits a source inside the
  on-link prefix of the address a connection arrived on (`.claude/rules/openhome.md`, the
  admission bullet), measured on Linux by B527, and intersecting with the class keeps a prefix
  wider than its class from admitting a public address (`lan.go:148-156`).
- **The peer, never a header.** `X-Forwarded-For` and its kin are not read: a page can set them
  (`auth-tls.md`, the Host bullet).

The residual: a device on the home network that relays others under its own address (a router's
SNAT or hairpin NAT, a VPN gateway, a reverse proxy) carries its clients in, and any device that
holds an address in the prefix is admitted. The Host check below still refuses a browser whose page
names another host, but a client that writes its own Host is no browser, and only the source stops
it.

Not taken in this PR: restricting the arrival interface, as OpenHome does (`SO_BINDTOIFINDEX`,
`IP_BOUND_IF`: `openhome.md`, the interface bullet). B527 measured that a handshake from outside
the prefix needs a route back, which the ordinary case lacks. It stays open as a follow-up (§11).

### 4.2 Host (R6)

The loopback socket keeps today's rule, an empty Host and any port included. The LAN socket takes
exactly the authority it is bound to.

| Host | Loopback socket | LAN socket |
|---|---|---|
| empty (an HTTP/1.0 client) | accept (unchanged) | 421 |
| `localhost`, `localhost.`, `127.0.0.1`, `[::1]`, with any port or none | accept (unchanged) | 421 |
| `A:P` | 421 (unchanged: not loopback) | accept |
| `A` with no port | 421 | 421, port 80 implied, unless P is 80 |
| `A:Q`, another port (a router's port-forward, a proxy) | 421 | 421 |
| `<name>.local:P`, in any case, with one trailing dot or none | 421 | accept (§4.5) |
| `<name>.local:Q` | 421 | 421 |
| another address, the box's own or not, `A2:P` | 421 | 421 |
| a rebinding name, `x.example:P`, that resolves to A | 421 | 421 |
| an IPv6 literal at an IPv4 socket | 421 | 421 |
| A's own literal where A is a ULA, `[A]:P`, in any spelling of that address | 421 | accept (compared as addresses) |
| a zoned link-local literal, `[fe80::…%25eth0]:P` | 421 | 421 |
| A's IPv4-mapped spelling, `[::ffff:A]:P` | 421 | accept (the same address, unmapped) |

An IP literal is compared as an address because no page can rebind a literal: only a name can be
re-pointed, and the only name the LAN socket takes is the one in §4.5.

### 4.3 Origin, in `csrfGuard` and the SSE gate (R6, R7)

The mechanics stay: an Origin is refused when it does not match, never for being absent, and
`null` is present and does not match (`auth-tls.md`, the `csrfGuard` bullet;
`admin.go:2373-2383`). `csrfGuard` passes GET and HEAD, so the event stream keeps its own gate
(`handlers_events.go:63-68`). Both gates call one function, `originMatches(r, origin)`, which
reads the request's socket; today's `originMatchesAdmin` takes no request, so the two gates change
together or not at all, and a structural test holds both to the one function.

| Origin, on a write or on `GET /api/events` | Loopback socket | LAN socket |
|---|---|---|
| absent | pass (unchanged) | pass |
| `null` | 403 | 403 |
| `http://127.0.0.1:P`, `http://localhost:P`, `http://[::1]:P`, P the loopback socket's port | pass (unchanged) | 403 |
| a loopback name on another port (an SSH tunnel on another local port) | 403 (unchanged, B193) | 403 |
| `http://A:P` | 403 | pass |
| `https://A:P` | 403 | 403: the LAN console is plain HTTP |
| `http://A:Q`, or `http://A` where P is not 80 | 403 | 403 |
| `http://<name>.local:P` | 403 | pass |
| `http://A2:P`, `https://x.example` | 403 | 403 |

Considered and not taken: refusing a write with no Origin on the LAN socket. The Fetch standard
has a browser send Origin on every request whose method is neither GET nor HEAD, so the console's
own fetches carry it and the rule would cost a browser that follows the standard nothing. But
loopback's rule is reject-if-mismatched because real browsers omitted it, and a LAN visitor whose
saves failed would have no other way in. It waits for a measurement (§11).

### 4.4 What a refusal says (R7)

- **`writeCrossOriginRefusal`** (`admin.go:2463-2476`). On the loopback socket, today's words: the
  loopback host and port and the equal-port `ssh -L` example, and, while the LAN socket listens,
  one more sentence: "It also answers on your home network at http://A:P." On the LAN socket:
  "admin refused: cross-origin request. This console must be opened at http://A:P.", with no
  tunnel advice, since the visitor is on the network already. Both stay `error: cross_origin`
  (`admin-console.md`, its first bullet).
- **The 421 body** (`errMsgForeignHost`, `admin.go:63-64`). Loopback: unchanged. LAN: "admin
  refused: this address answers only as http://A:P or http://<name>.local:P (a reverse proxy in
  front of it must send that Host)".
- **The source's 403**: §4.1's words, with no address in the body.
- **`noteForeignHost`** (`admin.go:2755-2780`) keeps a set per socket kind, each bounded at 16, so
  the LAN socket's refusals cannot use up loopback's; the LAN line's hint names its authority.
- **The source refusal's log line**: one Warn per kind of refusal (outside the network, outside
  the class), naming A's network and never the peer, as the DLNA listener's refusals do since
  B604 (`dlna-upnp.md`, the B48 bullet). So the console still logs no client but public mode's
  login lines (`auth-tls.md`, the login-lines bullet).

### 4.5 The mDNS name

The LAN socket takes `<name>.local`, the name the API's `.local` endpoint carries in
`/v1/health` (`internal/advertise/endpoints.go:158-171`, `localEndpoint`: `hostname.ASCII`'s first
label, and none for a container's generated id or a name with no valid form), read at each bind
whatever `mdns.enabled` says, compared without regard to case, one trailing dot allowed, on the
LAN socket's port.

Why it is safe: the Host check exists because a page's author controls a name that public DNS
answers and can re-point it at the box (DNS rebinding; `auth-tls.md`, the Host bullet). `.local` is
a special-use domain (RFC 6761, RFC 6762) that no public DNS answers, so a name under it is
answered on the home network or not at all, and whatever answers it is on a network the socket
admits anyway. Why it is worth having: the box's name is what a person types. No other name is
taken: a bare host name, or one under a router's search domain, resolves through unicast DNS,
which a page's author can control.

## 5. The posture shown (R9)

Every surface reads the LAN socket's state (`off`, `listening`, or `waiting` with a reason), never
the field.

### 5.1 The console

- A line at the foot of the console's menu (`internal/admin/templates/layout.html:275-287`,
  inside `.sidebar-foot`, so inside the drawer below 1024 px: `admin-console.md`, the top-bar
  bullet), rendered from the state at each page load through `pageData`
  (`internal/admin/handlers_pages.go:56-94`):
  - listening: "Open on your home network at http://A:P, with no password: anyone on that network
    can use this console."
  - waiting: "Not open on your home network yet: A is not on this box (tried again every 10 s).
    Until then this console answers on this machine only." The middle words are the reason's.
  - off: no line.
  A page loaded while the socket waits says that it may open, so a line a few seconds old never
  understates where the console answers.
- The Settings page's hint for Admin address (`internal/admin/templates/settings.html:605-620`)
  says the console also answers on the home network where `adminLANAddress` is set in
  bridge.yaml.
- The Diagnostics page shows the console's own doctor, whose `console-lan` line (§5.3) reads the
  serving process's state.

### 5.2 `GET /api/console`

A console route on both sockets that answers
`{"lan":{"state":"listening","url":"http://A:P","nameUrl":"http://<name>.local:P","reason":""}}`:
`state` one of `off`, `listening` and `waiting`, `reason` one of §3.2's codes or `stopped_serving`.
`bridge doctor` reads it over `adminAddress`. Nothing on `/v1` changes.

### 5.3 `bridge doctor`

One check, `console-lan`, right after `port-admin`, worded by one function in `internal/doctor` for
both doctors. It prints no line where the config sets no address and no running bridge reports a
socket.

| State | The line |
|---|---|
| running, listening | WARN: the console answers on the home network at http://A:P, with no password and over plain HTTP: anyone on that network can watch it and change the bridge's settings, music and devices. Hint: remove adminLANAddress from bridge.yaml and restart to keep it on this machine; don't forward this port on a router |
| running, listening, and the box does not hold A now | WARN, the same and: nothing on the network reaches it now. Hint: give the box a fixed address (a DHCP reservation), or change adminLANAddress and restart |
| running, waiting | WARN: adminLANAddress is set, but the console answers on this machine only: the reason's words; it is tried again every 10 s. Hint: the reason's own |
| running, but bound otherwise than the file now says (edited since the start) | WARN naming both, and that a restart applies the file |
| not running, and the file sets it | WARN: when the bridge runs, its console also answers on the home network at http://A:P, with no password; and whether the box holds A now |
| a running bridge that does not report it (older than this change) | ok: not checked: the running bridge does not say where its console answers |
| a config that does not load | not run, as the port checks (`internal/doctor/doctor.go:958-974`) |

The console's doctor hands the check the process's own state. The CLI fetches §5.2, bounded at a
second, and takes no answer for a bridge not running. init's preflight does not run the line; init
prints its own sentence when it writes the field (§5.4).

### 5.4 The banner and init's footer

The serve banner (`main.go:7155-7158`) keeps its `Admin console:` line on loopback and adds either
"Home network: http://A:P/ (no password: anyone on your network can use it)" or "Home network: not
open yet: the reason's words; tried again every 10 s". init's footer (`init.go:1270-1284`) adds the
first form when the run wrote the field. The launcher's "open the console" (`menu.go:775-784`)
still opens loopback, the URL a browser on this machine uses.

## 6. What a LAN visitor can do, and the residuals

### 6.1 Every console action

There is one console and no login, so a device on the home network can do everything a person at
the box's own browser can:

- read the library, the playlists, favorites and listening history, the devices' names, the logs
  and diagnostics (paths and addresses among them), and the settings;
- change every setting the console offers (DLNA, mDNS, Tailscale's mode, automatic installs, the
  enrichment mirrors' base URLs among them);
- add or remove library roots, which take any directory the bridge's user can read
  (`config.go:1360-1367`, `ManagedControlRoots`'s note);
- upload into a root, and trash, restore and purge files (an upload takes audio and its
  companions alone, and no dot segment: `internal/upload/paths.go:90-112`, `:193-198`);
- mint, rotate, expire and revoke device tokens, and approve pairing requests (a minted token reads
  the whole library over the API);
- install an update or roll one back, take a backup, and restart the bridge;
- drive the bridge's own player (B634): play to its DAC, the test tone, the player's and
  OpenHome's switches, the DAC's choice.

`/metrics` and the probes answer too. §8 says why no action is held back on the LAN socket.

### 6.2 Plain HTTP

Everything the console shows and is sent crosses the home network unencrypted, so anyone who can
see that traffic (another client of the same Wi-Fi with its passphrase, a device that diverts
traffic on the link) can watch it. One case to name: a pairing dialog opened in a LAN browser shows
a device token and a one-time code, and a watcher who redeems the code first gets the device's
credential, while the phone's redemption then fails where the user sees it (`auth-tls.md`, the
pairing-link bullet). Where the network is not trusted, pair from the box itself or over SSH; the
dialog says so when the page is not a secure context.

### 6.3 Secure-context features on the LAN (R5)

`http://A:P` is not a secure context (HTTPS, localhost and loopback are), so the browser withholds
the APIs that need one. The audit, by a search of `internal/admin/static` at `9f98e0c1`:

| Feature | Where | On the LAN |
|---|---|---|
| `crypto.subtle`, for each chunk's `Content-Digest` | `app.js:2847-2875`, `:2908-2916` | missing: chunks carry no digest; decided below |
| `crypto.getRandomValues`, for the shuffle | `internal/admin/static/player/audio.js:705-731` | works: it is not a secure-context API; its comment's premise ("isSecureContext is true") is corrected |
| `navigator.clipboard.writeText`, the pairing dialog's Copy buttons | `app.js:3443-3461` | missing: the catch selects the text for a copy by hand, as it does today for a write that fails |
| `navigator.mediaSession`, the lock-screen controls | `audio.js:754-788` | guarded before each use: a browser that offers it to secure contexts alone shows no lock-screen controls, and playback is unaffected (not measured per browser) |
| the upload's head and tail fingerprint, its resume identity | `app.js:2161-2171` | works: the console's own SHA-256, written for a page without SubtleCrypto |
| everything else: EventSource, fetch, `<audio>`, a dropped folder, localStorage | none | no secure context needed; nothing else in the console asks for one |

**The uploads' integrity, decided.** On the LAN each chunk is checked for its length and its
offset, and each file for its size, and the resume identity (the modified time and the head and
tail fingerprint) still keeps another version of a file from resuming (`admin-console.md`, the
B176 bullet); nothing checks a chunk's bytes. The page says so before an upload begins, where
`isSecureContext` is false ("From this device, uploads are checked for length only: the browser
gives the console SHA-256 only on the bridge's own machine or over HTTPS"), and at the end, in
place of "this browser did not expose SHA-256" (`app.js:2970-2974`), which blames the wrong thing.
Not taken: hashing each 4 MiB chunk with the console's own SHA-256. It would run on the page's
thread at about the rate the owner declined for an up-front whole-file digest (about 82 MB/s in the
page, B176), three files at a time, on a phone; and over plain HTTP whoever can change a chunk can
change the page that would hash it, so what it would add is a check on bytes in transit, which
TCP's checksum and each link's CRC already check. The PR corrects `chunkDigestHeaders`'s comment
("Both supported admin deployments are one", `app.js:2856-2858`).

### 6.4 The residuals

1. **No login.** Every device on the network is trusted with every action of §6.1.
2. **Plain HTTP**, as §6.2: anyone who can see the traffic can watch, and catch a pairing code.
3. **Relays inside the network** (§4.1): a router's SNAT or hairpin NAT, a VPN gateway, a reverse
   proxy that rewrites Host.
4. **A router's port-forward of P.** Refused by the source check where the router keeps the
   internet source, and by the Host check for any browser; a router that rewrites the source
   carries in any client that writes its own Host. The doctor's hint and the config comment say
   not to forward the port.
5. **Everything on the same subnet**, a Wi-Fi's guests among them, unless the guest network is a
   subnet of its own, which the source check then refuses.
6. **A machine that moves.** A laptop on another network that hands it the same address opens its
   console there; DLNA has the same residual (`dlna-upnp.md`, the B48 bullet). The posture is for
   a box that stays home.
7. **Firewalls.** The bridge adds no rule; a host firewall (ufw, firewalld, Windows Defender's
   inbound rules) decides whether the network reaches P, which the doctor cannot see. The repo's
   Windows deploy script opens the admin port on every profile for the loopback console
   (`deploy/windows/firewall-bridge-windows.ps1:57-69`), which this posture makes matter (§11).
8. **A Windows tunnel without the point-to-point flag** (Wintun: `dlna-upnp.md`, the selection
   bullet) is not told from a LAN, so an address on one is bound.
9. **Docker.** On a bridge network the container's address is not the home network's, and the
   source and Host checks refuse what a published port delivers; with host networking it is a
   native install.
10. **Rollback.** With the posture on, 0.2.3 refuses the settings, so the crash rollback of a
    0.2.4 install on probation does not put it back (§2.5, item 6).
11. **A restore onto another box** carries `adminLANAddress`; there the bind waits for an address
    that box may never hold, and the line and the doctor say so.
12. **No limits of its own.** No rate limit or header cap is added: the network gets the console's
    whole capacity, as loopback does.

## 7. Coexistence

- **Pairing a phone from a LAN browser.** The QR the Devices page mints (`POST /api/tokens`)
  carries the API's endpoints (`admin.Deps.Endpoints`: `auth-tls.md`, the bullet on the QR's
  `urls=`), never the Host the browser used, so a link minted through either socket is the same
  link. A laptop's browser shows the QR, and the phone scans it. A phone that is itself the
  browser cannot scan its own screen: it copies the dialog's fields, by selection on the LAN
  (§6.3), into the app's manual pairing. A tappable `onebit://pair` link would serve that phone;
  it is not in this PR (§11). Watchers: §6.2.
- **The iOS app** is unchanged. It pairs and syncs over the API port with its pinned certificate,
  and the console is not part of the wire protocol (`README.md:229`). No shape in `PROTOCOL.md`
  changes; one prose sentence does (§10.5).
- **mDNS** is unchanged: the Bonjour service still advertises the API, the console is not
  advertised, and its `.local` name is accepted on the LAN socket (§4.5).
- **Public mode** is unchanged: Validate refuses the field, `--public` refuses the flag, no plan is
  built there, and `boundaryMiddleware`'s public branch is untouched.
- **1-bit Cloud**: never (§2.5, item 7).
- **The API port, DLNA, OpenHome and the loopback track route** keep their listeners and their
  rules. The LAN socket shares only the admin port, which the OpenHome port clash reads already.
- **Tailscale.** The tailnet's ranges are refused as a LAN address (§2.2), and a tailnet peer is
  outside the prefix (§4.1); a subnet router that rewrites tailnet sources into the LAN is §6.4,
  item 3.
- **B634.** With the posture on, a phone on the network can drive the console's player on the
  bridge's own DAC, which is the point of shipping the two together.

## 8. Alternatives weighed

1. `adminLAN: true`, with `adminAddress` moved onto the private address (the brief's shape): lost
   to §1: ten local probes to move onto a second endpoint, a rewrite that can write a config
   Validate refuses, and off as two edits.
2. An allowlist of client networks (`adminLANAllow: [CIDR, …]`): lost: a second field, past the
   budget; its default would be the on-link network anyway; and a list is where `0.0.0.0/0` gets
   written.
3. A wildcard bind (`0.0.0.0:P`) with a source check: lost: it takes connections on every address
   the box has, the tailnet's, a VPN's and a public one, so one missing check opens all of them.
   DLNA binds a wildcard because SSDP needs one (`dlna-upnp.md`, the B48 bullet); the console needs
   no multicast.
4. A login or a passcode: refused by the owner. Public mode is that posture.
5. TLS on the console: outside the budget. A self-signed certificate warns in every browser, and
   only a trusted one would bring a secure context.
6. Following every LAN address, as the OpenHome surface does (`lan.Look`): lost: a second kind of
   listener, and an address nobody named.
7. Admission at accept, the connection closed unread, as OpenHome's: lost: the visitor gets no
   words, and R1 asks for the policy through the handler.
8. Refusing some actions on the LAN socket (roots, updates, tokens): lost: running the bridge from
   a browser needs roots and pairing, and two consoles that differ are a second policy to keep in
   step.
9. A Settings switch for the posture: lost: the console would change its own reach, a LAN visitor
   could not turn it on in any case, and a person at the box has the file.
10. The console's own SHA-256 for chunk digests on the LAN: lost (§6.3).
11. Refusing a write with no Origin on the LAN socket: deferred to a measurement (§4.3).
12. An exemption from the derived variables: lost (§2.3).
13. Restricting the arrival interface: deferred (§4.1).

## 9. The tests, red first

The tests come first, with a names-only shim so that they compile at main, and are shown red
there. The loopback rows are the controls: green at main and after, so loopback's behaviour does
not tighten silently (R6).

| Test | Package | What it pins | Red at main because |
|---|---|---|---|
| `TestTheLANAddressIsRefusedForEachReasonWithoutItsValue` | `internal/config` | every row of §2.2, each refusal naming the field and none of a secret-shaped value planted in it (R10) | the key is unknown: one other refusal for all |
| `TestALANAddressTakesEachPrivateClass` | `internal/config` | the three RFC 1918 blocks and a ULA load | the key is unknown |
| `TestNoLANAddressWritesNoKey` | `internal/config` | a config without the posture marshals with no `adminLANAddress` key, by unmarshalling and looking for the key (R8, rollback) | no field |
| `TestTheLANAddressIsSetByItsVariable` | `internal/config` | `BRIDGE_ADMIN_LAN_ADDRESS` sets it and is refused alike | no field |
| `TestTheLANClassIsThePrivateBlocksLessTailscales` | `internal/config` | `ConsoleLANClass` | no function |
| `TestTheLANSocketAdmitsOnlyItsOwnNetwork` | `internal/admin` | every source of §4.1 through `Handler()`, a request stamped with the LAN socket and its `RemoteAddr` (R1) | no LAN socket |
| `TestEachHostIsAnsweredAsItsSocketsRuleSays` | `internal/admin` | §4.2, both columns (R6) | the LAN column; the loopback column is green |
| `TestEachOriginIsAnsweredAsItsSocketsRuleSays` | `internal/admin` | §4.3 for a PATCH, a bodyless POST, an upload chunk's PUT and `GET /api/events` (R6, R7) | the LAN column |
| `TestTheEventStreamAndTheCSRFGuardShareOneOriginRule` | `internal/admin` | both gates call `originMatches` (structural) | no such function |
| `TestTheLoopbackGuardsKeepTheirAddressWhileTheLANSocketServes` | `internal/admin` | the LAN socket's bind leaves the loopback socket's Origin and hint as they were (R2) | no LAN socket |
| `TestEachRefusalSaysWhereToOpenTheConsole` | `internal/admin` | §4.4's 421 and 403 bodies, per socket, with and without the LAN socket listening (R7) | the words |
| `TestTheLANSocketsRefusalsNameNoPeerAndSpareLoopbacksLog` | `internal/admin` | §4.4's log lines | no such line |
| `TestTheMenuSaysWhereTheConsoleAnswers` | `internal/admin` | §5.1's three states (R9) | no line |
| `TestTheConsoleRouteReportsTheSocketsState` | `internal/admin` | §5.2 | no route |
| `TestALANSocketThatFailsLeavesLoopbackServingAndClosesItsConnections` | `internal/admin` | §3.5 (R2) | no LAN socket |
| `TestAShutdownStopsBothSocketsInOneWindowAndBindsNothingAfter` | `internal/admin` | §3.5 (R2) | no LAN socket |
| `TestAPairingQRMintedThroughTheLANSocketIsLoopbacks` | `internal/admin` | §7, the QR | no LAN socket |
| `TestAnUploadWithoutSubtleCryptoSendsNoDigestAndSaysSo` | `internal/admin`, under node | `globalThis.crypto` with no `subtle` and `isSecureContext` false: the chunk's PUT carries no `Content-Digest`, the server takes it through the LAN socket, and the end says length only, naming the cause (R5) | the words |
| `TestThePairingCopyFallsBackWithoutTheClipboard` | `internal/admin`, under node | no `navigator.clipboard`: the text is selected, nothing throws (R5) | green at main: the control for the fallback |
| `TestTheUploadPageSaysChunksAreCheckedForLengthOnly` | `internal/admin`, under node | §6.3's note before an upload | no note |
| `TestServeOpensTheConsoleOnItsLANAddress` | `cmd/bridge` | the real serve with a handed loopback listener and a `lantest` box for the LAN bind: a client from inside the network gets the console, one from outside gets 403, and the banner's line | no plan |
| `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` | `cmd/bridge` | a bind that fails: serve starts, one WARN, the menu says this machine only, `/api/console` says waiting, both doctors WARN, the loopback socket refuses `Host: A:P` and `Origin: http://A:P`, then a tick binds it and the line changes (R9) | no plan |
| `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` | `cmd/bridge` | `bridge status`, `refuseIfBridgeMayBeRunning`, `isAdminAlive` and `probeAdminRunning` against that serve (R3) | the key is unknown |
| `TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket` | `cmd/bridge` | `tryLibraryViaAdmin` (R3) | the key is unknown |
| `TestOnlyTheBindAndItsChecksReadTheLANAddress` | `cmd/bridge` | the AST walk of §3.6 (R3) | no field |
| `TestALANSocketThatDoesNotBindIsNoFailedStart` | `cmd/bridge` | B660's count is untouched | no plan |
| `TestInitWritesTheLANAddressItIsGiven`, `TestInitRefusesALANAddressWithPublic`, `TestInitAddressFlagsErrorChecksTheLANAddress` | `cmd/bridge` | §2.5, item 4 (R8) | no flag |
| `TestARewriteKeepsTheLANAddressWithTheAdminAddress`, `TestARewriteThatNamesAnAddressFlagSaysTheLANAddressIsNotKept`, `TestARewriteToPublicSaysTheLANAddressIsNotKept`, `TestARewriteDropsAKeptLANAddressTheCheckRefuses` | `cmd/bridge` | §2.5, item 5 (R4) | no field |
| `TestTheConsoleLANLineSaysEachState` | `internal/doctor` | §5.3's table (R9) | no check |
| `TestRunPrintsTheConsoleLANLineAfterTheAdminPort` | `internal/doctor` | the line's place | no check |
| `TestBridgeDoctorReadsTheRunningBridgesConsole` | `cmd/bridge` | the CLI asks §5.2 and words its answer | no route |

Existing guards that must keep passing, and cover the change without edits:
`TestEveryConfigLeafIsEnvSettable`, `TestPriorInstallTagsAreConfigs`,
`TestEveryServeHTTPServerRedactsPeerAddresses` (it finds the second `http.Server`),
`TestEverythingBelowTheBrandIsInTheDrawer` and `TestClientChunkDigestIsAcceptedByTheServerParser`.
No test binds the host's LAN addresses or lists its interfaces (`openhome.md`, the tests bullet):
the console's tests are requests through `Handler()`, and serve's take a `lantest` box.

## 10. The PR

### 10.1 Branch and commits

`feat/b989-lan-console`, from main. Two commits: the tests with a names-only shim, red on the LAN
rows and green on the loopback rows, then the change. Pushed and opened only with the owner's
go-ahead.

### 10.2 Files

- `internal/config/config.go` (the field, Validate's call), `internal/config/console_lan.go` (new:
  `ValidateConsoleLANAddress`, `ConsoleLANClass`).
- `internal/admin/admin.go` (the two servers, the socket's description, the per-socket guards and
  words), `internal/admin/middleware_auth.go` (the boundary per socket),
  `internal/admin/handlers_events.go` (the stream's gate on `originMatches`),
  `internal/admin/console_lan.go` (new: the plan, its retry, its state, `GET /api/console`),
  `internal/admin/handlers_pages.go` (`pageData`), the templates `layout.html` (the menu's line,
  the pairing dialog's sentence), `settings.html` and `upload.html`, and `static/app.js` (the
  upload's notes, the pairing sentence, the comment), `static/app.css` (the line's style) and
  `static/player/audio.js` (the comment).
- `internal/advertise/endpoints.go` (the `.local` name, exported for the bind).
- `internal/doctor/doctor.go` and `internal/doctor/console_lan.go` (new: the check and its words,
  `Deps.ConsoleLAN`).
- `cmd/bridge/main.go` (the plan, the first attempt, the banner, the test seams),
  `cmd/bridge/serve_console_lan.go` (new: the bind and its reasons), `cmd/bridge/doctor.go` (both
  doctors' sources), `cmd/bridge/init.go` and `cmd/bridge/init_rewrite.go` (the flag and the
  rewrite's rule), `cmd/bridge/banner_url.go` (the LAN line).
- Tests: §9.
- Records: §10.5.

### 10.3 Negative controls

Each mutation is made in a copy, its named test shown red, and the tree left as committed:

1. the source check removed: `TestTheLANSocketAdmitsOnlyItsOwnNetwork`.
2. the LAN socket's Host compared without its port: the port rows of
   `TestEachHostIsAnsweredAsItsSocketsRuleSays`.
3. the LAN socket given loopback's Host rule: the loopback rows of its LAN column.
4. loopback's Origin widened to the LAN authority: the loopback column (B193's rule).
5. the event stream's gate left on the old matcher: its rows of
   `TestEachOriginIsAnsweredAsItsSocketsRuleSays`.
6. a LAN bind's failure sent to `adminErr`:
   `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo`.
7. the menu's line read from the field rather than the socket: the same test's line.
8. `omitempty` dropped: `TestNoLANAddressWritesNoKey`.
9. one refusal quoting its value: `TestTheLANAddressIsRefusedForEachReasonWithoutItsValue`.
10. the rewrite's keep dropped: `TestARewriteKeepsTheLANAddressWithTheAdminAddress`.
11. a local probe dialling the LAN address:
    `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` and
    `TestOnlyTheBindAndItsChecksReadTheLANAddress`.
12. `chunkDigestHeaders` throwing without SubtleCrypto:
    `TestAnUploadWithoutSubtleCryptoSendsNoDigestAndSaysSo`.

### 10.4 Runs

`make fmt vet test build-all` through the build slot, on `go.mod`'s toolchain; `make docs-check`;
and, if a rules entry moves, `scripts/doc-split/verify_moves.py`. The browser check is the owner's
(§11).

### 10.5 Records

- `.claude/rules/auth-tls.md`: its first bullet, "The admin console is loopback-only with no auth,
  by design" (`:38-41`), says loopback only unless the operator opens it on the home network with
  `adminLANAddress`; a new bullet holds §4's rules (the source, the Host and the Origin of the LAN
  socket, the `.local` name, the words, no peer in a line); the `csrfGuard` bullet's loopback
  sentence (`:91-98`) becomes per socket. Its `paths:` gain `internal/config/console_lan*.go`,
  `internal/admin/console_lan*.go` and `cmd/bridge/serve_console_lan*.go`. It has about 33 KB of
  its budget left, so the new rules go here.
- `.claude/rules/admin-console.md`: its first bullet (`:16-24`) per socket, and a sentence for the
  upload's length-only note. It has about 1.7 KB left: if the edit would pass the budget, its
  largest entry moves out first (`scripts/doc-split/move_entry.py .claude/rules/admin-console.md
  --largest`). `config-lifecycle.md`, 0.6 KB from its budget, gains nothing.
- `CLAUDE.md`: the "Admin console is loopback-only" line (`CLAUDE.md:103`) and its long form
  (`ops/History-Cross-Repo.md:15`), in one commit; the `internal/admin` row of the package table
  (`CLAUDE.md:58`).
- `config/bridge.yaml.example`: the loopback paragraph of the `deployment.mode` block (`:134-136`),
  and a block for `adminLANAddress`: what it opens, its lack of a login, a fixed address for the
  box, a firewall that must let the network in, and no port-forward.
- `docs/docker.md`: `BRIDGE_ADMIN_LAN_ADDRESS` beside `BRIDGE_ADMIN_ADDRESS` in the variables
  table (`:324`), and under "Admin console access" (`:508`) that the posture needs host
  networking.
- `README.md:229`, `AGENTS.md:170` and `AGENTS.md:273`: their loopback-only sentences.
- The code's own words for the boundary, which would read as the reason for the code beside them
  (CLAUDE.md, on comments that rot): `DeploymentConfig`'s doc (`config.go:1261-1266`), Validate's
  `adminAddress` comment (`config.go:4008-4015`), `validateLoopbackAddress`'s (`:4785-4789`),
  `loopbackOnly`'s, which names this very feature as a regression to catch (`admin.go:2676-2681`),
  the server's and the body cap's "binds loopback" (`admin.go:68-75`, `:2286-2291`), the event
  stream's (`handlers_events.go:81-86`), and the Diagnostics page's `/metrics` sentence, "on this
  loopback listener only" (`internal/admin/templates/diagnostics.html:247-248`).
- `PROTOCOL.md:2638`: "the install endpoint is admin-loopback only" becomes the console's, which a
  loopback bridge serves on loopback and, where its operator opened it there, on the home network
  with no login. The iOS repo copies the file in a companion PR (the Mirror-PR rule); no wire
  change, and `ProtocolVersion` stays 1.
- The engineering log: an entry,
  `ops/engineering-log/2026-11/<date>-b989--feat-b989-lan-console.md`, with what was measured
  (§3.4's socket where the session can take an address away; the bind's errnos per OS where CI
  shows them; the owner's browser check).
- The backlog: the follow-ups of §11, item 4.
- The privacy page, for the owner (its text is B683's):

  > If you open the bridge's console to your home network (`adminLANAddress`), it has no password
  > and travels over plain HTTP. Any device on that network can open it, see what it shows (your
  > library, your listening history, your devices' names and the bridge's logs) and change the
  > bridge's settings, its music folders and files, and its paired devices. It is off unless you
  > turn it on, and the bridge answers it only to devices on the network that address belongs to.

## 11. Open points for the owner

1. The field's shape (§1): `adminLANAddress`, as here, or the brief's `adminLAN: true` with
   `adminAddress` on the private address.
2. The Windows deploy script's admin rule on every profile (§6.4, item 7): restrict it to the
   Private profile in this PR, or leave it.
3. The `.local` name (§4.5): taken as here, or IP literals alone.
4. Follow-ups for the backlog: a tappable `onebit://pair` link in the pairing dialog (§7);
   refusing a write with no Origin on the LAN socket, once measured in Safari, Chrome and Firefox
   (§4.3); restricting the arrival interface (§4.1); a menu line that follows the socket without a
   page load.
5. The browser check (an owner gate): a phone and a laptop on one network open the console at
   `http://A:P` and at `http://<name>.local:P`; the menu's line; a save; an upload and its
   length-only note; the pairing dialog's copy by selection; a refusal of another name.

## 12. The brief's review items, and where each is settled

| Item | Settled in |
|---|---|
| R1, the source policy, routed and VPN clients | §4.1; tested through the handler (§9) |
| R2, two sockets, one console | §3.1, §3.5 |
| R3, one canonical local endpoint | §1, §3.6 |
| R4, the init rewrite | §2.5, item 5 |
| R5, plain HTTP's secure context | §6.3 |
| R6, the loopback exception stays | §4.2, §4.3 |
| R7, the other gates and their hints | §4.3, §4.4 |
| R8, every config path | §2.5 |
| R9, the bound state, never the config's wish | §3.2, §5 |
| R10, refusals never echo the value | §2.2 |
