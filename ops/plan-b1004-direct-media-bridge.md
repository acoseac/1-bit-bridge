# B1004: direct media delivery, the bridge's half: the contracts and the bridge PR's plan

Plan, 2026-11-24, round 2. **Docs only**: this round writes no code, serves no route and leaves
`PROTOCOL.md` as it is; the section the bridge PR adds to it is drafted in §3.1, marked "Draft, not
served". Every current behaviour cited is `path:line` at bridge main `726221d2`; a rules file of the
repository is cited by its name alone (`wire.md`), as "Where things live" lists them.

**Status.** For review, round 2. Binding: the B1004 design, round 3, with its amendments A1 to A7,
which win where its text differs ("the design", cited by section, §1 to §11); round 0's brief, D1 to
D5, with its review items Y1 to Y9; and round 1's brief, F1 to F4, with its review items G1 to G5,
which win where F1 to F4 differ. Round 1 folds every P1 and P2 of round 0's reviews (§11 says where
each went) and refutes what it does not fold, with the evidence (§12, "Refuted in round 1"). Round 2
folds the rulings on round 1's reviews, each named by what it changes (§13 says where each went),
and gives the claims the rulings refuted one line each (§14, "Refuted in round 2"). Where the design
is silent the text says **proposal**, and §9 lists each one with who decides it. No contract here
needed a decision of the design changed; where the design's words allow two readings, §9.4 names
the one taken.

No host, address or key path appears here. `media.example` stands for the media host the owner has
not chosen yet (the design's §11), `<tenant>` for a tenant's identifier, and every path the control
plane chooses is a setting the bridge is handed (§7.1).

**In one paragraph.** The design meets the bridge at three local seams, and this plan fixes each.
**The signer** is asked over HTTP/1.1 on a Unix socket the control plane makes for one tenant: it
knows the tenant from the listener the bridge connects to and checks the bridge's UID by
`SO_PEERCRED`; the bridge sends the path it resolved, the variant and its stat, never a tenant,
bucket, key or lifetime, and never holds the capability key (§2). **The tenant's state** reaches the
bridge as two JSON files in a directory the control plane owns: a durable migration marker, and a
policy it rewrites atomically (the holds, the delivery mode, the legacy cutoff and the representations
whose publication is pending) with a `validUntil` past which it is stale (§4). **The metrics** are a
JSON document on a Unix socket only root may read: bytes by eligibility class since the process
started, under a boot ID, beside the state the bridge applies (§6). **One classifier** decides every
request from the state the bridge holds at that moment: `POST /v1/media-url` answers `bridge` only
with the class the byte routes (`/v1/read`, `/v1/download`, the console's `/api/player/audio` and
`/api/player/download`) then serve and count, and asks the signer for the rest (§3, §5). **It fails
closed**: a migrated tenant's originals are served from the bridge only by a policy the bridge took,
so a policy missing, unreadable or out of the bridge's trust, and a direct-media configuration that is
invalid, refuse what only a current policy admits, every migrated original where no policy was taken;
local and copying tenants, renditions and upstream tracks never depend on a policy, and the counters
run throughout (§4.6, §4.9). Everything is off by default and set through the environment, a value
that does not parse failing closed, so a binary rolled back below the bridge PR still loads its
settings (§7).

## 0. At a glance

| Today | Where | The bridge PR (B1004 PR 3) |
|---|---|---|
| The byte routes serve any file the resolver finds, with no policy check and no count but the access log's | `internal/api/files.go:264-365`; `internal/admin/player_audio.go:305-440`; `internal/api/middleware.go:210-304` | One classifier decides each request from the state the bridge holds, routing first: it refuses what it refuses, fails closed where a migrated tenant has no policy, and counts body bytes by class (§5, §6) |
| No route resolves a track's delivery | `internal/api/route_classification.go:189-364` | `POST /v1/media-url` and a method-less twin that answers 405, both `rateNone`; every `bridge` answer is the classifier's (§3, §5.2) |
| `/v1/health.features` has no direct-media key | `internal/api/api.go:2060-2131` | `directMediaV1`, between `diagnosticsSummary` and `dlnaArtwork`, while the resolver is on (§3.1) |
| No error carries `X-Onebit-Error-Code` | `internal/api/api.go:3082-3084` | Every application error of `/v1/media-url` carries it in every setting, and those of the byte routes while direct media is on (§3.3) |
| File validation answers `bad_request` and `not_found`, a rendition `variant_*` | `files.go:302-319`, `:405-482`, `:736-755` | Unchanged there; `/v1/media-url` makes the same checks and maps them to `invalid_path` and the design's codes (§3.2) |
| Each call of the resolver takes the roots afresh, and the console changes them live | `internal/fs/fs.go:258-262`, `:362-368`; `internal/admin/handlers_api.go:2124-2125` | One resolution a request: the open, the class and the signer's path all take its absolute path (§5.8) |
| A `BRIDGE_*` boolean or whole number that does not parse is logged and ignored | `internal/config/env.go:392-397`, `:429-434` | A direct-media variable that does not parse, or is white space alone, makes the configuration invalid and fails to its leaf's restrictive side (§4.9, §7.1) |
| The console's byte routes validate and serve on their own, and refuse the DSD compact tier's id | `player_audio.go:75`, `:305-440`, `:517-537` | The same classifier and counters at the same points, and the id taken (§5.7) |
| The bridge listens on no Unix socket; the console's `/metrics` serves the whole registry on the admin listener | `internal/admin/admin.go:2199` | A metrics socket, JSON, for root (§6) |
| No setting | `internal/config/config.go:49-110` | A `directMedia` block, off by default, set through `BRIDGE_DIRECT_MEDIA_*` (§7.1) |

## 1. The seams and who owns them

```text
app, console --POST /v1/media-url-------------> bridge --POST /v1/mint--> signer   (the tenant's own Unix socket)
app, console --GET the capability URL---------> media host (Worker) --> R2   (never through the bridge)
app, console --/v1/read, /v1/download,
               /api/player/audio, /download---> bridge   (bridge delivery: classified, guarded, counted)
control plane --writes--> state directory: migration marker, policy --read, watched--> bridge
control plane <--GET /v1/metrics, each minute-- bridge's metrics socket (root alone)
```

| Seam | The bridge (PR 3) | The control plane (PR 1) |
|---|---|---|
| The signer socket (§2) | the client: dials, sends, checks the answer, maps its errors | a listener per tenant, the peer check, every decision, the capability and its key, R2, the mint limit |
| The state directory (§4) | reads, validates and watches the marker and the policy; decides with them | writes both: every member, the heartbeat, the versions, the rollback's order, the directory's ownership |
| The metrics socket (§6) | counts and serves | samples each minute, marks restart gaps, evaluates every threshold, applies holds |

The bridge evaluates no threshold and applies no hold of its own: it counts and obeys.

## 2. D1: the signer socket

### 2.1 Transport: HTTP/1.1 on a Unix stream socket

Chosen over a framed protocol (length-prefixed JSON) because:

1. Both sides take framing, status codes, headers (`Retry-After`), body bounds and timeouts from
   their own HTTP stacks; a framed protocol hand-rolls each, and its codec needs a fuzz target on
   both sides.
2. The bridge speaks HTTP everywhere already, and its tests stand a fake signer up with `net/http`
   on a Unix listener, the fixture shape `internal/fsutil/fsutiltest/fsutiltest.go:40` binds.
3. Root on the host can ask the socket by hand with any HTTP client that dials a Unix socket.
4. The path carries the version (§2.9).

At 120 mints a minute a tenant (the design's §6), HTTP's overhead is nothing. Its cost, a header
parser on the signer, is bounded there as a framed reader's would be.

**Proposal**: one connection a request (the bridge's transport disables keep-alive). A Unix connect
costs microseconds, the signer checks the peer of every request (§2.2), and a pooled connection the
signer closed while idle can never fail a mint, which net/http does not retry for a POST.

### 2.2 Who is calling: the listener and the peer's UID (Y1)

As the design's §2 says, binding on the control plane's PR:

- The signer provisions a directory of its own for each tenant, holding one listener for that
  tenant, and knows the tenant **from the listener a connection arrives on**, never from anything
  the connection sends.
- At accept, before it reads a byte, it reads the peer's credentials (`SO_PEERCRED`) and compares the
  UID with **the registered numeric bridge UID** of that listener's tenant. A mismatch closes the
  connection unread and unanswered, and counts in the fleet's diagnostics, never against the
  listener's tenant (as the design's §6 keeps an invalid token out of a tenant's figures).
- Both checks hold together: a bridge's UID on another tenant's listener is refused, and so is any
  other UID on the right listener.
- The registry gives a numeric UID to one tenant at most: registering a UID another tenant holds is
  refused.
- A tenant's socket is removed before its UID can be given to another tenant.
- The request carries no tenant, bucket, key or lifetime, and one that names any is refused (§2.4).

The bridge learns the path from its settings (`directMedia.signerSocket`, §7.1) and from nothing else.

### 2.3 The bridge's check of the signer (proposal)

The bridge reads the peer's credentials on each connection it makes (on a connected Unix socket
`SO_PEERCRED` gives the listener's) and takes an answer only from the UID the control plane names
(`directMedia.signerUID`, §7.1); any other is §2.7's refusal of the socket, logged once per UID. A
signer directory the bridge's UID could write into would otherwise let any process of that UID stand
a fake signer at the path and hand the app a URL of its choosing. The app's own check of approved
hosts (the design's §4) and §2.6's check of the URL are the other two layers.

### 2.4 The request

The design names what the bridge sends and never sends; the path and the members' names are a
**proposal**.

```http
POST /v1/mint HTTP/1.1
Host: signer
Content-Type: application/json
X-Request-ID: <the bridge request's id>

{"path":"Artist/Album/01 Track.flac","variant":null,"stat":{"size":123456789,"mtimeNs":1732445400123456789}}
```

`Host` is a placeholder an HTTP/1.1 request must carry; over the socket it names nothing, and the
signer ignores it.

- `path` (string, required): the representation's path below `directMedia.libraryRoot` (§7.1), the R2
  mount: the absolute path of the request's one resolution (§5.8), made relative to that root, with
  forward slashes. In a one-root library whose root is the mount, it is the client's path as the
  resolver cleaned it (`fs.go:214-334`). It is the exact bytes the resolver stat'ed and the open
  opens: a backslash and a percent sign are ordinary bytes in it (`fs.go:304-321`), nothing decodes
  it, and a name that is not UTF-8 cannot be sent, as it cannot appear in the manifest. At most 4096
  bytes. The signer builds `tenants/<tenant>/music/` and the key's encoding itself (the design's §2).
- `variant` (string or null, required): `null` for the original. The bridge PR never sends a
  rendition (§5.2, step 2); the member is defined now so the registered, published variants the
  design's §2 allows need no new version.
- `stat` (object, required): `size` (bytes) and `mtimeNs` (Unix nanoseconds) of the bridge's own
  `os.Stat` of the file it would serve (`ResolveChecked`, `fs.go:386-399`), at the precision the stat
  gave; the signer compares them with R2's size and the mount's mtime metadata at the precision it
  holds (the design's §2).
- Nothing else: a request with any other member, a tenant, bucket, key, lifetime or expiry among
  them, is refused 400 `bad_request` (the design's §3).
- `X-Request-ID` (**proposal**): the bridge's request id (`internal/api/middleware.go:212-213`), so the
  two logs can be joined.

**The signer's path rule (G5)**, binding on the control plane's PR: the signer takes every path the
bridge's resolver takes, as bytes, and refuses (400 `invalid_path`, nothing minted, no R2 request)
only these:

- traversal and ambiguity, which the bridge never sends, since it sends the resolver's cleaned path: a
  NUL, a leading `/`, an empty, `.` or `..` element;
- the design's exclusions (its §2), and no others: a symlink escape; a hidden component, one that
  begins with `.`, which covers the trash, `.bridge-trash` (`internal/trash/trash.go:78`), and the
  upload's staging, `.bridge-upload` (`internal/upload/paths.go:37`), out of which an upload's commit
  renames its file (`internal/upload/upload.go:893`), and which the scanner never indexes
  (`internal/manifest/scanner.go:4070-4080`, `:4101`); and commit temporaries and databases, whose
  name patterns the control plane's PR states, each tested against a name the resolver takes.

A backslash, a percent sign, a `+`, white space and every other byte are a file's own name: the
signer decodes the JSON string once and nothing after, builds the key from those bytes and the
mount's own encoding of a name, and percent-encodes the key once for its R2 request, so a name like
`50%25 off\live.flac` reaches the key it names and no other. That is the design's "ambiguous
encoding" refused by construction: one decoding, one encoding.

### 2.5 The answers

`200 OK`, `Content-Type: application/json`, one of:

```json
{"delivery":"direct","url":"https://media.example/media?t=<capability>","expiresAt":"2026-11-24T12:10:00Z","size":123456789,"objectVersion":"<opaque>","etag":"\"<opaque>\""}
```

```json
{"delivery":"bridge","reason":"publication_pending"}
```

```json
{"delivery":"bridge","reason":"operator_rollback","policyVersion":43}
```

`reason` is one of the design's five, a closed set: `not_migrated`, `publication_pending`,
`local_variant`, `non_r2_source`, `operator_rollback` (the member's name is a **proposal**: the
design names the values only). A missing or stale R2 object is `bridge` with `publication_pending`,
or a 503 (§2.7), never a capability for other bytes (the design's §2). An `operator_rollback` answer
carries `policyVersion` (**proposal**: an integer, 1 to 2^53 − 1), the `version` the control plane
gave the policy that installs the rollback, its first with `deliveryMode` `bridge`, named whether or
not that policy is written yet; only a policy at or above it supersedes the answer (§4.8, §5.4).
Unknown members of an answer are ignored (§2.9). A `bridge` answer is never relayed as it stands: it
is a fact the bridge records and gives to the classifier, whose answer the client gets (§2.7, §5.4).

### 2.6 What the bridge checks in a direct answer

Before it relays a capability the bridge requires each of these, and otherwise answers 503
`media_unavailable` (§2.7), logging the check that failed by its name and never the URL:

- `url`: `https`, a host equal to `directMedia.capabilityHost` (ASCII, compared lower-case), no port
  or 443, no user information and no fragment, at most 8 KiB (**proposal**: the design's JWE runs to
  about a kilobyte).
- `expiresAt`: RFC 3339, after the bridge's now and at most 630 s ahead (the design's §3: issuance
  plus 600 s, 30 s of skew).
- `size`: equal to the `stat.size` the bridge sent.
- `objectVersion`: 1 to 256 bytes of visible ASCII; `etag`: a strong entity tag (RFC 9110, §8.8.3)
  of at most 258 bytes.

Then **the stat again** (**proposal**, the bridge's half of the design's "recheck stat around
HEAD"): the bridge stats the file once more after any answer. A size or mtime that moved discards the
answer, and the bridge records that the file at its new stat is not published (a publication fact,
§5.4), then answers what the classifier says of it, as for the signer's own `publication_pending`
(§2.7). The bytes on the bridge are no longer the ones the signer judged, and a byte request for
them is counted as the answer said (F2).

### 2.7 Errors, and what the bridge answers for each

The signer's errors take the bridge's envelope, `{"error":"<code>","message":"<text>"}`, with
`X-Onebit-Error-Code`. The bridge maps each by a closed table of constants, writes its own fixed
message (a 5xx body carries no error's text: `auth-tls.md`, B425) and logs the
signer's status and code alone.

| The signer answers | `POST /v1/media-url` answers |
|---|---|
| 200 `direct`, every check of §2.6 passed and the stat unchanged | the capability; a publication fact of that stat is cleared (§5.4) |
| 200 `bridge` `publication_pending` | the classifier's answer once the fact is recorded (§5.4): `bridge` `publication_pending`; a hold's 403, or 503 `policy_unavailable` where no policy was taken (§5.3) |
| 200 `bridge` `operator_rollback` with its `policyVersion` | the classifier's answer once the rollback fact is recorded (§5.4): `bridge` `operator_rollback`; a hold's 403, or 503 `policy_unavailable` where no policy was taken (§5.3); where the policy taken is already at or above that version and ends the rollback, 503 `media_unavailable`, logged, the client asking again |
| 200 `bridge` with any other reason, which contradicts the marker or the request (the bridge asks only of a migrated original) | 503 `media_unavailable`, logged: the bridge sends a client to its byte routes only with a class they give (F2) |
| 400 `invalid_path`: its own refusal (§2.4's rule) | 400 `invalid_path` |
| 400 `invalid_variant` | 400 `invalid_variant` |
| 400 `bad_request`: the bridge sent what v1 does not take | 500 `internal`, logged at Error |
| 403 `fair_use_refused`: the tenant is held, minting stopped | 403 `fair_use_refused` |
| 429 `rate_limited` with `Retry-After`: the mint limit (the design's §6) | 429 `rate_limited`, its `Retry-After` held to 1 to 3600 s |
| 503 `media_unavailable`, an optional `Retry-After`: R2, or the signer's state, cannot decide | 503 `media_unavailable`, the same `Retry-After` |
| 503 `policy_unavailable`: the signer has no current policy for the tenant | 503 `policy_unavailable` |
| anything else: another status or code, a body that does not decode, a direct answer §2.6 refuses, a `reason` outside the five, an `operator_rollback` without a `policyVersion` | 503 `media_unavailable` |
| no answer: the socket missing or refused, a connection closed unread (§2.2's refusal looks so), §2.3's refusal, §2.8's deadline, a resolver configuration that cannot run (§4.9) | 503 `media_unavailable` |
| the client went away first | 499 at Info (`internal/api/api.go:3115-3121`, B364) |

The signer never answers `direct_disabled`: during a rollback it answers `bridge` with
`operator_rollback`, since a rollback stops minting and installs bridge routing (the design's §9);
`direct_disabled` is the media host's word. The bridge retries nothing; the app asks again (the
design's §4).

### 2.8 Timeouts and bounds (proposal)

Dial 1 s; the whole exchange, from the dial to the answer's last byte, 5 s, inside the route's own
10 s (§3.4); an answer at most 16 KiB. They are starting values, which Phase 0's mint latency (the
design's §9) qualifies; a signer slower than the bound costs the client a 503, never a request that
hangs.

### 2.9 Versioning

The path carries the major version: `POST /v1/mint`, and `GET /v1/health` (**proposal**: `200` with
`{"status":"ok"}`, which the bridge's doctor asks, §7.2). Both sides ignore an answer's unknown
members; the signer refuses a request's (§2.4), so a v1 bridge sends v1's members alone. A breaking
change is `/v2/`, served beside `/v1/` until no bridge of the fleet sends v1; a signer that does not
serve `/v1/mint` answers 404, which the bridge answers 503 and logs as a signer that does not speak
v1.

### 2.10 Tests

The control plane's PR, the signer's side (Y1, G5): a connection on a tenant's listener from a UID
other than that tenant's bridge is closed unread, answers nothing and mints nothing, and counts in
the fleet's diagnostics alone; the same for another tenant's bridge UID on that listener; registering
one UID for two tenants is refused; a request that names `tenant`, `bucket`, `key` or `lifetime` is
400; a removed tenant's socket is gone before its UID is given again; each refusal of §2.4's path rule
is 400 `invalid_path` with no R2 request, `../<other tenant>/music/x` among them; and the names the
rule takes reach exactly the key they name, a literal `%2e`, `%2F`, `%25`, a backslash, a `+` and
`...` among them, decoded once and encoded once.

The bridge's PR: a fake signer on a Unix socket in the test's temp dir, bound by its basename from
that directory so a deep checkout stays inside `sun_path` (`build-ci-tests.md`, B544),
answering each row of §2.7 in turn; the request's JSON byte for byte, for a file named with a literal
`%2e` and a backslash among others; a fake signer that closes each connection unread (§2.2's refusal),
and one whose UID is not `signerUID` (the expected UID set to another number), each answered 503; each
check of §2.6 broken alone; a stat moved by a seam between the two stats, answered `bridge` with
`publication_pending` and its fetch counted so (§5.4); an `operator_rollback` answer without its
`policyVersion` answered 503, and one with it recorded at that version.

### 2.11 The capability key stays on the signer (F3)

The design's §2 and §3 decide the custody; this is what the bridge's half holds to.

- **Where it lives.** The capability key is symmetric (compact JWE, `alg=dir`, `enc=A256GCM`, one key
  a `kid`: the design's §3), so it exists in two places alone: the signer's service, which loads it
  through `LoadCredential=` (the design's §2) into a credentials directory of that service alone, and
  the media host, which decrypts with it. It is never in the bridge's process, settings, environment,
  data directory or state directory, nor behind the control plane's public API (the design's §2: no
  capability key or tenant-impersonation interface there).
- **Who can read it.** The signer's own unprivileged user and root on the host, and the media host's
  runtime. Not a bridge: each tenant's bridge runs as a UID of its own, and the control plane's
  `bridge@` unit gains `InaccessiblePaths=` for the R2 credential's directory, the signer's
  configuration and its runtime credentials (the design's §2, which notes the current unit lacks them),
  so the paths do not exist in a bridge's view of the filesystem. The bucket-read-only R2 credential
  the signer's checks use is held the same way, and its requests leave from the approved host alone.
- **How it rotates.** By `kid`, decryption first: a new key reaches the media host before the signer
  issues under it; the old key stays at the media host for the longest capability still out (600 s
  and 30 s of skew) and is then removed; an emergency removal deletes it at the media host at once,
  which invalidates every capability under it, and the app mints again once on `unknown_key_id` (the
  design's §3 and §4).
- **How a compromised bridge is kept from it.** It cannot read the key or the R2 credential (above),
  and the only path to a capability is the signer's socket for its own tenant: the listener and the
  UID checked together (§2.2), the mint limit, §2.4's path rule, and objects inside its tenant's
  prefix alone. So it can mint only its own tenant's capabilities, one object each, for ten minutes,
  at the limit's pace; it cannot forge, widen or lengthen one, or reach another tenant's prefix.
  What stays within its reach is its own tenant's: the bytes of its own mount, and the figures of its
  own metrics, which the media host's own counts of direct delivery do not depend on.
- **The bridge PR's part.** No setting, variable or file of the bridge names a key or a credential
  (the `directMedia` block holds sockets, a directory, a root, a UID and a host: §7.1), and a test
  holds the block to that; the bridge never logs a capability URL (§2.6).

## 3. D2: `POST /v1/media-url` and `directMediaV1`

### 3.1 The draft `PROTOCOL.md` section

The fenced text is the section the bridge PR adds to `PROTOCOL.md` after `GET /v1/download`
(`PROTOCOL.md:241-254`), and the list after it the sentences and rows it adds elsewhere; the app's
companion PR copies the file, never rewrites it (`CONTRIBUTING.md:9-21`). Y6 keeps both here this
round. Its heading takes the form the documented-endpoint guard reads
(`internal/api/routed_endpoints_test.go:77-80`). The bridge PR drops the "Draft, not served" line,
since its route is registered there, answering `404` `direct_media_not_supported` until a bridge
turns it on.

````markdown
### `POST /v1/media-url` (additive; `directMediaV1`)

**Draft, not served.** No bridge serves this route yet.

Says where a client fetches one representation of a track: from the media host, through a
capability URL of at most ten minutes (`direct`), or from this bridge (`bridge`). The `directMediaV1`
key in `/v1/health.features` advertises it; a bridge without the key answers this route `404`
`direct_media_not_supported`, and a client without it uses `/v1/read` and `/v1/download` as before.
Bearer-authenticated like every file route. `ProtocolVersion` stays 1.

**Request:**

```http
POST /v1/media-url
Content-Type: application/json

{"path":"Artist/Album/Track.flac","variant":null}
```

The body is one JSON object of at most 4 KiB with these members and no other:

| Member | Type | Meaning |
|---|---|---|
| `path` | string, required | The track's path, as `/v1/download` takes it. For an SACD image's track, the image's own path: a virtual track path answers `404` `not_found`, as on `/v1/read` |
| `variant` | string or null, optional | A rendition's id from the track's `variants` in `/v1/manifest`; absent or null for the original |

A client never chooses a tenant, bucket, key or lifetime: a body with any other member is `400`
`bad_request`.

**Response** (`200 OK`, `Cache-Control: no-store`), one of two shapes:

```json
{
  "delivery": "direct",
  "url": "https://media.example/media?t=<opaque-capability>",
  "expiresAt": "2026-11-24T12:10:00Z",
  "size": 123456789,
  "objectVersion": "<opaque-validator>",
  "etag": "\"<opaque-validator>\""
}
```

The client fetches `url` from a session of its own: no bearer token, cookie or client credential,
ordinary certificate and host validation, redirects refused, the bridge's pinning untouched. The URL
serves GET and HEAD and one range a request, as many ranges as the client needs, until `expiresAt`,
at most ten minutes after the answer; a client asks again before new work near it. `size`,
`objectVersion` and `etag` describe exactly the bytes the URL serves, and a resume sends `etag` as
`If-Range`.

```json
{ "delivery": "bridge", "reason": "local_variant" }
```

| `reason` | When | The client fetches from this bridge |
|---|---|---|
| `not_migrated` | This library's music is not delivered directly yet | the original |
| `publication_pending` | The file arrived or changed on this bridge, and its copy for direct delivery is not ready | the original |
| `local_variant` | A rendition: renditions are this bridge's own files | the rendition, with `variant` |
| `non_r2_source` | The track lives on an upstream UPnP server, or outside the library direct delivery covers | the track |
| `operator_rollback` | The operator has paused direct delivery | the original |

A client fetches from the bridge exactly as without `directMediaV1`, on the endpoint of its
operation: a range window on `/v1/read`, a stream or a download on `/v1/download`, a rendition with
`&variant=<id>` on either. A client takes a `reason` it does not know for a `bridge` answer. The
bridge judges the fetch again from what it knows when the fetch comes: a hold since refuses it, and a
file replaced since is judged as the file it is now (the refusals under "Fetching from the bridge"
below); a client that is refused asks this route again.

**Errors.** Each carries `X-Onebit-Error-Code` with its code, whether or not the bridge advertises
`directMediaV1`, and the JSON body repeats it; a HEAD's error carries the header alone.

| Status | `error` | When |
|---|---|---|
| 400 | `bad_request` | The body is not one JSON object of at most 4 KiB holding a string `path`, an optional `variant` and nothing else |
| 400 | `invalid_path` | `path` is malformed, names no library root, or names a directory or something that is not a file, or the media signer does not take it as this library's music |
| 400 | `invalid_variant` | `variant` is neither null nor a rendition id (1 to 128 bytes of `[a-z0-9-]`), a value of another type included |
| 401 | `unauthorized` | Missing or invalid bearer token |
| 403 | `fair_use_refused` | Delivery of this library's music is paused for fair use; the operator has been told |
| 404 | `not_found` | Nothing at `path`, an SACD image's virtual track path included |
| 404 | `variant_not_found` | The track has no rendition `variant` |
| 404 | `direct_media_not_supported` | This bridge does not advertise `directMediaV1` |
| 405 | `method_not_allowed` | Any method but POST; carries `Allow: POST` |
| 410 | `variant_stale` | The rendition is older than its source |
| 410 | `variant_missing_on_disk` | The rendition's file is gone |
| 429 | `rate_limited` | Too many capability requests for this library; carries `Retry-After` |
| 500 | `internal` | Server-side failure |
| 503 | `media_unavailable` | Direct delivery cannot be resolved now; ask again later |
| 503 | `policy_unavailable` | The delivery policy that decides this track cannot be read now, the bridge's or the media signer's; ask again later |

A `503` never means "fetch from the bridge instead": a client fetches from the bridge only on a
`bridge` answer. This route draws from no rate-limit bucket of the bridge's; it is a read, and the
media signer's own limit answers its `429`.

#### Fetching from the bridge under direct delivery (additive; `directMediaV1`)

Where the bridge's operator has turned direct delivery on, whether or not the bridge advertises
`directMediaV1` (it advertises the key while it resolves), `/v1/read` and `/v1/download` add three
answers, for a track's original alone, never for a rendition or a track proxied from an upstream
server:

| Status | `error` | When |
|---|---|---|
| 403 | `fair_use_refused` | Delivery of this library's music is paused for fair use |
| 403 | `direct_media_required` | This library's music is delivered directly now, and this app predates direct delivery: update the app |
| 503 | `policy_unavailable` | The bridge cannot read a current delivery policy; ask again later |

There, every error `/v1/read` and `/v1/download` write carries `X-Onebit-Error-Code`, but for
net/http's own answers to a path or method no route serves, `http.ServeContent`'s own `416` and
`412`, and an upstream server's answer relayed (see Error responses). `/v1/read` takes
`&variant=<id>` exactly as `/v1/download` does, with the same `404` and `410` answers.

#### The media host's answers

The media host, not the bridge, serves a capability URL. Its errors carry `X-Onebit-Error-Code`; an
edge block of the host's network may carry none, and never means the bridge will serve the bytes.
Its codes: `400` `invalid_range`, `unsupported_range`; `401` `invalid_token`, `token_expired`,
`unknown_key_id`; `403` `forbidden`, `fair_use_refused`, `direct_disabled`; `404` `not_found`;
`405` `method_not_allowed` with `Allow`; `412` `object_changed`, `precondition_failed`; `416`
`range_not_satisfiable` with `Content-Range: bytes */<size>`; `429` `rate_limited` with
`Retry-After`; `503` `media_unavailable`, `policy_unavailable`. A client asks `POST /v1/media-url`
again once on `token_expired` or `unknown_key_id`, and once on `direct_disabled`, then follows that
answer; a second refusal stops. A failure of authentication, TLS or transport never sends a client to
the bridge by itself.
````

The same PR changes `PROTOCOL.md` in four other places, each drafted here:

- `/v1/health`'s `features` paragraph (`PROTOCOL.md:189`): "`directMediaV1` (additive) advertises
  `POST /v1/media-url`. The three answers direct delivery adds to `/v1/read` and `/v1/download` come
  from a bridge whose operator turned direct delivery on, whether or not it advertises the key; a
  bridge with direct delivery off serves as it always has."
- `/v1/read`'s section (`PROTOCOL.md:233-239`): "`variant` (additive, optional) selects a rendition
  exactly as on `/v1/download`, with the same answers."
- "Write rate limit" (`PROTOCOL.md:2566-2578`): "`POST /v1/media-url` is a read, and draws from no
  bucket of the bridge's."
- "Error responses" (`PROTOCOL.md:2478-2550`): the new pairs below, each in its status's place, and
  the existing rows named after the table.

| Status | `error` code | When |
|-------:|--------------|------|
| 400 | `invalid_path` | `POST /v1/media-url`: `path` is malformed, names no library root, names a directory or something that is not a file, or is not this library's music |
| 400 | `invalid_variant` | `POST /v1/media-url`: `variant` is neither null nor a rendition id |
| 403 | `fair_use_refused` | `POST /v1/media-url`, or `/v1/read` or `/v1/download` of a track's original, while delivery of this library's music is paused for fair use (`directMediaV1`) |
| 403 | `direct_media_required` | `/v1/read` or `/v1/download` of a track's original after this library moved to direct delivery, from an app that predates it (`directMediaV1`) |
| 404 | `direct_media_not_supported` | `POST /v1/media-url` on a bridge that does not advertise `directMediaV1` |
| 405 | `method_not_allowed` | `/v1/media-url` with any method but POST; carries `Allow: POST` |
| 503 | `media_unavailable` | `POST /v1/media-url`: direct delivery cannot be resolved now |
| 503 | `policy_unavailable` | `POST /v1/media-url`, or `/v1/read` or `/v1/download` of a track's original, while the bridge or the media signer holds no current policy that decides it (`directMediaV1`) |

In the existing rows, `400` `bad_request` adds `POST /v1/media-url`'s body; `404` `not_found`,
`404` `variant_not_found`, `410` `variant_stale` and `410` `variant_missing_on_disk` add
`POST /v1/media-url`; `429` `rate_limited` adds the media signer's per-library limit.

The draft follows the design's §3 for the request, both answers, the five reasons and the codes. Four
things in it are **proposals** the design does not state (§9.1, item 4): `bad_request` for a body,
`direct_media_not_supported` while the route is off, the member name `reason`, and
`invalid_variant`'s alphabet.

### 3.2 The validation it reuses, and how today's results map (D2, Y4, Y5)

The handler makes `serveFile`'s checks in `serveFile`'s order (`files.go:264-340`), by the same code,
before anything of direct media:

| Step | Today's code | `/v1/download` answers | `POST /v1/media-url` answers |
|---|---|---|---|
| 1. Routing, without a variant, before any stat or signer call (Y4) | `GetUPnPRouting`, `files.go:281-300` | the proxied track | 200 `bridge` `non_r2_source`; a lookup error 500 `internal`, a client gone 499 |
| 2. The source path, resolved once (G3): NUL, `..`, containment, the owning root, the stat | `ResolveChecked`, `fs.go:214-334`, `:386-399`; `writeResolveError`, `files.go:736-755` | 400 `bad_request` (a bad path, an unknown root), 404 `not_found`, 500 `internal` | 400 `invalid_path`, 404 `not_found`, 500 `internal` |
| 3. A directory | `files.go:306-309` | 400 `bad_request` | 400 `invalid_path` |
| 4. Not a regular file: a named pipe, a socket, a device | `fsutil.NotAFile`, `files.go:316-319`, `:373-376` | 400 `bad_request` | 400 `invalid_path` |
| 5. A rendition: the store, its row, its freshness, its sidecar | `serveVariant`, `files.go:405-482`, through the store adapter that adopts a relocated sidecar (`cmd/bridge/main.go:116-167`) | 404 `variant_not_found`, 410 `variant_stale`, 410 `variant_missing_on_disk`, 500 `internal` | the same answers; a stale rendition is asked for again as on a download (`files.go:458-464`); a missing sidecar is not reaped, which the download's open does (`:491-595`) |
| 6. The file is not under the R2 mount: step 2's absolute path lies outside `directMedia.libraryRoot` | (no such step) | (no such step) | 200 `bridge` `non_r2_source` |
| 7. The classifier | §5 | (no such step) | 200 `bridge` with its reason, a 403 or 503, or the signer (§2) |

`invalid_variant` is decided before step 1: a `variant` member present, not null, and not a string
of 1 to 128 bytes of `[a-z0-9-]` (**proposal**: the ids' alphabet, so a new family needs no bridge
change). The console's `variantIDPattern` (`player_audio.go:75`) refuses the DSD compact tier's ids,
and the bridge PR moves it to the same alphabet (§5.7). Step 5's lookup and freshness become one
function `serveVariant` and the route both call (§7.2), so the two cannot drift.

Existing endpoints keep every code (Y5): `/v1/read` and `/v1/download` answer steps 1 to 5 as
today and add only §3.1's three refusals.

### 3.3 The error header's contract (Y5, F4)

- **Which endpoints adopt the vocabulary.** `POST /v1/media-url` in full (§3.1). `/v1/read` and
  `/v1/download` for their three new refusals, and the header on the errors they already send while
  direct media is on. No other `/v1` route.
- **How the header is set.** One middleware outside the request log marks the writer of every request
  whose path is `/v1/media-url`, in every setting, or `/v1/read` or `/v1/download` while
  `directMedia.enabled` is on, a value of it that does not parse counting as on (§7.1); the chain
  becomes `withAltSvc(protocolHeader(errorCodes(requestLogging(recoverer(mux)))))` (`api.go:1523`,
  **proposal**). `writeError` (`api.go:3082-3084`), which `writeErrorLog` and `writeFileErrorLog` end
  in (`:3115-3136`), sets `X-Onebit-Error-Code` to its code where the writer it is handed carries the
  mark, found through `Unwrap` as `http.ResponseController` finds a deadline (the request log's writer
  unwraps: `middleware.go:168-170`). So `authed`'s 401 (`api.go:2961-2994`), the twin's 405, every
  refusal of the handlers and the recoverer's 500 (`middleware.go:317-341`, which writes through
  `writeError` with the request log's writer) carry it. The middleware sits outside the request log
  and forwards the request it was handed, so the log's ordering invariant (`middleware.go:201-209`)
  is untouched, and a bridge with direct media off answers `/v1/read` and `/v1/download` exactly as
  today, headers included.
- **The console's two byte routes** take the same mark from a wrapper on their two registrations
  (`internal/admin/admin.go:2143-2144`) while direct media is on, which the console's `writeError`
  (`internal/admin/handlers_api.go:4279-4281`) reads alike; the console's boundary, which answers
  before any route, sets none. The error-table guard reads `internal/api` alone
  (`internal/api/error_table_test.go:178-195`, `scanErrorPairs(".")`), so the console's codes stay
  out of `PROTOCOL.md`, as every console code is.
- **The alternative**, the header on every `/v1` envelope error, is additive but changes every
  route's headers, and is the owner's to take instead (§9.1).

**Every application error of the media routes** (F4). "Yes": sent with the header; "no": sent
without it; "never": not sent in that setting. Off: `directMedia.enabled` off. On, not advertised:
on (a value that does not parse included), `resolverEnabled` off or not parsing, the resolver
`invalid` (§4.9). Advertised: on, the resolver on, whether or not its signer settings or
`libraryRoot` can run (§4.9).

| Route | Status and `error` | Off | On, not advertised | Advertised |
|---|---|---|---|---|
| `/v1/media-url` | 401 `unauthorized` | yes | yes | yes |
| `/v1/media-url` | 404 `direct_media_not_supported` | yes | yes | never |
| `/v1/media-url` (the twin) | 405 `method_not_allowed`, with `Allow: POST` | yes | yes | yes |
| `/v1/media-url` | 500 `internal` (a panic, through the recoverer) | yes | yes | yes |
| `/v1/media-url` | 400 `bad_request`, `invalid_path`, `invalid_variant`; 403 `fair_use_refused`; 404 `not_found`, `variant_not_found`; 410 `variant_stale`, `variant_missing_on_disk`; 429 `rate_limited`; 500 `internal` (a fault); 503 `media_unavailable`, `policy_unavailable` | never | never | yes |
| `/v1/read`, `/v1/download` | 401 `unauthorized`; 400 `range_required` (`/v1/read`); 400 `bad_request` (a path, a root, a directory, not a file); 404 `not_found`, `variant_not_found`; 410 `variant_stale`, `variant_missing_on_disk`; 500 `internal` (a fault, a panic) | no | yes | yes |
| `/v1/read`, `/v1/download` (a routed track) | 500 `internal` (the proxy unwired); 502 `upnp_bad_res_url`, `upnp_upstream_unreachable`; 503 `upnp_server_offline` (`internal/api/upnp_proxy.go:95-113`) | no | yes | yes |
| `/v1/read`, `/v1/download` (an original) | 403 `fair_use_refused`, `direct_media_required`; 503 `policy_unavailable` | never | yes | yes |
| `/api/player/audio`, `/api/player/download` | 400 `bad_path`, `bad_request`; 404 `not_found`; 405 `method_not_allowed` (the handler's, which the GET pattern leaves unreachable); 410 `variant_stale`, `variant_missing_on_disk`; 500 `internal`; 503 `upstream_unavailable`; a routed track's 502 and 503 (`cmd/bridge/player_wiring.go:44-46`) | no | yes | yes |
| `/api/player/audio`, `/api/player/download` (an original) | 403 `fair_use_refused`; 503 `policy_unavailable` | never | yes | yes |
| the console's two routes | the console's boundary refusals, which every console route shares (its loopback, Host, session and Origin checks) | no | no | no |

Not application errors, and never carrying the header: net/http's 404 or 405 for a path or method no
route serves (a POST to `/v1/read`); `http.ServeContent`'s own 416 (plain text, with `Content-Range:
bytes */<size>`) and 412; an upstream server's answer relayed once it has answered; and a client gone
before its answer, recorded 499, which reaches no one.

- **The error table.** PROTOCOL.md's table (`PROTOCOL.md:2478-2550`) gains §3.1's pairs, which
  `TestEverySentErrorCodeIsInTheProtocolTable` and `TestEveryProtocolErrorRowIsSent`
  (`internal/api/error_table_test.go:33`, `:56`) hold to what the handlers send. Every status and
  code is a constant at its call, which the guard reads (`error_table_test.go:222-226`); the signer's
  codes reach a client only through §2.7's table of constants, never relayed as read.
- **Tests through the registered middleware.** Every row of the table above, in each of its three
  settings, through `Server.Handler()` (`api.go:1490-1524`), the chain production runs, and through
  the console's mux for its routes: the header equal to the body's `error`, a HEAD's header without a
  body, the recoverer's 500 through a handler made to panic, and a route outside these carrying none.

### 3.4 A read-class route (Y7)

- **Registry.** `{pattern: "POST /v1/media-url", kind: boundedRoute, rateClass: rateNone, handler:
  withCtxTimeout(10*time.Second, s.authed(s.mediaURL))}` and the twin, `{pattern: "/v1/media-url",
  kind: boundedRoute, rateClass: rateNone, handler: s.mediaURLMethod}` (the entry's shape,
  `route_classification.go:149-170`); the header's mark comes from §3.3's middleware, not the entry.
- **Not the write bucket.** `rateNone` (`internal/api/ratelimit.go:286-290`), so the route never
  draws from `limits.write` (`PROTOCOL.md:2566-2578`, whose reads are unlimited by design, `:2572`).
  `TestRouteRegistry_everyMutatingRouteIsRateWrite` classes a POST as a mutation
  (`internal/api/rate_class_test.go:33-64`), so the route joins its exemption list with its reason:
  it resolves playback, and the signer's per-tenant mint limit (the design's §6) bounds it.
- **The other registry guards.** Both patterns join `TestRouteRegistry_completeness`
  (`route_classification_test.go:27-94`); the twin joins `TestEveryRoutedEndpointIsDocumented`'s
  exemptions with its reason, the 405 of a documented route (`routed_endpoints_test.go:186-192`);
  both are bounded routes.
- **The demo bridge.** It answers the route as any bridge does: a read, so not among the
  `demo_read_only` refusals (`PROTOCOL.md:191`), and served only while `directMedia.resolverEnabled`
  is on, which the demo's settings keep off until the rollout's gates pass (the design's §1 and §10:
  disclosure and recovery precede all non-operator direct traffic, the public demo included). Its
  shared token makes every demo user share one mint limit and one fair-use state (§9.1).

### 3.5 A ranged read of a local variant (Y8)

Checked against the handlers: `/v1/read` asks only that a `Range` header be there (`files.go:228-234`)
and runs `serveFile` (`:235`), which takes `variant` as `/v1/download` does (`:331-340`), into
`serveVariant` and `http.ServeContent` (`:601-603`). Both endpoints serve a ranged read of a
rendition today, with the same answers, and `PROTOCOL.md`'s error table already names both
(`PROTOCOL.md:2500`). The contract (§3.1): a client keeps the endpoint of its operation, a range
window on `/v1/read?path=&variant=` with its `Range`, a stream or a download on
`/v1/download?path=&variant=` with a `Range` for a resume; `/v1/read`'s section gains the sentence.
Both count as `local_variant` (§6), and a request with a variant never asks the routing table
(`files.go:281`; the console's too, `player_audio.go:329`).

## 4. D4: the tenant's state

### 4.1 Two files the control plane writes, and the bridge reads

The control plane writes two files into one directory it owns, `directMedia.stateDir` (§7.1); the
bridge reads them as it starts, and watches them. Rejected: a push over a socket the bridge listens
on (a write path into the bridge, lost at every restart, needing an acknowledgment of its own), and a
pull from the signer (a broken signer would then block the rollback that routes around it). Files
survive either side's restart, a rollback can be written with the signer down, and their trust is
the filesystem's: the control plane's to write, the bridge's to read (§4.4).

- **The migration marker**, `migration.json` (G1): the tenant's migration state, durable, written at
  each change of it and at no other time. It is the classification the bridge holds itself, so a
  policy lost or gone stale is told apart from a tenant never migrated: no marker in a present,
  trusted directory (§4.4) is `local`, and a migrated marker with no policy the bridge took refuses
  (§4.6).
- **The policy**, `policy.json`: what changes while the tenant is migrated (holds, the delivery mode,
  the cutoff, what is pending), rewritten on every change and as a heartbeat (§4.7).

The names are a **proposal**; the control plane's PR may name the directory, never what is in it.

### 4.2 The marker

```json
{ "schema": 1, "migration": "direct_enabled", "since": "2026-11-24T12:00:00Z" }
```

| Member | Type | The bridge's use |
|---|---|---|
| `schema` | `1` | any other: the marker is not taken |
| `migration` | `local`, `copying`, `verified`, `direct_enabled` | an original's class (§5) |
| `since` | RFC 3339 | echoed in the metrics; no decision reads it |

No `validUntil`: the marker is durable, and stays as it is until the next change of the tenant's
migration. Unknown members are ignored. A marker that is there and is not taken (unreadable, not
JSON, another schema, a member out of its set) counts as `direct_enabled` (§4.6): the bridge cannot
tell which tenant it is, and takes the reading that refuses.

### 4.3 The policy

```json
{
  "schema": 1,
  "version": 42,
  "issuedAt": "2026-11-24T12:00:00Z",
  "validUntil": "2026-11-24T12:10:00Z",
  "inventoryGeneration": 7,
  "deliveryMode": "direct",
  "holds": [{"id": "<opaque>", "since": "2026-11-24T03:00:00Z"}],
  "legacyCutoff": {"since": "2027-02-01T00:00:00Z", "minClientVersion": "2.4"},
  "publicationPending": [{"path": "Artist/Album/01 New Track.flac", "variant": null}]
}
```

How a handler learns each fact Y2 names, what it does with it (the migration state is the
marker's, §4.2), and each member's bound. A time is RFC 3339 in UTC, written with `Z` and at most
nine fractional digits, so 20 to 30 bytes; an integer is at most 2^53 − 1, which every JSON reader
holds exactly.

| Member | Type and bound | The bridge's use |
|---|---|---|
| `schema` | `1` | any other: the file is not taken |
| `version` | integer, 1 to 2^53 − 1 | moved by every change (§4.5); orders the policy against the signer's rollback answer (§5.4); echoed in the metrics (§6.2) |
| `issuedAt`, `validUntil` | a time each | freshness (§4.6) |
| `inventoryGeneration` | integer, 0 to 2^53 − 1 | echoed in the metrics; no decision reads it (the signer holds the inventory) |
| `deliveryMode` | `direct`, or `bridge` while an operator rollback is in force | `bridge` makes a migrated original `operator_rollback` (§5); it is the bridge's form of the media host's `direct_disabled` |
| `holds` | array of at most 64, empty for none: `id`, 1 to 64 bytes of `[A-Za-z0-9_-]`, and `since`, a time | any hold refuses the migrated originals (§5.5); `id` and `since` are the control plane's, counted in the metrics |
| `legacyCutoff` | null, or `since`, a time, and `minClientVersion`, 1 to 32 bytes of `[0-9A-Za-z.+-]` | the cutoff in force from `since`; the first app version with `directMediaV1` (§5.6) |
| `publicationPending` | array of at most 7,000 representations: `path` in §2.4's form, byte for byte, its JSON string at most 1,024 bytes as written between its quotes, escapes included; and `variant`, null for the original or a rendition's id (1 to 128 bytes of `[a-z0-9-]`, §3.2) | a migrated original listed here is `publication_pending`; each recorded before its publication (the design's §2) and removed once its object is verified. The bridge PR reads the entries whose `variant` is null; an entry for a rendition is defined now, so published renditions need no new schema |

**The size is part of the contract.** The control plane writes the policy compact, with no white
space between tokens (the example above is spaced for reading), so that at every bound it fits the
bridge's read of 8 MiB (§4.4). Counted as written:

| At its bound | Bytes |
|---|---|
| one hold, `{"id":"<64 bytes>","since":"<30 bytes>"}` | 114 |
| 64 holds and the 63 commas between them | 7,359 |
| one pending entry, `{"path":"<1,024 bytes>","variant":"<128 bytes>"}` | 1,176 |
| 7,000 entries and the 6,999 commas between them | 8,238,999 |
| everything else: the braces, the other members' names and values at their bounds, the cutoff's object | 339 |
| the whole file | 8,246,697, 141,911 bytes under 8,388,608 |

7,120 entries is the most that fits; the bound is the round number below it, and a member added
later takes its bytes from the margin. The control plane never writes past a bound: past 7,000
pending representations the rest of a backlog, and a representation whose path's string runs past
1,024 bytes, are left out of the list (which ones stay is the control plane's choice), and what is
left out falls to publication facts (§5.4) or to `eligible`, never to a hold, the delivery mode or
the cutoff, which are always written whole. **Proposal**: every member and bound (§9.1); the design
names the facts (Y2), not their encoding.

Unknown members are ignored (additive). A member of the wrong type, a value outside its set or past
its bound, a `validUntil` not after `issuedAt` or more than an hour after it, or an `issuedAt` more
than 30 s ahead of the bridge's clock: the file is not taken (`field_invalid`), and a file past 8 MiB
is not read (`too_large`). The control plane and the bridge run on one host and read one clock, so a
step of that clock moves the writer's times with the reader's; a forward step makes the last file
stale until the next write (60 s at most, §4.7).

Tests (the bridge PR): the policy at every member bound, compact, as the table counts it, is taken;
one entry more, a path's string a byte longer, a hold more, and each other member a step past its
bound are each `field_invalid`; a file of 8 MiB and one byte is `too_large`.

### 4.4 Trust: what the bridge could have written itself

The bridge trusts what it reads in the state directory, and what it does not find there, only where
it could not have changed either, judged on the descriptors it reads through, never on a path asked
again. Each refusal has its reason in the metrics (§6.2):

- **The way there.** The directory is opened an element at a time from `/`, each element as a
  directory without following a link (`O_DIRECTORY`, `O_NOFOLLOW`): a link on the way, the
  directory's own name included, is untrusted (`link`), and so is an element the bridge's UID owns
  (`owner`) or can write (`writable`), since what is below it is then its to replace.
- **The directory and its two files.** Each is owned by root or `signerUID` (§7.1), the control
  plane's one identity, never the bridge's UID, read from its own descriptor (`fstat`): any other
  owner is untrusted (`owner`). Each refuses the bridge's writes, asked of the kernel as an open would
  ask (`faccessat2` with `W_OK` and `AT_EACCESS`, relative to the descriptor the entry was reached
  through): one it could write is untrusted (`writable`). The files are opened through the
  directory's descriptor without following a link, as regular files (`fsutil.OpenAsFile`'s rule,
  which never waits on a named pipe), and read up to 8 MiB.
- **A missing directory is untrusted** (`state_dir`) while direct media is enabled: only a marker
  missing inside a present, trusted directory means `local` (§4.6). The control plane makes the
  directory when it provisions the tenant (§4.7), so a missing one is a fault or a mistyped path,
  never a word; what that costs a local tenant is §9.1's item 7.

Otherwise the state is **untrusted**: the bridge takes the marker as `direct_enabled` and takes no
policy (§4.6), so no process of the bridge's UID can lift a hold or a migration by editing,
replacing or removing a file. A policy file that is not taken because the bridge could write it, or
owns it, still adds its holds, since a forger gains nothing by them; nothing else of it is taken. A
bridge that runs as root can prove nothing out of its own reach, every write asked of the kernel being
its own, so its state is untrusted (`writable`; the doctor says so, §7.2). Its tests run as the
bridge's own UID, so the allowed owners and the bridge's UID are a seam of the check, as the metrics
socket's allowed UID is (§7.3). **Proposal**: the rule and its checks; the design names the
directory's purpose, not its proof.

### 4.5 Versions

A policy whose `version` is below the one taken in this process is not taken (`version_regressed`).
One at the same `version` is taken where nothing but `issuedAt` and `validUntil` moved (a heartbeat),
and otherwise not (`version_reused`). Kept in memory: a restart takes the file as it is.

### 4.6 Freshness, and the states

The bridge stats both files every 2 s (**proposal**) and reads one again when its identity, size or
mtime moved. Its decisions use the last policy it took, whatever that file's age; age decides one
thing, whether that policy is **current** (now before its `validUntil`, beside a configuration that
can run and a metrics socket that serves, §4.9, §6.1):

| The marker | The policy | What the bridge decides |
|---|---|---|
| missing inside a present, trusted directory (§4.4), `local` or `copying` | not read | every original is `not_migrated`; no policy is consulted, nothing is refused (F1) |
| `verified` | not read | every original is `verified`; no policy is consulted, nothing is refused |
| `direct_enabled`, or untrusted, its directory missing included, or not taken (§4.2, §4.4) | current | §5.3's table in full |
| the same | stale, or current beside a configuration that cannot run or a metrics socket that does not serve | §5.3's table on the last policy's values, the `eligible` class refused 503 `policy_unavailable` |
| the same | none taken since the process started: missing, unreadable, not taken (§4.3, §4.5), or untrusted (§4.4) | every migrated original refused 503 `policy_unavailable`, its holds and cutoff unknown and every exemption unproven; renditions, routed tracks and files outside the R2 mount served |

That is the design's "missing policy refuses eligible migrated-original fallback" with A2 and A3, and
F1: where the bridge took a policy, a rollback, a pending file and pre-cutoff traffic are served by
its values and only the eligible class waits for a current one; where it took none, it cannot prove
any migrated original exempt, so it refuses them all; and in neither does "no policy" mean "no
holds". A hold the last policy names stands whatever its age: only a later policy releases it. Local
and copying tenants and every rendition never depend on a policy (F1). The counters run in every
row, the refusals counted (§6.2), and each change of state is logged once, by its reason, never a
value.

### 4.7 What the control plane promises (its PR)

- The state directory exists from the tenant's provisioning on, it and its two files owned by root
  or `signerUID`, and neither it nor any directory above it is a link or the bridge's UID's to own or
  write (§4.4).
- The marker is written before a tenant leaves `local`, at every change of its migration, and is
  never removed while the tenant is past `local`: a marker missing from the directory is the word
  "local", and a missing directory is no word at all (§4.4). At a move to
  `direct_enabled` the policy is written first, so the bridge never holds a migrated marker beside no
  policy but by a fault.
- Every write is atomic: a temporary in the same directory, synced, renamed over the file, the
  directory synced.
- The policy is written again at least every 60 s with a `validUntil` at most ten minutes after its
  `issuedAt` (**proposal**: the capability's lifetime, so a control plane that stops writing costs the
  `eligible` class alone, after ten minutes).
- `version` moves on every change to anything but the two times, and never goes back.
- A hold is written when it is applied (the design's §6), and leaves the policy only by the audited
  manual release.
- `directMedia.enabled` is set before the marker leaves `local` and is never unset while the tenant
  is migrated: off is the control plane's word for "no direct media on this bridge", and a bridge
  that is off applies no guard (§7.1). A rollback is the policy's `deliveryMode`, never the switch.

### 4.8 A rollback

A rollback stops direct minting, publishes `direct_disabled` and installs explicit bridge routing
(the design's §9). The bridge's explicit route is `deliveryMode: "bridge"` in a new `version`:
`/v1/media-url` then answers `bridge` with `operator_rollback` for every migrated original without
asking the signer, which a rollback may be routing around, and the byte routes count those bytes
`operator_rollback`. Where the signer stops minting before the bridge has taken that file, its
`bridge` `operator_rollback` answer is a fact the bridge records (§5.4), so the client's fetch is
counted `operator_rollback` from its first byte. **Proposal**: the control plane publishes
`direct_disabled` once each affected bridge's metrics report the rollback's `version` (§6.2), or
after a bound. A rollback's file leaves `holds` as they were (§5.5).

**The order the bridge relies on** (**proposal**): the signer's answer names the version of the
policy that installs the rollback (`policyVersion`, §2.5), and the bridge's rollback fact stands until
it takes a policy at or above that version, which carries the rollback or, written after it, ends it.
A heartbeat of an earlier policy is below that version whatever its `issuedAt`, so it never clears
the fact. Chosen over the other order, the rollback's policy written before the signer stops minting
and every later policy carrying it, because this one asks nothing of the order of the rollback's three
steps, which the design's §9 lists without fixing one, nor of the bridge's poll against the signer's
answer, and compares no time another process wrote: an `issuedAt` may run 30 s ahead of the bridge's
clock (§4.3), and round 1's rule, any policy issued after the fact, let a heartbeat of the policy
before the rollback clear it.

### 4.9 A configuration that cannot run (F1, G2)

Off is a value: `directMedia.enabled` unset, empty or `false`, which leaves the bridge as it is today.
Anything else that is not a configuration that can run is **invalid**, and an invalid configuration
fails closed. It never makes a policy current, since it cannot vouch that the bridge runs as the
control plane set it: the `eligible` class is refused, and every migrated original where no policy
was taken (§4.6). The classifier, the counters and the watcher run all the same, the metrics socket
serves where its own setting can, and the start and the metrics say which setting cannot run, by its
path and never its value. Each kind adds an effect of its own, and a variable that does not parse,
white space alone included (§7.1), fails to its leaf's restrictive side:

| What cannot run | Its own effect, beside the above |
|---|---|
| `enabled` that does not parse | it reads as on: the classifier, the guard, the error header (§3.3) and the counters run |
| `resolverEnabled` that does not parse | the resolver reads as `invalid` (§6.2): `directMediaV1` is not advertised, `POST /v1/media-url` answers as with the resolver off (404 `direct_media_not_supported`), and the signer is never asked |
| `signerUID` that does not parse, or a string leaf of white space alone | its leaf cannot run: its row below |
| `stateDir` empty, too long, or not a directory | the state is untrusted (§4.4): the marker `direct_enabled`, no policy |
| `libraryRoot` empty, or equal to no configured root as written after the config's own path resolution | every library root counts as the R2 mount (§5.8), and the last row's signer gate |
| `metricsSocket` empty or past `sun_path`'s 107 bytes | the metrics socket is not served (§6.1) |
| a host other than Linux, where neither the peer's credentials nor the state's trust can be read | the state is untrusted, and neither socket is served |
| `signerSocket`, `signerUID` or `capabilityHost` empty or invalid while the resolver is on, and `libraryRoot` as above | the signer gate: every resolve that would ask the signer (§5.2's steps 6, 9 and 10, with a policy taken or without) answers 503 `media_unavailable`, asks nothing and records no fact; the classifier's other answers stand (a `bridge` answer with its class, a hold's 403), and `directMediaV1` is advertised as `resolverEnabled` says |

A resolver that cannot run never turns into bridge delivery: a client whose resolve answers 503 does
not fetch from the bridge (§3.1), so a typo costs direct delivery, never the ceilings, and leaves the
byte routes' guard and counts running. An invalid `libraryRoot` stops every signer call because the
signer's path is made relative to it (§2.4): a root that cannot run would name the wrong object, or
none. Rejected: round 0's rule, which turned direct media off with a reason, so a typo removed every
hold and every count (§11); and round 1's, which read a `resolverEnabled` that does not parse as on.

Tests (the bridge PR, each a real start with the variables set in the process's environment): one
configured root, a migrated marker and a current policy file, with `libraryRoot` empty, a
subdirectory of the root, and its parent: per case, a resolve of an original under the root answers
503 `media_unavailable` with nothing at the fake signer, and the byte routes answer as §4.6 gives a
configuration that cannot run (an `eligible` original 503 `policy_unavailable`, a `pre_cutoff` one
served). `resolverEnabled` misspelled beside `enabled` on and a signer configuration that can run: no
`directMediaV1`, the route's 404 with its header, nothing at the fake signer, the start and the
metrics naming `directMedia.resolverEnabled` (the resolver `invalid`), and an `eligible` fetch
refused 503 `policy_unavailable`. `enabled` misspelled: the byte routes' guard, their error header
and the counters all run, and the metrics name `directMedia.enabled`. Each switch set to white space
alone, as each misspelling. And the control: both switches valid on the same setup, and the resolve
reaches the fake signer.

## 5. Fallback eligibility: one classifier for every byte route and the resolver (Y2, Y3, Y4, F2)

### 5.1 The classes

Every body byte a byte route serves is in exactly one class, a closed set, the metrics' keys (§6.2):

| Class | What | Counts toward the bridge ceilings |
|---|---|---|
| `eligible` | A migrated original, published, no rollback, after the legacy cutoff: the brief's direct-eligible fallback | **yes** |
| `pre_cutoff` | A migrated original, published, no rollback, before the legacy cutoff (A3: visibility only) | no |
| `publication_pending` | A migrated original whose publication is pending (A2) | no |
| `operator_rollback` | A migrated original during an operator rollback (A2) | no |
| `not_migrated` | An original of a tenant `local` or `copying`, or with no marker in its present, trusted directory (§4.6) | no |
| `verified` | An original of a tenant `verified`, not yet `direct_enabled` | no |
| `local_variant` | A rendition: every rendition is a file of the bridge's own (the design's §5) | no |
| `non_r2_source` | A track proxied from an upstream UPnP server, or a file outside the R2 mount | no |

A migrated original is an original of a tenant whose marker is `direct_enabled` or untrusted (§4.6):
the ceilings apply only to original-media fallback while the migration state is `direct_enabled`
(the design's §6). The bridge counts every class; the control plane evaluates the ceilings over
`eligible` (the design's §6; A2, evaluation suspended during a rollback; A3, the guard on from the
cutoff; A4, the trailing slices). On the wire a class is its own name as the `bridge` answer's reason,
`verified` sent as `not_migrated` (the design's five), and `pre_cutoff` and `eligible` are never sent:
the resolver asks the signer for them.

### 5.2 One classifier (F2, G4)

One function, `classify(request, state)`, in `internal/directmedia`, decides every byte request of
the four byte routes and every resolve, per request, from the state the bridge holds when the
request comes; nothing carries a class from one request to another. Its inputs: the request (its
surface: a `/v1` byte route, a console byte route or the resolver; its `X-Client-Version`; a routed
track or not; a rendition or not; the absolute path of its one resolution; and a stat: on a byte
route the stat of the file it opened, taken from the open before the first byte, never the
resolution's, on both surfaces, `files.go:342` and `player_audio.go:422`; on the resolver the stat
it sends the signer, §2.4), and the state (the configuration's validity, the marker, the last policy
taken and whether it is current, the facts learned from the signer, §5.4, and whether the metrics
socket serves). It decides in this order, each step from what it has:

1. **Routing**, for a request without a variant: a routed track is `non_r2_source` before any stat,
   signer call or policy read (Y4; `files.go:281-300`; `player_audio.go:329-340`).
2. **A rendition**: after its validation (§3.2, steps 2 to 5), `local_variant`, before any
   direct-routing restriction (the design's §5). A published rendition, which the design's §2 allows,
   is later work: the resolver would ask the signer with `variant` (§2.4), and the policy's pending
   entries already name renditions (§4.3).
3. **The R2 mount**: a file outside it is `non_r2_source` (§5.8).
4. **The migration**, the marker's (§4.6): `local`, `copying` or none in a trusted directory,
   `not_migrated`; `verified`, `verified`. Nothing after this step is asked of them.
5. **A hold** (the last policy's, and those of a file not taken for `writable`, §4.4): refused 403
   `fair_use_refused`, whatever follows.
6. **No policy taken**: refused 503 `policy_unavailable`.
7. **A rollback**: the policy's `deliveryMode` `bridge`, or a rollback fact whose version the policy
   taken has not reached (§5.4): `operator_rollback`.
8. **Pending**: the representation in `publicationPending`, or, on a byte route and on the
   resolver's second pass alone, a publication fact matching its path and its stat (§5.4):
   `publication_pending`.
9. **The cutoff**: not in force, `pre_cutoff`; in force, on a `/v1` byte route, an app below the
   cutoff's version or sending none: refused 403 `direct_media_required` (§5.6).
10. **Eligible**: served while the policy is current (§4.6); otherwise refused 503
    `policy_unavailable`.

**The resolver classifies twice.** Its first pass comes before the signer, and reads the policy
alone at step 8: a publication fact never keeps it from asking, so once an object is published the
signer's `direct` reaches the client and clears the fact (§5.4). A class of steps 1 to 4, 7 or 8 is
answered `bridge` with that class as its reason, the signer not asked; a hold's refusal is answered
as it is. Steps 6, 9 and 10 are what direct delivery is for: the resolver asks the signer (§2) and
relays a capability, or records a `bridge` answer as a fact (§5.4) and classifies again. The second
pass reads the fresh fact, so a `publication_pending` or `operator_rollback` from the signer is
answered `bridge` with that reason, or with a hold's 403 or the no-policy 503 (§2.7); a second pass
that comes to step 9 or 10 again is the signer and the policy disagreeing, answered 503
`media_unavailable` and logged. Where the signer settings or `libraryRoot` cannot run (§4.9), every
resolve that would ask the signer, at steps 6, 9 and 10, answers 503 `media_unavailable`, asks
nothing and records nothing; the first pass's other answers stand. So a client the resolver sends
to the bridge, and fetches at once, is served and counted as the class it was answered with (F2);
where the state moves between the two, the fetch is judged as the state then stands (§5.4).

### 5.3 The table

An original under the R2 mount of a migrated tenant (step 4 passed), as the policy's state stands:

| Row | The policy says | Class | Policy current, no hold | Policy stale (§4.6) | Under a hold | No policy taken (§4.6) | `/v1/media-url` |
|---|---|---|---|---|---|---|---|
| 3 | `deliveryMode` `bridge`, or a rollback fact above the policy's version (§5.4) | `operator_rollback` | served | served | 403 `fair_use_refused` | 503 `policy_unavailable` | `bridge` `operator_rollback`, no signer |
| 4 | the representation pending, listed or by a fact of its stat (§5.4) | `publication_pending` | served | served | 403 | 503 | listed: `bridge` `publication_pending`, no signer; by a fact alone: the signer (§5.2) |
| 5 | before the cutoff, or none set | `pre_cutoff` | served | served | 403 | 503 | the signer |
| 6 | the cutoff in force; an app at or above its version, or the console | `eligible` | served | 503 `policy_unavailable` | 403 | 503 | the signer |
| 7 | the cutoff in force; on `/v1`, an app below its version or sending none (§5.6) | none: refused | 403 `direct_media_required` | 403 `direct_media_required` | 403 `fair_use_refused` | 503 | (the resolver applies no cutoff) |

Rows 1 and 2 are the marker's alone: `not_migrated` and `verified`, served in every column, the
resolver answering `bridge` `not_migrated` with no signer. Before the policy: a routed track (step 1)
and a file outside the R2 mount (step 3) are `non_r2_source`, a rendition (step 2) `local_variant`;
each is served in every column, the resolver answering `bridge` with its class as the reason; none
reads the policy or calls the signer.

**The resolver in each column.** With a stale policy, rows 3 and 4 are answered from its values and
rows 5 and 6 asked of the signer, which may still mint: direct delivery does not wait for the
bridge's policy. Row 4 by a fact alone is row 5 or 6 to the resolver's first pass, so it asks the
signer too (§5.2). With no policy taken, the resolver asks the signer and relays a capability; a
`bridge` answer it records as a fact, and the classifier still answers 503 `policy_unavailable`, the
answer the byte route would give (F2). Under a hold it answers 403 and asks nothing. With the signer
unavailable, or its settings or `libraryRoot` unable to run (§4.9), row 3, a listed row 4 and the
marker's rows stand, and every resolve that would ask the signer answers 503 `media_unavailable`
(§2.7), no fact recorded.

**Refusals' order.** A hold first (row 7 under a hold is `fair_use_refused`: the tenant is paused
whatever the app), then no policy, then the cutoff, then freshness.

**Tests.** Each row in each column, through `Handler()` for `/v1/read` (with a `Range`),
`/v1/download`, a HEAD of each and `POST /v1/media-url`, and through the console's mux for its two
routes, the class read back from the metrics socket. By the briefs' names: pre-cutoff visibility-only
traffic (row 5: served and counted `pre_cutoff`, never `eligible`, a stale policy still serving it);
`verified` (row 2); publication pending (row 4, listed and by a fact); a rollback (row 3, by the
policy and by the signer's answer at its version, with a hold and without, its bytes
`operator_rollback`); existing holds (rows 3 to 7 refused `fair_use_refused` on every route, rows 1
and 2 and the rows before the policy served); the cutoff (row 7 against row 6, by
`X-Client-Version`); staleness (row 6 alone refused, the moment an injected clock passes
`validUntil`); and Y4's routed track with no local file, the signer's socket absent and no policy
(200 `bridge` `non_r2_source`, the bytes proxied and counted `non_r2_source`, a resolver seam
recording no stat). The restart tests (F1, G1), each a real serve started over the state directory
and the variables as production sets them: a held, migrated tenant whose policy is then removed,
emptied, made invalid or made writable by the bridge's UID, a marker made writable, and the state
directory itself removed, each restarted: every migrated original refused 503 on all four byte
routes, the resolver's `bridge` answers from the fake signer answered 503 `policy_unavailable`,
renditions and routed tracks served, the counters running and the reason logged; a `local` and a
`copying` marker, and no marker in a present, trusted directory, each with no policy at all: every
original served, `not_migrated`; and a negative control that reads "no policy taken" as
`not_migrated` turns the first set red.

### 5.4 What the bridge learns from the signer, and what a later byte request does (F2, G4)

The signer can know what the bridge's policy does not yet say: that an object is not published at
the bridge's stat (its `bridge` `publication_pending`), or that a rollback began (its `bridge`
`operator_rollback`). The bridge keeps each such answer as a **fact**, which the classifier reads
beside the policy, per request:

| Fact | Identity | Recorded on | Lifetime | Cleared by |
|---|---|---|---|---|
| a publication fact: this representation, at this stat, is not published | the path below the R2 mount (§2.4), `size`, `mtimeNs` | the signer's `bridge` `publication_pending` for that stat; the bridge's own second stat that moved (§2.6), for the new stat | 600 s (**proposal**: a capability's lifetime) | the signer's `direct` for that identity; a replacement, which changes the identity; a restart |
| the rollback fact: the signer reports a rollback | the tenant, and the `policyVersion` its answer names (§2.5), the highest where several came | the signer's `bridge` `operator_rollback` | 600 s | a policy taken at or above that version, which carries the rollback or ends it (§4.8); a restart. Never a policy below it, a heartbeat of an earlier policy included |

On a byte route a fact's stat is the opened file's (§5.2), so a file replaced between the resolution
and the open is judged as the file the route serves. A publication fact shapes the byte routes' class
and the resolver's second pass, never its first: it never keeps the resolver from asking the signer
(§5.2). Facts are bounded at 10,000 (**proposal**), the oldest dropped first, which the mint limit
keeps far off: at 120 answers a minute a tenant and 600 s each, no more than 1,200 are ever alive. A
fact never decides anything a hold decides: a hold refuses first (step 5).

**A byte request after its resolver answer**, judged as the state stands when it comes (G4):

- **Publication completed since**: the fact still matches, so a fetch on the old answer is
  `publication_pending` (exempt) until the fact's lifetime ends or a `direct` answer for that
  identity clears it. The client's next resolve asks the signer whatever the fact (§5.2), gets
  `direct` and clears it, so a fetch after it is judged by the policy (row 5 or 6), never
  `publication_pending`.
- **A hold arrived since**: refused 403 `fair_use_refused` (step 5).
- **The file was replaced since**: its stat no longer matches the fact, so the fetch is judged as the
  file it is now, by the policy (row 5 or 6, or refused while the policy is not current); a client
  refused resolves again, and the signer's answer for the new stat records its own fact.
- **The policy went stale since**: a publication or rollback fact still serves the fetch (rows 3 and
  4 are served when stale); an `eligible` fetch is refused 503.
- **The policy caught up since** (the path listed, or the rollback written): the same class, now by
  the policy.
- **A rollback ended since**: the policy that ends it is at or above the fact's version, so it
  supersedes the fact, and the fetch is judged by it.
- **A heartbeat of an earlier policy since**: below the fact's version, so the fact stands and the
  fetch stays `operator_rollback` (§4.8).
- **A restart, or a fact dropped at the bound**: the policy alone decides, as above; a refused client
  resolves again.

Tests (G4): a fetch at once and one delayed past the fact's lifetime; a replacement between the
resolve and the fetch; the bound filled and the oldest fact dropped; a restart between them; and, for
each, a policy change between the two (a hold, the path listed, staleness, a rollback written and
then ended), each fetch's answer and its class asserted. Beside them: an older policy's heartbeat
taken after the rollback fact leaves the class `operator_rollback`, and a policy at the fact's
version clears it; a fresh `publication_pending` answer is answered `bridge`, and its fetch served
from the bridge and counted `publication_pending`; once publication completes, the next resolve
reaches the signer, gets `direct`, and the next fetch is not `publication_pending`; and a seam that
replaces the file between the resolution and the open, on each surface, has the fetch classified and
counted by the opened file's stat.

### 5.5 Exempt from the count is not clearing a hold (Y2)

Three questions, three columns, none derived from another: what counts toward the ceilings
(`eligible` alone), what a hold refuses (every migrated original: rows 3 to 7), and what a missing
policy refuses (`eligible`, where a policy was taken; every migrated original, where none was).
Nothing the bridge does adds or clears a hold: a hold leaves the policy only by the control plane's
audited release (the design's §6), and a rollback's file keeps `holds` as they were ("operator
rollback installs an explicit local route without clearing holds", the design's §6).

**The reading taken** (§9.4). The design's §6 applies "the same hold to minting, Worker access and
eligible bridge fallback", one sentence after it gives that population: original-media fallback while
the migration state is `direct_enabled`. A2 narrows "the bridge ceilings' eligible class", which is the
count. So a hold refuses a held tenant's rollback route, pending files and pre-cutoff traffic as well;
the other reading, a hold that refuses `eligible` alone, would let a held tenant stream from the bridge
through a rollback, and is the owner's to choose instead (§9.1).

### 5.6 Old apps after the cutoff (proposal)

A3 counts requests from apps without `directMediaV1` toward visibility only until the cutoff, and the
design's §10 answers unsupported original-media requests `403 direct_media_required` after it; neither
says how the bridge tells such an app. **Proposal**: by the request's `X-Client-Version` (`PROTOCOL.md`,
"Client version reporting"; read today at `api.go:2973-2975`) against `legacyCutoff.minClientVersion`,
compared as the updater compares versions (`internal/updater/updater.go:1148-1189`: `normalizeForSemver`,
then `x/mod`'s `semver`). Below it, with no header, or with one that does not parse, a `/v1`
request for a migrated original after the cutoff is row 7. The console's two routes are the bridge's
own page and never row 7 (§5.7). Before the cutoff nothing is told apart: the guard is off until it
(A3), so rows 5 and 6 differ by the cutoff alone. Rejected: a header the app would send for bridge
delivery alone, which needs an app change for a fact its version gives already.

### 5.7 The console's byte routes (Y3)

`servePlayerBytes` (`player_audio.go:305-440`) is `serveFile`'s twin with codes of its own and the same
steps: routing at `:329-340`, skipped for a rendition as `serveFile` skips it (`serveRoutedAudio`,
`:517-537`), the source at `:342-356`, resolved once, the rendition at `:359-420`, then the open and
`http.ServeContent` at `:422-439`. The bridge PR calls the same `classify` (console surface) for an
original after the open at `:422`, on the opened file's stat (`serveInfo`) and before the first byte,
and puts the same counting writer around the body of all three kinds. A refusal answers in the
console's envelope with §3.1's codes and the header (§3.3); an `<audio>` element sees only the
status, which the console reads as the design's §5 says.

The same PR moves the console's `variantIDPattern` (`player_audio.go:75`) to the alphabet
`/v1/media-url` takes (§3.2): today it does not match `optimized-dsd-v2-44100-16`, the DSD compact
tier's id that `pickPlayableVariant` (`:175-195`) prefers and the web player requests for a DSD track
(`internal/admin/static/player/format.js:161-163`), so `/api/player/audio` refuses that rendition 400
at `:316-319`, and a `local_variant` answer would send the console to a refusal (the pattern's refusal
was checked by running it over each family's id; the player's request was not run end to end). A
test plays that rendition through the route.

The console's resolver (`POST /api/player/media-url`), its use of the answer in place of `audioURL`
(`internal/admin/static/player/api.js:253-256`) and its renewal are PR 5's (Y9); what they share with
`/v1/media-url` is one resolver function, §2's signer client, §5's classifier, and §3.1's request,
answers, reasons and codes, behind the console's own session and CSRF refusals
(`auth-tls.md`, `csrfGuard`).

### 5.8 One resolution a request (G3)

A request resolves its path once: `ResolveChecked` (`fs.go:386-399`), whose `resolveParts` takes the
roots under one read lock and returns the absolute path it checked (`fs.go:258-262`, `:321-333`). The
open (`files.go:342`; `player_audio.go:422`), the class and the signer's path all take that absolute
path, and nothing resolves the client's path again: no `SplitRoot` (`fs.go:362-368`), which round 0
called a second time. So a root the console adds or removes while the request runs
(`internal/admin/handlers_api.go:2124-2125`) changes the next request alone: with one R2 root,
`B/song.flac` stays the R2 file it was opened as, even where a root named `B` is added between the two
steps.

The file is under the R2 mount where that absolute path is `directMedia.libraryRoot` or below it,
compared lexically, both as the config's own path resolution writes them; the signer's path is the
absolute path made relative to it (§2.4). A nested root inside the mount is therefore the mount's,
whose files reach the same keys under either root's spelling. A `libraryRoot` that equals no
configured root counts every root as the R2 mount and shuts the signer gate (§4.9), so an alias of
the mount's own spelling fails closed.

**Lexical containment rests on a promise, and the control plane makes it** (its PR, §7.4): on every
tenant with direct media enabled, `roots` is a managed control (`deployment.managedControls`,
`config.ManagedControlRoots`, `internal/config/config.go:1380`, whose routes the console refuses,
`internal/admin/admin.go:2026-2027`), no root holds a link, and no root, nor any directory above one,
is reached through a link. The resolver's path is lexical (`fs.go:321-333`) and the open follows
links, so without the promise a link in another root that leads into the mount would serve the
mount's bytes as `non_r2_source`, and a check of the opened file's device would not mend it: the
right device beside a lexical path still names `../<the other root>/...` to the signer. The bridge
walks no tree for links; the promise is the control plane's to keep.

Tests: a seam between the resolution and the classification adds a root named like the path's first
element (`Resolver.SetRoots`); the request is opened, classified and counted as the R2 file it
resolved to, and a negative control that resolves the path a second time turns it red. And the path
the signer receives: a file in the mount, resolved through the mount's root and through a root
nested inside it, reaches the fake signer as one path below the mount, byte for byte.

### 5.9 Outside the guard

The DLNA file route and the loopback track route serve renderers and a player on the bridge's own
network: a public-mode bridge starts no DLNA listener (`api.go:2132-2138`) and refuses UPnP upstreams
(`config.go:4010-4018`), and a cloud tenant runs no local player, so neither carries a cloud tenant's
bytes. They stay outside the guard and the counters, and a bridge that turned direct media on beside
them would leave their bytes uncounted. Artwork, booklets, waveforms and spectra are the bridge's own
caches, not media; the bridge's own player opens its files through no route.

## 6. D3: the metrics endpoint

### 6.1 Transport, and who may read

HTTP/1.1 on a Unix stream socket at `directMedia.metricsSocket` (§7.1), one route, `GET /v1/metrics`
(any other path 404, any other method 405). Serve makes it as it starts: a socket left at the path by an
earlier process is removed; anything else there (a regular file, a directory) is left as it is, and the
endpoint stays closed, said once, while the `eligible` class is refused (§4.6): bytes that count toward
the ceilings are never served where the collector cannot see them. The resolver goes on, since the
media host counts direct delivery itself (the design's §6). The socket is mode 0600, in a directory of
the bridge's own (the control plane's unit gives the bridge one, §7.4). At accept the bridge reads the
peer's credentials and serves UID 0 alone (**proposal**: "root-readable", the design's §6, as a check;
any other peer is closed unread). Serve's teardown closes the server and removes the socket, its
goroutine stopped by the teardown's context and joined before the store closes
(`cli-serve.md`), and its `http.Server` takes its error log from handshakelog as every
server of the module does.

Chosen over the console's `/metrics` (`internal/admin/admin.go:2199`), which serves the whole
registry, a float per series, on the admin listener behind the console's posture: this is one tenant's
own figures, exact integers, for root alone.

### 6.2 Format

The design names the facts, the bytes by eligibility class and the boot ID; the route, the document
and its keys are a **proposal**.

```json
{
  "schema": 1,
  "bootId": "4f1c2b0e9d8a7c6b5a4f3e2d1c0b9a88",
  "startedAt": "2026-11-24T06:00:00.123Z",
  "sampledAt": "2026-11-24T07:00:00.456Z",
  "config": { "state": "valid", "invalid": [] },
  "resolver": "on",
  "marker": { "state": "direct_enabled", "reason": null },
  "policy": {
    "state": "current",
    "reason": null,
    "version": 42,
    "inventoryGeneration": 7,
    "validUntil": "2026-11-24T07:09:00Z",
    "holds": 0
  },
  "facts": { "publication": 0, "rollback": null },
  "bytes": {
    "eligible": 0,
    "pre_cutoff": 0,
    "publication_pending": 0,
    "operator_rollback": 0,
    "not_migrated": 0,
    "verified": 0,
    "local_variant": 0,
    "non_r2_source": 0
  },
  "refused": {
    "fair_use_refused": 0,
    "direct_media_required": 0,
    "policy_unavailable": 0
  }
}
```

- `bootId`: 128 random bits drawn as the process starts (`crypto/rand`), in hex. `startedAt`: the
  process's start. `sampledAt`: the bridge's clock at the answer.
- `config`: `valid` or `invalid`, and the yaml path of each setting that cannot run (§4.9), never a
  value. `resolver`: `on` (served and advertised, whether or not its signer gate is shut, §4.9),
  `off`, or `invalid` (its switch does not parse: neither served nor advertised, the signer never
  asked).
- `marker`: its migration state, or `untrusted`, with a `reason`, null where the marker was taken:
  `missing` (inside a trusted directory, so `local`), `unreadable`, `not_json`, `schema_unknown` or
  `field_invalid` (each read as `direct_enabled`, §4.2), or a refusal of trust, which makes it
  `untrusted` (§4.4): `writable`, `owner` (an owner other than root and `signerUID`, the bridge's UID
  among them), `link` (a link at or on the way to the state directory or a file) or `state_dir` (the
  directory missing, not a directory, or its setting unable to run).
- `policy`: §4.6's state (`current`, `stale`, `none`, or `invalid` where the newest file was not
  taken); `reason`, where it was not taken, one of `missing`, `unreadable`, `too_large`, `not_json`,
  `schema_unknown`, `field_invalid`, `version_regressed`, `version_reused`, or a refusal of trust as
  the marker's (`writable`, `owner`, `link`, `state_dir`), else null; and the taken file's
  `version`, `inventoryGeneration`, `validUntil` and count of `holds`, null where none was taken.
- `facts`: the publication facts alive, and the version of a rollback fact that stands, null for
  none (§5.4).
- `bytes`: every class of §5.1, each a JSON integer (an unsigned 64-bit count, exact), cumulative since
  `startedAt`. The collector reads them as integers of 64 bits (§9.2): a count past 2^53 would lose
  its last digits in a float, which a tenant's process never reaches, at 9 PB.
- `refused` (**proposal**): the byte routes' refusals of §3.1, cumulative, so a cutoff's effect is
  visible.
- Every key is present in every answer, zeros included; a key may be added (the collector ignores what
  it does not know); a breaking change is `schema: 2`.

### 6.3 What a counted byte is

A body byte of a `200` or `206` to a GET that the response writer accepted (its `Write` returned it).
The counting writer, which the route puts around the body once the class is decided, records the
status its `WriteHeader` is given, a first `Write` before any being a `200` as net/http has it, and
counts a `Write`'s returned length only where the method is GET and that status is `200` or `206`:
around `http.ServeContent` for a file or a rendition (`files.go:364`, `:603`; `player_audio.go:439`),
around the proxy for a routed track (`internal/api/upnp_proxy.go:95-113`; `player_audio.go:531`). So
`ServeContent`'s own 416 text, its 412, a proxy's error relayed and any error body count nothing, nor
do headers, a HEAD (net/http eats its writes) or a 304. A transfer the client abandons counts what was
accepted before it ended: attempted delivery, as the design's §6 says of the Worker's counts. A
multi-range `206` counts its part headers too: the bridge serves those, and the media host refuses
them. One atomic counter a class. The writer implements `Unwrap` and `Flush` and no `ReadFrom`, which
loses no zero-copy path: the request log's writer offers none already (`middleware.go:121-126`), and
every listener is TLS.

Its tests, read through the metrics socket and never the counters' fields: a full GET adds exactly the
file's size; a range `a-b` adds `b-a+1`; a HEAD, a 304, a 412 and a 416 add nothing; a refusal adds
nothing to `bytes` and one to `refused`; a routed track's body adds its length to `non_r2_source`, and
its upstream's error nothing; a rendition through `/v1/read` and through `/v1/download` adds to
`local_variant`; a client that hangs up mid-body adds what its writer accepted, never more than the
size; concurrent transfers sum exactly, under `-race`.

### 6.4 Restarts

The counters live in the process and start at zero under a new `bootId`; nothing is persisted (the
design's §6: the collector marks restart gaps). The collector takes a change of `bootId` for a restart:
the bytes between its last sample and the old process's end are the gap, and the new process's are
counted from `startedAt`. A counter below its last sample under the same `bootId` is a fault to report,
never a negative delta.

## 7. D5: the bridge PR

### 7.1 Settings, all off by default

| Setting | Variable | Meaning |
|---|---|---|
| `directMedia.enabled` | `BRIDGE_DIRECT_MEDIA_ENABLED` | Read the state, run §5's classifier on the byte routes, count, serve the metrics socket |
| `directMedia.resolverEnabled` | `BRIDGE_DIRECT_MEDIA_RESOLVER_ENABLED` | Serve `POST /v1/media-url` and advertise `directMediaV1`; with `enabled` off it stays off |
| `directMedia.libraryRoot` | `BRIDGE_DIRECT_MEDIA_LIBRARY_ROOT` | The library root that is the R2 mount, one of `libraryRoots` (§5.8) |
| `directMedia.stateDir` | `BRIDGE_DIRECT_MEDIA_STATE_DIR` | The directory holding the marker and the policy (§4) |
| `directMedia.metricsSocket` | `BRIDGE_DIRECT_MEDIA_METRICS_SOCKET` | §6 |
| `directMedia.signerSocket` | `BRIDGE_DIRECT_MEDIA_SIGNER_SOCKET` | §2 |
| `directMedia.signerUID` | `BRIDGE_DIRECT_MEDIA_SIGNER_UID` | §2.3 (proposal) |
| `directMedia.capabilityHost` | `BRIDGE_DIRECT_MEDIA_CAPABILITY_HOST` | §2.6 |

- The variables are derived from the struct (`internal/config/env.go:12-37`, `:69-79`), and
  `TestEveryConfigLeafIsEnvSettable` holds the new leaves to it; the paths resolve as every
  path-typed setting does.
- **A direct-media variable that does not parse fails closed** (G2). Today a boolean or a whole
  number that does not parse is logged and ignored, the default kept (`env.go:392-397`, `:429-434`).
  For a `directMedia` leaf the overrides also record its yaml path, never its value, in the loaded
  config (which `Clone` keeps), and the start treats the configuration as invalid and the leaf as its
  restrictive side (§4.9): `enabled` on, `resolverEnabled` invalid, any other leaf unable to run.
  Only an unset or empty variable leaves the default (`env.go:252`); white space alone is a value
  that does not parse, where today a string leaf takes it as written and a boolean ignores it
  (`env.go:390-398`). In `bridge.yaml` a value that does not decode refuses the whole file, as every
  setting's does.
- **The control plane sets them through the environment** (**proposal**): a bridge refuses a
  `bridge.yaml` that holds a key it does not know (`internal/config/wholenumbers.go:46`,
  `KnownFields`), so a `directMedia` key in the file would stop a binary rolled back below PR 3 from
  starting, and make the updater's settings check refuse every candidate older than PR 3; a variable an
  older binary does not know is never read (`env.go:252`, `:270`). A console save writes no variable's
  value into the file (`internal/config/runtime.go:477-496`), and none of these leaves is in the
  settings PATCH, so the variables never reach the file.
- Serve reads them once, as it starts, for every consumer, so no setting's halves split
  (`config-lifecycle.md`); what is live is the state directory's content (§4.6).
  `ops/settings-apply-semantics.md` gains no row and the console shows none.
- A configuration that cannot run fails closed, never off (§4.9); off is `enabled` unset, empty or
  `false`.

### 7.2 Files

New, `internal/directmedia`, importing neither `internal/api` nor `internal/admin`, so both surfaces
share it:

- `class.go`: the classes, the five reasons and the three refusals, closed sets.
- `state.go`: the state directory's trust (§4.4), the marker's and the policy's decode and checks
  (§4.2, §4.3, §4.5), the states (§4.6), and the watcher, a goroutine on serve's context, its clock and
  its poll injectable for tests.
- `classify.go`: `classify`, §5.2's order and §5.3's table, pure: the request and the state in; a
  class, or a refusal, and whether the resolver asks the signer, out.
- `facts.go`: §5.4's publication and rollback facts, their identity, lifetime, bound and clearing.
- `count.go`: the counters, the boot ID, and the counting writer (§6.3).
- `metrics.go`, `peercred_linux.go`, `peercred_other.go`: §6's server, and the peer's credentials for
  both sockets (`syscall.GetsockoptUcred`; elsewhere the configuration cannot run, §4.9).
- `signer.go`: §2's client, its DTOs, §2.6's checks and §2.7's closed outcomes.
- `resolve.go`: the one resolver, called after a surface's own validation (§3.2): `classify` first,
  then the signer, then `classify` again for a `bridge` answer (§5.2). `/v1/media-url` calls it now,
  `/api/player/media-url` in PR 5.

Changed:

- `internal/api/media_url.go` (new): the handler and its twin; the body's decode (`http.MaxBytesReader`
  at 4 KiB, its members strict); every refusal a constant pair, for the error-table guard.
- `internal/api/files.go`: `classify` and the counter in `serveFile` for an original after the open
  at `:342`, on its stat (`opened`) and before `:364`, in `serveVariant` around `:603`, and around the
  proxy at `:296`, from the one resolution at `:302`;
  `serveVariant`'s lookup and freshness (`:406-467`) one function the route and the download share.
- `internal/api/upnp_proxy.go`: the counter around the proxied body (`:95-113`).
- `internal/api/api.go`: `WithDirectMedia`; `directMediaV1` (the capacity's count and list,
  `:2042-2060`); `writeError`'s header (`:3082-3084`); `errorCodes` in the chain (`:1523`).
- `internal/api/route_classification.go`: the two entries.
- `internal/api/*_test.go`: §7.3; `wantAllHealthFeatures` (`health_features_complete_test.go:31-72`);
  the registry, rate-class and documented-endpoint guards (§3.4); the answers' fixtures for the app,
  through `ios_fixture_export_test.go`'s export.
- `internal/admin/admin.go`, `internal/admin/player_audio.go`: `Deps.DirectMedia` beside
  `ProxyUPnPAudio` and `BeginPlaybackSession` (`admin.go:417-430`); `classify` and the counter in
  `servePlayerBytes` and `serveRoutedAudio`; the header's mark on the two registrations
  (`admin.go:2143-2144`); `variantIDPattern` (§5.7).
- `internal/config/config.go`, `internal/config/env.go`: `DirectMediaConfig` and `Config.DirectMedia`
  (`config.go:49-110`), its shape check, and the record of a direct-media variable that does not parse
  (§7.1).
- `cmd/bridge`: the wiring beside `api.New`'s chain (`main.go:4967-4999`) and the console's deps
  (`:6654`, `:6658`), in a file of its own: the state read once before any listener accepts, then the
  watcher and the metrics socket on serve's context, joined in the teardown before the store closes.
- `cmd/bridge`, the doctors (**proposal**): a `direct-media` line in both: the configuration's
  validity, the marker's and the policy's states with their reasons, a root run's untrusted state,
  and whether the signer's `GET /v1/health` answers.
- `PROTOCOL.md`: §3.1, its rows and sentences; the app's companion PR mirrors it
  (`CONTRIBUTING.md:9-21`).
- The rules: the route's bullet in `wire.md`; a rules file for `internal/directmedia` and the byte
  routes' guard, with its row in "Where things live"; the record under `ops/engineering-log/`.

### 7.3 Tests

- **The fake signer** (`internal/directmedia/directmediatest`): an HTTP server on a Unix listener in
  the test's temp dir, bound by its basename (B544); programmable answers, delays and closes; every
  request's bytes recorded.
- **The error vocabulary**: §3.3's table in its three settings; `bad_request` for each body refused
  (not JSON, past 4 KiB, a second value after the object, an unknown member, each of `tenant`,
  `bucket`, `key` and `lifetime`, a `path` missing or not a string); `invalid_variant` for each
  `variant` §3.2 refuses (a number, `true`, an object, an array, the empty string, 129 bytes, a byte
  outside `[a-z0-9-]`), each with its header; `invalid_path` for each of §3.2's steps 2 to 4; the
  `enabled`-on, `resolverEnabled`-off state, in which the byte routes refuse and carry the header and
  the route answers 404 with it.
- **Variant routing**: a rendition answered `bridge` `local_variant` with no signer and no policy;
  `/v1/read?variant=` and `/v1/download?variant=` with a `Range` serve the same bytes and count
  `local_variant`; a request with a variant never asks the routing table; the console plays the DSD
  compact tier's rendition (§5.7).
- **The classifier**: §5.3's rows in each column, §5.4's later byte requests, and §5.8's root change.
- **Fail closed** (F1, G1, G2): §5.3's restart tests; each row of §4.9's table at a real start, the
  variables set in the process's environment as production sets them, never a config struct built by
  hand: the eligible class refused, the counters running, the reason in the log and the metrics, a
  local tenant still served where its state is trusted; and §4.9's own tests, the two switches
  apart, white space alone among the values that do not parse, and the control that reaches the
  fake signer.
- **The counters' exactness**: §6.3's list.
- **The state**: each refusal of §4.2, §4.3 and §4.5 by its `reason`; a heartbeat taken; staleness by
  an injected clock; a replaced file taken at the next poll, the poll injected, never a sleep; each
  untrusted case of §4.4 by its reason, the allowed owners and the bridge's UID set through the
  check's seam (a file and the directory the bridge's UID can write; a directory the bridge's UID
  owns at mode 0555; a file of another owner; a state directory that is a link, and one below a link;
  a directory above it the bridge's UID can write; the directory missing; a root run), with a writable
  or bridge-owned policy's holds still applied and nothing else of it taken.
- **The metrics socket**: the golden document; every key present at zero; served to the allowed UID
  and a peer of any other closed unread (the allowed UID a seam on the server: the test's own UID, then
  another number); a stale socket replaced; a regular file at the path left as it is, the endpoint
  closed and the eligible class refused; the socket gone after the teardown.
- **The custody** (F3): the `directMedia` block holds no key or credential leaf, and no log line of a
  resolve carries the capability URL.
- **The wiring**: a real serve with the settings on, a fake signer and a state directory:
  `/v1/media-url` end to end, the metrics socket answering, the watcher and the socket joined before
  the store closes; and a serve with everything off answering every byte route as before, byte for
  byte, headers included.
- **Negative controls** for each load-bearing assertion (`build-ci-tests.md`): the hold
  check dropped from the classifier, "no policy taken" read as `not_migrated`, a variable that does
  not parse read as off, the routing step moved after the stat, a second resolution of the path, a
  `bridge` answer relayed without its fact, the counter put before the status check, the mark dropped
  from one path, each turning its tests red.

### 7.4 What stays the control plane's, and the others'

- **The control plane's (PR 1).** The signer service and its user; a listener per tenant, the UID
  registry with one tenant a UID, the peer check and the socket's removal before a UID is given again
  (§2.2); §2.4's path rule; the capability key and the R2 credential, their custody and rotation
  (§2.11); every decision behind an answer: R2, publication tracking, the inventory generation, the
  migration states and their order, the mint limit and its caches; the state directory, its
  ownership, the marker and the policy, their writes, bounds, heartbeat and versions, holds and their
  release, rollbacks and the version the signer names for one (§4.3, §4.7, §4.8); the managed
  roots, `roots` among the managed controls and no link in or above a root (§5.8); sampling the
  metrics as integers of 64 bits, the restart gaps, every threshold and the daily job (A1, A2, A4);
  the cutoff's date and `minClientVersion`, from the app's release and its adoption; the `bridge@`
  unit: §7.1's variables, a runtime directory of the bridge's own for the metrics socket, and the
  design's `InaccessiblePaths=` for the R2 credential and the signer's files, none of which the
  bridge reads; the front proxy's caps.
- **The Worker's (PR 2).** The capability URL's behaviour, CORS and codes; §3.1 restates the codes from
  the design for the app.
- **The app's (PR 4).** The resolver's use, the separate session, resume, renewal, the one re-resolve,
  `X-Client-Version` sent as today.
- **The console's (PR 5).** `POST /api/player/media-url` on §7.2's resolver, `crossorigin="anonymous"`,
  renewal 60 to 90 s before `expiresAt` (A7).

### 7.5 Landing

PR 3 merges with everything off, as the design's §10 allows disabled additive code to, its
`PROTOCOL.md` section beside the app's mirror PR: linked reviews, the same fixtures, and both CI
results before any bridge turns `resolverEnabled` on. `enabled` can go on for a tenant before
`resolverEnabled`, so its bridge bytes are counted by class before any app resolves, which Phase 0's
figures can use; its byte routes then refuse what §5.3 refuses, whether or not the bridge advertises
`directMediaV1` (§3.1).

## 8. What this plan does not do

- No code, no served route, no change to `PROTOCOL.md` (§3.1 is its draft), no rules file and no log
  entry: a rule lands with the code it describes.
- Nothing of the signer's decision, the capability's format, R2, KV or the media host, beyond the
  contract the bridge holds them to (§2.4, §2.11).
- No threshold, hold or release in the bridge: it counts and obeys.
- The console's player and its UI (PR 5).

## 9. Proposals and questions

### 9.1 For the owner

1. The hold's reach (§5.5): every migrated original, the rollback route included (taken), or
   `eligible` alone.
2. An old app after the cutoff (§5.6): `X-Client-Version` against the policy's `minClientVersion`, a
   `/v1` request without the header refused after the cutoff, the console never.
3. `X-Onebit-Error-Code` on `/v1/media-url` always and on the two byte routes while direct media is on
   (taken, §3.3), or on every `/v1` envelope error.
4. What the design does not name: `400` `bad_request` for a body, `404` `direct_media_not_supported`
   while off, the answer's member `reason`, `invalid_variant`'s alphabet.
5. The demo (§3.4): off until PR 6's gates; one mint limit and one fair-use state shared by every demo
   user.
6. The settings through the environment, for rollbacks (§7.1).
7. The risks of failing closed (§4.4, §4.6, §4.9): a migrated tenant whose policy is lost, or whose
   configuration has a typo, has its originals refused from the bridge until the control plane mends
   it; a bridge that runs as root never trusts its state. And a tenant still `local` whose state
   directory is missing, or whose `stateDir` is mistyped, while direct media is enabled for it, has
   every original under the R2 mount refused 503 `policy_unavailable`, its whole library though none
   of it is in R2 yet, until the path is mended; its renditions and upstream tracks are served, and
   the start, the metrics (`state_dir`) and the doctor say why.
8. The facts' lifetime and bound (§5.4): 600 s and 10,000.
9. The policy's bounds (§4.3): 64 holds with ids of 64 bytes at most, 7,000 pending representations
   with paths of 1,024 bytes at most as written, times of 30 bytes, the file compact, 8,246,697
   bytes at every bound against the 8 MiB read; a backlog past them left to facts or `eligible`.
10. The state directory's ownership (§4.4): the directory and its two files owned by root or
    `signerUID`, the control plane's one identity, never the bridge's UID, and the way to them free
    of links and of directories the bridge's UID owns or can write.
11. The rollback's order (§4.8): the signer's answer names the version of the rollback's policy, and
    the bridge's fact clears only at or above it (taken), or the control plane's writes ordered
    instead, the rollback's policy before the signer stops minting.

### 9.2 For the control plane's PR

1. The signer's API (§2.4 to §2.9): `POST /v1/mint` and `GET /v1/health`, the members, the answers, the
   codes, a connection a request, and §2.4's path rule; an `operator_rollback` answer naming the
   version of the policy that installs the rollback (`policyVersion`, §2.5, §4.8).
2. The signer's UID, handed to the bridge (`signerUID`, §2.3); one tenant a UID (§2.2).
3. The capability key's custody and rotation (§2.11).
4. The state directory (§4): made at the tenant's provisioning, owned with its files by root or
   `signerUID` and reached through no link or directory the bridge's UID owns or can write (§4.4,
   §4.7), the marker written before a tenant leaves `local`, the policy before a move to
   `direct_enabled`, a write of the policy at least every 60 s, `validUntil` at most ten minutes on,
   the versions, a rollback's `direct_disabled` published once the bridges report its `version`, and
   `directMedia.enabled` never unset on a migrated tenant.
5. The metrics (§6): root samples the JSON each minute, reads the counts as 64-bit integers, and reads
   `bootId` for restarts.
6. `publicationPending`'s entries in §2.4's form, recorded before publication; the policy written
   compact and within every bound of §4.3, a backlog past them cut from the list alone, with a test
   that its writer at every bound writes a file the bridge takes.
7. The unit (§7.4).
8. The bounds Phase 0 qualifies: §2.8's timeouts, §2.6's URL length.
9. The managed roots (§5.8): `roots` among the managed controls on every tenant with direct media
   enabled, no link in a root, and no root or directory above one reached through a link.

### 9.3 For the app's PR and the console's

1. **Open**: a `bridge` answer carries no lifetime in v1. Does the app want one, so that one answer
   can serve the work after it without asking again?
2. A `reason` it does not know read as a `bridge` answer; a fetch refused after a `bridge` answer
   followed by one resolve again (§3.1).
3. `variant` on `/v1/read`, documented (§3.5).
4. The console answers with `/v1/media-url`'s reasons and codes (§5.7).

### 9.4 The readings taken where the design allows two

- "Eligible bridge fallback" in the design's §6 hold sentence: the population the sentence before it
  names, not A2's narrowed count (§5.5).
- "Missing policy": no current policy; the last policy taken still decides the exempt classes, and
  where none was taken every migrated original is refused (§4.6, F1).
- `verified`: `not_migrated` on the wire (the design's five reasons), a class of its own in the metrics
  (Y8).

## 10. The briefs' items

| Item | Where |
|---|---|
| D1, the signer socket | §2 |
| D2, `POST /v1/media-url` and `directMediaV1` | §3 |
| D3, the metrics endpoint | §6 |
| D4, the policy snapshot | §4 |
| D5, the bridge PR | §7 |
| Y1, the signer authenticates by both | §2.2, §2.3, §2.10 |
| Y2, fallback eligibility | §4.3, §4.6, §5 |
| Y3, the console's byte routes | §5.7 |
| Y4, routing before stat | §3.2 (step 1), §5.2, §5.3 |
| Y5, the error vocabulary | §3.2, §3.3 |
| Y6, the draft stays in the plan | §3.1 |
| Y7, a read-class route | §3.4 |
| Y8, ranged reads of a local variant | §3.5, §5.1 |
| Y9, the console's surface, bounded | §5.7, §7.4 |
| F1, no policy, no enforcement gap | §4.6, §4.9, §5.3 (its restart tests) |
| F2, one classification | §5.2, §5.4 |
| F3, the mint key stays on the signer | §2.11 |
| F4, the error header's contract | §3.3 |
| G1, the migration marker | §4.1, §4.2, §4.6 |
| G2, a malformed variable fails closed | §4.9, §7.1 |
| G3, one root snapshot a request | §5.8 |
| G4, one classifier, per request | §5.2, §5.4 |
| G5, the signer's path rule | §2.4, §2.10 |

## 11. Round 0's reviews: where each P1 and P2 went

| Finding | Severity | Round 1 |
|---|---|---|
| A restart with the policy missing, unreadable or not taken lifts every hold and the cutoff (the `none` state) | P1 (three reviews) | Fixed: the marker, §4.6's states and §5.3's restart tests (F1, G1) |
| A policy refused as writable lifts the holds it guards | P1 | Fixed: an untrusted state refuses, a writable file's holds still apply (§4.4) |
| An invalid configuration turns direct media off, removing holds and counters | P1, P2 | Fixed: it fails closed, the resolver apart (§4.9, G2) |
| The resolver's `bridge` answers (the signer's, the second stat's, a rollback before the policy) disagree with the byte routes' class | P2 (three reviews) | Fixed: one classifier, and the signer's answers kept as facts (§5.2, §5.4, F2, G4) |
| The header is missing on the resolver's errors while direct media is off, and on the recoverer's 500 | P2 | Fixed: §3.3's mechanism and its table (F4) |
| The header and the refusals where `enabled` is on and `resolverEnabled` off | P2 | Fixed: the draft's wording and §3.3's table, with a test |
| The signer's path and the mint key's containment | P2 | Fixed: §2.4's path rule (narrowed by G5), one tenant a UID, §2.11 (F3); the blanket refusal refuted (§12) |
| Per-representation publication missing from the policy | P2 | Fixed: pending entries carry `variant` (§4.3) |
| The resolver's behaviour under a stale policy missing from the table | P2 | Fixed: §5.3's columns and its resolver paragraph |
| The console refuses the DSD compact tier's id | P2 | Fixed in PR 3's scope (§5.7) |
| The console's routes and the error-table guard | P2 | Answered: §3.3 (the guard reads `internal/api` alone) |
| The counting writer would count 416 and 500 bodies | P2 | Made explicit (§6.3); already specified, and the rest refuted (§12) |
| Tool and host names in the plan | P2 | Fixed: the front proxy, the metrics registry's format and the player daemon described, not named; the rules files cited by their names alone, and the root instruction file by its section's title; the `Host` placeholder explained (§2.4). The design's components refuted (§12) |

The P3s taken: the clock (§4.3), 64-bit integers (§6.2, §9.2), a metrics socket that cannot serve
(§6.1), an alias of the mount's path (§5.8), the request's `Host` (§2.4), and the tool named at §7.4.

## 12. Refuted in round 1

- **That R2, the Worker and KV are tools the plan must not name** (part of a P2 on naming, which asked
  it as a question). They are the components the design is about, named by the design this plan
  holds to section by section, so a contract that described them instead could not be matched to its
  section. What was a third party's product beside them (the front proxy, the metrics registry's
  format, the player daemon) is now described, not named, and the rules files are cited by their
  names (§11).
- **That the signer should refuse a backslash and a percent escape in a path** (the fix proposed for a
  P2 on the mint key). A backslash and a percent sign are ordinary filename bytes the resolver accepts
  (`internal/fs/fs.go:304-321`; nothing in `resolveParts` decodes), so the refusal would make tracks
  `/v1/download` serves today fail to resolve. Narrowed as G5 binds: traversal, ambiguity and the
  design's exclusions refused, the rest taken as bytes, decoded once and encoded once (§2.4).
- **That a missing policy file alone should keep the `none` state** (a P1's proposed fix). Beside a
  migrated marker a missing policy refuses as every other lost policy does (G1), so the file's absence
  is no exception (§4.6).
- **That the counting writer counts 416 and 500 bodies, and loses `sendfile`** (a P2). Round 0's §6.3
  counted "a body byte of a `200` or `206` to a GET" alone; round 1 spells out how the writer knows
  the status. No zero-copy path is lost: the request log's writer embeds only `http.ResponseWriter`,
  so it offers no `ReadFrom` today (`internal/api/middleware.go:121-126`), and every listener is TLS.
- **That a console save would write the `directMedia` variables into `bridge.yaml`** (a P3). A save
  writes each leaf a variable set as the file holds it, and no key the file did not write
  (`internal/config/runtime.go:477-496`, `filedEnvValues`), and none of these leaves is in the settings
  PATCH.
- **That a metrics socket that cannot serve should turn the resolver off** (a P3's proposed fix).
  Direct delivery is counted by the media host itself (the design's §6), so a closed socket hides only
  the bridge's bytes: the `eligible` class, which counts toward the ceilings, is refused instead
  (§6.1), and the resolver goes on.

## 13. Round 2: where each ruling went

| Ruling | Where |
|---|---|
| The policy's size is part of its contract: every member bounded, the worst case counted against the 8 MiB read, a backlog past the bounds cut from the list alone | §4.3 (the bounds, the count and their tests), §9.1, §9.2 |
| A heartbeat never clears the rollback fact: the signer names the version of the rollback's policy, and the fact clears only at or above it | §2.5, §2.7, §2.10, §4.8 (the order, and why), §5.2, §5.3, §5.4 (the heartbeat's test), §6.2, §9.1, §9.2 |
| A missing state directory is untrusted while direct media is enabled, the parent directory's clause dropped | §4.1, §4.4, §4.6, §4.7, §5.1, §5.2, §5.3 (its restart test), §6.2, §7.3, §9.1 (what it costs a local tenant) |
| A switch that does not parse fails to its restrictive side: `enabled` on, `resolverEnabled` invalid | §0, §3.3, §4.9 (its tests, the two switches apart), §6.2, §7.1, §7.3 |
| An invalid `libraryRoot` stops every signer call | §3.3, §4.9 (its test), §5.2, §5.3, §5.8 |
| Trust asks the owner as well as the write, on the opened descriptors, through no link; the owners named, the tests given a seam and each refusal a reason | §4.4, §4.7, §6.2, §7.3, §9.1, §9.2 |
| White space alone in a direct-media variable does not parse | §4.9, §7.1 |
| A publication fact never stops the resolver asking the signer: its first pass reads the policy alone, its second the fresh fact | §5.2, §5.3, §5.4 (its tests) |
| The class that counts and exempts is judged on the opened file's stat, on both surfaces | §5.2, §5.4 (its test), §5.7, §7.2 |
| The no-link premise is the control plane's promise of managed roots, and the path the signer receives is tested | §5.8, §7.4, §9.2 |
| The version comparison's module named without its host | §5.6 |
| A `variant` of another type is `invalid_variant`, in the draft and in the tests | §3.1, §7.3 |

## 14. Refuted in round 2

Four claims the verification refuted; the plan needs no change for them, and these lines settle each:

- **That the resolver must not ask the signer while no policy is taken**: direct delivery is the
  signer's and the media host's, and the signer refuses where it holds no policy of its own
  (`:249`); a `bridge` answer then goes through the classifier, which answers 503
  `policy_unavailable` as the byte routes would (`:240-241`, `:928-941`, `:961-969`).
- **That a stale policy with the signer down strands older clients**: no byte route asks the signer
  (`:62-65`, `:1313`), a stale policy serves pre-cutoff fetches (`:951`), and after the cutoff an
  app without `directMediaV1` is refused by design whatever the policy or the signer
  (`:953`, `:1065-1074`).
- **That the error-code middleware's matching by path is unsound**: it marks the exact paths the
  registry registers (`:527-539`, `:586-589`), a path no route serves is net/http's own answer
  outside the contract (`:569-572`), and the tests run through the chain production runs
  (`:579-582`).
- **That one tenant can mint another's files through a link**: the signer takes the tenant from
  the listener alone (`:103-115`) and builds the key under that tenant's prefix from a path it
  refuses to let climb (`:151`, `:164-181`), so a link on the bridge's disk changes which bytes the
  bridge serves, never which key the signer builds (`:316-322`, `:1119-1127`).
