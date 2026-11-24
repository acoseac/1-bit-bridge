# B1004: direct media delivery, the bridge's half: the contracts and the bridge PR's plan

Plan, 2026-11-24, round 0. **Docs only**: this round writes no code, serves no route and leaves
`PROTOCOL.md` as it is; the section the bridge PR adds to it is drafted in §3.1, marked "Draft, not
served". Every current behaviour cited is `path:line` at bridge main `726221d2`.

**Status.** For review. Binding: the B1004 design, round 3, with its amendments A1 to A7, which win
where its text differs ("the design", cited by section, §1 to §11); this round's brief, D1 to D5; and
the brief's review items Y1 to Y9, which win where the brief differs. Where the design is silent the
text says **proposal**, and §9 lists each one with who decides it. No contract here needed a decision
of the design changed; where the design's words allow two readings, §9.4 names the one taken.

No host, address or key path appears here. `media.example` stands for the media host the owner has
not chosen yet (the design's §11), `<tenant>` for a tenant's identifier, and every path the control
plane chooses is a setting the bridge is handed (§7.1).

**In one paragraph.** The design meets the bridge at three local seams, and this plan fixes each.
**The signer** is asked over HTTP/1.1 on a Unix socket the control plane makes for one tenant: it
knows the tenant from the listener the bridge connects to and checks the bridge's UID by
`SO_PEERCRED`, and the bridge sends the path it resolved, the variant and its stat, never a tenant,
bucket, key or lifetime (§2). **The policy** reaches the bridge as a JSON file the control plane
replaces atomically and the bridge reads and watches: the migration state, the inventory generation,
the delivery mode (a rollback), the holds, the legacy cutoff and the paths whose publication is
pending, with a `validUntil` past which it is stale (§4). **The metrics** are a JSON document on a
Unix socket only root may read: bytes by eligibility class since the process started, under a boot ID,
beside the policy version the bridge applies (§6). On those, `POST /v1/media-url` resolves a track
to `direct` (the signer's capability) or `bridge` (one of the design's five reasons) after the
validation `/v1/download` makes today (§3), and every byte route (`/v1/read`, `/v1/download`, the
console's `/api/player/audio` and `/api/player/download`) classifies what it serves by one decision
table, routing first, counts it, and refuses only what the table refuses: a held tenant's migrated
originals, an old app's after the legacy cutoff, and, while no current policy is there, the one
class that counts toward the bridge's emergency ceilings (§5). Everything is off by default and set
through the environment, so a binary rolled back below the bridge PR still loads its settings (§7).

## 0. At a glance

| Today | Where | The bridge PR (B1004 PR 3) |
|---|---|---|
| The byte routes serve any file the resolver finds, with no policy check and no count but the access log's | `internal/api/files.go:264-365`; `internal/admin/player_audio.go:305-440`; `internal/api/middleware.go:210-304` | One decision table classifies each request, routing first, refuses what it refuses, and counts body bytes by class (§5, §6) |
| No route resolves a track's delivery | `internal/api/route_classification.go:189-364` | `POST /v1/media-url` and a method-less twin that answers 405, both `rateNone` (§3) |
| `/v1/health.features` has no direct-media key | `internal/api/api.go:2060-2131` | `directMediaV1`, between `diagnosticsSummary` and `dlnaArtwork`, while the resolver is on (§3.1) |
| No error carries `X-Onebit-Error-Code` | `internal/api/api.go:3082-3084` | The three media routes opt in, and every error they write carries it, `authed`'s 401 included (§3.3) |
| File validation answers `bad_request` and `not_found`, a rendition `variant_*` | `files.go:302-319`, `:405-482`, `:736-755` | Unchanged there; `/v1/media-url` makes the same checks and maps them to `invalid_path` and the design's codes (§3.2) |
| The console's byte routes validate and serve on their own | `player_audio.go:305-440`, `:517-537` | The same decision and counters, at the same points (§5.6) |
| The bridge listens on no Unix socket; the console's `/metrics` is Prometheus on the admin listener | `internal/admin/admin.go:2199` | A metrics socket, JSON, for root (§6) |
| No setting | `internal/config/config.go:49-110` | A `directMedia` block, off by default, set through `BRIDGE_DIRECT_MEDIA_*` (§7.1) |

## 1. The seams and who owns them

```text
app, console --POST /v1/media-url-------------> bridge --POST /v1/mint--> signer   (the tenant's own Unix socket)
app, console --GET the capability URL---------> media host (Worker) --> R2   (never through the bridge)
app, console --/v1/read, /v1/download,
               /api/player/audio, /download---> bridge   (bridge delivery: classified, guarded, counted)
control plane --writes--> policy file --read, watched--> bridge
control plane <--GET /v1/metrics, each minute-- bridge's metrics socket (root alone)
```

| Seam | The bridge (PR 3) | The control plane (PR 1) |
|---|---|---|
| The signer socket (§2) | the client: dials, sends, checks the answer, maps its errors | a listener per tenant, the peer check, every decision, the capability, R2, the mint limit |
| The policy file (§4) | reads, validates and watches it; decides with it | writes it: every member, the heartbeat, the versions, the rollback's order |
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

- `path` (string, required): the representation's path below the library root that is the R2 mount
  (`directMedia.libraryRoot`, §7.1), forward slashes, exactly the bytes the resolver stat'ed: the
  suffix `bridgefs.Resolver.SplitRoot` gives for that root (`internal/fs/fs.go:362-368`), after the
  resolver's own checks and cleaning (`fs.go:214-334`). In a one-root library it is the client's
  path, cleaned. At most 4096 bytes. The signer builds `tenants/<tenant>/music/` and the key's
  encoding itself (the design's §2).
- `variant` (string or null, required): `null` for the original. The bridge PR never sends a
  rendition (§5.2, step 3); the member is defined now so the registered, published variants the
  design's §2 allows need no new version.
- `stat` (object, required): `size` (bytes) and `mtimeNs` (Unix nanoseconds) of the bridge's own
  `os.Stat` of the file it would serve (`ResolveChecked`, `fs.go:386-399`), at the precision the stat
  gave; the signer compares them with R2's size and the mount's mtime metadata at the precision it
  holds (the design's §2).
- Nothing else: a request with any other member, a tenant, bucket, key, lifetime or expiry among
  them, is refused 400 `bad_request` (the design's §3).
- `X-Request-ID` (**proposal**): the bridge's request id (`internal/api/middleware.go:212-213`), so the
  two logs can be joined.

### 2.5 The answers

`200 OK`, `Content-Type: application/json`, one of:

```json
{"delivery":"direct","url":"https://media.example/media?t=<capability>","expiresAt":"2026-11-24T12:10:00Z","size":123456789,"objectVersion":"<opaque>","etag":"\"<opaque>\""}
```

```json
{"delivery":"bridge","reason":"publication_pending"}
```

`reason` is one of the design's five, a closed set: `not_migrated`, `publication_pending`,
`local_variant`, `non_r2_source`, `operator_rollback` (the member's name is a **proposal**: the
design names the values only). A missing or stale R2 object is `bridge` with `publication_pending`,
or a 503 (§2.7), never a capability for other bytes (the design's §2). Unknown members of an answer
are ignored (§2.9).

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
HEAD"): the bridge stats the file once more after any answer, and a size or mtime that moved
discards the answer, the client getting `bridge` with `publication_pending`: the bytes on the bridge
are no longer the ones the signer judged.

### 2.7 Errors, and what the bridge answers for each

The signer's errors take the bridge's envelope, `{"error":"<code>","message":"<text>"}`, with
`X-Onebit-Error-Code`. The bridge maps each by a closed table of constants, writes its own fixed
message (a 5xx body carries no error's text: `.claude/rules/auth-tls.md`, B425) and logs the
signer's status and code alone.

| The signer answers | `POST /v1/media-url` answers |
|---|---|
| 400 `invalid_path`: its own refusal (a hidden component, `.bridge-trash/`, staging, a temporary, a database) | 400 `invalid_path` |
| 400 `invalid_variant` | 400 `invalid_variant` |
| 400 `bad_request`: the bridge sent what v1 does not take | 500 `internal`, logged at Error |
| 403 `fair_use_refused`: the tenant is held, minting stopped | 403 `fair_use_refused` |
| 429 `rate_limited` with `Retry-After`: the mint limit (the design's §6) | 429 `rate_limited`, its `Retry-After` held to 1 to 3600 s |
| 503 `media_unavailable`, an optional `Retry-After`: R2, or the signer's state, cannot decide | 503 `media_unavailable`, the same `Retry-After` |
| 503 `policy_unavailable`: the signer has no current policy for the tenant | 503 `policy_unavailable` |
| anything else: another status or code, a body that does not decode, a direct answer §2.6 refuses, a `reason` outside the five | 503 `media_unavailable` |
| no answer: the socket missing or refused, a connection closed unread (§2.2's refusal looks so), §2.3's refusal, §2.8's deadline | 503 `media_unavailable` |
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

The control plane's PR, the signer's side (Y1): a connection on a tenant's listener from a UID other
than that tenant's bridge is closed unread, answers nothing and mints nothing, and counts in the
fleet's diagnostics alone; the same for another tenant's bridge UID on that listener; a request that
names `tenant`, `bucket`, `key` or `lifetime` is 400; a removed tenant's socket is gone before its
UID is given again.

The bridge's PR: a fake signer on a Unix socket in the test's temp dir, bound by its basename from
that directory so a deep checkout stays inside `sun_path` (`.claude/rules/build-ci-tests.md`, B544),
answering each row of §2.7 in turn; the request's JSON byte for byte; a fake signer that closes each
connection unread (§2.2's refusal), and one whose UID is not `signerUID` (the expected UID set to
another number), each answered 503; each check of §2.6 broken alone; a stat moved by a seam between
the two stats, answered `bridge` with `publication_pending`.

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
bridge checks again when it serves the bytes: authentication, as always, and the refusals under
"Fetching from the bridge" below.

**Errors.** Each carries `X-Onebit-Error-Code` with its code, and the JSON body repeats it; a HEAD's
error carries the header alone.

| Status | `error` | When |
|---|---|---|
| 400 | `bad_request` | The body is not one JSON object of at most 4 KiB with `path` and an optional `variant` and nothing else |
| 400 | `invalid_path` | `path` is malformed, names no library root, or names a directory or something that is not a file, or the media signer does not take it as this library's music |
| 400 | `invalid_variant` | `variant` is neither null nor a rendition id (1 to 128 bytes of `[a-z0-9-]`) |
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
| 503 | `policy_unavailable` | The media signer holds no current delivery policy; ask again later |

A `503` never means "fetch from the bridge instead": a client fetches from the bridge only on a
`bridge` answer. This route draws from no rate-limit bucket of the bridge's; it is a read, and the
media signer's own limit answers its `429`.

#### Fetching from the bridge under direct delivery (additive; `directMediaV1`)

Where the bridge's operator has turned direct delivery on (such a bridge advertises
`directMediaV1` while it resolves), `/v1/read` and `/v1/download` add three answers, for a track's
original alone, never for a rendition or a track proxied from an upstream server:

| Status | `error` | When |
|---|---|---|
| 403 | `fair_use_refused` | Delivery of this library's music is paused for fair use |
| 403 | `direct_media_required` | This library's music is delivered directly now, and this app predates direct delivery: update the app |
| 503 | `policy_unavailable` | The bridge cannot read a current delivery policy; ask again later |

There, every error `/v1/read`, `/v1/download` and `/v1/media-url` write carries
`X-Onebit-Error-Code`, but for net/http's own answers to a path or method no route serves,
`http.ServeContent`'s own `416` and `412`, an upstream server's answer relayed (see Error responses)
and a `500` the server's panic recovery writes. `/v1/read` takes `&variant=<id>` exactly as
`/v1/download` does, with the same `404` and `410` answers.

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
  `POST /v1/media-url` and the answers it adds to `/v1/read` and `/v1/download`; a bridge without it
  serves as it always has."
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
| 503 | `policy_unavailable` | `POST /v1/media-url` while the media signer holds no current policy, or `/v1/read` or `/v1/download` of a track's original while the bridge holds none (`directMediaV1`) |

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
| 2. The source path: NUL, `..`, containment, the owning root, the stat | `ResolveChecked`, `fs.go:214-334`, `:386-399`; `writeResolveError`, `files.go:736-755` | 400 `bad_request` (a bad path, an unknown root), 404 `not_found`, 500 `internal` | 400 `invalid_path`, 404 `not_found`, 500 `internal` |
| 3. A directory | `files.go:306-309` | 400 `bad_request` | 400 `invalid_path` |
| 4. Not a regular file: a named pipe, a socket, a device | `fsutil.NotAFile`, `files.go:316-319`, `:373-376` | 400 `bad_request` | 400 `invalid_path` |
| 5. A rendition: the store, its row, its freshness, its sidecar | `serveVariant`, `files.go:405-482`, through the store adapter that adopts a relocated sidecar (`cmd/bridge/main.go:116-167`) | 404 `variant_not_found`, 410 `variant_stale`, 410 `variant_missing_on_disk`, 500 `internal` | the same answers; a stale rendition is asked for again as on a download (`files.go:458-464`); a missing sidecar is not reaped, which the download's open does (`:491-595`) |
| 6. The owning root is not the R2 mount | `SplitRoot`, `fs.go:362-368` | (no such step) | 200 `bridge` `non_r2_source` |
| 7. The decision table | §5 | (no such step) | 200 `bridge` with its reason, a 403, or the signer (§2) |

`invalid_variant` is decided before step 1: a `variant` member present, not null, and not a string
of 1 to 128 bytes of `[a-z0-9-]` (**proposal**: the ids' alphabet, so a new family needs no bridge
change). Not the console's `variantIDPattern` (`player_audio.go:75`), which refuses the DSD compact
tier's ids (§8). Step 5's lookup and freshness become one function `serveVariant` and the route
both call (§7.2), so the two cannot drift.

Existing endpoints keep every code (Y5): `/v1/read` and `/v1/download` answer steps 1 to 5 as
today and add only §3.1's three refusals.

### 3.3 The error vocabulary against today's helpers (Y5)

- **Which endpoints adopt it.** `POST /v1/media-url` in full (§3.1). `/v1/read` and `/v1/download`
  for their three new refusals, and the header on the errors they already send. No other route.
- **How the header is set.** `writeError` (`api.go:3082-3084`), which `writeErrorLog` and
  `writeFileErrorLog` end in (`:3115-3136`), sets `X-Onebit-Error-Code` to its code whenever the
  writer it is handed carries the opt-in that a route's middleware puts there (`withErrorCode`,
  **proposal**). The three media routes' registry entries wrap their whole chain in it, so their 401
  from `authed` (`api.go:2961-2994`), their 405 and every refusal of their handlers carry it, and no
  other route changes. The opt-in marks nothing while `directMedia.enabled` is off, so a bridge with
  direct media off answers `/v1/read` and `/v1/download` exactly as today, headers included. The
  alternative, the header on every `/v1` envelope error, is additive but changes every route's
  headers, and is the owner's to take instead (§9.1).
- **Errors raised before the resolver.** Auth: `authed`'s 401 inside the opt-in. Method: a
  method-less twin, `/v1/media-url`, registered beside `POST /v1/media-url`. Go's mux hands the twin
  every request that is not a POST, a HEAD included, and the twin answers `method_not_allowed` with
  `Allow: POST`; without it, net/http's own plain-text 405 carries no code (`PROTOCOL.md:2550`).
- **What carries no code.** net/http's 404 and 405 for a path or a method no route serves;
  `http.ServeContent`'s own 416 and 412 on a byte route (`files.go:364`, `:603`); an upstream UPnP
  server's answer relayed once it has answered (`PROTOCOL.md:2550`); and a panic's 500, which the
  recoverer writes outside every route's chain (`internal/api/middleware.go:317-341`). The media
  host's range codes are its own (§3.1).
- **The error table.** PROTOCOL.md's table (`PROTOCOL.md:2478-2550`) gains §3.1's pairs, which
  `TestEverySentErrorCodeIsInTheProtocolTable` and `TestEveryProtocolErrorRowIsSent`
  (`internal/api/error_table_test.go:33`, `:56`) hold to what the handlers send. Every status and
  code is a constant at its call, which the guard reads (`error_table_test.go:222-226`); the signer's
  codes reach a client only through §2.7's table of constants, never relayed as read.
- **Tests through the registered middleware.** Every case drives `Server.Handler()`
  (`api.go:1490-1524`), the chain production runs: on every refusal of the three routes, the header
  equals the body's `error`, and a HEAD's carries the header and no body; a route outside the three
  carries no header.

### 3.4 A read-class route (Y7)

- **Registry.** `{pattern: "POST /v1/media-url", kind: boundedRoute, rateClass: rateNone, handler:
  withCtxTimeout(10*time.Second, withErrorCode(s.authed(s.mediaURL)))}` and the twin, `{pattern:
  "/v1/media-url", kind: boundedRoute, rateClass: rateNone, handler: withErrorCode(s.mediaURLMethod)}`
  (the entry's shape, `route_classification.go:149-170`).
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
(`files.go:281`).

## 4. D4: the policy snapshot

### 4.1 Written by the control plane, read by the bridge

The control plane writes a file; the bridge reads it as it starts and watches it. Rejected: a push
over a socket the bridge listens on (a write path into the bridge, lost at every restart, needing an
acknowledgment of its own), and a pull from the signer (a broken signer would then block the rollback
that routes around it). A file survives either side's restart, a rollback can be written with the
signer down, and its trust is the filesystem's: the control plane's to write, the bridge's to read.

The path is `directMedia.policyFile` (§7.1), in a directory the control plane owns. **Proposal**: a
bridge that does not run as root takes the file only where it could not have written it or its
directory (`faccessat2`, `W_OK` with `AT_EACCESS`, refused for both), so no slip in provisioning lets a
process of the bridge's UID lift its own holds; it opens it as a regular file (`fsutil.OpenAsFile`,
which never waits on a named pipe) and reads at most 8 MiB.

### 4.2 Format

```json
{
  "schema": 1,
  "version": 42,
  "issuedAt": "2026-11-24T12:00:00Z",
  "validUntil": "2026-11-24T12:10:00Z",
  "migration": "direct_enabled",
  "inventoryGeneration": 7,
  "deliveryMode": "direct",
  "holds": [{"id": "<opaque>", "since": "2026-11-24T03:00:00Z"}],
  "legacyCutoff": {"since": "2027-02-01T00:00:00Z", "minClientVersion": "2.4"},
  "publicationPending": ["Artist/Album/01 New Track.flac"]
}
```

How a handler learns each fact Y2 names, and what it does with it:

| Member | Type | The bridge's use |
|---|---|---|
| `schema` | `1` | any other: the file is not taken |
| `version` | integer, 1 or more | moved by every change (§4.3); echoed in the metrics (§6.2) |
| `issuedAt`, `validUntil` | RFC 3339 | freshness (§4.4) |
| `migration` | `local`, `copying`, `verified`, `direct_enabled` | an original's class (§5) |
| `inventoryGeneration` | integer, 0 or more | echoed in the metrics; no decision reads it (the signer holds the inventory) |
| `deliveryMode` | `direct`, or `bridge` while an operator rollback is in force | `bridge` makes a migrated original `operator_rollback` (§5); it is the bridge's form of the media host's `direct_disabled` |
| `holds` | array, empty for none | any hold refuses the migrated originals (§5.4); `id` and `since` are the control plane's, counted in the metrics |
| `legacyCutoff` | null, or `since` and `minClientVersion` | the cutoff in force from `since`; the first app version with `directMediaV1` (§5.5) |
| `publicationPending` | array of paths | a migrated original listed here is `publication_pending`: each path in §2.4's form, byte for byte, recorded before its publication (the design's §2) and removed once its object is verified |

A rendition's publication is not here: every rendition is `local_variant` in the bridge PR (§5.2,
step 3). Unknown members are ignored (additive). A member of the wrong type, a value outside its set,
a `validUntil` not after `issuedAt` or more than an hour after it, an `issuedAt` more than 30 s ahead of
the bridge's clock, more than 100,000 pending paths, or a path past 4096 bytes: the file is not taken.
**Proposal**: every member and bound; the design names the facts (Y2), not their encoding.

### 4.3 Versions

A file whose `version` is below the one taken in this process is not taken (`version_regressed`).
One at the same `version` is taken where nothing but `issuedAt` and `validUntil` moved (a heartbeat),
and otherwise not (`version_reused`). Kept in memory: a restart takes the file as it is.

### 4.4 Freshness, and the four states

The bridge stats the file every 2 s (**proposal**) and reads it again when its identity, size or
mtime moved. Its decisions use the last file it took, whatever that file's age; age decides one thing,
whether that file is **current** (now before its `validUntil`):

| State | When | What the bridge decides |
|---|---|---|
| `none` | no file taken since the process started: none there, or none valid | migration `local`, no rollback, no hold, no cutoff: every original is `not_migrated`, nothing is refused, and `/v1/media-url` never asks the signer |
| `current` | the last file taken is before its `validUntil` | §5's table in full |
| `stale` | past its `validUntil`, no newer file taken | §5's table on the last file's values, the `eligible` class refused 503 `policy_unavailable` |
| `invalid` | the newest file was not taken | as `current` or `stale` by the last file taken, as `none` if none was; the metrics say why (§6.2) |

This is the design's "missing policy refuses eligible migrated-original fallback" with A2 and A3: a
rollback, a pending file and pre-cutoff traffic are served by the last policy taken, and local and
copying traffic and every rendition never depend on a policy at all (Y2, D4). A hold the last file
names stands whatever its age: only a later file releases it.

### 4.5 What the control plane promises (its PR)

- The file exists from the tenant's provisioning, before `directMedia.enabled` is set, and is never
  removed or emptied while the tenant is past `local`: `none` is the state of a tenant never
  migrated, and the bridge cannot tell a lost file from one that never was.
- Every write is atomic: a temporary in the same directory, synced, renamed over the file, the
  directory synced.
- It is written again at least every 60 s with a `validUntil` at most ten minutes after its
  `issuedAt` (**proposal**: the capability's lifetime, so a control plane that stops writing costs the
  `eligible` class alone, after ten minutes).
- `version` moves on every change to anything but the two times, and never goes back.
- A hold is written when it is applied (the design's §6), and leaves the file only by the audited
  manual release.

### 4.6 A rollback

A rollback stops direct minting, publishes `direct_disabled` and installs explicit bridge routing
(the design's §9). The bridge's explicit route is `deliveryMode: "bridge"` in a new `version`:
`/v1/media-url` then answers `bridge` with `operator_rollback` for every migrated original without
asking the signer, which a rollback may be routing around, and the byte routes count those bytes
`operator_rollback`. A client told `direct_disabled` by the media host that asks again before the
bridge has taken the file gets the signer's answer, the same one, since minting stopped first.
**Proposal**: the control plane publishes `direct_disabled` once each affected bridge's metrics report
the rollback's `version` (§6.2), or after a bound, so the bytes are counted `operator_rollback` from
the first. A rollback's file leaves `holds` as they were (§5.4).

## 5. Fallback eligibility: one decision table for every byte route (Y2, Y3, Y4)

### 5.1 The classes

Every body byte a byte route serves is in exactly one class, a closed set, the metrics' keys (§6.2):

| Class | What | Counts toward the bridge ceilings |
|---|---|---|
| `eligible` | A migrated original, published, no rollback, after the legacy cutoff: the brief's direct-eligible fallback | **yes** |
| `pre_cutoff` | A migrated original, published, no rollback, before the legacy cutoff (A3: visibility only) | no |
| `publication_pending` | A migrated original whose publication is pending (A2) | no |
| `operator_rollback` | A migrated original during an operator rollback (A2) | no |
| `not_migrated` | An original of a tenant `local` or `copying`, or of a bridge with no policy (§4.4) | no |
| `verified` | An original of a tenant `verified`, not yet `direct_enabled` | no |
| `local_variant` | A rendition: every rendition is a file of the bridge's own (the design's §5) | no |
| `non_r2_source` | A track proxied from an upstream UPnP server, or a file below a root that is not the R2 mount | no |

A migrated original is an original of a tenant `direct_enabled`: the ceilings apply only to
original-media fallback while the migration state is `direct_enabled` (the design's §6). The bridge
counts every class; the control plane evaluates the ceilings over `eligible` (the design's §6; A2,
evaluation suspended during a rollback; A3, the guard on from the cutoff; A4, the trailing slices).

### 5.2 The order

Each step decides from what it has, and none needs a later step's input:

1. **Routing**, for a request without a variant: a routed track is `non_r2_source` before any stat,
   signer call or policy read (Y4; `files.go:281-300`; `player_audio.go:329-340`).
2. **The source's validation**: §3.2, steps 2 to 4.
3. **A rendition**: its validation (§3.2, step 5), then `local_variant`, before any direct-routing
   restriction (the design's §5). A published rendition, which the design's §2 allows, is later
   work: the policy would name what is published and the resolver would ask the signer with
   `variant` (§2.4).
4. **The owning root**: not the R2 mount, `non_r2_source`.
5. **The policy** (§4): the migration, then a hold, then the delivery mode, then the pending list,
   then the cutoff, then freshness, as §5.3 orders them.

### 5.3 The table

An original under the R2 root, past steps 1 to 4:

| Row | Migration | And | Class | Served while current, no hold | Under a hold | No current policy (§4.4) | `/v1/media-url` |
|---|---|---|---|---|---|---|---|
| 1 | `local`, `copying`, or no policy | | `not_migrated` | yes | yes | yes | `bridge` `not_migrated`, no signer |
| 2 | `verified` | | `verified` | yes | yes | yes | `bridge` `not_migrated`, no signer |
| 3 | `direct_enabled` | `deliveryMode` `bridge` | `operator_rollback` | yes | 403 `fair_use_refused` | yes | `bridge` `operator_rollback`, no signer; held, 403 |
| 4 | `direct_enabled` | the path in `publicationPending` | `publication_pending` | yes | 403 | yes | `bridge` `publication_pending`, no signer; held, 403 |
| 5 | `direct_enabled` | before the cutoff, or none set | `pre_cutoff` | yes | 403 | yes | the signer (§2); held, 403 |
| 6 | `direct_enabled` | the cutoff in force; an app at or above its version, or the console | `eligible` | yes | 403 | 503 `policy_unavailable` | the signer; held, 403 |
| 7 | `direct_enabled` | the cutoff in force; on `/v1`, an app below its version or sending none (§5.5) | none: refused | 403 `direct_media_required` | 403 `fair_use_refused` | 403 `direct_media_required` | (a client that calls the route has `directMediaV1`) |

Before the policy: a routed track (step 1) and a file outside the R2 root (step 4) are
`non_r2_source`, a rendition (step 3) `local_variant`; each is served in every column, and
`/v1/media-url` answers `bridge` with its class as the reason; none reads the policy or calls the
signer.

**The signer unavailable.** No byte route calls the signer, so every column stands; `/v1/media-url`
answers rows 1 to 4 and the rows before the policy without it, and rows 5 and 6 503
`media_unavailable` (§2.7).

**Refusals' order.** A hold first (row 7 under a hold is `fair_use_refused`: the tenant is paused
whatever the app), then the cutoff, then freshness.

**Tests.** Each row in each column, through `Handler()` for `/v1/read` (with a `Range`),
`/v1/download`, a HEAD of each and `POST /v1/media-url`, and through the console's mux for its two
routes, the class read back from the metrics socket. By the brief's names: pre-cutoff visibility-only
traffic (row 5: served and counted `pre_cutoff`, never `eligible`, a stale policy still serving it);
`verified` (row 2); publication pending (row 4); a rollback (row 3, with a hold and without, its bytes
`operator_rollback`); existing holds (rows 3 to 7 refused `fair_use_refused` on every route, rows 1
and 2 and the rows before the policy served); the cutoff (row 7 against row 6, by `X-Client-Version`);
staleness (row 6 alone refused, the moment an injected clock passes `validUntil`); no policy (row 1 for
every original); and Y4's routed track with no local file, the signer's socket absent and no policy
file (200 `bridge` `non_r2_source`, the bytes proxied and counted `non_r2_source`, a resolver seam
recording no stat).

### 5.4 Exempt from the count is not clearing a hold (Y2)

Three questions, three columns, none derived from another: what counts toward the ceilings
(`eligible` alone), what a hold refuses (every migrated original: rows 3 to 7), and what a missing
policy refuses (`eligible` alone). Nothing the bridge does adds or clears a hold: a hold leaves the
policy only by the control plane's audited release (the design's §6), and a rollback's file keeps
`holds` as they were ("operator rollback installs an explicit local route without clearing holds",
the design's §6).

**The reading taken** (§9.4). The design's §6 applies "the same hold to minting, Worker access and
eligible bridge fallback", one sentence after it gives that population: original-media fallback while
the migration state is `direct_enabled`. A2 narrows "the bridge ceilings' eligible class", which is the
count. So a hold refuses a held tenant's rollback route, pending files and pre-cutoff traffic as well;
the other reading, a hold that refuses `eligible` alone, would let a held tenant stream from the bridge
through a rollback, and is the owner's to choose instead (§9.1).

### 5.5 Old apps after the cutoff (proposal)

A3 counts requests from apps without `directMediaV1` toward visibility only until the cutoff, and the
design's §10 answers unsupported original-media requests `403 direct_media_required` after it; neither
says how the bridge tells such an app. **Proposal**: by the request's `X-Client-Version` (`PROTOCOL.md`,
"Client version reporting"; read today at `api.go:2973-2975`) against `legacyCutoff.minClientVersion`,
compared as the updater compares versions (`internal/updater/updater.go:1148-1189`: `normalizeForSemver`,
then `golang.org/x/mod/semver`). Below it, with no header, or with one that does not parse, a `/v1`
request for a migrated original after the cutoff is row 7. The console's two routes are the bridge's
own page and never row 7 (§5.6). Before the cutoff nothing is told apart: the guard is off until it
(A3), so rows 5 and 6 differ by the cutoff alone. Rejected: a header the app would send for bridge
delivery alone, which needs an app change for a fact its version gives already.

### 5.6 The console's byte routes (Y3)

`servePlayerBytes` (`player_audio.go:305-440`) is `serveFile`'s twin with codes of its own and the same
steps: routing at `:329-340` (`serveRoutedAudio`, `:517-537`), the source at `:342-356`, the rendition at
`:359-420`, then the open and `http.ServeContent` at `:422-439`. The bridge PR puts §5.2's step 5 after
`:356` for an original, from the same `internal/directmedia` the API calls, and the same counting writer
around the body of all three kinds. A refusal answers in the console's envelope with §3.1's codes and
`X-Onebit-Error-Code`; an `<audio>` element sees only the status, which the console reads as the
design's §5 says. The console's resolver (`POST /api/player/media-url`), its use of the answer in place
of `audioURL` (`internal/admin/static/player/api.js:253-256`) and its renewal are PR 5's (Y9); what they
share with `/v1/media-url` is one resolver function, §2's signer client, §5's table, and §3.1's request,
answers, reasons and codes, behind the console's own session and CSRF refusals
(`.claude/rules/auth-tls.md`, `csrfGuard`).

### 5.7 Outside the guard

The DLNA file route and the loopback track route serve renderers and an MPD on the bridge's own
network: a public-mode bridge starts no DLNA listener (`api.go:2132-2138`) and refuses UPnP upstreams
(`config.go:4010-4018`), and a cloud tenant runs no MPD, so neither carries a cloud tenant's bytes. They
stay outside the guard and the counters, and a bridge that turned direct media on beside them would
leave their bytes uncounted. Artwork, booklets, waveforms and spectra are the bridge's own caches, not
media; the bridge's own player opens its files through no route.

## 6. D3: the metrics endpoint

### 6.1 Transport, and who may read

HTTP/1.1 on a Unix stream socket at `directMedia.metricsSocket` (§7.1), one route, `GET /v1/metrics`
(any other path 404, any other method 405). Serve makes it as it starts: a socket left at the path by an
earlier process is removed; anything else there (a regular file, a directory) is left as it is, and the
endpoint stays closed, said once. The socket is mode 0600, in a directory of the bridge's own (the
control plane's unit gives the bridge one, §7.4). At accept the bridge reads the peer's credentials and
serves UID 0 alone (**proposal**: "root-readable", the design's §6, as a check; any other peer is
closed unread). Serve's teardown closes the server and removes the socket, its goroutine stopped by
the teardown's context and joined before the store closes (`.claude/rules/cli-serve.md`), and its
`http.Server` takes its error log from handshakelog as every server of the module does.

Chosen over the console's Prometheus `/metrics` (`internal/admin/admin.go:2199`), which serves the whole
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
  "policy": {
    "state": "current",
    "reason": null,
    "version": 42,
    "inventoryGeneration": 7,
    "validUntil": "2026-11-24T07:09:00Z",
    "holds": 0
  },
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
- `policy`: §4.4's state; `reason`, where the newest file was not taken, one of `missing`,
  `unreadable`, `too_large`, `not_json`, `schema_unknown`, `field_invalid`, `version_regressed`,
  `version_reused`, `writable` (§4.1), else null; and the taken file's `version`,
  `inventoryGeneration`, `validUntil` and count of `holds`, null for `none`.
- `bytes`: every class of §5.1, each a JSON integer (an unsigned 64-bit count, exact), cumulative since
  `startedAt`.
- `refused` (**proposal**): the byte routes' refusals of §3.1, cumulative, so a cutoff's effect is
  visible.
- Every key is present in every answer, zeros included; a key may be added (the collector ignores what
  it does not know); a breaking change is `schema: 2`.

### 6.3 What a counted byte is

A body byte of a `200` or `206` to a GET that the response writer accepted (its `Write` returned it),
counted by a writer the route puts around the body once the class is decided: around
`http.ServeContent` for a file or a rendition (`files.go:364`, `:603`; `player_audio.go:439`), around the
proxy for a routed track (`internal/api/upnp_proxy.go:95-113`; `player_audio.go:531`). Not counted:
headers, an error's body (written before the counter, or under another status, `ServeContent`'s 416
text included), a HEAD (net/http eats its writes), a 304. A transfer the client abandons counts what was
accepted before it ended: attempted delivery, as the design's §6 says of the Worker's counts. A
multi-range `206` counts its part headers too: the bridge serves those, and the media host refuses
them. One atomic counter a class.

Its tests, read through the metrics socket and never the counters' fields: a full GET adds exactly the
file's size; a range `a-b` adds `b-a+1`; a HEAD, a 304 and a 416 add nothing; a refusal adds nothing
to `bytes` and one to `refused`; a routed track's body adds its length to `non_r2_source`; a rendition
through `/v1/read` and through `/v1/download` adds to `local_variant`; a client that hangs up mid-body
adds what its writer accepted, never more than the size; concurrent transfers sum exactly, under
`-race`.

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
| `directMedia.enabled` | `BRIDGE_DIRECT_MEDIA_ENABLED` | Read the policy, apply §5 on the byte routes, count, serve the metrics socket |
| `directMedia.resolverEnabled` | `BRIDGE_DIRECT_MEDIA_RESOLVER_ENABLED` | Serve `POST /v1/media-url` and advertise `directMediaV1`; needs `enabled` |
| `directMedia.libraryRoot` | `BRIDGE_DIRECT_MEDIA_LIBRARY_ROOT` | The library root that is the R2 mount, one of `libraryRoots` |
| `directMedia.policyFile` | `BRIDGE_DIRECT_MEDIA_POLICY_FILE` | §4 |
| `directMedia.metricsSocket` | `BRIDGE_DIRECT_MEDIA_METRICS_SOCKET` | §6 |
| `directMedia.signerSocket` | `BRIDGE_DIRECT_MEDIA_SIGNER_SOCKET` | §2 |
| `directMedia.signerUID` | `BRIDGE_DIRECT_MEDIA_SIGNER_UID` | §2.3 (proposal) |
| `directMedia.capabilityHost` | `BRIDGE_DIRECT_MEDIA_CAPABILITY_HOST` | §2.6 |

- The variables are derived from the struct (`internal/config/env.go:12-37`, `:69-79`), and
  `TestEveryConfigLeafIsEnvSettable` holds the new leaves to it; the paths resolve as every
  path-typed setting does.
- **The control plane sets them through the environment** (**proposal**): a bridge refuses a
  `bridge.yaml` that holds a key it does not know (`internal/config/wholenumbers.go:46`,
  `KnownFields`), so a `directMedia` key in the file
  would stop a binary rolled back below PR 3 from starting, and make the updater's settings check
  refuse every candidate older than PR 3; a variable an older binary does not know is never read
  (`env.go:252`, `:270`).
- Serve reads them once, as it starts, for every consumer, so no setting's halves split
  (`.claude/rules/config-lifecycle.md`); what is live is the policy file's content (§4.4). None is in
  the settings PATCH, so `ops/settings-apply-semantics.md` gains no row and the console shows none.
- A block that cannot run turns direct media off with its reason, said once, and serve starts
  (**proposal**: the output player's rule, so a rollback in an incident never stops a tenant; the
  metrics socket's absence is the control plane's alarm): a required path empty, a socket path past
  `sun_path`'s 107 bytes, `libraryRoot` not a configured root, `resolverEnabled` without `enabled`, a
  host other than Linux (the peer's credentials are Linux's).

### 7.2 Files

New, `internal/directmedia`, importing neither `internal/api` nor `internal/admin`, so both surfaces
share it:

- `class.go`: the classes, the five reasons and the three refusals, closed sets.
- `policy.go`: the file's decode and checks (§4.2, §4.3), the states (§4.4), and the watcher, a
  goroutine on serve's context, its clock and its poll injectable for tests.
- `decide.go`: §5.2's step 5 and §5.3's table, pure: the policy, the time, the surface and the client's
  version in; a class or a refusal out.
- `count.go`: the counters, the boot ID, and the counting writer (`Unwrap` for
  `http.ResponseController`, `Flush`).
- `metrics.go`, `peercred_linux.go`, `peercred_other.go`: §6's server, and the peer's credentials for
  both sockets (`syscall.GetsockoptUcred`; elsewhere refused, §7.1).
- `signer.go`: §2's client, its DTOs, §2.6's checks and §2.7's closed outcomes.
- `resolve.go`: the one resolver, called after a surface's own validation (§3.2): the path below the
  root, its stat, a rendition or not, and the policy in; an answer or a refusal out. `/v1/media-url`
  calls it now, `/api/player/media-url` in PR 5.

Changed:

- `internal/api/media_url.go` (new): the handler and its twin; the body's decode (`http.MaxBytesReader`
  at 4 KiB, its members strict); every refusal a constant pair, for the error-table guard.
- `internal/api/files.go`: the decision and the counter in `serveFile` after `:319` for an original,
  in `serveVariant` around `:603`, and around the proxy at `:296`; `serveVariant`'s lookup and
  freshness (`:406-467`) one function the route and the download share.
- `internal/api/upnp_proxy.go`: the counter around the proxied body (`:95-113`).
- `internal/api/api.go`: `WithDirectMedia`; `directMediaV1` (the capacity's count and list,
  `:2042-2060`); `writeError`'s header (`:3082-3084`) and `withErrorCode`.
- `internal/api/route_classification.go`: the two entries, and `withErrorCode` on `/v1/read` and
  `/v1/download` (`:206-207`).
- `internal/api/*_test.go`: §7.3; `wantAllHealthFeatures` (`health_features_complete_test.go:31-72`);
  the registry, rate-class and documented-endpoint guards (§3.4); the answers' fixtures for the app,
  through `ios_fixture_export_test.go`'s export.
- `internal/admin/admin.go`, `internal/admin/player_audio.go`: `Deps.DirectMedia` beside
  `ProxyUPnPAudio` and `BeginPlaybackSession` (`admin.go:417-430`); the decision and the counter in
  `servePlayerBytes` and `serveRoutedAudio` (§5.6).
- `internal/config/config.go`: `DirectMediaConfig` and `Config.DirectMedia` (`:49-110`), its shape
  check.
- `cmd/bridge`: the wiring beside `api.New`'s chain (`main.go:4967-4999`) and the console's deps
  (`:6654`, `:6658`), in a file of its own: the policy read once before any listener accepts, then
  the watcher and the metrics socket on serve's context, joined in the teardown before the store
  closes.
- `cmd/bridge`, the doctors (**proposal**, optional): a `direct-media` line in both, the policy's state
  and version and whether the signer's `GET /v1/health` answers.
- `PROTOCOL.md`: §3.1, its rows and sentences; the app's companion PR mirrors it
  (`CONTRIBUTING.md:9-21`).
- `.claude/rules/`: the route's bullet in `wire.md`; a rules file for `internal/directmedia` and the
  byte routes' guard, with its row in CLAUDE.md's "Where things live"; the record under
  `ops/engineering-log/`.

### 7.3 Tests

- **The fake signer** (`internal/directmedia/directmediatest`): an HTTP server on a Unix listener in
  the test's temp dir, bound by its basename (B544); programmable answers, delays and closes; every
  request's bytes recorded.
- **The error vocabulary**: every row of §2.7 and of §3.1's table through `Handler()`, the header equal
  to the body's code, a HEAD's header without a body; `bad_request` for each body refused (not JSON,
  past 4 KiB, a second value after the object, an unknown member, each of `tenant`, `bucket`, `key` and
  `lifetime`, a `variant` of another type); `invalid_path` for each of §3.2's steps 2 to 4; the 401 and
  the 405 with the header; no header on a route outside the three.
- **Variant routing**: a rendition answered `bridge` `local_variant` with no signer and no policy;
  `/v1/read?variant=` and `/v1/download?variant=` with a `Range` serve the same bytes and count
  `local_variant`; a request with a variant never asks the routing table.
- **The decision table**: §5.3's rows in each column, on the four byte routes and the resolver
  (§5.3's list).
- **The counters' exactness**: §6.3's list.
- **The snapshot**: each refusal of §4.2 and §4.3 by its `reason`; a heartbeat taken; staleness by an
  injected clock; a replaced file taken at the next poll, the poll injected, never a sleep; a file the
  bridge could write not taken (`writable`; skipped as root).
- **The metrics socket**: the golden document; every key present at zero; served to the allowed UID
  and a peer of any other closed unread (the allowed UID a seam on the server: the test's own UID, then
  another number); a stale socket replaced; a regular file at the path left as it is, the endpoint
  closed; the socket gone after the teardown.
- **The wiring**: a real serve with the settings on, a fake signer and a policy file: `/v1/media-url`
  end to end, the metrics socket answering, the watcher and the socket joined before the store closes;
  and a serve with everything off answering every byte route as before, byte for byte.
- **Negative controls** for each load-bearing assertion (`.claude/rules/build-ci-tests.md`): the hold
  check dropped from the decision, the routing step moved after the stat, the counter put before the
  status check, the opt-in dropped from one route, each turning its tests red.

### 7.4 What stays the control plane's, and the others'

- **The control plane's (PR 1).** The signer service and its user; a listener per tenant, the UID
  registry, the peer check and the socket's removal before a UID is given again (§2.2); every decision
  behind an answer: keys and their rotation, R2, publication tracking, the inventory generation, the
  migration states and their order, the mint limit and its caches; the policy file, its writes, its
  heartbeat and versions, holds and their release, rollbacks and their order (§4.5, §4.6); sampling the
  metrics, the restart gaps, every threshold and the daily job (A1, A2, A4); the cutoff's date and
  `minClientVersion`, from the app's release and its adoption; the `bridge@` unit: §7.1's variables, a
  runtime directory of the bridge's own for the metrics socket, and the design's `InaccessiblePaths=`
  for the R2 credential and the signer's files, none of which the bridge reads; HAProxy's caps.
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
figures can use.

## 8. What this plan does not do

- No code, no served route, no change to `PROTOCOL.md` (§3.1 is its draft), no rules file and no log
  entry: a rule lands with the code it describes.
- Nothing of the signer's decision, the capability's format, R2, KV or the media host.
- No threshold, hold or release in the bridge: it counts and obeys.
- The console's player and its UI (PR 5).
- A defect found on the way, outside B1004: the console's `variantIDPattern` (`player_audio.go:75`)
  does not match `optimized-dsd-v2-44100-16`, the DSD compact tier's id that `pickPlayableVariant`
  (`:175-195`) prefers and the web player requests for a DSD track
  (`internal/admin/static/player/format.js:161-163`), so `/api/player/audio` refuses that rendition
  400 at `:316-319` (the pattern's refusal was checked by running it over each family's id; the
  player's request was not run end to end).

## 9. Proposals and questions

### 9.1 For the owner

1. The hold's reach (§5.4): every migrated original, the rollback route included (taken), or
   `eligible` alone.
2. An old app after the cutoff (§5.5): `X-Client-Version` against the policy's `minClientVersion`, a
   `/v1` request without the header refused after the cutoff, the console never.
3. `X-Onebit-Error-Code` on the three media routes alone (taken, §3.3), or on every `/v1` envelope
   error.
4. What the design does not name: `400` `bad_request` for a body, `404` `direct_media_not_supported`
   while off, the answer's member `reason`, `invalid_variant`'s alphabet.
5. The demo (§3.4): off until PR 6's gates; one mint limit and one fair-use state shared by every demo
   user.
6. The settings through the environment, for rollbacks (§7.1); a block that cannot run turning the
   feature off rather than refusing the settings.

### 9.2 For the control plane's PR

1. The signer's API (§2.4 to §2.9): `POST /v1/mint` and `GET /v1/health`, the members, the answers, the
   codes, a connection a request.
2. The signer's UID, handed to the bridge (`signerUID`, §2.3).
3. The policy file (§4.2 to §4.6): the members, a write at least every 60 s, `validUntil` at most ten
   minutes on, the versions, never removed past `local`, a rollback's `direct_disabled` published once
   the bridges report its `version`.
4. The metrics (§6): root samples the JSON each minute, and reads `bootId` for restarts.
5. `publicationPending`'s paths in §2.4's form, recorded before publication.
6. The unit (§7.4).
7. The bounds Phase 0 qualifies: §2.8's timeouts, §2.6's URL length.

### 9.3 For the app's PR and the console's

1. **Open**: a `bridge` answer carries no lifetime in v1. Does the app want one, so that one answer
   can serve the work after it without asking again?
2. A `reason` it does not know read as a `bridge` answer.
3. `variant` on `/v1/read`, documented (§3.5).
4. The console answers with `/v1/media-url`'s reasons and codes (§5.6).

### 9.4 The readings taken where the design allows two

- "Eligible bridge fallback" in the design's §6 hold sentence: the population the sentence before it
  names, not A2's narrowed count (§5.4).
- "Missing policy": no current file, the last file taken still deciding the exempt classes (§4.4).
- `verified`: `not_migrated` on the wire (the design's five reasons), a class of its own in the metrics
  (Y8).

## 10. The brief's items

| Item | Where |
|---|---|
| D1, the signer socket | §2 |
| D2, `POST /v1/media-url` and `directMediaV1` | §3 |
| D3, the metrics endpoint | §6 |
| D4, the policy snapshot | §4 |
| D5, the bridge PR | §7 |
| Y1, the signer authenticates by both | §2.2, §2.3, §2.10 |
| Y2, fallback eligibility | §4.2, §4.4, §5 |
| Y3, the console's byte routes | §5.6 |
| Y4, routing before stat | §3.2 (step 1), §5.2, §5.3 |
| Y5, the error vocabulary | §3.2, §3.3 |
| Y6, the draft stays in the plan | §3.1 |
| Y7, a read-class route | §3.4 |
| Y8, ranged reads of a local variant | §3.5, §5.1 |
| Y9, the console's surface, bounded | §5.6, §7.4 |
