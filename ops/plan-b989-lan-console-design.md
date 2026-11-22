# B989: the console on the home network, an explicit opt-in with no login

Design and PR plan, 2026-11-22, for bridge 0.2.4 beside B634 (the console plays to the bridge's own
DAC). This round writes no code.

**Status.** For review, round 3. Binding: the owner's decision of 2026-11-22 (open the bridge's
console from another computer or phone on the home network, without a login, as an explicit
opt-in), the first brief's review items R1 to R10, the second brief's D1 to D16, of which D11 to D16
win wherever other text differs, and the third brief's F1 to F12, of which F9 to F12 win wherever
other text differs; each is settled below and indexed in §12. Rounds 1 and 2 stand where round 3
says nothing different: one field beside a loopback `adminAddress` (§1), a second socket that
serves the same handler (§3.1), a description per socket (§3.1), one Origin matcher that the event
stream shares (§4.3), the loopback endpoint audit (§3.7), who owns the retry and the shutdown (§3.4,
§3.6), the kernel's hold of the interface (§3.3) and the policy read again (§3.5). One choice
departs from the shape the first brief sketched, and §1 says why before anything else. Every
current behaviour cited is `path:line` at bridge main `9f98e0c1`.

No host, address or key path appears here. `A` is the private address an operator opens the
console on, `P` the console's port (`adminAddress`'s, 7789 by default), `A2` any other address,
and `<name>.local` the box's mDNS name.

**In one paragraph.** A loopback bridge gains one optional field, `adminLANAddress`: a private IP
address of the box (RFC 1918 IPv4, or an IPv6 unique local address outside Tailscale's range).
With it set, `bridge serve` serves the one console on a second socket bound to `A:P`, beside the
loopback socket, which works exactly as it does today. The kernel holds the second socket to A's
interface (on Linux it takes no connection that arrives on another interface; on macOS and Windows
a handshake completes only with a peer that A's interface reaches), and where a platform gives no
such hold, or the kernel refuses it, the socket stays closed and says why. The socket admits a
connection only from inside A's own network, answers only a request that names `A:P` or
`<name>.local:P`, and refuses a write that a page of another origin sends. Every 5 seconds serve
reads the box's interfaces again: a socket whose interface, address, network or name no longer
reads as it did at its bind is retired, and a reading that fails in any part retires it too. From
the moment a retirement begins its socket admits no request, every event stream on it ends, and
every connection it holds is closed, kept-alive ones included; another socket binds only on a
reading taken after all of that, and only where that reading admits A. It has no login and speaks
plain HTTP, so anyone on that network can watch and use the console. If A does not bind at start,
serve starts all the same with the console on loopback, says so at WARN, and tries again every 5
seconds. The console's menu, `bridge doctor` and the serve banner say which it is, from the socket's
state and from where the address came at the start (the file, the environment, or both), never from
the config as it reads now; and while bridge.yaml holds the key, empty or not, they say, as `bridge
init` says before it saves, that bridge 0.2.3 and earlier refuse the file and so would not be put
back by an update's rollback. Public mode, the API port the app uses, the app itself and 1-bit
Cloud are unchanged; public mode refuses the field.

## 0. At a glance

| Today | Where | B989 |
|---|---|---|
| The console binds `adminAddress` alone, which loopback mode holds to a loopback host | `internal/config/config.go:4069`, `:4785-4814`; `internal/admin/admin.go:2226-2240` | `adminAddress` unchanged; `adminLANAddress` adds a second socket on `A:P` (§2, §3) |
| A socket bound to an address takes a connection to it from any interface; OpenHome holds its sockets to their interface where it can, and leaves one listening unrestricted where the kernel refuses | `internal/player/openhome/restrict_linux.go:11-20`; `.claude/rules/openhome.md`, the interface bullet | The kernel holds the LAN socket to A's interface, or the socket stays closed (§3.3) |
| OpenHome reads the box's addresses every 5 s and keeps what is bound when a listing fails; `lan.HostInterfaces` lists an interface whose addresses do not read with none | `openhome.md`, the listener bullet; `internal/player/lan/lan.go:49-60` | The LAN socket's policy is read again every 5 s; a change, or a reading that fails in any part, retires the socket: it admits no request from then on, its streams end, its connections close, and another binds only on a reading taken after that (§3.5) |
| Every request must come from loopback | `internal/admin/middleware_auth.go:28-39`; `admin.go:2682-2696` | The loopback socket: unchanged. The LAN socket: a source inside A's on-link network and private class (§4.1) |
| A Host must be a loopback name or literal, any port or none, or empty | `admin.go:2719-2739` | The loopback socket: unchanged. The LAN socket: `A:P` or `<name>.local:P`, each spelling judged alike in Host and Origin by one function (§4.2) |
| An Origin must be a loopback name on the listener's port, in `csrfGuard` and in the SSE gate | `admin.go:2547-2605`; `internal/admin/handlers_events.go:63-68` | Per socket, through one function both gates call (§4.3) |
| The guards' address is one field, `boundAdminAddr`, which `ServeListener` sets | `admin.go:1813`, `:2248` | Each socket's own description, stamped on the requests it carries (§3.1) |
| An admin listener's error stops serve, and the API with it | `cmd/bridge/main.go:7386-7409` | Still so for the loopback socket; the LAN socket's never stops serve (§3.6) |
| A refusal's hint names loopback and an SSH tunnel | `admin.go:63-64`, `:2463-2476`, `:2755-2780` | Per socket (§4.4) |
| An upload without SubtleCrypto says "this browser did not expose SHA-256" at its end | `internal/admin/static/app.js:2970-2974` | Said before the upload and after it, naming the real cause (§6.3) |
| Load records which leaves a `BRIDGE_*` variable set, and nothing says where a running bridge's setting came from | `internal/config/env.go:235-238`, `:274-278` | The plan records the address's source at boot; `/api/console` and both doctors report it, apart from the file as it reads now (§2.6) |
| A key 0.2.3 does not know makes it refuse the file, whatever the key's value | `internal/config/wholenumbers.go:46` | Said before init saves, in the config example, on the console and in both doctors while the key is in the file, empty or not (§2.7) |
| Nothing says where the console answers | none | A line at the foot of the console's menu, `GET /api/console`, the `console-lan` and `console-lan-rollback` lines in both doctors, a banner line (§5) |

## 1. Where this departs from the brief

The brief sketches a boolean opt-in (`adminLAN: true`) with `adminAddress` moved onto the private
address. This design keeps `adminAddress` as it is, loopback, and puts the private address in the
opt-in field itself: `adminLANAddress: <A>`. It is still one field, inside the budget, and every
rule the brief asks for holds. Why:

1. **R3's hazard does not arise.** Ten call sites in six files dial `adminAddress` as the bridge's
   local endpoint (§3.7). With `adminAddress` on a private address, each would have to learn a
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
4. **Rollback is the same either way.** An older bridge refuses any key it does not know, whatever
   its value (`internal/config/wholenumbers.go:46`, `KnownFields(true)`), so neither shape loads on
   0.2.3 while its key is in the file, an empty one included, and both cost nothing while the key
   is absent (§2.5, item 6; §2.7).

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

A value the variable set is refused in the same words with the variable named beside the field,
"adminLANAddress, set by BRIDGE_ADMIN_LAN_ADDRESS: …", and never its value (D7): Load knows which
leaves a variable set (`envSet`, §2.3).

`config.ConsoleLANClass(a netip.Addr) (netip.Prefix, bool)` gives the private class an address
falls in (one of the three RFC 1918 blocks, or fc00::/7) and false for any other, Tailscale's
range included, so Validate and the source policy (§4.1) refuse the same addresses. It lives in
`internal/config/console_lan.go`, so Validate and the console (`internal/admin`, which imports
config and no player package) read one definition.

Validate does not ask whether this box holds A. It is a shape check, never a question about the
host (`.claude/rules/config-lifecycle.md`, the `Validate()` bullet), and an address the box does not
hold yet is runtime state (§3.2). Nor does it ask which platform reads it: a ULA loads on macOS,
where its socket says that the platform cannot hold it (§3.3), so a config carried from one box to
another keeps loading.

### 2.3 The environment variable

`BRIDGE_ADMIN_LAN_ADDRESS`, derived from the field as every leaf's variable is
(`internal/config/env.go:12-34`, held by `TestEveryConfigLeafIsEnvSettable`), with no exemption: a
variable someone sets is as explicit as the file. It takes the same checks. A console save writes
the file's own value for it, never the variable's (B761's rule, `config-lifecycle.md`).

Two facts of the derivation decide what turning it off takes (D7). A variable set to the empty
string sets nothing (`env.go:223-224`, `:252`), so the variable can open the console over a file
without the key, and can never close it over a file that has one. And Load records the leaf of
every variable that set one (`envSet`, `:235-238`, `:274-278`), which is where the plan takes the
address's source from (§2.6).

### 2.4 Who reads it

Serve's boot snapshot, once, to build the bind's plan (§3.2) and record where the address came from
(§2.6); Validate; `bridge init`; and `bridge doctor`, for what the file and the running bridge say
(§5.3). No consumer reads it live, so a change takes a restart, and every surface that says where
the console answers reads the socket's state, never the field (R9). A console save of other
settings keeps a hand edit of it, since B535's merge writes a leaf another writer changed as the
file holds it (`config-lifecycle.md`, the lock-and-merge bullet). The key's presence, which the
value alone does not show (§2.7), is read apart, by `config.LANKeyOf`.

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
   `initAddressFlagsError`. That function may name a typed address in its other refusals (its
   docblock, `:136-140`); this flag's refusal never names its value (D6, R10). It is Validate's
   refusal with the flag in place of the field, "--admin-lan-address: …", since what is refused
   there can be a secret pasted onto the wrong flag, and init's output reaches a terminal's
   scrollback and an installer's log. With `--public` the flag is refused before anything is
   written (exit 2), in words that name the flag and public mode and no value. A run that writes
   the field says what it opens and what it costs the rollback before it saves (§2.7), and prints
   the LAN URL in its footer (§5.4). init's prompts ask nothing about it: the posture is opted into
   by name.
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
6. **The updater (B613, B761).** A save through `Save`, `SaveIf` or the holder's merge marshals the
   config whole (`internal/config/config.go:4984-4996`, `Config.marshal` at `:5008-5031`;
   `internal/config/outside.go:182`), and `omitempty` writes no key for an empty value, so a file
   the bridge wrote with the posture off is what 0.2.3 loads, and an empty key a hand edit left is
   gone at the next such save of any setting (a restore writes its snapshot's bytes as they are).
   While the key is in the file, an
   empty one included (D16), 0.2.3 refuses the file, and the existing rules decide: an install's
   settings check is the candidate's own, which takes it; the crash rollback asks its backup to
   load the settings and puts none back that refuses them (`internal/updater/probation.go:198-229`);
   and the console's Roll back is refused once the installed version has opened the data dir
   (`RollbackRefusal`, `config-lifecycle.md`, the B692 bullet). So while the key is in the file,
   the crash rollback of a 0.2.4 install still on probation is lost, recorded in the backup's words
   ("the backup, …, refuses this bridge's settings", `cmd/bridge/serve_crashed_install.go:230`),
   which the doctor's probation lines show (`cmd/bridge/doctor_install_probation.go`), and removing
   the line, the key itself, restores it at the next start that does not end cleanly (§2.7). The
   variable costs none of it: 0.2.3 does not know it and ignores it. There is no save to refuse
   here: B761's `ChecksOffRefused` (`internal/updater/checksoff.go:332-342`) guards a console save,
   and this field has none.
7. **Managed settings and 1-bit Cloud.** Public mode refuses the field (§2.2, first row), and every
   1-bit Cloud tenant is a public-mode bridge, so the posture never reaches one: a control plane
   writes no such key, and a tenant given the variable fails its load, which opens nothing.
   `managedSettingImplies` (`config.go:1460`) gains no entry.

### 2.6 Where the address came from (D7, D14)

- **Recorded at boot, apart from the file.** Serve's plan takes from the boot snapshot the address
  it binds and its source: `file` (the file sets the key and no variable set the leaf),
  `environment` (`BRIDGE_ADMIN_LAN_ADDRESS` set the leaf, `envSet`, and the file has no key) or
  `both` (the variable set the leaf over a file that has the key, whatever the key holds, the
  variable winning). It records too what the file's key held at boot, apart from the address it
  binds (F7): `absent`; `empty`, an empty value or `null`; `address`, with the file's own address;
  or `not_an_address`, a value the field's check refuses, which nothing ever repeats (R10). A value
  the variable overrides is never checked at the load, since Validate checks the leaf as the
  variable left it, so it can be anything, a pasted secret included: the plan reads the file's own
  value from the boot snapshot's bytes with `config.LANKeyOf` (§2.7), which returns the key's
  presence and its value, and keeps the value only where `ValidateConsoleLANAddress` takes it. The
  socket's state carries all of it, `GET /api/console` reports it (§5.2), and none of it is read
  again while serve runs: what the file says now is the doctor's own read, said apart (§5.3).
- **What clears each.** `file`: remove the `adminLANAddress` line from bridge.yaml, the key itself
  (§2.7), and restart. `environment`: unset `BRIDGE_ADMIN_LAN_ADDRESS` where the service is started
  (the systemd unit's `Environment=` or `EnvironmentFile=`, launchd's `EnvironmentVariables`, the
  container's `-e` or Compose's `environment:`) and restart; a variable set to the empty string
  sets nothing (§2.3), so emptying it is unsetting it. `both`: the two, since unsetting the
  variable alone makes the file's address the one serve binds at its next start (D14), and
  removing the key alone leaves the variable's (D7). `both` over an empty key
  (`adminLANAddress: ""` beside the variable, F7): unsetting the variable alone turns the console
  off the home network at the next start, and the empty line costs the rollback (§2.7) until it is
  removed. `both` over
  a key that is `not_an_address`: unsetting the variable alone makes the next start refuse
  bridge.yaml, in Validate's words, until that line is corrected or removed.
- **The doctor is not the service.** `bridge doctor` runs with its shell's environment, which need
  not be the service's: it never judges the source from its own environment. It reports the
  running bridge's source as `GET /api/console` gives it, and, each named as what it is, what the
  file says now (the key, its value) and whether its own environment sets the variable.
- **One wording.** One function in `internal/doctor` words the source for both doctors, the
  console's line and the banner: "set in bridge.yaml (adminLANAddress)", "set by
  BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment", "set by BRIDGE_ADMIN_LAN_ADDRESS in the
  bridge's environment, over bridge.yaml's adminLANAddress", each followed by its clearing
  sentence, and for `both` by what the file's key held at boot: "unsetting the variable alone opens
  the console at bridge.yaml's address at the next start" (`address`, the address named, as the
  boot read it); "unsetting the variable alone turns the console off the home network at the next
  start; bridge.yaml's empty adminLANAddress line still costs the rollback: remove it" (`empty`);
  "unsetting the variable alone makes the next start refuse bridge.yaml: its adminLANAddress does
  not read as an address" (`not_an_address`, the value never repeated).

### 2.7 The rollback, said before it is lost (D8, D15, D16)

What costs the rollback is the key in the file, never the posture. An older bridge's strict
decoding refuses a key it does not know whatever its value (`wholenumbers.go:46`), so
`adminLANAddress: ""`, which turns the listener off, blocks 0.2.3 exactly as an address does
(D16). An older bridge ignores a variable it does not know, since it derives its variables from its
own leaves (`env.go:12-34`), so the posture set by the variable alone costs no rollback: 0.2.3
starts on the file and serves the console on loopback alone. What is lost while the key is in the
file: the crash rollback of a 0.2.4 install on probation (the backup's settings check refuses the
file, `cmd/bridge/serve_crashed_install.go:229-233`, and the start says so), and a downgrade by
hand, which 0.2.3 refuses to start. The recovery, in every place below: remove the
`adminLANAddress` line from bridge.yaml, the key itself and not only its value; then every start
that does not end cleanly asks the backup again (`serve_crashed_install.go:28-31`) and puts it
back where the rollback's other checks allow it, and a downgrade by hand starts.

The key's presence is read from the file's bytes, never from the value: `config.LANKeyOf(raw)`
decodes them, without the strict check, into a struct whose field is a `yaml.Node`, which yaml.v3
fills for a key that is there whatever its value, `null` and an empty value included (it hands
such a field the node before it reads any value: `decode.go:492-495` at the v3.0.1 the module
builds), and leaves empty for a key that is not, through its own merge keys and aliases. It
returns the key's presence and, for a scalar, its value, which its caller checks before it keeps
it (§2.6). Serve records it at boot (§2.6), and each doctor reads the file as it is when it runs,
since the rollback reads the file as it is then.

Said before it happens:

| Where | When | What it says |
|---|---|---|
| `config/bridge.yaml.example`, the field's comment | always | what the field opens, with no login and over plain HTTP; then "While this key is in the file, even empty, bridge 0.2.3 and earlier refuse the file: an update's automatic rollback to one of them is not made, and one installed by hand will not start. Remove the whole line before you go back to one. BRIDGE_ADMIN_LAN_ADDRESS instead costs no rollback: earlier versions ignore it and keep the console on this machine." |
| `bridge init` | before `saveInitConfig` (`cmd/bridge/init.go:960`), and so before any service starts, in every run that writes the field, prompted or not (`--yes` and `--yes --force` alike, F7): `--admin-lan-address`, or a rewrite that keeps it (§2.5, item 5). A first install says it while the file holds no key; a rewrite that keeps one says it while the file still holds its own bytes, before the save replaces them (F12) | the no-login sentence (§5.4) and the rollback sentence, with its recovery (D15); a run that keeps the config whole writes nothing and says it in its footer |
| the console's menu line | while the boot source is `file` or `both`, or the file held the key at boot | the posture line (§5.1), then "bridge 0.2.3 and earlier refuse this bridge.yaml while it holds the adminLANAddress line, even empty: remove the line before you go back to one" |
| `bridge doctor` and the console's doctor | while the file holds the key now, empty or not, whatever the running bridge does | `console-lan-rollback` (§5.3) |
| the serve banner | as the menu line | one line (§5.4) |

The variable's case is said apart wherever the source is `environment`: "set by
BRIDGE_ADMIN_LAN_ADDRESS: bridge 0.2.3 and earlier ignore that variable, so a rollback to one of
them starts with the console on this machine only".

## 3. The bind

### 3.1 Two sockets, one console

- One `Handler()` (`admin.go:1895-2176`) behind two `http.Server`s, both built by one constructor
  with today's settings (`admin.go:2266-2310`): handshakelog's `ErrorLog`, `ReadHeaderTimeout`
  10 s, `ReadTimeout` 30 s, `IdleTimeout` 120 s, `WriteTimeout` unset (`admin-console.md`, the
  `WriteTimeout` bullet).
- Each server's `BaseContext` returns a context carrying that socket's description: its kind
  (loopback or LAN) and its bound address and, for the LAN socket, its policy: the reading it was
  bound on (§3.5), so the network it admits, the interface that holds it, the hold (§3.3) and the
  `.local` name it answers to, with the policy's version. The loopback socket's context is the
  shutdown context, as today; the LAN socket's is the policy's own, a child of the shutdown context
  that its retirement cancels, so every request on that socket, an event stream included, sees its
  context end when the policy is retired (§3.5). Every guard reads the description of the
  request's own socket, and a LAN socket's description never changes: a new policy is a new socket,
  a new server and a new version. A request that
  carries none, as one through a test's own `httptest` server around `Handler()` does, is the
  loopback socket's, which is today's behaviour, and needs a loopback source anyway.
- `boundAdminAddr` becomes the loopback socket's bound address alone, which the LAN socket never
  writes (R2: a second `ServeListener` call would have overwritten it). The LAN socket's state is
  an atomic pointer that the posture's surfaces read (§5).
- `ServeListener(ctx, lis)` stays the loopback-only path every existing test takes.
  `ServeConsole(ctx, lis, plan)` is serve's, with a nil plan for no posture.

### 3.2 At start

- runServe builds the plan from the boot snapshot (loopback mode with the field set) and makes its
  first attempt synchronously, once the console exists and before its goroutine starts
  (`main.go:7043`), so the banner can say what happened.
- **The platform first (D13).** Where §3.3 says this platform holds no interface for A's family (a
  ULA on macOS; any GOOS but linux, darwin and windows), the plan is `waiting` with
  `not_supported_here`, binds nothing, and is not tried again, since nothing at run time changes
  the answer.
- **One reading, whole or nothing (D12).** An attempt reads the interfaces through
  `lan.ReadInterfacesFrom(list, addrs)`, new beside `HostInterfaces`, which OpenHome keeps:
  `lan.ReadInterfaces` hands it `net.Interfaces` and `net.Interface.Addrs`, and a test hands it its
  own, so no package variable stands in for the host. It reads every interface and each one's
  addresses, and a failure of any of them fails the whole reading with a `*lan.UnreadableError`
  that names the interface, where `HostInterfaces` lists such an interface with no addresses
  (`internal/player/lan/lan.go:49-60`). It keeps what `HostInterfaces` drops (F8): an address that
  is no `*net.IPNet`, which `HostInterfaces` skips (`lan.go:61-65`), is kept as the address alone,
  as `prefixOf` keeps an address whose mask is no prefix (`lan.go:75-98`); an address that is no
  IP fails the reading. On Windows the same reading takes the interface table of A's family in one
  call (`GetIpInterfaceTable`, §3.3), and it is whole only where the table and the listing name the
  same interfaces of that family: an interface one names and the other does not, one added between
  the two calls among them, fails it. A reading that fails is `interfaces_unreadable`, and nothing
  is bound.
- In a whole reading the attempt finds the one interface that holds A, and A's on-link prefix from
  that interface's own entry for it. None is `address_not_on_this_box`. A listed more than once, on
  two interfaces or twice on one, is `address_on_two_interfaces`. A kept as the address alone, with
  no network (no mask, or one that is no prefix), is `address_has_no_network`: a socket would admit
  nothing but the box itself, so none is bound. An interface that is loopback or point-to-point is
  `address_on_a_tunnel`, and one that is not up or not running `interface_down`. On Windows the
  table is then judged as §3.3 says: a row of A's family that breaks the strong host model is
  `interface_not_strong_host`, naming that interface, and a table that does not read
  `host_model_unreadable`.
- **Then the socket, held before its bind (D11).** It binds `A:P` through a `net.ListenConfig`
  whose `Control` holds the socket to the interface's index (§3.3) before the bind and reads the
  hold back. An index that is not known (0 or less) is `interface_unknown`, a hold the kernel
  refuses is `restriction_refused`, and a read-back that does not name the index is
  `restriction_unconfirmed`; each makes `Control` return its error, so `Listen` closes the socket
  and nothing listens. The hold is the console's own function, never OpenHome's
  `restrictToInterface`, which leaves a socket it could not hold listening unrestricted and says
  why (`restrict_linux.go:11-20`). Then the bind's own errors: `EADDRNOTAVAIL` is
  `address_not_on_this_box` (the address left between the reading and the bind), `EADDRINUSE`
  is `port_in_use`, `EACCES` is `port_not_permitted`, anything else `bind_failed`.
- Bound: INFO `console listening on the home network`, with the URL, the network, the interface and
  the hold (`SO_BINDTOIFINDEX`, `IP_BOUND_IF` or `strong_host`); the state is `listening`, and the
  reading it was bound on is its policy (§3.5).
- Not bound: WARN `console: not listening on the home network`, with the reason's code and words
  and the address (Validate has taken it as a private IP by then, so it is no pasted secret), once
  per reason; the state is `waiting`, the console runs on loopback alone, and serve goes on. Nor
  is it a failed start for the install's probation: only the loopback socket's errors reach
  `adminErr` and `bindRefusalIsTheHosts` (`main.go:7386-7409`).

### 3.3 The interface, held by the kernel (D1, D11, D13)

A socket bound to A takes, by default, a connection to A that arrives on any interface: Linux and
macOS keep the weak host model, and a router, a VPN, a container bridge or a tailnet subnet route
can deliver one. The source policy (§4.1) cannot tell such a connection from a neighbour's when its
source lies inside A's prefix: a VPN that hands its clients addresses in the home network's range,
a host route to one, a spoofed source. So the kernel holds the LAN socket to A's interface, and
where a platform gives no hold the bridge can rely on, the opt-in stays closed there, said as
`not_supported_here` or the refusal's own reason.

| Platform | IPv4 (RFC 1918) | IPv6 ULA | What holds, and its limit |
|---|---|---|---|
| Linux 5.7 and later; 5.0 and later with `CAP_NET_RAW` | `SO_BINDTOIFINDEX` on the listener before its bind, read back with `getsockopt` | the same: a `SOL_SOCKET` option, which upstream's IPv6 listener lookup compares as IPv4's does. A held socket binds an IPv6 address of host or link scope only on the interface that holds it: measured unprivileged on a 7.0 kernel in this round, `[::1]` held to another interface refused its bind (`EADDRNOTAVAIL`) where `127.0.0.1` bound and refused the dial; a ULA, of global scope, held to its own interface binds, as A does. So the IPv6 hold is qualified in network namespaces, never on loopback (§9.5, F4) | **Arrival.** The kernel hands the listener only a SYN that arrived on A's interface, and one from any other finds no listener and is reset: socket(7), "only packets received from that interface are processed by the socket"; `__inet_lookup_skb` looks a listener up by `inet_iif`, and `inet_bound_dev_eq` matches a held socket only on that index (B527's reading of the kernel's v7.0 source and headers, `ops/engineering-log/2026-11/2026-11-22-b527--test-b527-bindtoifindex-measured.md`). Measured unprivileged on a 7.0 kernel by B527: a socket held to another index refused a loopback dial 50 times in 50, held to loopback's own index it took every one, and the index read back as set. The box's own connection to A counts as arriving on A's interface (B527's case (a)), which the source policy admits as it admits loopback. Limit: a box with two interfaces in one subnet refuses what arrives on the other. |
| Linux before 5.7 without `CAP_NET_RAW` (a `systemd --user` service has none, nor does the bridge's image run as its non-root user) | refused: `restriction_refused` (`EPERM` from 5.0 to 5.6, `ENOPROTOOPT` before 5.0) | the same | None: the opt-in stays closed on such a box, a 5.4 kernel among them. |
| macOS | `IP_BOUND_IF`, read back | refused in this PR: `not_supported_here` | **The return path.** The kernel scopes what the socket sends, its SYN-ACK included, to A's interface: a SYN that arrived on another interface is not refused, but its handshake completes only with a peer that A's interface reaches. Measured on macOS on 2026-10-23 (`internal/player/openhome/restrict_darwin.go:13-19`): a loopback listener held to en0, dialled on loopback, timed out, and a listener on en0's address held to en0 took the box's own connection, which arrives on lo0. `IPV6_BOUND_IF` is the IPv6 twin and has never been measured, so a ULA there is refused until it is (§11). Limit: a half-open connection from another interface waits in the listen queue until the kernel drops it, and a peer that can send from another interface and receive on A's link is admitted, as anyone on that link is. |
| Windows | **the strong host model, whole** (F3): the interface table of A's family, one `GetIpInterfaceTable` call for `AF_INET` a reading (x/sys/windows' `MibIpInterfaceRow`), every row reading `WeakHostSend`, `WeakHostReceive` and `ForwardingEnabled` false | the same, from the table for `AF_INET6` | **Arrival and the return path, both.** Under strong host receive on every other interface, a packet for A that arrives on one of them is discarded before TCP sees it; under strong host send on A's interface, the stack sends a packet from A only out of A's interface, by a route lookup confined to that interface's routes, so the SYN-ACK to a SYN that arrived elsewhere leaves through A's interface (Microsoft, "Strong and Weak Host Models", TechNet Magazine: the send process asks the setting of the interface that holds the source, the receive process that of the interface a packet arrived on; `MIB_IPINTERFACE_ROW`'s documentation: both host flags false by default since Vista, on every interface but the Teredo interface of a host-specific relay). Rather than argue which flag of which row can reach A's traffic (weak host receive on A's interface, weak host send on another), the console takes the model whole, as Windows ships it, and any departure from it on any row closes the socket. Every row is part of the policy (§3.5), so one that changes, appears or goes retires the socket (F11). A table that does not read is `host_model_unreadable`, and a row that breaks the model `interface_not_strong_host`, naming its interface. Limit: the drop is Microsoft's documented behaviour, read at every reading and measured on no CI leg, which has no second network (the owner's gate, §11); a box that forwards on any interface (Internet Connection Sharing, a virtual switch's NAT) or receives weakly anywhere keeps the console closed, and a ULA beside a relaying Teredo interface is refused by that interface's own row. |
| any other GOOS | refused: `not_supported_here` | refused | None. |

So every platform the bridge ships for holds A's interface where the table says, and the opt-in is
refused on: a Linux kernel before 5.7 without `CAP_NET_RAW`; a ULA on macOS; a Windows box whose
interface table of A's family departs from the strong host model on any row, or does not read; and
any other GOOS.

**The qualification is real sockets, apart from the handler tests (D13).** A request through
`Handler()` with a synthetic `RemoteAddr` proves the source policy and nothing about interfaces.
The holds are qualified by their own tests (§9.5): on Linux, the console's hold refuses an IPv4 SYN
from another interface, unprivileged, in every Linux run, and in a CI job with root, network
namespaces put a routed client inside A's prefix on another interface, for an RFC 1918 address and a
ULA alike (F4); on macOS, the hold scopes its replies, on the `macos-latest` leg; on Windows, the
table is read from the stack, on the `windows-latest` leg. Each skips locally where its premise is
missing and fails where `CI` is set, the repository's rule for such a test (`build-ci-tests.md`, the
host-address bullet), so a green CI leg shows that it ran. A routed SYN on Windows is the owner's
gate (§11).

### 3.4 While it waits

Every 5 seconds the plan tries again: one reading, at most one bind, on serve's context, joined
before the console's servers shut down; a test drives it with an injected tick, never a sleep. A
reason that changes is said again, and a bind is said at INFO. `not_supported_here` is not tried
again. Why a retry, not a WARN to restart: the Linux service is a `systemd --user` unit
(`internal/packaging/state.go:31`), and `internal/player/lan/lan.go:2-5` records that on a
Raspberry Pi the interface and DHCP often come up after the service starts. Without a retry such a
box would come up on loopback alone after most boots.

Why 5 seconds, one interval for the retry and the reconciliation (`consoleLANInterval`): it bounds
the window of §3.5, and it is the cadence at which OpenHome reads the same interfaces already
(`openhome.md`, the listener bullet). A reading is one `net.Interfaces()` and one address read an
interface: on Linux a netlink dump each, on Windows a `GetAdaptersAddresses` call each
(`internal/api/endpoints_cache.go:23-31`, which caches /v1/health's walk for that cost), and on
Windows one `GetIpInterfaceTable` call more; the PR measures it on its Windows leg (§10.4).

### 3.5 While it listens: the policy read again, and its retirement (D2, D12, F1, F2, F9, F11)

The policy a LAN socket was bound on is one reading, and each of its parts is compared at every
reading (§9.4 has a case for each part, and a structural check that leaves none without one):

| Part of the policy | Read from |
|---|---|
| the interface that holds A, by index and by name | the listing |
| that interface's flags: up, running, loopback, point-to-point | the listing |
| A's on-link prefix there | that interface's own entry for A |
| A held once, by that interface alone | every interface's addresses |
| on Windows, every row of A's family's interface table: the interface each row is of, and its three flags | `GetIpInterfaceTable` (§3.3) |
| the `.local` name | the host's name as `advertise` reads it (§4.5) |

Every 5 seconds, while the socket listens, the plan takes a new reading and compares it with the
socket's policy:

| The new reading | What happens |
|---|---|
| the same in every part | nothing |
| A on no interface (renumbered, the lease lost) | the socket is retired; `waiting`, `address_not_on_this_box` |
| A on another interface, by index or by name | retired, then bound on a fresh reading (below) |
| A's prefix changed, narrowed or widened | retired, then bound on a fresh reading |
| A kept with no network | retired; `waiting`, `address_has_no_network` |
| the interface down, not running, loopback or point-to-point | retired; `waiting` with that reason |
| A listed more than once | retired; `waiting`, `address_on_two_interfaces` |
| on Windows, a row of the table that changed, appeared or went, a row that breaks the model, or a table that does not read (F11) | retired, then bound on a fresh reading where that reading admits A, `waiting` with its reason where it does not |
| the `.local` name changed | retired, then bound on a fresh reading |
| a reading that fails, the listing or any one interface's addresses | retired; `waiting`, `interfaces_unreadable` (D12) |

**Fail closed.** A reading that fails decides nothing, and deciding nothing closes the socket: the
console has no login, so eligibility it cannot establish is no eligibility. That is OpenHome's rule
turned the other way round, since its surface keeps what is bound on a listing that fails
(`openhome.md`, the listener bullet), and the console does not read through `HostInterfaces`,
whose interface that does not read looks like one without A (§3.2). Each retirement is said once
at WARN, naming what changed (`address_gone`, `address_moved`, `prefix_changed`,
`interface_changed`, `interface_down`, `host_model_changed`, `name_changed`,
`interfaces_unreadable`), and a rebind at INFO.

**The fence: nothing is admitted once a retirement begins (F1, F9).** The plan keeps one mutex,
`plan.mu`, and under it, for each policy: its version, a number that only grows; whether it is
current or retired; its count of requests in flight; and its context, which the LAN server's
`BaseContext` returns (§3.1). Every request on a LAN socket passes the fence first, in the first
middleware of the LAN chain, ahead of the source check: in one critical section of `plan.mu` it
reads whether the policy its description names by version is current, and if it is, counts itself
in. A request whose policy is not current is answered 503 with `Connection: close` ("this
console's home-network socket is being reopened: try again in a moment"), and nothing else of the
chain runs. At its end, in a deferred call, so that a handler's panic counts it out too, the
request counts itself out under the same mutex, and the last one out of a retired policy wakes the
retirement's wait. A retirement, in one critical section of the same mutex, marks the policy
retired and cancels its context. There is no
third case: a request either counted itself in before that section, and then its context has
ended, so an event stream returns at once (its loop selects on its request's context:
`internal/admin/handlers_events.go:39-42`, `:90`, `:293-295`), or it reads the policy after it, and
is refused. A request cannot pass the check and count itself in once the retirement has looked,
since the check and the count are one critical section, the one the retirement takes (F9).

**Retired before the policy is replaced (F1, F2).** After that section the retirement closes the LAN
server (`http.Server.Close`), which closes its listener, waits for its `Serve` to return and then
closes every connection it tracks, new, active and idle (`net/http/server.go:3222-3240` at
go1.27.3, the toolchain `go.mod` names), kept-alive connections and those an event stream holds
among them; the console hijacks no connection, so every one is tracked. A connection accepted just
before the close is closed too, since `Serve` tracks it before it returns (`:3557-3603`), but its
own goroutine has started and can read a request before the close lands: that request meets the
fence and is refused (F1). `Close` does not wait for a handler, so the retirement then waits until
the policy's count is 0, on a condition of `plan.mu`, bounded at 5 seconds, the console's shutdown
window; a handler that outlives the bound is said at WARN, its count left on a policy nothing
admits by, and the retirement goes on. Only then does the plan take a fresh reading, never the one
that found the change (F2): where that reading admits A, a new socket binds with it as its policy
and with a new version; where it does not, the state reads `waiting` with its reason, and §3.4's
retry takes it from there. Between the retirement and the rebind no LAN socket exists.

**The window.** A change is acted on within one interval and a reading's time: until then the old
policy stands for new connections and old ones alike, which §6.4 names. Once a retirement has
begun, nothing is admitted under the policy it retires, and a request admitted before it has its
context ended and is waited for. A netlink subscription on Linux would shorten the window, and is
not taken (§8).

### 3.6 A failure while serving, the loopback socket's failure, and shutdown

- The LAN server's `Serve` returning anything but `http.ErrServerClosed`: WARN
  (`stopped_serving`), the socket retired as §3.5 retires it, so no connection outlives the state,
  then `waiting`, and §3.4 binds again.
- The loopback socket is unchanged: its bind or serve error is `adminErr`, which stops serve and
  the API (`main.go:7386-7409`); the LAN socket is closed on that way out.
- Shutdown: the plan's loop, which retries and reads again, stops and is joined first, so no bind
  or rebind follows the stop; the LAN policy is retired through the fence, so nothing more is
  admitted on it and its streams end; then both servers shut down together under one 5-second
  window (the API's rule for its LAN and tailnet servers, CLAUDE.md); then the background-scan,
  catalog and backup drains run once, as today (`admin.go:2328-2338`).

### 3.7 One canonical local endpoint (R3)

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

The new field has four readers: Validate, serve's plan, init and the doctor (§2.4). A structural
test walks `cmd/bridge` and `internal` and holds it to them, so no later probe dials the LAN
address.

## 4. The guards

The layers keep their order (`admin.go:2158-2175`): the boundary (source, then Host), `csrfGuard`
(Content-Type, then Origin), the session (public mode alone), the mux. The LAN socket's rules apply
to every request it carries, whatever the live config says. The loopback socket's are today's.

### 4.1 The source policy (R1)

The LAN socket admits a connection whose peer address, unmapped and without a zone, lies inside
both A's on-link prefix (the mask A has on its interface, in the reading the socket was bound on)
and A's private class (§2.2). The box's own address inside that prefix, a process on this box
dialling A, is admitted, as the loopback socket admits it. Anything else is answered 403,
`admin refused: this console's home-network address answers only to devices on its own network`.
The check sits in `boundaryMiddleware`, where `loopbackOnly` sits for the loopback socket.

Why that set:

- **Binding a private address proves nothing about a caller.** Linux takes a connection to A that
  arrives on any interface (the weak host model), and a router, a VPN gateway or a tailnet subnet
  route can deliver one from another network. The console has no login to tell them apart, so the
  network decides, and the interface (§3.3) with it.
- **Not "any private source".** A VPN or another VLAN numbered in private space is not the home
  network: private space alone is not enough (`internal/player/lan/lan.go:7-13`).
- **It is the answer the box already gives.** The OpenHome surface admits a source inside the
  on-link prefix of the address a connection arrived on (`.claude/rules/openhome.md`, the
  admission bullet), measured on Linux by B527, and intersecting with the class keeps a prefix
  wider than its class from admitting a public address (`lan.go:148-156`).
- **The peer, never a header.** `X-Forwarded-For` and its kin are not read: a page can set them
  (`auth-tls.md`, the Host bullet).

**A routed client inside the prefix (D1)** is the interface's to refuse, since its source passes
this check: on Linux its SYN finds no listener, and on macOS and Windows its handshake never
completes, because the reply leaves through A's interface (§3.3).

The residual: a device on the home link that relays others under its own address (a router's SNAT
or hairpin NAT, a VPN gateway, a reverse proxy) carries its clients in, and any device that holds
an address in the prefix and is reached through A's interface is admitted. The Host check below
still refuses a browser whose page names another host, but a client that writes its own Host is no
browser, and only the source stops it.

### 4.2 Host (R6, D3, D4, D5, F5)

The loopback socket keeps today's rule, an empty Host and any port included. The LAN socket takes
exactly the authority it is bound to, and reads a Host and an Origin through one parser, so a
spelling is taken or refused alike by both:

- **One parser (F5).** `parseLANAuthority(raw string) (lanAuthority, bool)` reads an authority as
  RFC 3986 writes one, with no user information: a host, then a colon and a port, or neither. Its
  input is the Host header's value as it arrived, or an Origin's value after its exact, lower-case
  prefix `http://`; anything else in front, `https://` and `null` among them, is no LAN Origin.
  Every byte must be printable ASCII and none of `@`, `/`, `?`, `#`, `%`, `\` or a space, so a user,
  a path, a query, a fragment, an escape and a zone are refused in either header alike. (net/http
  answers 400 to a Host holding any of those but `%` before a guard runs,
  `net/http/server.go:1074-1075` at go1.27.3, so for them the refusal is an Origin's; the parser
  keeps it for both all the same.) The host is an IPv6 literal in brackets, which
  `netip.ParseAddr` must take with no zone and not in the
  IPv4-mapped form (D3); or an IPv4 literal `netip.ParseAddr` takes, dotted, with no leading zero;
  or a name of letters, digits, `-` and `_`, in labels of 1 to 63 bytes with one trailing dot at
  most, that does not read as digits and dots alone; and nothing else, so an IPv6 literal without
  brackets, whose colons outnumber one, is refused. The port is the digits after the colon, 1 to
  65535 with no leading zero, and an empty port or none is 80 (D5). What it returns is canonical:
  an address, or a name in ASCII lower case without its trailing dot (D4), and the port as a
  number.
- **One comparison.** The Host check and `originMatches` (§4.3) both call it and compare its answer
  with the socket's two authorities: A with P, and the `.local` name with P. Neither reads a LAN
  authority through `url.Parse`, `url.URL.Hostname` or `url.URL.Port`, which split an Origin and a
  Host by different rules (the table of spellings below), and a structural test holds both to the
  one parser.

| Host | Loopback socket | LAN socket |
|---|---|---|
| empty (an HTTP/1.0 client) | accept (unchanged) | 421 |
| `localhost`, `localhost.`, `127.0.0.1`, `[::1]`, with any port or none | accept (unchanged) | 421 |
| `[::ffff:127.0.0.1]:P` | accept (unchanged: `loopbackHostname` takes it, `admin.go:2653-2674`) | 421 |
| `A:P` | 421 (unchanged: not loopback) | accept |
| `A` with no port, or `A:` | 421 | accept where P is 80; 421 otherwise |
| `A:80` | 421 | accept where P is 80; 421 otherwise |
| `A:Q`, another port (a router's port-forward, a proxy), or P with a leading zero | 421 | 421 |
| `<name>.local:P`, in any case, with one trailing dot or none | 421 | accept (§4.5) |
| `<name>.local` with no port, or `<name>.local:80` | 421 | accept where P is 80; 421 otherwise |
| `<name>.local..:P`, two trailing dots | 421 | 421 |
| `<name>.local:Q` | 421 | 421 |
| another address, the box's own or not, `A2:P` | 421 | 421 |
| a rebinding name, `x.example:P`, that resolves to A | 421 | 421 |
| an IPv6 literal at an IPv4 socket | 421 | 421 |
| A's own literal where A is a ULA, `[A]:P`, in any spelling of that address | 421 | accept (compared as addresses) |
| a zoned link-local literal, `[fe80::…%25eth0]:P` | 421 | 421 |
| A's IPv4-mapped spelling, `[::ffff:A]:P` | 421 | 421: the field takes no mapped address (§2.2), and the socket answers to the one form the field holds (D3) |
| A in another IPv4 spelling (one decimal number, a leading zero) | 421 | 421 |

An IP literal is compared as an address because no page can rebind a literal: only a name can be
re-pointed, and the only name the LAN socket takes is the one in §4.5. A browser sends what its URL
parser serialized, lower case, the default port elided, an IPv4 address in dotted form, so every
row a browser can produce is one of those taken or one refused alike in both headers.

**The spellings two parsers would answer differently (F5).** Each row is asked as a Host and as an
Origin (`http://` and the spelling), and the one parser gives both the answer in the last column.
The middle column is why one parser is needed: what reading an Origin with `url.Parse` and a Host
with `url.URL{Host: …}.Hostname()` and `.Port()` would give.

| Spelling | Two parsers | One parser, on a LAN socket at P |
|---|---|---|
| `u@A:P`, a user | `url.Parse` takes A as the Origin's host, a check of its own having to refuse the user; a Host carrying it never reaches the guard, since net/http refuses the `@` itself | refused in both |
| `A:P/`, `A:P?x`, `A:P#f` | an Origin alone can carry them, since net/http refuses a Host with any of them, and `url.Parse` keeps A as the Origin's host beside the path, the query or the fragment | refused in both |
| `HTTP://A:P` | `url.Parse` lower-cases the scheme and takes it | refused: no browser sends it, and the prefix is exact |
| `A.:P`, a trailing dot on an IPv4 literal | a dot trimmed before `netip.ParseAddr` in one path and not the other takes A in one header alone | refused in both: a trailing dot is a name's, and digits and dots alone are no name |
| `A:P` where A is a ULA, without brackets | `Hostname` splits at the last colon and reads the literal before it as A | refused in both |
| `[A%25eth0]:P`, `[A%eth0]:P` | `url.Parse` unescapes the `%25`, `Hostname` keeps it | refused in both: no `%` is taken |
| `A:+P`, a sign before the port | `url.Parse` refuses it, while `Port` gives no port and `Hostname` the whole | refused in both |
| `A:`, an empty port | both read no port, as 80 | taken in both where P is 80, refused in both otherwise |

### 4.3 Origin, in `csrfGuard` and the SSE gate (R6, R7)

The mechanics stay: an Origin is refused when it does not match, never for being absent, and
`null` is present and does not match (`auth-tls.md`, the `csrfGuard` bullet;
`admin.go:2373-2383`). `csrfGuard` passes GET and HEAD, so the event stream keeps its own gate
(`handlers_events.go:63-68`). Both gates call one function, `originMatches(r, origin)`, which
reads the request's socket and, on the LAN socket, hands the Origin's value to the one parser
§4.2's Host check calls (F5); today's `originMatchesAdmin` takes no request, so the two gates
change together or not at all, and a structural test holds both to the one function.

| Origin, on a write or on `GET /api/events` | Loopback socket | LAN socket |
|---|---|---|
| absent | pass (unchanged) | pass |
| `null` | 403 | 403 |
| `http://127.0.0.1:P`, `http://localhost:P`, `http://[::1]:P`, P the loopback socket's port | pass (unchanged) | 403 |
| `http://[::ffff:127.0.0.1]:P` | pass (unchanged, as its Host) | 403 |
| a loopback name on another port (an SSH tunnel on another local port) | 403 (unchanged, B193) | 403 |
| `http://A:P` | 403 | pass |
| `http://A` or `http://A:80` | 403 | pass where P is 80; 403 otherwise |
| `https://A:P` | 403 | 403: the LAN console is plain HTTP |
| `http://A:Q`, another port | 403 | 403 |
| `http://<name>.local:P`, in any case, with one trailing dot or none | 403 | pass |
| `http://<name>.local` or `http://<name>.local:80` | 403 | pass where P is 80; 403 otherwise |
| `http://[::ffff:A]:P` | 403 | 403, as its Host is 421 (D3) |
| `http://[A]:P` where A is a ULA, in any spelling | 403 | pass, as its Host |
| an Origin with a user, a path, a query or a fragment | as today | 403 |
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
- **The fence's 503** (§3.5): "this console's home-network socket is being reopened: try again in
  a moment", with `Connection: close`, to a request that reached a socket whose policy is retired;
  it names no address, and its line is the retirement's own (§3.5), never one per request.
- **`noteForeignHost`** (`admin.go:2755-2780`) keeps a set per socket kind, each bounded at 16, so
  the LAN socket's refusals cannot use up loopback's; the LAN line's hint names its authority.
- **The source refusal's log line**: one Warn per kind of refusal (outside the network, outside
  the class), naming A's network and never the peer, as the DLNA listener's refusals do since
  B604 (`dlna-upnp.md`, the B48 bullet). So the console still logs no client but public mode's
  login lines (`auth-tls.md`, the login-lines bullet). A connection the interface refuses (§3.3)
  never reaches the console, which says nothing of it.

### 4.5 The mDNS name

The LAN socket takes `<name>.local`, the name the API's `.local` endpoint carries in
`/v1/health` (`internal/advertise/endpoints.go:158-171`, `localEndpoint`: `hostname.ASCII`'s first
label, and none for a container's generated id or a name with no valid form), read at each reading
(§3.5) whatever `mdns.enabled` says, compared as §4.2 compares a name (ASCII lower case, one
trailing dot), on the LAN socket's port. `hostname.ASCII` returns an ASCII name byte for byte, its
case included (`auth-tls.md`, the bullet on a reported host name), which is why the comparison folds
case on both sides, and a converted name is ASCII already.

Why it is safe: the Host check exists because a page's author controls a name that public DNS
answers and can re-point it at the box (DNS rebinding; `auth-tls.md`, the Host bullet). `.local` is
a special-use domain (RFC 6761, RFC 6762) that no public DNS answers, so a name under it is
answered on the home network or not at all, and whatever answers it is on a network the socket
admits anyway. Why it is worth having: the box's name is what a person types. No other name is
taken: a bare host name, or one under a router's search domain, resolves through unicast DNS,
which a page's author can control.

## 5. The posture shown (R9)

Every surface reads the LAN socket's state (`off`, `listening`, or `waiting` with a reason) and the
source the plan recorded at boot (§2.6), never the field as it reads now.

### 5.1 The console

- A line at the foot of the console's menu (`internal/admin/templates/layout.html:275-287`,
  inside `.sidebar-foot`, so inside the drawer below 1024 px: `admin-console.md`, the top-bar
  bullet), rendered from the state at each page load through `pageData`
  (`internal/admin/handlers_pages.go:56-94`):
  - listening: "Open on your home network at http://A:P, with no password: anyone on that network
    can use this console." Then the source's words and its clearing sentence (§2.6), and where the
    file held the key at boot, the rollback sentence (§2.7).
  - waiting: "Not open on your home network yet: A is not on this box (tried again every 5 s).
    Until then this console answers on this machine only." The middle words are the reason's;
    `not_supported_here` says "this platform cannot hold the console to the interface A is on"
    and no retry. Then the source and, where it applies, the rollback sentence, since the key
    costs the rollback whether or not the socket listens.
  - off: no line, unless the file held the key at boot (an empty one, D16), which shows the
    rollback sentence alone.
  A page loaded while the socket waits says that it may open, so a line a few seconds old never
  understates where the console answers.
- The Settings page's hint for Admin address (`internal/admin/templates/settings.html:605-620`)
  says the console also answers on the home network where `adminLANAddress` or its variable is
  set.
- The Diagnostics page shows the console's own doctor, whose `console-lan` and
  `console-lan-rollback` lines (§5.3) read the serving process's state and the file.

### 5.2 `GET /api/console`

A console route on both sockets that answers

```json
{"lan":{"state":"listening","address":"<A>","url":"http://A:P","nameUrl":"http://<name>.local:P",
  "reason":"","source":"both","fileKeyAtBoot":"address","fileAddressAtBoot":"<the file's own A>",
  "network":"<A's network>","interface":"<its name>","hold":"SO_BINDTOIFINDEX"}}
```

`state` one of `off`, `listening` and `waiting`; `reason` one of §3.2's codes or `stopped_serving`;
`address` the address the plan took at boot, the effective one, whichever source gave it;
`source` one of `file`, `environment` and `both`, or empty while off; `fileKeyAtBoot` one of
`absent`, `empty`, `address` and `not_an_address`, and `fileAddressAtBoot` the file's own address,
present for `address` alone, so that beside a variable the boot file's value is named apart from
the effective one (F7) and a value the field's check refuses is never repeated (R10); `hold` one of
`SO_BINDTOIFINDEX`, `IP_BOUND_IF` and `strong_host`, empty while not listening. The fields are the
socket's and the boot's (§2.6), never the file's now. `bridge doctor` reads it over `adminAddress`.
Nothing on `/v1` changes.

### 5.3 `bridge doctor`

Two checks, `console-lan` and `console-lan-rollback`, right after `port-admin`, worded by one
function in `internal/doctor` for both doctors. `console-lan` prints no line where neither the file,
the doctor's own environment nor a running bridge says anything of the posture.

| State | The line |
|---|---|
| running, listening | WARN: the console answers on the home network at http://A:P, with no password and over plain HTTP: anyone on that network can watch it and change the bridge's settings, music and devices; held to <interface> (the hold). Then the running bridge's source and what clears it (§2.6). Hint: the clearing sentence; don't forward this port on a router |
| running, listening, source `both` | the same, and what the file's key held at boot, in §2.6's words: the file's own address, or that it is empty and costs the rollback, or that it does not read as an address and the next start without the variable refuses the file (F7) |
| running, waiting | WARN: the console answers on this machine only: the reason's words; tried again every 5 s (not for `not_supported_here`). Hint: the reason's own |
| running, waiting, on Windows, `interface_not_strong_host` | WARN: the console answers on this machine only: <interface> takes packets for other interfaces' addresses, sends from another interface's address, or forwards; the console opens on the home network only where every interface of A's family keeps Windows' strong host model, its default (§3.3). Hint: turn that setting off on <interface>, or keep the console on this machine |
| running, and the file now differs from what it took at boot (edited since, D14) | WARN naming both: the running bridge's address and source at its start, and what the file says now, and what the next start binds (the variable, if the running bridge's source is `environment` or `both`, wins over the file again) |
| the doctor's own environment sets the variable and the running bridge's source is not `environment` or `both`, or the other way round | WARN: this shell's BRIDGE_ADMIN_LAN_ADDRESS is not the running bridge's: the service's environment is the one that counts (D14) |
| not running, and the file or this environment sets it | WARN: when the bridge runs, its console also answers on the home network at http://A:P, with no password; whether the box holds A now; and the source this shell sees, with the warning that the service's environment may differ |
| a running bridge that does not report it (older than this change) | ok: not checked: the running bridge does not say where its console answers |
| a config that does not load | not run, as the port checks (`internal/doctor/doctor.go:958-974`) |

| `console-lan-rollback` | The line |
|---|---|
| the file holds the key now, an address or empty | WARN: bridge.yaml holds adminLANAddress (or "holds adminLANAddress, empty"): bridge 0.2.3 and earlier refuse this file, so an update's automatic rollback to one of them is not made and one installed by hand will not start. Hint: remove the whole adminLANAddress line before you go back to one; emptying it is not enough. Where the posture is wanted, BRIDGE_ADMIN_LAN_ADDRESS costs no rollback |
| the file holds no key | no line |

The console's doctor hands the checks the process's own state and the holder's file. The CLI
fetches §5.2, bounded at a second, takes no answer for a bridge not running, and reads the file it
found with `config.LANKeyOf`. init's preflight does not run the lines; init says its own before it
saves (§2.7).

### 5.4 The banner and init

The serve banner (`main.go:7155-7158`) keeps its `Admin console:` line on loopback and adds either
"Home network: http://A:P/ (no password: anyone on your network can use it), set in bridge.yaml"
(or by the variable) or "Home network: not open yet: the reason's words; tried again every 5 s",
and, where the file held the key at boot, the rollback sentence (§2.7). init says the no-login
sentence and the rollback sentence before it saves (§2.7, D15), and its footer
(`init.go:1270-1284`) adds the LAN URL when the run wrote the field. The launcher's "open the
console" (`menu.go:775-784`) still opens loopback, the URL a browser on this machine uses.

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

### 6.2 Plain HTTP, and a pairing in clear (D9)

Everything the console shows and is sent crosses the home network unencrypted, so anyone who can
see that traffic (another client of the same Wi-Fi with its passphrase, a device that diverts
traffic on the link) can watch it.

A pairing is the case to name exactly. The Devices page's mint (`POST /api/tokens`) answers with
the device's bearer token itself: the pairing link carries it beside its one-time code, since every
shipped app refuses a link without `token=` (`auth-tls.md`, the pairing-link bullet), and the
dialog shows it for a pairing by hand, "Token (shown once)" with a Copy button
(`internal/admin/templates/layout.html:358-364`). So a watcher of a LAN pairing holds a working
device credential the moment the answer crosses, without redeeming anything. What ends it depends
on the path the phone takes:

- **The QR, or a link the app opens.** The app redeems the code, which rotates the token, so the
  link's token, the watcher's copy, is dead once the phone has paired; a watcher who redeems the
  code first makes the phone's redemption fail where the user sees it. Until the phone pairs, the
  watcher's copy works.
- **By hand.** The app keeps the very token the dialog showed, nothing rotates it, and nothing
  fails: the watcher's copy works for as long as the device's does. A token minted and never paired
  with stays valid as well.

So interception need not show as a failed redemption, and a token minted on the LAN has crossed it
in clear whatever the phone does next. A rotation from the Devices page is the same: its answer
carries the new raw token (`internal/admin/static/app.js:3506-3531`). The console says so before
any token is minted or rotated on the LAN socket, and says what to do after (F6, F8, F10). Each
page knows the socket it was served on (`pageData` carries its kind), which decides these words,
never `isSecureContext`:

- **Before the mint.** The pairing dialog's form step (`layout.html:313-335`) shows, above
  Generate token: "This page is not encrypted. The token you generate here will cross your network
  in the clear, and anyone who can see that traffic can use it to read your library. To keep it
  off the network, pair from the bridge's own machine, or over an SSH tunnel to it." Generate token
  comes after it, and nothing is sent before it is pressed.
- **Before a rotation.** The Rotate button's confirmation (`app.js:3515`) gains: "This page is not
  encrypted: the new token would cross your network in the clear, as the old one did. Rotate from
  the bridge's own machine, or over an SSH tunnel to it." It still asks, and rotating stays
  possible, since every action of the console is the LAN visitor's (§6.1, §8).
- **Beside the result.** The result step of a mint or a rotation made on the LAN socket keeps,
  beside the token: "This token crossed your network unencrypted. Whether or not the device has
  paired with it, end that from the bridge's own machine, or over an SSH tunnel to it: there,
  rotate this device's token and pair the device again, or revoke the device. Rotating from this
  page would send the new token across the network the same way." No remedy it gives sends a new
  credential across the LAN (F10).

Not taken: refusing a mint or a rotation on the LAN socket, since pairing from a browser on the
network is what the posture is for; and hiding the manual fields, since the token crosses the
network in the mint's answer whatever the page shows (§8).

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
2. **Plain HTTP**, as §6.2: anyone who can see the traffic can watch, and take a device token as
   a mint or a rotation on the LAN answers it; a token paired by hand, or never paired, stays
   theirs until it is rotated from the bridge's own machine or revoked, and nothing fails to show
   it (D9). The console says so before the token is minted or rotated, and says that remedy beside
   it (F6, F10).
3. **Relays inside the network** (§4.1): a router's SNAT or hairpin NAT, a VPN gateway, a reverse
   proxy that rewrites Host; and a VPN the operator set up to put its clients on the home link
   (proxy ARP, a VPN bridged into it), whose clients are on that link by its design. On Linux a
   client that arrives on the VPN's own interface is refused by the hold (§3.3).
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
   bullet) is not told from a LAN, so an address on one is bound, and held to that tunnel.
9. **Docker.** On a bridge network the container's address is not the home network's, and the
   source and Host checks refuse what a published port delivers; with host networking it is a
   native install on the host's kernel, whose hold needs Linux 5.7 or later for the image's
   non-root user (§3.3).
10. **Rollback.** While bridge.yaml holds the key, an address or empty, 0.2.3 refuses the settings,
    so the crash rollback of a 0.2.4 install on probation does not put it back and a downgrade by
    hand does not start, until the line is removed (§2.7). The variable alone costs neither.
11. **A restore onto another box** carries `adminLANAddress`; there the bind waits for an address
    that box may never hold, and the line and the doctor say so.
12. **No limits of its own.** No rate limit or header cap is added: the network gets the console's
    whole capacity, as loopback does.
13. **The holds' limits** (§3.3). On macOS the hold is the return path, not arrival: a SYN from
    another interface waits half-open in the listen queue, and a flood of them can fill it, which
    denies the console and admits nothing. On Windows the strong host model holds both directions,
    and its drop of such a SYN is Microsoft's documented behaviour, read at every reading and
    measured only at the owner's gate. The opt-in stays closed on a Linux kernel before 5.7
    without `CAP_NET_RAW`, on a ULA on macOS, on a Windows box any interface of whose family
    departs from the strong host model (one that forwards for a NAT among them), and on any other
    GOOS.
14. **Two interfaces in one subnet.** On Linux the socket refuses what arrives on the interface A
    is not on, so a neighbour whose frames reach that one is refused, closed and not open; on macOS
    and Windows replies leave through A's interface.
15. **The window of §3.5.** For up to one interval and a reading's time after a change of the box's
    network, the old policy stands. Once its retirement begins, nothing more is admitted under it;
    a request admitted before has its context ended, and one that does not stop for it finishes its
    work within the wait's bound or past it, its connection closed and its answer going nowhere.
16. **An older bridge given the variable alone** starts with the console on loopback, saying
    nothing of a variable it does not know: closed, and silent.

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
  rules. The LAN socket shares only the admin port, which the OpenHome port clash reads already,
  and OpenHome's interface helper is untouched: the console's hold is its own (§3.2).
- **Tailscale.** The tailnet's ranges are refused as a LAN address (§2.2), and a tailnet peer is
  outside the prefix (§4.1). In CLI mode a peer arrives on the tailnet's interface, which the Linux
  hold refuses before the source check; the embedded node (tsnet mode) runs its own stack and
  never reaches a kernel socket. A subnet router that rewrites tailnet sources into the LAN is
  §6.4, item 3.
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
13. Leaving the arrival interface unrestricted, as round 1 did: lost to D1: the source policy admits
    a routed client inside the prefix, and only the interface tells it from a neighbour (§3.3).
14. OpenHome's `restrictToInterface` for the console: lost to D11: it leaves a socket it could not
    hold listening unrestricted (`restrict_linux.go:17-19`), which suits a surface that admits at
    accept and has no console behind it.
15. The arrival interface checked per connection: not available. No platform gives an accepted TCP
    connection's arrival interface to an unprivileged query: Linux's `TCP_SAVED_SYN` keeps the
    SYN's headers and not its interface, and `IP_PKTINFO` is a datagram's.
16. `SO_BINDTODEVICE` by the interface's name, which a kernel before 5.0 has: not taken. It holds by
    a name that can change between the reading and the hold, and before 5.7 it needs
    `CAP_NET_RAW`, which an unprivileged install of the bridge has nowhere.
17. Keeping what is bound on a reading that fails, OpenHome's listing rule: lost to D12 (§3.5).
18. A netlink subscription on Linux in place of the poll: not taken. The poll bounds the window on
    every platform alike, and a subscription is a second mechanism per platform; it is the way to a
    shorter window if 5 s proves long (§11).
19. On Windows, strong host send on A's interface alone (round 2's rule): lost to F3. It left weak
    host receive on A's interface and weak host send on the others to be argued one by one; the
    model is taken whole (§3.3), at the cost of a box that forwards for a NAT on any interface,
    which the doctor names (§5.3).
20. A ULA on macOS through `IPV6_BOUND_IF`, unmeasured: deferred to a measurement (§11).
21. Taking the IPv4-mapped spelling in Host and Origin, unmapped, in place of refusing it in both
    (D3): not taken. The field takes no mapped address, the canonical URI of a track refuses the
    form too (`openhome.md`, the Insert bullet), and one spelling of A keeps the matcher small.
22. Hiding the manual pairing fields on the LAN socket: not taken: the token crosses the network in
    the mint's answer whatever the page shows (§6.2).
23. A count of handlers alone, then `Close` and a wait (round 2's retirement): lost to F1 and F9. A
    request could pass a check, pause, and count itself in after the retirement had seen none; the
    fence makes the check and the count one section of the mutex that retires the policy (§3.5).
24. A second `Close` after the wait, for a connection the server tracked late: not needed. `Close`
    waits for `Serve` to return before it closes what it tracks (§3.5), and whatever a connection's
    goroutine reads after the retirement began meets the fence.
25. Rebinding on the reading that found the change: lost to F2. A reading taken before the wait can
    be stale by its end, so the rebind reads again.
26. Reading an Origin with `url.Parse` and a Host with `url.URL`'s methods, and comparing the parts:
    lost to F5. The two split some spellings differently (§4.2's table), so one parser reads both.
27. The pairing warning in the result step alone (round 2's): lost to F6. It came after the token
    had crossed the network; the warning comes before the mint and before a rotation (§6.2).
28. Advising a rotation from the LAN page after a pairing there: lost to F10. A rotation's answer
    carries the new token across the same network; the remedy is the bridge's own machine (§6.2).
29. Refusing a mint or a rotation on the LAN socket: not taken. Pairing from a browser on the
    network is what the posture serves; the console warns first instead (§6.2).

## 9. The tests, red first (D10)

### 9.1 Three kinds, kept apart

The tests come first, in a commit of their own with a names-only shim, so that they compile at
main. A test is counted red only for an assertion that fails at main with the shim in place, never
for a name the shim had to supply. Three kinds:

- **Behavioural reds** (§9.3): an assertion of the test fails at main with the shim, and the table
  names it.
- **Green baseline controls** (§9.4): green at main, with the shim and after the change, so they
  pin what must not move; each is shown red by its negative control (§10.3), never at main.
- **After-only controls** (§9.4, second table): their precondition is the change itself (a LAN
  socket that listens), so at main they fail their precondition, which is not counted; each is
  shown red by its negative control.

The real-socket qualification of the holds (§9.5) is kept apart from all three, since no handler
test can show it (D13).

### 9.2 The shim

Names only, each body inert until the change gives it its own: `Config.AdminLANAddress` with its
tag (so the key loads at main with the shim, which is why `TestALANAddressTakesEachPrivateClass` is
a control and not a red); `config.ValidateConsoleLANAddress` (takes every value),
`config.ConsoleLANClass` (false for every address), `config.LANKeyOf` (absent for every file);
`lan.ReadInterfacesFrom` and `lan.ReadInterfaces` (`HostInterfaces`, no error);
`admin.ServeConsole` (serves loopback alone, the plan ignored), the plan's type and its seams (the
reading, the hold, the tick, the fence's and the retirement's), `parseLANAuthority` (no authority
for any input), the page data's socket kind, and `GET /api/console`'s DTO with no route;
`consolelan.Hold` (sets nothing, reads nothing back), `consolelan.Decide` (no hold for any
platform), `consolelan.HostModel` (a strong table, read from nothing); `doctor.Deps.ConsoleLAN`;
init's `--admin-lan-address` flag (parsed, ignored) and its save's seam (`initHost.save`, the real
save).

### 9.3 Behavioural reds

| Test | Package | What it pins | Red at main, with the shim, because |
|---|---|---|---|
| `TestTheLANAddressIsRefusedForEachReasonWithoutItsValue` | `internal/config` | every row of §2.2, each refusal naming the field and none of a secret-shaped value planted in it (R10) | the shim's check takes every row |
| `TestAVariablesLANAddressIsRefusedAsTheFilesIs` | `internal/config` | the same rows through `BRIDGE_ADMIN_LAN_ADDRESS`, the variable named beside the field and the value nowhere (D7) | the same |
| `TestTheLANClassIsThePrivateBlocksLessTailscales` | `internal/config` | `ConsoleLANClass` | the shim answers false |
| `TestTheLANKeysPresenceIsReadApartFromItsValue` | `internal/config` | `LANKeyOf` over a file with no key, `adminLANAddress: ""`, `null`, an address, and the key reached through a merge key (D16) | the shim answers absent for each |
| `TestTheConfigExampleSaysWhatTheKeyCostsTheRollback` | `internal/config` | the example's block carries §2.7's sentence (D8) | the example has no block |
| `TestTheLANSocketAdmitsOnlyItsOwnNetwork` | `internal/admin` | every source of §4.1 through `Handler()`, a request stamped with the LAN socket and its `RemoteAddr` (R1) | the admitted rows: `loopbackOnly` refuses every LAN source |
| `TestEachHostIsAnsweredAsItsSocketsRuleSays` | `internal/admin` | §4.2, both columns (R6) | the LAN column's accepted rows |
| `TestEachOriginIsAnsweredAsItsSocketsRuleSays` | `internal/admin` | §4.3 for a PATCH, a bodyless POST, an upload chunk's PUT and `GET /api/events` (R6, R7) | the LAN column's passed rows |
| `TestHostAndOriginTakeEachSpellingAlike` | `internal/admin` | one table of spellings (mapped, `.local` in each case and with one dot or two, every port form, on a socket at P and at 80, IPv4 and ULA, and every row of §4.2's table of spellings two parsers would answer differently), each asked as a Host and as an Origin: both answers agree, and agree with the table (D3, D4, D5, F5) | the table's accepted spellings (at main both headers refuse every one alike, so the agreement half alone would be green) |
| `TestTheEventStreamAndTheCSRFGuardShareOneOriginRule`, `TestHostAndOriginShareOneAuthorityParser` | `internal/admin` | both gates call `originMatches`; it and the Host check call `parseLANAuthority`; and no code of the LAN socket's reads an authority through `url.Parse`, `url.URL.Hostname` or `url.URL.Port` (structural, F5) | the shim's functions are called by neither gate |
| `TestTheLANAuthorityParserIsOneGrammar` | `internal/admin` | `parseLANAuthority` over its grammar's every edge (§4.2): each refused byte, a bracketed literal with a zone or mapped, digits and dots that are no IPv4 literal, a label of 64 bytes, a port of 0, 65536 or with a leading zero, an empty port; and what it returns is canonical, a second parse of its answer giving the same (F5) | the shim parses nothing |
| `TestEachRefusalSaysWhereToOpenTheConsole` | `internal/admin` | §4.4's 421 and 403 bodies, per socket, with and without the LAN socket listening (R7) | the LAN words |
| `TestTheLANSocketsRefusalsNameNoPeerAndSpareLoopbacksLog` | `internal/admin` | §4.4's log lines | no such line |
| `TestTheMenuSaysWhereTheConsoleAnswers` | `internal/admin` | §5.1's three states (R9) | no line |
| `TestTheMenuLineSaysWhereTheAddressCameFromAndWhatItCostsTheRollback` | `internal/admin` | the source's words per source, the rollback sentence where the file held the key, an empty key's line while off (D7, D8, D16) | no line |
| `TestTheConsoleRouteReportsTheSocketsState` | `internal/admin` | §5.2 | no route: 404 |
| `TestAHoldTheKernelRefusesLeavesTheLANSocketClosed` | `internal/admin` | an injected hold that answers `EPERM`, `ENOPROTOOPT`, an index of 0, and a read-back of another index: nothing listens, the state is `waiting` with each reason, the loopback socket serves (D11) | no state |
| `TestAReadingThatFailsBindsNothing` | `internal/admin` | an injected reading that fails whole, and one whose one interface's addresses fail: nothing binds, `interfaces_unreadable` (D12) | no state |
| `TestEachPlatformAndFamilyIsDecided` | `internal/consolelan` | `Decide(goos, family)` gives §3.3's table, GOOS a parameter, on every leg (D13) | the shim decides no hold anywhere |
| `TestTheStrongHostTableDecidesTheWindowsHold` | `internal/consolelan` | injected tables: each of the three flags on A's row and on another's, a table that does not read, and a table and a listing that name different interfaces, each closed with its reason and the interface named; a strong table held (D13, F3) | the shim holds every table |
| `TestAReadingOfAWithNoNetworkBindsNothing` | `internal/admin` | A kept as the address alone (no `*net.IPNet`, a mask that is no prefix, a mask of no ones): nothing binds, `address_has_no_network` (F8) | no state |
| `TestReadInterfacesFailsWholeOnAnyInterfacesAddresses`, `TestReadInterfacesKeepsAnAddressThatIsNoIPNetAsTheAddressAlone` | `internal/player/lan` | `ReadInterfacesFrom` over a lister and an address reader of the test's: one interface's addresses failing fails the reading, naming it; an address that is no IP fails it; one that is no `*net.IPNet` is kept as the address alone (F8, `lan.go:49-98`) | the shim is `HostInterfaces`, which lists the first with no addresses and skips the second |
| `TestServeHoldsItsLANSocketThroughConsolelan` | `cmd/bridge` | serve's bind calls the hold it is given with the reading's index before the bind, and a hold that errs leaves no LAN socket; the reading and the Windows table are the ones it is given (F8, the package boundary) | serve binds nothing for the LAN, so no hold is called |
| `TestAnUploadWithoutSubtleCryptoSendsNoDigestAndSaysSo` | `internal/admin`, under node | `globalThis.crypto` with no `subtle` and `isSecureContext` false: the chunk's PUT carries no `Content-Digest`, the server takes it through the LAN socket, and the end says length only, naming the cause (R5) | the words |
| `TestTheUploadPageSaysChunksAreCheckedForLengthOnly` | `internal/admin`, under node | §6.3's note before an upload | no note |
| `TestThePairingDialogWarnsBeforeGenerateOnTheLANSocket` | `internal/admin` | a page the LAN socket served renders §6.2's warning in the dialog's form step, before Generate token in the document's order, and the recovery advice beside the result's token; a page the loopback socket served renders neither (D9, F6, F8) | no warning |
| `TestARotationOnTheLANSocketWarnsBeforeItIsSent` | `internal/admin`, under node | on a page the LAN socket served, Rotate's confirmation carries §6.2's warning before any request is sent, a confirmation refused sends nothing, and the result carries the recovery advice, whose words offer no rotation from the page; on a loopback page none of it (F6, F10) | no warning |
| `TestServeOpensTheConsoleOnItsLANAddress` | `cmd/bridge` | the real serve with a handed loopback listener and a stand-in box for the LAN bind (§9.6): a client from inside the network gets the console, one from outside gets 403, and the banner's line | serve binds nothing for the LAN |
| `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` | `cmd/bridge` | a bind that fails: serve starts, one WARN, the menu says this machine only, `/api/console` says waiting, both doctors WARN, the loopback socket refuses `Host: A:P` and `Origin: http://A:P`, then a tick binds it and the line changes (R9) | no WARN, no state |
| `TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo` | `cmd/bridge` | serve with the variable and a file without the key listens, `source` is `environment`, and the doctor's line names the variable and how to clear it (D7) | serve binds nothing for the LAN |
| `TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment` | `cmd/bridge` | serve started with the variable and the doctor run without it, and the other way round: the line names the serving process's source, and says apart what this shell sets (D14) | no line |
| `TestUnsettingTheVariableMakesTheFilesAddressTheOneServed` | `cmd/bridge` | a file with A and a variable with A2: serve binds A2, source `both`; restarted without the variable it binds A, source `file`; the line said so before (D14) | serve binds nothing for the LAN |
| `TestAnEmptyKeyIsReportedAsCostingTheRollback` | `cmd/bridge` | a file with `adminLANAddress: ""`: no LAN socket, `/api/console` reports the key at boot and empty, both doctors' `console-lan-rollback` WARN says to remove the line (D16) | no route, no line |
| `TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback` | `cmd/bridge` | `adminLANAddress: ""` with `BRIDGE_ADMIN_LAN_ADDRESS` set: serve binds the variable's address, source `both`, `fileKeyAtBoot` `empty`; the doctor's line says that unsetting the variable turns the console off the home network and that the line costs the rollback; `console-lan-rollback` warns (F7) | serve binds nothing for the LAN |
| `TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne` | `cmd/bridge` | a variable's A2 over a file's A: `address` A2 and `fileAddressAtBoot` A; over a file whose value is a secret-shaped string the check refuses: `fileKeyAtBoot` `not_an_address`, and the value in no answer, line or log (F7, R10) | no route |
| `TestInitWritesTheLANAddressItIsGiven`, `TestInitRefusesALANAddressWithPublic`, `TestInitAddressFlagsErrorChecksTheLANAddress` | `cmd/bridge` | §2.5, item 4 (R8) | the shim's flag writes nothing and refuses nothing |
| `TestInitNeverEchoesARefusedLANAddress` | `cmd/bridge` | init's real path, its whole output read, for each of §2.2's refusals of a secret-shaped value and for the flag with `--public`: exit 2, and the value nowhere (D6) | init takes the flag and exits 0 |
| `TestInitWarnsOfTheLostRollbackBeforeItSaves` | `cmd/bridge` | init's real path with `--yes`, the writer the test hands its stderr reading bridge.yaml as the rollback sentence is written. A first install with the flag: the file is absent then. A `--yes --force` rewrite over a file whose key it keeps: the file holds its original bytes then, byte for byte, and the save that follows is observed apart, through `initHost.save`, whose stand-in records its call and then makes the real save, after the sentence. Each before the `initHost` stand-in records an install or a start (D15, F7, F12) | no sentence |
| `TestARewriteKeepsTheLANAddressWithTheAdminAddress`, `TestARewriteThatNamesAnAddressFlagSaysTheLANAddressIsNotKept`, `TestARewriteToPublicSaysTheLANAddressIsNotKept`, `TestARewriteDropsAKeptLANAddressTheCheckRefuses` | `cmd/bridge` | §2.5, item 5 (R4) | `priorInstallFile` reads no such field; no line |
| `TestBridgeDoctorReadsTheRunningBridgesConsole` | `cmd/bridge` | the CLI asks §5.2 and words its answer | no route |
| `TestTheConsoleLANLineSaysEachState`, `TestTheConsoleLANLinesNameTheSourceAndWhatClearsIt`, `TestTheRollbackLineIsReadFromTheFileAsItIsNow` | `internal/doctor` | §5.3's two tables (R9, D7, D8, D16) | no check |
| `TestRunPrintsTheConsoleLANLinesAfterTheAdminPort` | `internal/doctor` | the lines' place | no check |

### 9.4 Controls

Green baseline controls, green at main, with the shim and after:

| Test | Package | What it pins | Shown red by |
|---|---|---|---|
| `TestALANAddressTakesEachPrivateClass` | `internal/config` | the three RFC 1918 blocks and a ULA load | a block left out of `ConsoleLANClass` |
| `TestNoLANAddressWritesNoKey` | `internal/config` | a config without the posture marshals with no `adminLANAddress` key, by unmarshalling and looking for the key (R8, rollback) | `omitempty` dropped (§10.3, 8) |
| `TestTheLANAddressIsSetByItsVariable` | `internal/config` | `BRIDGE_ADMIN_LAN_ADDRESS` is derived and sets the leaf, `envSet` with it | the field exempted from the derived variables |
| the loopback columns of `TestEachHostIsAnsweredAsItsSocketsRuleSays` and `TestEachOriginIsAnsweredAsItsSocketsRuleSays` | `internal/admin` | loopback's rules do not tighten or widen silently (R6) | loopback's Origin widened to the LAN authority (§10.3, 4) |
| `TestThePairingCopyFallsBackWithoutTheClipboard` | `internal/admin`, under node | no `navigator.clipboard`: the text is selected, nothing throws (R5) | the catch's selection removed |
| `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` | `cmd/bridge` | `bridge status`, `refuseIfBridgeMayBeRunning`, `isAdminAlive` and `probeAdminRunning` against a serve whose LAN bind failed (R3) | a local probe dialling the LAN address (§10.3, 11) |
| `TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket` | `cmd/bridge` | `tryLibraryViaAdmin` (R3) | the same |
| `TestOnlyTheBindAndItsChecksReadTheLANAddress` | `cmd/bridge` | the AST walk of §3.7 (R3) | the same |
| `TestALANSocketThatDoesNotBindIsNoFailedStart` | `cmd/bridge` | B660's count is untouched | a LAN bind's failure sent to `adminErr` (§10.3, 6) |
| `TestTheProductionSeamsAreConsolelansAndLans` | `cmd/bridge` | serve's default hold, Windows table and reading are `consolelan.Hold`, `consolelan.HostModel` and `lan.ReadInterfaces`, compared as functions, so `internal/consolelan`'s qualification (§9.5) is of the code serve runs (F8) | a default replaced by another function |
| `TestReadInterfacesAgreesWithHostInterfacesWhereNothingFails` | `internal/player/lan` | where every read answers and every address is a `*net.IPNet`, `ReadInterfacesFrom` gives what `HostInterfaces` gives, prefixes and all (`lan.go:75-98`'s cases: a 16-byte IPv4 address and mask, a mask that is no prefix, a mask of no ones) (F8) | a prefix read another way in the new path alone |

After-only controls, whose precondition is a LAN socket that listens:

| Test | Package | What it pins | Shown red by |
|---|---|---|---|
| `TestTheLoopbackGuardsKeepTheirAddressWhileTheLANSocketServes` | `internal/admin` | the LAN socket's bind leaves the loopback socket's Origin and hint as they were (R2) | the LAN bind written into `boundAdminAddr` |
| `TestALANSocketThatFailsLeavesLoopbackServingAndClosesItsConnections` | `internal/admin` | §3.6 (R2) | the failure leaving its connections open |
| `TestAShutdownStopsBothSocketsInOneWindowAndBindsNothingAfter` | `internal/admin` | §3.6 (R2) | the plan's loop joined after the servers' shutdown |
| `TestAPairingQRMintedThroughTheLANSocketIsLoopbacks` | `internal/admin` | §7, the QR | the QR built from the request's Host |
| `TestTheLANSocketIsRetiredWhenItsAddressIsRenumbered`, `…WhenItsMaskNarrows`, `…WhenItsAddressMovesToAnotherInterface` | `internal/admin` | §3.5's rows through an injected reading: a kept-alive connection opened before the change gets no answer to its next request, and a new connection is judged by the new policy (a source in the old /24 and outside the new /25 refused; the moved socket held to the new index) (D2) | no reconciliation; the address compared alone; the index compared alone (§10.3, 18 and 19) |
| `TestAKeptAliveConnectionIsClosedBeforeTheNewSocketBinds` | `internal/admin` | the old connections' close is seen before the new listener's bind, through the plan's seams (D2) | a retirement that closes the listener alone, and a rebind before the close (§10.3, 20 and 21) |
| `TestAReadingThatFailsAfterTheBindClosesTheLANSocket` | `internal/admin` | after admission, a listing that fails and one interface whose addresses fail each retire the socket and its kept-alive connection within one tick; a whole reading binds again (D12) | a failed reading that keeps the socket, and a reading through `HostInterfaces` (§10.3, 22 and 23) |
| `TestEachPolicyFieldRetiresTheSocketWhenItChanges` | `internal/admin` | a case for each part of §3.5's policy table: the index, the name, each flag, the prefix, A held twice, each flag of A's Windows row and of another's, a Windows row added and one removed, the `.local` name; each with a kept-alive connection that gets no answer after the change; and a structural half that fails when the policy's type gains a field no case names (F8, F11) | a part left out of the comparison (§10.3, 19 and 34) |
| `TestNoRequestIsAdmittedOnceARetirementBegins` | `internal/admin` | a connection opened before a retirement, its request sent once the retirement's section of `plan.mu` has passed and before its `Close` (the retirement held at its seam there): 503 with `Connection: close`, and a handler that records every request it gets records none (F1) | the fence removed, round 2's count alone (§10.3, 32) |
| `TestAnEventStreamEndsAtItsSocketsRetirement` | `internal/admin` | a `GET /api/events` open across a retirement held before its `Close`: the stream ends at the retirement's section, before the close, and the retirement's wait returns only once the stream's handler has (F1) | the policy's context left uncancelled, the stream then ending at the close alone (§10.3, 33) |
| `TestAnAdmissionParkedBetweenItsCheckAndItsCountIsSeenByTheRetirement` | `internal/admin` | a request parked at the admission's seam, between its check and its count; a retirement begun meanwhile waits for that section, then finds the request counted, cancels its context and waits for it, and never completes with a count of 0 while the request is parked; a request after the retirement's section is refused. Ordered by the two seams alone, with no sleep (F9) | the check and the count in two sections of the mutex: the retirement completes, then the parked request runs (§10.3, 32) |
| `TestTheRebindTakesAFreshReadingAfterTheWait` | `internal/admin` | the reading that finds the change says A moved to another interface; while the retirement waits on a handler the test holds, the injected reading changes again, A gone in one case and on a third interface in another; the rebind follows the fresh reading, no socket in the first and one held to the third interface in the second, never the second interface (F2) | the rebind on the reading that found the change (§10.3, 35) |

### 9.5 The holds, on real sockets (D13)

| Test | Platform, family | Where it runs | What it shows | Red at main, with the shim, because | Its control |
|---|---|---|---|---|---|
| `TestTheConsolesHoldRefusesASYNFromAnotherInterface` | Linux, IPv4 | every Linux run, unprivileged | the console's `consolelan.Hold` on `127.0.0.1:0`, held to an index that is not loopback's: a loopback dial is refused (`ECONNREFUSED`, 2 s bound), held to loopback's own it is taken, the index reads back (B527's cases (c) and (d), for the console's code). IPv6 is not asked here: a socket held to another interface cannot bind `[::1]` (§3.3, F4) | the shim's hold sets nothing: the dial is taken | the hold made a no-op |
| `TestARoutedClientInsideThePrefixGetsNoConsole` | Linux, IPv4 and a ULA | a gate job of its own with root (§10.4) | three network namespaces and two veth pairs: the box, with A in its prefix on one link and another address on the second; a client on A's link; a routed client on the second link whose address lies in A's prefix, with a host route each way (the shape of a VPN that hands out home addresses); once with an RFC 1918 A and once with a ULA, each the box's own address on its own veth, which a socket held to that veth binds (F4). The real plan and handler run in the box's namespace on its real interfaces: the client on A's link gets 200, the routed client's connect is refused, and the source policy and the Host check are shown to admit it without the hold | the shim's `ServeConsole` binds nothing for the LAN | the hold removed: the routed client gets the console's 200 |
| `TestTheConsolesHoldScopesItsRepliesOnMacOS` | macOS, IPv4 | the `macos-latest` leg | `127.0.0.1:0` held to an interface that is not lo0 does not connect on a loopback dial within 2 s; held to lo0 it connects at once; `IP_BOUND_IF` reads back | the shim's hold sets nothing: the dial connects | the hold made a no-op |
| `TestTheStrongHostModelIsReadFromTheStack` | Windows, IPv4 and IPv6 | the `windows-latest` leg | `consolelan.HostModel` reads the interface table of each family from the stack, each row's interface and flags those `GetIpInterfaceEntry` gives for it, and the decision §3.3 makes of the rows read; it records the runner's rows in its log, and asserts no particular answer of them, since a runner's own adapters may forward | the shim's table is read from nothing | the read replaced by a strong table |

The three that need only the host follow the repository's rule for a test whose premise a host can
lack: each skips locally where its premise is missing (a non-loopback interface, the stack's table)
and fails where `CI` is set (`build-ci-tests.md`, the host-address bullet), so a green CI leg shows
it ran. The routed client's test skips without root and without `BRIDGE_QUALIFY_ROUTED=1`, which
only its own job sets, and that job requires its `--- PASS` line (§10.4).

**The namespaces, and Go's threads (F8).** A network namespace belongs to an OS thread, not to a
process: a Go program that switched one with `setns` would have to lock the goroutine to its thread
(`runtime.LockOSThread`) and never unlock it, since an unlocked thread returns to the scheduler's
pool in the namespace it switched to, while a socket keeps the namespace of the thread that made it.
The test never switches a namespace from Go. It makes the namespaces and the veth pairs with `ip`
(found by `exec.LookPath`, run by its absolute answer: `build-ci-tests.md`, the exec bullet), and
runs each side in a process of its own, the test binary itself (`os.Executable`) re-executed under
`ip netns exec`, whose helper mode a variable of the test's selects: the box's process runs the
plan, the hold and the handler, and each client's process dials and reports. Each process is in one
namespace for its whole life, and the test deletes the namespaces it made in its cleanup. The
namespaces need root: an unprivileged user namespace is refused where AppArmor restricts them,
Ubuntu's default since 23.10, and a veth pair to the box needs `CAP_NET_ADMIN` there (B527's list
of what it could not measure).

### 9.6 Existing guards, and what no test does

Existing guards that must keep passing, and cover the change without edits:
`TestEveryConfigLeafIsEnvSettable`, `TestPriorInstallTagsAreConfigs`,
`TestEveryServeHTTPServerRedactsPeerAddresses` (it finds the second `http.Server`),
`TestEverythingBelowTheBrandIsInTheDrawer`, `TestClientChunkDigestIsAcceptedByTheServerParser`,
`TestEachSocketIsRestrictedToItsInterfaceWhereThePlatformLets` and
`TestALoopbackSocketRestrictedToAnotherIndexRefusesALoopbackConnection` (OpenHome's, untouched).
No test outside §9.5 binds the host's LAN addresses or lists its interfaces (`openhome.md`, the
tests bullet): the console's tests are requests through `Handler()` and sockets on loopback that
stand for A, as `internal/player/lan/lantest` stands for OpenHome's (a listener on 127.0.0.1
whose connections report the source the test names), with a reading of the test's that can fail
whole or for one interface. The package boundary (F8): `internal/consolelan`'s tests qualify the
holds and the Windows table on real sockets and the real stack; `internal/admin`'s, the plan, the
fence and the guards over seams; `internal/player/lan`'s, the reading; and `cmd/bridge`'s, that
serve wires those three into the plan by default and calls them in their order
(`TestServeHoldsItsLANSocketThroughConsolelan`, `TestTheProductionSeamsAreConsolelansAndLans`), so
no layer's green stands in for another's.

## 10. The PR

### 10.1 Branch and commits

`feat/b989-lan-console`, from main. Two commits: the tests with a names-only shim (§9.2), red where
§9.3 says and green where §9.4 says, then the change. Pushed and opened only with the owner's
go-ahead.

### 10.2 Files

- `internal/config/config.go` (the field, Validate's call), `internal/config/console_lan.go` (new:
  `ValidateConsoleLANAddress`, `ConsoleLANClass`, `LANKeyOf`), `config/bridge.yaml.example` (the
  block, §2.7).
- `internal/player/lan/lan.go` (`ReadInterfacesFrom`, `ReadInterfaces`, `UnreadableError`;
  `HostInterfaces` unchanged).
- `internal/consolelan` (new): `decide.go` (§3.3's table, GOOS a parameter), `hold_linux.go`
  (`SO_BINDTOIFINDEX` and its read-back), `hold_darwin.go` (`IP_BOUND_IF` and its read-back),
  `hold_windows.go` (the table, `GetIpInterfaceTable`, and the strong host model's judgement of
  every row), `hold_other.go` (refuses), and §9.5's tests, the routed client's helper processes
  among them.
- `internal/admin/admin.go` (the two servers, the socket's description, the per-socket guards and
  words), `internal/admin/middleware_auth.go` (the boundary per socket),
  `internal/admin/handlers_events.go` (the stream's gate on `originMatches`),
  `internal/admin/console_lan.go` (new: the plan, its reading, its comparison, the fence and the
  retirement, its state, `GET /api/console`), `internal/admin/console_lan_authority.go` (new:
  `parseLANAuthority`), `internal/admin/handlers_pages.go` (`pageData` and its socket kind), the
  templates `layout.html` (the menu's line, the pairing dialog's warning and its recovery advice),
  `settings.html` and `upload.html`, and `static/app.js` (the upload's notes, the rotation's
  warning, the recovery advice, the comment), `static/app.css` (the line's style) and
  `static/player/audio.js` (the comment).
- `internal/advertise/endpoints.go` (the `.local` name, exported for the reading).
- `internal/doctor/doctor.go` and `internal/doctor/console_lan.go` (new: both checks and their
  words, the source's words, `Deps.ConsoleLAN`).
- `cmd/bridge/main.go` (the plan, the first attempt, the banner, the test seams),
  `cmd/bridge/serve_console_lan.go` (new: the reading, the hold and the source wired in),
  `cmd/bridge/doctor.go` (both doctors' sources), `cmd/bridge/init.go` and
  `cmd/bridge/init_rewrite.go` (the flag, its refusals, the warning before the save, the save's
  seam `initHost.save`, the rewrite's rule), `cmd/bridge/banner_url.go` (the LAN line).
- `.github/workflows/gate.yml`: a job of its own for `TestARoutedClientInsideThePrefixGetsNoConsole`
  (§10.4), and its name in the `gate` job's `needs`.
- Tests: §9. Records: §10.5.

### 10.3 Negative controls

Each mutation is made in a copy, its named test shown red, and the tree left as committed:

1. the source check removed: `TestTheLANSocketAdmitsOnlyItsOwnNetwork`.
2. the LAN socket's Host compared without its port: the port rows of
   `TestEachHostIsAnsweredAsItsSocketsRuleSays`.
3. the LAN socket given loopback's Host rule: the loopback rows of its LAN column.
4. loopback's Origin widened to the LAN authority: the loopback column (B193's rule).
5. the event stream's gate left on the old matcher: its rows of
   `TestEachOriginIsAnsweredAsItsSocketsRuleSays`, and the structural test.
6. a LAN bind's failure sent to `adminErr`:
   `TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo` and
   `TestALANSocketThatDoesNotBindIsNoFailedStart`.
7. the menu's line read from the field rather than the socket: the same test's line.
8. `omitempty` dropped: `TestNoLANAddressWritesNoKey`.
9. a refusal quoting its value, Validate's and init's:
   `TestTheLANAddressIsRefusedForEachReasonWithoutItsValue` and
   `TestInitNeverEchoesARefusedLANAddress` (D6).
10. the rewrite's keep dropped: `TestARewriteKeepsTheLANAddressWithTheAdminAddress`.
11. a local probe dialling the LAN address:
    `TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback` and
    `TestOnlyTheBindAndItsChecksReadTheLANAddress`.
12. `chunkDigestHeaders` throwing without SubtleCrypto:
    `TestAnUploadWithoutSubtleCryptoSendsNoDigestAndSaysSo`.
13. a hold the kernel refuses falling back to an unrestricted socket, as OpenHome's does:
    `TestAHoldTheKernelRefusesLeavesTheLANSocketClosed` (D11).
14. the hold made a no-op: `TestTheConsolesHoldRefusesASYNFromAnotherInterface`,
    `TestTheConsolesHoldScopesItsRepliesOnMacOS`, and in the root job
    `TestARoutedClientInsideThePrefixGetsNoConsole`, whose routed client then gets 200 (D1).
15. the read-back skipped while the hold sets another index:
    `TestAHoldTheKernelRefusesLeavesTheLANSocketClosed`'s read-back case (D11).
16. a ULA on macOS taken through `IPV6_BOUND_IF`: `TestEachPlatformAndFamilyIsDecided` (D13).
17. a Windows row with weak host send read as strong: `TestTheStrongHostTableDecidesTheWindowsHold`
    (D13).
18. no reconciliation while the socket listens: the three retirement tests (D2).
19. the reconciliation comparing the address alone, or the index alone: the mask and the move
    tests (D2).
20. a retirement that closes the listener and not its connections:
    `TestAKeptAliveConnectionIsClosedBeforeTheNewSocketBinds` (D2).
21. a rebind before the old connections' close: the same test (D2).
22. a reading that fails keeping the socket: `TestAReadingThatFailsAfterTheBindClosesTheLANSocket`
    (D12).
23. the reading through `HostInterfaces`, an interface that does not read listed with none: the
    one-interface cases of `TestAReadingThatFailsBindsNothing` and
    `TestAReadingThatFailsAfterTheBindClosesTheLANSocket` (D12).
24. the IPv4-mapped spelling unmapped in the Host check alone:
    `TestHostAndOriginTakeEachSpellingAlike` (D3).
25. the trailing dot trimmed, or the case folded, in the Host check alone: the same (D4).
26. an absent port read as 80 in the Origin check alone: the same (D5).
27. the source taken from the doctor's own environment:
    `TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment` (D7, D14).
28. the example's or the menu's rollback sentence removed:
    `TestTheConfigExampleSaysWhatTheKeyCostsTheRollback`,
    `TestTheMenuLineSaysWhereTheAddressCameFromAndWhatItCostsTheRollback` (D8).
29. init's warning moved after the save, to its footer, and apart, the save made before the
    warning in the rewrite's path alone: `TestInitWarnsOfTheLostRollbackBeforeItSaves`'s first
    install and its rewrite (D15, F12).
30. the key's presence read from its value: `TestTheLANKeysPresenceIsReadApartFromItsValue` and
    `TestAnEmptyKeyIsReportedAsCostingTheRollback` (D16).
31. the pairing warning rendered on a loopback page too, or nowhere:
    `TestThePairingDialogWarnsBeforeGenerateOnTheLANSocket` (D9).
32. the fence removed, round 2's count alone, and apart, the fence's check and its count in two
    sections of `plan.mu`: `TestNoRequestIsAdmittedOnceARetirementBegins` and
    `TestAnAdmissionParkedBetweenItsCheckAndItsCountIsSeenByTheRetirement` (F1, F9).
33. the policy's context left uncancelled at the retirement:
    `TestAnEventStreamEndsAtItsSocketsRetirement` (F1).
34. one part of the policy left out of the comparison, each Windows row's among them, one part at a
    time: its case of `TestEachPolicyFieldRetiresTheSocketWhenItChanges` (F8, F11).
35. the rebind on the reading that found the change: `TestTheRebindTakesAFreshReadingAfterTheWait`
    (F2).
36. A's Windows row alone judged, round 2's rule: `TestTheStrongHostTableDecidesTheWindowsHold`'s
    rows of another interface (F3).
37. an Origin read through `url.Parse` and a Host through `url.URL`'s methods: the rows of §4.2's
    table of spellings in `TestHostAndOriginTakeEachSpellingAlike`, and
    `TestHostAndOriginShareOneAuthorityParser` (F5).
38. the pairing warning moved into the result step, and apart, the rotation's confirmation left as
    it is: `TestThePairingDialogWarnsBeforeGenerateOnTheLANSocket` and
    `TestARotationOnTheLANSocketWarnsBeforeItIsSent` (F6, F8).
39. the recovery advice offering a rotation from the page:
    `TestARotationOnTheLANSocketWarnsBeforeItIsSent` (F10).
40. `fileAddressAtBoot` filled from the effective address, and apart, a value the check refuses
    kept: `TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne` (F7).
41. the source over an empty key read as `environment`:
    `TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback` (F7).
42. serve's bind made without its hold, and apart, a default seam replaced:
    `TestServeHoldsItsLANSocketThroughConsolelan` and `TestTheProductionSeamsAreConsolelansAndLans`
    (F8).
43. `ReadInterfacesFrom` skipping an address that is no `*net.IPNet`, as `HostInterfaces` does:
    `TestReadInterfacesKeepsAnAddressThatIsNoIPNetAsTheAddressAlone` and
    `TestAReadingOfAWithNoNetworkBindsNothing` (F8).

### 10.4 Runs

`make fmt vet test build-all` through the build slot, on `go.mod`'s toolchain; `make docs-check`;
and, if a rules entry moves, `scripts/doc-split/verify_moves.py`. §9.5's Linux IPv4 test runs in
every Linux run, unprivileged, wherever the PR is built and tested; the macOS and Windows legs
(`.github/workflows/gate.yml:532-642`, `go test ./...`) run theirs, which fail rather than skip
where `CI` is set; and the routed client's test runs in a job of its own. The Windows leg also
measures a reading's cost (§3.4), and its log records the runner's interface tables, which the
record gives. The browser check and a routed SYN on Windows are the owner's (§11).

**The routed client's job (F8)**, written to the gate's conventions and `build-ci-tests.md`'s rules:

- `runs-on: ubuntu-latest`, `timeout-minutes: 15`, `permissions: contents: read`;
  `actions/checkout` and `actions/setup-go` pinned by their SHAs as every job of the gate pins them,
  `persist-credentials: false`, and `go-version-file: go.mod` with `check-latest: true` and
  `cache: false`, as `dsd-measure` and `installer` take them (`gate.yml:244-293`, `:704-724`).
- One `bash` step under `set -euo pipefail`. It builds the test binary as the runner's own user,
  `go test -c -tags ts_omit_webclient,ts_omit_debugeventbus -o "$RUNNER_TEMP/consolelan.test"
  ./internal/consolelan`, with the release tags, so the build-site census reviews the site with its
  tags; it prints `ip -V`; it runs the binary under `sudo` with `BRIDGE_QUALIFY_ROUTED=1`,
  `-test.run` naming the one test, `-test.v` and `-test.count=1`, through `tee` into a log under
  `$RUNNER_TEMP`; and it requires the test's `--- PASS:` line, and each case's, by `grep -q`, as
  `native-qualification` requires its own (`gate.yml:674-692`), so a skip, or a run that ran
  nothing, fails the job. Building as the user keeps the module and build caches the user's: only
  the test binary runs as root. The gate already takes root in a job (`dsd-measure` mounts a tmpfs
  with `sudo`, `gate.yml:288-291`).
- The job's name goes into the `gate` job's `needs` (`gate.yml:728`), whose verdict reads every
  need's result (`:758-774`): a job not named there is waited for by nothing (`build-ci-tests.md`,
  the `needs` bullet).
- The test is listed in the race shards too, where it skips (no root, no variable), so it runs in
  one place and is in no shard's way (`build-ci-tests.md`, the bullet on a test that runs in no
  shard).

### 10.5 Records

- `.claude/rules/auth-tls.md`: its first bullet, "The admin console is loopback-only with no auth,
  by design" (`:38-41`), says loopback only unless the operator opens it on the home network with
  `adminLANAddress`; a new bullet holds the posture's rules: the kernel's hold per platform and
  what is refused where, Windows' strong host model taken whole (§3.3), the policy read again,
  fail-closed retirement, the fence that admits nothing once a retirement begins and the fresh
  reading a rebind takes (§3.5), the source, the one parser for Host and Origin and its spellings,
  the `.local` name, the words, no peer in a line, where the address came from and what clears it
  (§2.6), the rollback the key costs, empty or not, said before init saves (§2.7), and the warning
  before a token is minted or rotated on the LAN socket, with its remedy (§6.2); the `csrfGuard`
  bullet's loopback sentence
  (`:91-98`) becomes per socket. Its `paths:` gain `internal/config/console_lan*.go`,
  `internal/admin/console_lan*.go`, `internal/consolelan/**` and
  `cmd/bridge/serve_console_lan*.go`. It has about 33 KB of its budget left, so the new rules go
  here.
- `.claude/rules/admin-console.md`: its first bullet (`:16-24`) per socket, and a sentence for the
  upload's length-only note and the pairing and rotation warnings. It has about 1.7 KB left: if
  the edit would
  pass the budget, its largest entry moves out first (`scripts/doc-split/move_entry.py
  .claude/rules/admin-console.md --largest`). `config-lifecycle.md`, 0.6 KB from its budget, gains
  nothing.
- `CLAUDE.md`: the "Admin console is loopback-only" line (`CLAUDE.md:103`) and its long form
  (`ops/History-Cross-Repo.md:15`), in one commit; the `internal/admin` row of the package table
  (`CLAUDE.md:58`), and a row for `internal/consolelan`.
- `config/bridge.yaml.example`: the loopback paragraph of the `deployment.mode` block (`:134-136`),
  and a block for `adminLANAddress`: what it opens, its lack of a login, a fixed address for the
  box, a firewall that must let the network in, no port-forward, the platforms that cannot hold it
  (§3.3), and the rollback sentence (§2.7).
- `docs/docker.md`: `BRIDGE_ADMIN_LAN_ADDRESS` beside `BRIDGE_ADMIN_ADDRESS` in the variables
  table (`:324`), and under "Admin console access" (`:508`) that the posture needs host
  networking and a host kernel of 5.7 or later.
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
  `ops/engineering-log/2026-11/<date>-b989--feat-b989-lan-console.md`, with what was measured: the
  holds' qualification per platform and family (which leg, which job), the runner's Windows
  interface tables and a reading's cost there, the bind's errnos per OS where CI shows them, the
  IPv6 bind a held socket refuses (§3.3), and the owner's browser check and Windows routed SYN.
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
   (§4.3); a menu line that follows the socket without a page load; a ULA on macOS, once
   `IPV6_BOUND_IF` is measured as `IP_BOUND_IF` was (§3.3); a netlink subscription on Linux, if 5 s
   proves long (§3.5); one adapter listing a reading on Windows, if the PR measures the walk dear
   (§3.4).
5. The owner's gates: the browser check (a phone and a laptop on one network open the console at
   `http://A:P` and at `http://<name>.local:P`; the menu's line; a save; an upload and its
   length-only note; the pairing dialog's warning before Generate token, the rotation's before its
   request, the recovery advice and the copy by selection; a refusal of another name), and on a
   Windows box with a second network, a client on that network whose address lies in A's prefix
   gets no console (§3.3).
6. The gate's new job (§10.4): the gate takes root already, in `dsd-measure`'s mount, but has not
   run a test binary as root.
7. The interval (§3.4): 5 s, OpenHome's, or shorter at a reading's cost.
8. Windows' cost (§3.3): with the strong host model taken whole, a box whose adapters forward for a
   NAT, or receive weakly, keeps the console closed.

## 12. The briefs' review items, and where each is settled

| Item | Settled in |
|---|---|
| R1, the source policy, routed and VPN clients | §4.1, §3.3; tested through the handler (§9.3) and on real sockets (§9.5) |
| R2, two sockets, one console | §3.1, §3.6 |
| R3, one canonical local endpoint | §1, §3.7 |
| R4, the init rewrite | §2.5, item 5 |
| R5, plain HTTP's secure context | §6.3 |
| R6, the loopback exception stays | §4.2, §4.3 |
| R7, the other gates and their hints | §4.3, §4.4 |
| R8, every config path | §2.5 |
| R9, the bound state, never the config's wish | §3.2, §5 |
| R10, refusals never echo the value | §2.2, §2.5, item 4 |
| D1, a routed client inside the prefix | §3.3 (the kernel's hold per platform, its limit), §4.1; §9.5, §10.3, 14 |
| D2, a stale admission policy | §3.5; §9.4's retirement tests, §10.3, 18 to 21 |
| D3, the IPv4-mapped Host | §4.2, §4.3 (refused alike in both); §10.3, 24 |
| D4, `.local` names | §4.2, §4.5 (case and one trailing dot, alike); §10.3, 25 |
| D5, port 80 | §4.2, §4.3 (absent, empty and `:80`, alike); §10.3, 26 |
| D6, no value echoed by init | §2.5, item 4; `TestInitNeverEchoesARefusedLANAddress`, §10.3, 9 |
| D7, turning it off | §2.3, §2.6, §5.3; §10.3, 27 |
| D8, the lost rollback said before it happens | §2.7, §5.1, §5.3; §10.3, 28 |
| D9, pairing on the LAN | §6.2, §6.4, item 2; §10.3, 31 |
| D10, the test table | §9.1 to §9.5 |
| D11, fail closed on the hold | §3.2, §3.3; §10.3, 13 and 15 |
| D12, fail closed on the reading | §3.2, §3.5; §10.3, 22 and 23 |
| D13, each platform and family decided | §3.3, §9.5; §10.3, 16 and 17 |
| D14, the serving process's provenance | §2.6, §5.2, §5.3; §10.3, 27 |
| D15, the warning before init saves | §2.7; §10.3, 29 |
| D16, an empty key still breaks the rollback | §2.5, item 6, §2.7, §5.3; §10.3, 30 |
| F1, retirement admits nothing after it starts | §3.1 (the policy's context), §3.5 (the fence, `Close`'s order); §9.4, §10.3, 32 and 33 |
| F2, the rebind takes a fresh reading | §3.5; `TestTheRebindTakesAFreshReadingAfterTheWait`, §10.3, 35 |
| F3, Windows' host model, both directions | §3.3 (the model taken whole), §5.3; §10.3, 36 |
| F4, the Linux IPv6 qualification reaches its assertion | §3.3 (the bind measured), §9.5 (IPv4 unprivileged, a ULA in the namespaces) |
| F5, one parser for Host and Origin | §4.2 (`parseLANAuthority`, the table of spellings), §4.3; §10.3, 37 |
| F6, pairing on the LAN | §6.2 (warned before the mint and the rotation, the remedy beside the result), §6.4, item 2; §10.3, 38 |
| F7, provenance and rollback edges | §2.6 (`both` over an empty key, the boot file's value), §2.7 (`--yes`), §5.2; §10.3, 29, 40 and 41 |
| F8, the pairing warning's order | §6.2; `TestThePairingDialogWarnsBeforeGenerateOnTheLANSocket`, §10.3, 38 |
| F8, coverage and package boundaries | §3.2 and §3.5 (every part, `lan.go`'s edges), §9.4, §9.5 (the namespaces and Go's threads), §9.6, §10.4 (the job's rules); §10.3, 34, 42 and 43 |
| F9, the fence's admission is atomic | §3.5 (one section for the check and the count); `TestAnAdmissionParkedBetweenItsCheckAndItsCountIsSeenByTheRetirement`, §10.3, 32 |
| F10, no new credential over the LAN | §6.2 (the remedy on the bridge's own machine); §10.3, 39 |
| F11, every Windows row revalidated | §3.3, §3.5 (the table in the policy); §10.3, 34 |
| F12, the rewrite test asserts what is possible | §2.7, §9.3 (`TestInitWarnsOfTheLostRollbackBeforeItSaves`); §10.3, 29 |
