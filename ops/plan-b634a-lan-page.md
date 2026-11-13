# B634a: the bridge player's LAN page: browser access, browse, transport and adding a track

Design, round 2 (B634a). Docs only: this file is the round's one change; round 1's plan
(`ops/plan-b634-lan-player-design.md`) stays as it was, a record. Base: `orch/b634` at 7e57c8a5 (main at
4678b983 plus round 1's plan); every `file:line` below is at 4678b983.

**Inputs**: the brief (B634a of the advisor's three-way split), its budget and exit; its review (Codex, Carbon,
2026-11-13, binding); the round-1 findings it gives this round (§11). Nothing of round 1's PR 1 is here: no
control layer or turn, no maintenance hold, no lease, no binding seam.

## 1. The hazard

A browser on the LAN reaches the bridge's player, on a headless box whose owner has a browser and not the app.
At 4678b983 it reaches nothing: the console binds loopback and refuses another Host
(`internal/admin/admin.go:2634`, `:2671`), and OpenHome's SOAP is off by default
(`internal/config/player.go:138-147`). Four things must stay true:

1. **Strangers, on the page's routes**: another LAN host cannot play, pause, stop, seek or edit the queue
   through the page, guess the passcode faster than a per-address limit allows, or keep the owner from logging
   in (§4). **The passcode protects the page, not the player** (review point 1): OpenHome's SOAP writes take no
   passcode (`internal/player/openhome/writes.go:112-146`), so while `output.player.openhome` is on any LAN
   controller can stop or steer playback, as today; the page and the console say so (§4.5), and protecting
   every surface is out of budget (R1, §7).
2. **No secret in clear**: the passcode and the session cookie cross the LAN inside TLS alone (§3.2).
3. **Coexistence**: the app's casts and OpenHome controllers go on working, and a page command never silently
   overwrites a queue it did not read: every page write but Stop and Pause carries its last read's generation.
4. **Honesty**: no track the bound DAC cannot play is offered as playable; short evidence is "unknown", and a
   rate the player converts is "converted", never "bit-exact" (§5).

Kept: no admin route on the LAN (`auth-tls.md:38-41`); no page in public mode or 1-bit Cloud; no DAC holder stopped.

## 2. The budget

**In**, each the reuse of a mechanism that exists, with the one extension it needs:

| Mechanism | Reused | Extended by |
|---|---|---|
| The listener (§3) | OpenHome's LAN binding, admission and limits; the pair serve loaded; net/http's TLS | the binding moved into a package both surfaces use; a Host and an Origin rule; two keys read at boot |
| The passcode (§4.1) | an `adminauth.Store` at its own file: `ResetPassword`, `SignOutEverywhere`, `Verify`, `ValidateSession` | nothing in `adminauth` |
| Sessions, lockout (§4.3, §4.4) | adminauth's sessions; `adminauth.RateLimiter` per address | a `__Host-` cookie; a passcode floor |
| Browse (§5) | the console's catalog; GET /v1/outputs' DAC reading; the published DSD set; `library.Resolver.Member` | three indexed facts per catalog row; the reading hands its altsets on; a catalog read its caller bounds |
| Transport, add (§6) | the engine's methods OpenHome calls; `IfGeneration`; OpenHome's budget and edit gate | the gate's constant moved beside `engine.ReserveTimeout` |

**Out**: the DAC switch and the tone (B634b); the hold, the lease, the turn and round 1's control layer; any new
engine or queue primitive; compound edits, `Delete`, `DeleteAll` (B634c); a request past the write deadline (no
202); DSD writes and choice; covers; repeat, shuffle; Signal Path; IPv6 (§3.4); OpenHome auth (R1); a live switch.

## 3. The listener and HTTPS

### 3.1 OpenHome's binding and admission, shared

- **One mechanism, two users.** PR 1's first commit moves OpenHome's socket machinery, unchanged, into
  `internal/player/lanlisten`: the listing every 5 s (`internal/player/openhome/listener.go:400-528`), one IPv4
  socket per served address (`servedOf`, `:529-538`; `Listen`, `:973`, `tcp4`, `SO_BINDTOIFINDEX`), the gate at
  accept (`judge`, `:828-850`: the socket's address served now; a source in its on-link prefix, of the LAN's
  classes, never loopback or the box's own; then the caps), retirement and the stop. OpenHome keeps its SOAP,
  SSDP and tests, green across the move; no admission rule changes; the page is a second user with its own port.
- **The page's limits**: 32 connections, 8 from one source, counted after admission; 5 s for the header (which
  also bounds the TLS handshake), 5 s for a body, 10 s for the request and the write, 30 s idle; 16 KiB of
  headers; bodies of 1 KiB (login) and 4 KiB (writes). **No request slots** (§4.4). **HTTP/1.1 alone**
  (`http.Server.Protocols`): one request a connection, so the caps bound the requests under way, as OpenHome's.

### 3.2 HTTPS on the bridge's certificate

The pair serve loaded (`servertls.LoadOrGenerateWithOptions`, `cmd/bridge/main.go:3775`), which the API presents
and the app pins, is the page's one certificate, never the SNI manager's Tailscale or autocert routes
(`:3814-3846`); nothing mints, rotates or reloads it for the page; TLS 1.2 at least; `ErrorLog` handshakelog's.
**Plain HTTP** gets net/http's fixed `400 Bad Request` ("Client sent an HTTP request to an HTTPS server.") and a
close, the handler never run (round 1's probe, go1.27.3); no redirect, no HSTS (`auth-tls.md:749`). **The first
visit** warns (self-signed): the console's panel and `bridge output passcode status` print the pair's SHA-256
fingerprint (`bridge cert info`'s) to compare with the browser's; the page shows none (an attacker's could).
**Residual R7**: a LAN host that answers for the box's name or address (an mDNS or ARP spoof) with a certificate
of its own meets the browser's warning, and a user who goes past it hands it the passcode or the cookie; so the
console and the page's login say to compare the fingerprint at every warning, not the first alone.

### 3.3 Host and Origin (Carbon 10)

**The Host rule**, first on every request (421 otherwise; each refused name logged once, 16 at most):

- **The port** is the socket's own, written (the page's port is 1024 to 65535, which a browser always writes).
- **An IPv4 literal** is taken when it is the address the connection arrived on, as OpenHome's row 1 takes it
  (`openhome.md`, the SOAP table). **An IPv6 literal** is refused: the page binds no IPv6 address (§3.4).
- **A name**, lower-cased with one trailing dot dropped, is taken when it ends in `.local` and is exactly a DNS
  name of the pair serve loaded, its `<short>.local` (`internal/tls/tls.go:402-434`). The bare host name,
  `localhost`, a custom endpoint's, a Tailscale name and any other are refused.
- **When names change**: the pair is read once, at serve's start, so the names taken are the names presented
  all run long, and `bridge cert rotate` reaches both at the next start. mDNS on or off changes nothing: it
  decides whether a client resolves the name, not whether the name is the box's. A box renamed while serve runs
  keeps its old `.local` name until a rotation and a restart (`tls-cert-sans` warns); a name not in ASCII is
  taken in the IDNA form certified (`auth-tls.md:561-656`).

**Origin**: every POST carries `Origin` equal to `https://` and the request's Host (scheme, name, port), or it is
403 `cross_origin` before its body is read: another service's page on another port of the box is same-site, so
`SameSite` passes it, and this rule does not. A POST is JSON; a GET carries no Origin and needs the session; no
answer carries CORS. **Headers**: a CSP of `default-src 'self'`, `frame-ancestors 'none'`, `base-uri 'none'` and
`form-action 'none'`, `nosniff`, `Referrer-Policy: same-origin`, `no-store` under `/api/page/`; the script paints
every library string as text (`textContent`), never as markup.

### 3.4 IPv6 (review point 9)

The page keeps OpenHome's binding rule, IPv4 alone (`servedOf`, `Listen`'s `tcp4`). **Residual R5**: a LAN over
IPv6 alone cannot open the page; a dual-stack browser refused on IPv6 falls back to IPv4 at once (RFC 8305).
Binding IPv6 is a later PR, with a Host rule for bracketed literals and zones, and the limiter's IPv6 key (§4.4).

### 3.5 Its settings, and when it runs

- **Two keys** in the player block, read as `openhome` and `openhomePort` are (`player.go:490-499`):
  `output.player.lanPage`, off by default, and `lanPagePort`, nil for 7793, 1024 to 65535 (a port every browser
  writes, §3.3) or 0 for a kernel-picked one. A release that does not know them turns its player off, logged.
- **Read once, at boot**, as OpenHome's opt-in (`openhome.md:21`): `planServePlayer` starts the page only when it
  starts the player with the boot snapshot's `lanPage` on, after launching the engine, with no readiness gate
  (until the engine runs, the transport and the add are 503 `player_not_running`); the player's shutdown stops it
  first, before OpenHome and the engine, in a bound `servePlayerBounds` gains. Not started though asked for (the
  player off, a clash, a store that does not read): said once at boot.
- **The port clash** is `config.OpenHomePortClash`'s rule, one name more: at boot it leaves the page off and the
  player running, naming the setting, never the port; the console's switch refuses it (409 `lanpage-port-clash`);
  while the page is on, the settings save refuses a listener moved onto its port.
- **On and off**: the console's Player page, `POST /api/output/player/lanpage {enabled}`, its fourth write of
  `output.player`, under the console's lock as the other three, answering `restart` or `unchanged`; no CLI writes
  it. Off acts at the restart, the sign-out at once (§4.1). A first setup is the console's or the CLI's (W2).
- **Never in public mode or on 1-bit Cloud**: no page where `IsPublic()` holds (`player.go:643-645`); the console
  answers 403 `player_not_available`. **managedSettings**: `playerLANPage`, owned by a managed `playerEnabled` too.

## 4. The passcode, sessions and the lockout

### 4.1 The store, its write and its reads (review point 3; Codex 4, Carbon 7, Carbon 6)

- **The page passcode's own store**: `<dataDir>/player-page-auth.json`, an `adminauth.Store` (`OpenStore`,
  `internal/adminauth/store.go:302`) with one account, `player-page`, whose password is the passcode: never
  `adminauth.json`, the console's; not bridge.yaml, whose CLI write a running serve would not take and every copy
  of the config would carry. Backups do not carry it; a restore leaves the box's own.
- **The write path is adminauth's public API, unchanged**: a set or change is `ResetPassword("player-page",
  passcode, adminauth.EndSessions)` (`:426`), which also installs the first; a sign-out of every browser, the
  passcode kept, is `SignOutEverywhere()` (`:509`). Both commit through `commitAndConfirm` (`:525-606`): the
  rename only over the bytes the write was built from (`commitLocked`, `:981`), then after `confirmSettle` the
  file read and written again where a running bridge's in-flight write put back what it replaced. Calling it
  closes round 1's rotate-then-restore race (Codex 4, Carbon 7), as adminauth's tests pin
  (`TestARotationIsNotLostToAWriteTheBridgeHadInFlight`, `TestASignOutIsNotLostToAWriteTheBridgeHadInFlight`).
- **Who writes**: the console (loopback), `POST /api/output/player/lanpage/passcode` (typed twice, or generated)
  and `.../signout`, through the running page's store, so its sessions end in memory before the answer
  (`tookOwnSignOutLocked`, `:684`), or one opened for the write when no page runs; and `bridge output passcode set
  | generate | sign-out | status` beside serve, as `bridge admin reset-password` (`cmd/bridge/admin.go:62-144`):
  `refuseDataDirAway` first, no data-dir lock, refused on a public bridge, a file made as root given the data
  dir's owner (`store.go:1620`). None prints the passcode or its length; each logs a line naming the page.
- **The read path**: a login is `Verify` (`store.go:704`), which reads the file at every attempt: no cache. A
  session check is `ValidateSession` (`:1108`) and its stat gate (`refreshIfChangedLocked`, `:1147-1184`). **The
  gate is safe here** (Carbon 6): (1) every write renames a file made beside this one (`:1588`), a new inode,
  which the gate sees at the next request; (2) it misses (`fileStamp.matches`, `:1407-1425`) a same-size rewrite
  in place within one clock tick, which no writer of this file makes, or two renames between two of serve's
  stats recycling the inode it saw at its size and time: two writes by another process in one timestamp tick;
  (3) even then a sign-out is late, never lost: its marker is an event every writer carries over
  (`auth-tls.md:186-230`), and serve reads the file whole at its next login, its shutdown flush and each session
  write, at least every 30 s while a session makes requests (`store.go:234`): that case ends within 30 s of use.
- **Residual R2, the confirmation interval** (`store.go:542-546`): a session write of the running bridge stalled
  over `confirmSettle` (250 ms off Windows, `:611`) between its byte check and its rename lands after the CLI's
  confirmation, putting the old credential and marker back: the old passcode works again and its sessions stay.
  The next login with the new passcode is refused, which shows it; the command run again sets it. A kernel lock
  would close it (#1039 declined one). The console's own writes share the store's mutex with every session write.
- **No clear**: adminauth removes no account; to shut the page, sign every browser out (at once) and turn it off
  (at the restart). A store with no account is no passcode: the login, whose `Verify` reads the file, answers 409
  `no_passcode` ("No passcode is set: set one on the console or with `bridge output passcode set`").

### 4.2 The passcode's rules

**10 to 64 characters of printable ASCII** (U+0020 to U+007E), checked by the console and the CLI before
`ResetPassword`, which takes any non-empty string (`store.go:113-121`): nothing to normalize, under bcrypt's 72
bytes. **Generated**: 12 of 31 symbols with no look-alikes (about 8 × 10^17 values), shown once, the console's
first offer. **The hash**: adminauth's bcrypt at cost 12 (`adminBcryptCost`, `store.go:71`), every attempt.

### 4.3 Sessions

`CreateSession("player-page")` after a `Verify`: 256 random bits kept as their SHA-256, 24 h idle and 7 days at
most, 1,024 at most (`store.go:129-149`), kept across a restart. **The cookie**: `__Host-bridge-page`, `Secure`,
`HttpOnly`, `SameSite=Strict`, `Path=/`, no `Domain`, `Max-Age` 7 days. **Round 1's High 1 is refuted**: RFC
6265bis §4.1.3.2 asks a `__Host-` cookie for `Secure`, a secure origin, no `Domain` and `Path=/`, nothing of the
port, and cookies are not scoped by port (RFC 6265 §8.5), so it works on 7793. **Residual R3**: hence it also goes
to another HTTPS service on the box's name; its pages cannot use it on the page (the Origin rule).

### 4.4 The lockout, in order (Carbon 8; review point 7)

The login's steps, each before the next, with **no shared slot or turn in any of them**:

1. Admission at accept: an on-link IPv4 source, at most 8 of its connections open.
2. TLS; the Host and Origin rules; a JSON POST, its body at most 1 KiB, read within 5 s.
3. The passcode's shape (§4.2): another shape is 401 `wrong_passcode` at once, hashing and counting nothing.
4. **The per-client limit**: `AllowAndReserve(source, "player-page")` on a `RateLimiter` of the page's own
   (`internal/adminauth/ratelimit.go:263`): 5 failures in 15 minutes per address, the reservation the count
   (`auth-tls.md:412-414`). Past it: 429 `throttled` at once, hashing and holding nothing ("Too many tries from
   this device. Try again in 15 minutes."). No 5 s delay: it would hold a connection; the limit is no secret.
5. `Verify`, bcrypt: nothing queues it, so the owner's passcode is checked at once whatever others do.
6. On success `RecordSuccess` (`:383`) clears that address's bucket, and the session is made.

A stranger's failures count against its own address alone, never another client's; nothing is counted
bridge-wide; no lockout is permanent; an address causes at most 5 hashes per 15 minutes, the caps bounding those
at once. Any other API route without a session is 401 at once; `/` and `/static/` serve embedded bytes.
**Residual R4, one host with many addresses**: k addresses are k buckets, 5k guesses per 15 minutes, about
121,000 a day for a /24's 253, which a generated passcode outlasts and a chosen one only by its strength; with 4
it can hold the 32 connections, keeping the page (not the player) from everyone while it does, with no lockout
after; clients behind one NAT share a bucket. Were IPv6 bound, the limiter keys it by /64 (`ratelimit.go:114`).

### 4.5 What pages and logs say, and OpenHome's trust (review point 1)

Never the passcode, its hash, a session token, the cookie or a Cookie header. A refused login is one Warn naming
the source address (canonical, no port) and its word (`wrong_passcode`, `throttled`, `no_passcode`, `unreadable`),
once per address a window, which PR 1's rules patch adds beside `auth-tls.md:786-801`; the console counts refusals.
Error bodies carry fixed words, never an error's text or a path. **OpenHome's trust is said** beside the login and
in the footer while OpenHome runs, and beside the console's switch: "This passcode protects this page. While
casting from apps is on, the player also takes commands from any device on this network with no passcode: it can
play, pause, stop and change this player's queue."

## 5. Browse and eligibility (Claude 4; review points 4 and 5)

### 5.1 What the page lists

- **The bridge's own player**: its title is the player's room (`openhome.FriendlyName`), and its first line says
  it plays on the DAC connected to the box, not in this browser, apart from the console's web player.
- **The console's catalog** (`internal/admin/catalog.go:101-163`), exported as a reader its caller's context
  bounds (it waits for the build's flight on `DoChan`; the build unchanged): albums, an album's tracks, artists
  and search, as the web player's handlers slice them (`handlers_player.go:254`); not ready within the budget,
  503 `library_loading`. **Three facts more per row**, which the index holds and the catalog's one query reads
  beside the rest (`internal/manifest/catalog_refs.go:99-191`; `librarycat/catalog.go:22-49`): channels
  (`tags_json.channels`, `internal/manifest/types.go:228-239`), the sample format (`tracks.sample_format`, B513,
  `types.go:68-87`), a DFF's compression. No new index, no new read.
- **Each track** carries its `id`, the DLNA track id of the original at its path (`library.Reference`,
  `internal/player/library/library.go:150`), never a rendition's or a path, and its verdict. The browser's
  `playerPlayabilityDTO` (`handlers_player.go:879`) is not used.

### 5.2 The verdict, from the endpoint (review point 4)

Four reads decide it: the row, the library's refusals, the bound endpoint's playback altsets and the DSD set the
backend published. **A refusal outranks `unknown`, which outranks `playable`**: nothing is playable without the
evidence for every term.

| Verdict | Word | When |
|---|---|---|
| `not_playable` | `routed`, `sacd_virtual_track`, `unknown_container`, `lossy_refused` | not one of the bridge's playable originals (the resolver's words, `library.go:96-131`; the decoders', `internal/player/decode/refuse.go:12-20`) |
| `not_playable` | `unsupported_channel_count`, `unsupported_bit_depth`, `not_yet_supported`, `float_pcm_refused`, `unsupported_compression` | the decoders' limits on the facts indexed: channels but 2; a depth but 16, 24, 32; 32-bit FLAC; float PCM; DST |
| `not_playable` | `dac_rate`, `dac_depth` | PCM the endpoint does not take: a stereo altset takes a packing of the depth, none at this rate; or none takes one |
| `not_playable` | the set's word, `dsd_off` | a DSD rate the published set refuses, or a set that plays no DSD |
| `unknown` | `dac_not_read` | no whole reading of the bound DAC: the player not running, its output unavailable, a reading not whole or past its 500 ms |
| `unknown` | `format_not_indexed`, `channels_not_indexed` | the rate, the depth or (32-bit WAV, AIFF) the sample format not indexed; no channel count, which is every FLAC, WAV, AIFF and ALAC row: the index holds channels for DSF, DFF, Ogg, WavPack and APE alone (`types.go:228-235`) |
| `playable` | `pcm` | one altset of the endpoint has 2 channels, the rate (listed, or inside its continuous range) and a format of `playback.Packings(depth)` (`internal/player/playback/packing.go:21-31`) |
| `playable` | `native`, `dop` | a DSD rate the published set plays natively or as DoP |
| `converted` | the plan's rate, bits | a DSD rate the set converts: "Plays converted to PCM, 176.4 kHz, 24 bits", never "bit-exact" or "lossless" |

- **The altsets are combinations**, never GET /v1/outputs' summary (`maxPCMRate`, `pcmBitDepths`): a rate and a
  format count only within one altset, from the reading that projects the element's `dac`
  (`readPlayerDACObserved`, `cmd/bridge/serve_player_output_dac.go:89-140`), which PR 2 has hand them on beside
  its projection, from the same bracketed reading and checks (the wire's bytes unchanged). A `Bits` line decides
  nothing: the kernel takes the container. **The DSD set**: `Backend.Facts`' epoch (`serve_player_output.go:160`).
- **The review's cases**: a 16-bit track on a DAC whose one PCM format is `S32_LE` (B735's Brooklyn) is `pcm`;
  altsets of 44.1, 48, 96 and 192 kHz make 88.2 kHz `dac_rate`, never judged by the top rate; a five-channel DSF
  is refused, and a six-channel FLAC, its channels not indexed, is `unknown`, never `playable`.
- **On the page** `channels_not_indexed`, the common case, reads "Plays if the file is stereo: the library does
  not record its channels". **A hint, never the gate**: a `not_playable` track is offered no add, an `unknown` one
  is; the engine's open decides, and its skip, with the decoder's reason, is what the queue shows.

### 5.3 Membership before the insert (review point 5)

`InsertAfter` checks the reference, the IDs and the capacity alone (`internal/player/engine/edits.go:9-41`), so the
add checks first, as OpenHome's confinement does (`writes.go:422-443`): the id's shape (`trackref.IsID`,
`internal/player/trackref/trackref.go:196`), then `library.Resolver.Member(ctx, id)` (`library.go:490`), opening
nothing, then the edit gate (§6.2), then the engine. `Member`'s refusal is 409 `not_playable` with its word,
nothing inserted: an upstream's track (`routed`), an SACD virtual track, a track since removed (`not_indexed`).

### 5.4 The DAC, shown

Read-only, from GET /v1/outputs' own facts in-process (`bridgeOutput.facts`): the element's state, reason and
last error, the DAC's name and formats redacted as there (B543), its DSD rates as the set gives them. The state
is read every 2 s while visible (one engine snapshot), the DAC at load, on a change of state and when asked, in the
element's shared, bounded flight (`serve_player_output_dac.go:16-60`). A skipped entry says why; entries are named
as the console names the track heard (`consolePlayer.name`, `serve_player_console.go:461`), never from metadata.

## 6. Transport and adding a track (review points 2 and 8)

### 6.1 The calls (review point 8)

The engine's own methods (`internal/player/engine/engine.go:438-529`), those OpenHome's writes call:

| The page's action | The call | `IfGeneration` |
|---|---|---|
| Play; Pause; Stop | `Play(ctx, g)`; `Pause(ctx)`; `Stop(ctx)` | Play alone |
| Next; Previous | `Next(ctx, g)`; `Previous(ctx, g)` | yes |
| Seek, PCM alone | `SeekAbsolute(ctx, seconds, g)` | yes |
| Play an entry of the queue | `PlayID(ctx, id, g)` | yes |
| Add a track | `InsertAfter(ctx, afterID, ref, nil, g)` | yes |

- **In**: `PlayID` (OpenHome's SeekId). **Out**: `Delete`, `DeleteAll` (B634c's), `PlayIndex`, `SeekRelative`,
  `SetRepeat`, `SetShuffle`. **Seek** is offered for PCM alone (`timeSeek.dsd` is `unsupported`, B674).
- **`afterID`** is from the page's read: the selection for "Play next" (0, the head, when none), the last ID for
  "Add to the end" (0 when empty). **`ref`** is `engine.NewReference(id)`, the bare id serve's player names an
  original by, no metadata (R6); one track a request (a body naming more is 400).
- **The generation** is the page's last read's (`Snapshot.Generation`'s string, read back by `ParseGeneration`;
  `snapshot.go:99-104`), checked in the step that runs the command (`owner.go:582-583`). Each answer carries the
  state after it, so the next click carries the generation the last one made.

### 6.2 Admission is not completion (review point 2)

**One budget** from the request's arrival at its handler, the write deadline less a second (9 s), the call's
context the request's with that deadline, as OpenHome's writes (`writes.go:25-49`). **The edit gate is OpenHome's**:
`InsertAfter` is called only with `editBudget` (`engine.ReserveTimeout` and a second; `writes.go:43-48`, `:250-255`)
left, last and after `Member`, as `playlistInsert` orders it (`:386-411`); PR 2 moves the constant beside
`ReserveTimeout` (`engine.EditBudget`) for both to read. **The answers**, each with the state now:

| The engine's answer | The page's answer | What changed |
|---|---|---|
| nil | 200 | the command's effect |
| `ReasonConflict` (`errors.go:25`) | 409 `stale` | nothing |
| wraps `ErrNotAdmitted` (`errors.go:160`), or the gate refused | 503 `busy`: "Nothing changed. Try again." | nothing |
| `ReasonCancelled` without `ErrNotAdmitted`: admitted, no answer in the budget (`engine.go:633-685`) | 504 `outcome_unknown`: "The player took it and did not answer in time; this is the player now." | perhaps its effect, now or later |
| `ReasonClosed`, or no engine | 503 `player_not_running` | nothing |
| another refusal | 409, its word (`full`, `seek_out_of_range`, `output_unavailable`, ...) | nothing |

**The page never replays**: after `busy` it offers the action again; after `outcome_unknown` it reads the state at
once and at its next poll; it sends nothing itself. A browser that leaves cancels its request's context: a command
not yet admitted does not run, an admitted one may. **Its limits** are OpenHome's (`openhome.md`, the write
budget's bullet): an admitted edit outlasts the budget only through a stall on the engine's owner.

## 7. Coexistence and the residuals (Carbon 11; review point 6)

- **Which wins**: the engine's owner runs commands in arrival order. SOAP's writes carry no generation
  (`writes.go:119-144`): a controller's last admitted command wins, as today. A page command wins only where
  nothing visible changed since the page's read; else it changes nothing (409 `stale`), and the page reads again:
  "The player or its queue changed since this page read it (the 1-bit app, another controller, or a track
  ending). Nothing was changed." A track boundary moves the generation, so a Next or a Seek sent across one is
  `stale`: it was meant for the track before. **All read one engine snapshot**: the page every 2 s, the app by
  its OpenHome polling, GET /v1/outputs per request. **With the browser and phone closed**, playback goes on.
- **R1, OpenHome's trust**: while `output.player.openhome` is on, any LAN controller plays, pauses, stops and
  edits the queue without the passcode (§1, §4.5). Authenticating OpenHome is out of budget.
- **B755** (2.4 / 0.2.5; a design study first): the bridge's player would play another bridge's tracks. Until
  then the engine takes only this bridge's own playable originals (`bridge-player.md`, B470 5c2), so the page
  lists and adds those alone: another server's track is `routed`, which `Member` refuses.
- **B671** (2.3, provisional): the app would hand the phone's queue, track and position to the bridge's player in
  one tap. A queue it hands over is the engine's like any other: the page reads it with its generation and never
  overwrites it silently: a page write made before the handover is `stale`.
- **R6, a page entry's form**: a bare track id, no metadata, which an OpenHome controller reads as every stored
  reference (`openhome.md`, the queue's reads: Read's Uri as stored) and shows as an entry it did not insert
  (question 1, §11). **An album is one tap per track** (B634c's residual); **the page removes nothing**: Next
  skips an added track, a cast or a controller removes it.

## 8. PROTOCOL.md, and the page's routes

**No change, no Mirror PR**: the page's routes are not `/v1` nor the app's contract (as the console's `/api/` is
not); `/v1/health` gains no flag; GET /v1/outputs keeps its bytes. **The routes**, any other path 404: with no
session `GET /` (the login form, or the player), `GET /static/<file>`, `POST /api/page/login`; with one, `POST
/api/page/logout`, `GET /api/page/state`, `GET /api/page/library/` (`albums`, `albums/<id>`, `artists`,
`artists/<id>`, `search`), `POST /api/page/transport` and `/add`; no console route is under `/api/page/`.

## 9. The PRs

**Runs**, in the build slot: `-race -count=50` on new cases (half the cores at most); `go vet` with the tags on
linux/amd64 and linux/386, and windows/amd64 and darwin/arm64 on touched packages; each touched package with
`-race -timeout 120m`; the pinned gofmt; `make test`, `make build-all`, `make docs-check`. **Each security case**
(Claude 10) pairs its refusal with an authorised request through the same HTTPS listener; its **control** removes
that one guard as an uncommitted edit, the listener kept: the refusal reddens, the authorised request stays green,
`git status` clean after. Tests use `lantest`'s loopback box, a test pair and the real engine on a stand-in
backend, never the host's interfaces, `/proc` or `/sys`. **Rules** (none this round): PR 1 adds the packages to
`openhome.md`'s globs (44 KB of 100) with the page's surface, its posture to `auth-tls.md`, `playerLANPage` to
`config-lifecycle.md` (at 99.9%: a long form moved out first, `move_entry.py`), a fourth Player write to
`admin-console.md`; PR 2, the verdict.

### PR 1: browser access

- **Scope**: §3, §4, the shell (login, logout, `no_passcode`, the state and DAC read-only). **Depends on**
  nothing unmerged. **Files**: `internal/player/lanlisten/` (new, moved with its tests from `openhome/listener.go`,
  `restrict_*.go`); `internal/playerpage/` (new: server, TLS, Host, Origin, login, sessions, shell);
  `internal/config/player.go`; `cmd/bridge/serve_player_page.go`, `output_passcode.go` (new); the console's Player
  page (switch, port, URLs, fingerprint, passcode, sign-out, refusals, §4.5's words);
  `ops/settings-apply-semantics.md`; the log; the rules patch.
- **Security cases**, each over the HTTPS listener:

| Case | Refused | Authorised beside it | Control |
|---|---|---|---|
| `TestTheLANListenerServesNoAdminRoute` (Claude 8) | every console route pattern (its table exported for the test), with a session and without: 404, but the two the page shares: `GET /` is the page's own document (its marker, never the console's layout), and `/static/<name>` of each console asset is 404 or, for a name the page embeds too, the page's own bytes, never the console's | `GET /api/page/state` 200 | the console's handler as the page's fallback |
| `TestAPlainHTTPRequestGetsNoPageFormOrCookie` | plain HTTP: the fixed 400, no `Set-Cookie`, no `<form`, no session | HTTPS `GET /` with its form | the socket serving plain HTTP beside TLS |
| `TestTheSessionCookieIsHostPrefixedAndSecure` | a session cookie lacking `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/` or `__Host-`, or carrying a `Domain` | the next request served with it | `Secure` dropped |
| `TestASourceOutsideTheOnLinkPrefixIsRefusedAtAccept` | closed before any TLS byte, counted against nothing | a source in the prefix logs in | the gate without the source check |
| `TestAHostTheRuleDoesNotTakeIsRefused` | another IPv4 address, an IPv6 literal, the bare host name, `localhost`, a custom endpoint's name, another port: 421 | the arrival address and the `.local` name | each arm dropped alone reddens its row |
| `TestAPostWithAnotherOriginOrNoneIsRefused` | none, `null`, another port or host, `http:`: 403 before the body, nothing changed | the right Origin | the check dropped |
| `TestAStrangerWithoutASessionReachesNoPlayerRoute` | every `/api/page/` route but the login: 401, the engine's command count unchanged | the same with a session | the session check dropped |
| `TestAPublicBridgeStartsNoPage` | public mode binds nothing; the console's switch 403 | a loopback serve answers | the public check dropped |
| `TestAPasscodeSetElsewhereEndsEveryPageSession` | after `bridge output passcode set` (the command's function, its store a second instance on the file, as another process's is), two sessions 401 at their next request; after the console's, before it answers | the new passcode logs in | the console's write with `KeepSessions`; the session check from a cache of tokens checked once |
| `TestTheLoginIsLimitedPerAddressAndNeverKeepsTheOwnerOut` | from A five wrong, then the right one: 429 at once, no hash (the verify seam not reached), nothing held | B logs in at once; A after the window, a success clearing it | the limiter keyed on the user name alone |
| `TestNoLoginWaitsForAnother` | strangers' checks held at the verify seam | the owner's login, from another address, answers | one turn around `Verify` |

- **Also**: `TestAMalformedPasscodeCostsNoHashAndNoCount`, `TestAPasscodeIsNeverLoggedOrEchoed` (`loggingtest`),
  `TestThePageIsOffByDefaultAndReadAtBoot`, `TestThePagesPortClashLeavesThePageOffAndNamesTheSetting`,
  `TestAManagedPlayerSwitchOwnsThePage`, `TestASnapshotCarriesNoPagePasscode`,
  `TestThePageSaysWhatThePasscodeDoesNotCover`; structural, `TestThePageServesItsOwnRoutesAlone` (§8),
  `TestThePagePackageImportsNoConsoleOrAPI` and `TestThePagesPasscodeIsWrittenOnlyThroughAdminauth` (go/ast: opened
  by `adminauth.OpenStore`, written by `ResetPassword` and `SignOutEverywhere` alone, its file named nowhere else;
  control: an `os.WriteFile` of it).
- **Tier**: standard; on the owner's Pi, a phone's and a laptop's browser by `.local` name and by address, the
  warning answered by the fingerprint, plain HTTP answered 400, a second device throttled as the owner logs in.

### PR 2: the page

- **Scope**: §5, §6, §7. **Depends on** PR 1. **Files**: `internal/playerpage/` (routes, script);
  `cmd/bridge/serve_player_page.go` (the verdict, a pure function of the row, the altsets and the set; the calls);
  `internal/admin/catalog.go`; `manifest/catalog_refs.go`; `librarycat/catalog.go`; `serve_player_output_dac.go`;
  `engine/reserve.go`; `openhome/writes.go`; the log; the rules patch.
- **Cases**, red on its base:

| Case | What it holds | Control |
|---|---|---|
| `TestAWidenedFormatIsPlayable`, `TestAMissingIntermediateRateIsNotPlayable`, `TestMultichannelContentIsNeverPlayable` | §5.2's three cases: 16 bits in `S32_LE`; 88.2 kHz `dac_rate`; the DSF refused, the FLAC `unknown` | `maxPCMRate` for the altsets' rates; the depth's own container for `Packings`; no channel count read as 2 |
| `TestADSDRateIsJudgedByThePublishedSet`, `TestNoWholeReadingOfTheDACIsUnknown`, `TestAConvertedRateIsNeverCalledBitExact` | native, DoP, converted, refused, off; `dac_not_read`; the words served and the script's, comments stripped | a converted rate worded as PCM |
| `TestAnInsertOfATrackTheLibraryDoesNotServeIsRefused` | `routed`, `sacd_virtual_track`, `not_indexed`: 409, the generation and the IDs unchanged; a playable track's add lands | `Member` skipped |
| `TestEveryPageActionIsItsEngineCall`, `TestAStaleGenerationIsRefusedAndChangesNothing` | §6.1's rows; a cast through SOAP between the page's read and each guarded write: 409 `stale`, the cast's queue whole; Pause and Stop served | the add's generation dropped |
| `TestAnAdmittedCommandPastItsBudgetIsUnknownAndNotReplayed` | a Play held in the stand-in's Start past the budget: 504, then the state shows it playing, and the script (under node where installed) sends nothing more | a cancellation after admission answered 200 |
| `TestAnInsertWhoseReplyIsLostLandsOnce`, `TestACommandTheEngineNeverAdmittedChangesNothing`, `TestAnInsertLateInItsBudgetIsRefusedBeforeTheEngine` | the request's context ended once the owner admits the add (`engine.WithAdmission`): the entry there once at the next read; 503 `busy`, nothing changed; the gate before the engine | the gate removed |
| `TestThePageTheAppAndAControllerAtOnce`, `TestPlaybackGoesOnWithThePageClosed` | a cast, another controller's Insert and the page's stale add: the cast's queue whole, the Insert where it was put, the add refused, then landed once read again; the next track heard with no page open | the add's generation dropped |

- **Also**: `TestTheAddTakesOneTrack` (a body naming two is 400), `TestEveryPageWordHasItsSentence` (under node).
- **Tier**: standard; on the owner's Pi, browse on each DAC (the Brooklyn among them), the verdicts against what
  plays; play, pause, seek, next, add; the app casting meanwhile; both closed mid-track, the next track heard.

## 10. Deferred

- **B634b**: the live DAC switch and the quiet PCM tone, with round 1's findings on them (`carry-b634b.md`).
- **B634c**: compound edits, removal and clearing, and the first insert after `DeleteAll` (`carry-b634c.md`).

## 11. The answers

| This brief's review, and round 1's findings this round owns | Answered in |
|---|---|
| 1. The passcode protects the page, not the player (Codex High 1) | §1 item 1, §4.5, R1 (§7) |
| 2. Admission is not completion (Codex High 2) | §6.2; PR 2's admission cases |
| 3. The passcode's store, named (Codex 3, Carbon 1) | §4.1: the store, the write path, the read path, why its stat gate is safe; R2 |
| 4. PCM eligibility from the endpoint (Codex 4, Carbon 2) | §5.2; PR 2's verdict cases |
| 5. Membership before the insert (Codex 5) | §5.3; PR 2's membership case |
| 6. B671 and B755 (Codex 6, Carbon 4) | §7 |
| 7. The lockout's order (Carbon 3) | §4.4; R4 |
| 8. The exact engine calls (Carbon 5) | §6.1 |
| 9. IPv6 (Carbon 6) | §3.4; R5 |
| Round 1: Codex 4 and Carbon 7, the passcode's write | §4.1: `commitAndConfirm` through its public entries |
| Round 1: Carbon 6, the stat cache | §4.1: the read path |
| Round 1: Carbon 8, the login's turn | §4.4: there is none |
| Round 1: Carbon 10, Host validation | §3.3 |
| Round 1: Claude 4, PCM eligibility | §5.2 |
| Round 1: Carbon 11, the residuals | §7 |
| Round 1: Claude 8, the admin-route test's overlap with `/` and `/static/` | PR 1, `TestTheLANListenerServesNoAdminRoute` |
| Round 1: Claude 10, authorised paths and a control per security case | §9 |
| Round 1: Carbon High 1, `__Host-` on port 7793 | §4.3: refuted |

**Nothing found needs a mechanism outside the budget**: each extension in §2 is a read, a move or a constant over
code that exists; the index's missing channel count for FLAC, WAV, AIFF and ALAC is answered "unknown" (§5.2).
**Questions for the orchestrator**: (1) how 1-bit shows an OpenHome entry it did not insert, a bare id with no
metadata (R6); (2) the owner's OK on port 7793, a 10-character floor, and sessions kept across a restart.
