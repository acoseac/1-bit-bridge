---
paths:
  - "internal/auth/**"
  - "internal/adminauth/**"
  - "internal/tls/**"
  - "internal/doctor/certsans*.go"
  - "internal/tlsacme/**"
  - "internal/pairing/**"
  - "internal/pairingcode/**"
  - "internal/authredirect/**"
  - "internal/admin/admin.go"
  - "internal/admin/middleware_auth*.go"
  - "internal/admin/pairing*.go"
  - "internal/admin/handlers_login*.go"
  - "internal/admin/handlers_pairing*.go"
  - "internal/admin/handlers_cert*.go"
  - "internal/admin/handlers_autocert*.go"
  - "internal/admin/handlers_token_lifecycle*.go"
  - "internal/admin/templates/login*.html"
  - "internal/api/api.go"
  - "internal/api/pairing*.go"
  - "internal/api/middleware*.go"
  - "internal/api/ratelimit*.go"
  - "cmd/bridge/cert*.go"
  - "cmd/bridge/token*.go"
  - "internal/advertise/**"
  - "internal/baseurl/**"
---
# Auth, pairing, TLS and security posture

The console's loopback and public postures, CSRF, sessions and login tickets, pairing, bearer tokens, the TLS certificate and its pin, and what a log line or an error body may say.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Auth, pairing, TLS and security posture

- **The admin console is loopback-only with no auth, by design.** Anyone on the
  host already owns the token store and the DB, so auth on top would be theatre.
  Don't add a layer that bypasses the loopback constraint; SSH-tunnel for remote
  admin. Public mode is the separate, credentialed posture.
- **…and "loopback" is a fact about the request's HOST as well as its source,
  so a loopback console refuses a Host that names another host with 421**
  (2026-09-29, backlog B170). `loopbackOnly` and `csrfGuard` judged a request
  by its source and its Origin, never its Host. A page served from a name its
  author controls can re-point that name at 127.0.0.1 (DNS rebinding): the
  operator's browser then sends the page's requests to the console, from
  127.0.0.1, and hands the page the answers, since to the browser they are the
  page's own origin, and a same-origin GET carries no Origin (measured with
  the real binary on main: a request naming another host was answered on
  every read route tried, and only a POST's Origin was refused; the record
  is in the log). `loopbackHostOnly` sits inside
  `loopbackOnly` in `boundaryMiddleware`'s loopback branch and answers 421 to
  any Host that `loopbackHostname` (the Origin allowlist's rule: `localhost`,
  a trailing dot, 127.0.0.0/8, `::1`) does not take, **with any port or none**:
  an `ssh -L 17789:127.0.0.1:7789` tunnel sends `Host: localhost:17789`. **An
  empty Host passes**: every browser sends one, and only an HTTP/1.0 client can
  leave it out. The configured admin host needs no case of its own, since
  `validateLoopbackAddress` admits nothing the rule refuses
  (`TestEveryAdminAddressLoopbackModeTakesIsALoopbackHost`); a config that
  learns another loopback name teaches the rule the same name. A refused name
  is logged once (`noteForeignHost`, at most 16 names), which is how an
  operator's own proxy shows up. **The cost is a reverse proxy that forwards
  the browser's Host**: Caddy's `reverse_proxy` does by default, and so does
  nginx with `proxy_set_header Host $host` (measured in Docker on dido: 421
  for those, 200 with Caddy's `header_up Host {upstream_hostport}` and with
  nginx's default `proxy_pass`), and Traefik by its documentation
  (`passHostHeader` defaults to true; not measured); `docs/docker.md` says
  how. Don't
  accept a forwarding header as the proof instead: a page can set
  `X-Forwarded-For` on a same-origin request. Public mode is untouched (a
  tenant console behind the host's proxy arrives from 127.0.0.1 under the
  tenant's name, and its session cookie is what a rebinding page cannot
  carry). The DLNA listener takes the same rule on its own addresses (under
  **DLNA, UPnP and discovery**), and the loopback track route a stricter
  one: `127.0.0.1`, `[::1]` or `localhost` only, and no forwarded request
  (`output.md`). A console POST through a tunnel on ANOTHER
  local port is still refused by the Origin check, which compares the
  admin port (backlog B193) — **don't widen `originMatchesLoopbackMode` to
  admit it**; the refusal is a typed JSON `cross_origin` body whose message
  names the admin listener's host and port so the console can say what to
  change (backlog B471).
- **`csrfGuard`**: body-bearing mutations must be `application/json`; body
  detection uses `ContentLength != 0 || len(TransferEncoding) > 0` because
  net/http strips the header. **A bodyless POST is deliberately allowed
  through.** The Origin allowlist is reject-if-MISMATCHED, not reject-if-missing
  — failing closed locks out real operators. The only relaxation is
  `application/octet-stream` on PUT (upload); **`multipart/form-data` stays
  refused everywhere**, because it is a CORS *simple type* and therefore
  forgeable cross-origin, while octet-stream + PUT forces a preflight the bridge
  never answers. **A mismatched Origin answers with `writeCrossOriginRefusal`
  (`error: cross_origin`), never a bare `http.Error` status line** — the
  console's `errorFromResponse` reads the JSON, and in loopback mode the
  message names the admin host and port (from the listener via
  `adminListenHostPort` / `adminHintHost`, not the page URL) plus an
  equal-port `ssh -L` example: IPv6 literals as `[::1]`, localhost and IPv4
  as written, empty/unspecified → `127.0.0.1` (B471). The SSE Origin gate
  reuses the same writer.
- **Any long-lived GET needs its own Origin gate** — `csrfGuard` lets GETs
  through, which is right for one-shot reads and wrong for a held SSE
  connection.
- **In public mode `/metrics` is a console page: a signed-in session reads
  it, and without one it answers only a scraper whose address
  `metrics.allowCidrs` lists, over a connection that carries no forwarding
  header** (2026-09-29, backlog B171). Since #472 it sat on
  `isAuthBypassPath` behind `metricsGate`, which admitted any loopback
  source, and a proxy on the bridge's own host relays every request from
  127.0.0.1, so the gate could not tell the proxy's clients from a local
  scraper (measured with the real binary; the record is in the log). The
  bypass is gone, and `metricsGate` with it: `metricsScrapeVouched`, in
  `sessionMiddleware`, is the one exemption, and an unauthenticated
  `/metrics` gets a plain 403 (`errMsgMetricsNeedsSession`), never the
  login redirect, which a scraper follows to HTML it cannot parse.
  **Loopback is not implied, and the forwarding headers are not the fix on
  their own**: a relay that adds none (a TCP relay such as an SNI router,
  nginx's default `proxy_pass`) cannot be told from a local scraper by
  anything in the request, so only the operator can say that nothing on the
  host relays connections to the console, by listing `127.0.0.1/32` (and
  `::1/128`). The header check (`Forwarded`, `X-Forwarded-For`, `-Host`,
  `-Proto`, `X-Real-IP`, `Via`) keeps a listed address honest when a proxy
  that announces itself sits on it too. A link-local source's zone
  (`fe80::1%en0`) is dropped before the match (`remoteIP`): no CIDR names
  one, and `net.ParseIP` refuses it. **The cost**: a same-host
  Prometheus on a public bridge that relied on the implied loopback gets
  403 until its address is listed. The first such refusal from this host or
  a private network logs one Warn per process naming the fix
  (`noteRefusedScrape`); one from a public address (a scanner) or carrying a
  forwarding header (a proxy that announces itself) logs nothing, and a
  relay that adds no header reads as a local scraper there too, so its first
  refusal logs the Warn. Loopback mode is unchanged: its boundary admits this
  host alone, and `metrics.allowCidrs` plays no part there, as it never did
  (the boundary refused a non-loopback source before the old gate ran, so
  #803's list only ever took effect in public mode).
- **Pairing token delivery is read-many**: `Poll` returns the token on every
  authorized poll while Approved, and only a client `DELETE` or TTL+grace
  consumes it. A network blip must be recoverable — **don't add a "clear `RawToken` on first
  read" or a `StateDelivered` terminal**; the pollSecret bearer plus the cert pin
  are the safety surface. The per-request timer
  generation guard is required because `Stop()` returning false doesn't mean the
  callback won't fire; `Approve`/`Decline` check the wall clock themselves; the
  cert-rotation guard fails CLOSED including on an empty current fingerprint;
  `snapshot()` redacts `RawToken` and `PollHash`.
- **`onTimer` transitions the row to Expired UNDER the lock before revoking
  out-of-lock** — otherwise a concurrent poll hands iOS a token the revoke then
  destroys. Revoke-then-delete with bounded retry; `Delete` refuses an
  Expired-with-token row so the revoke lifecycle stays owned by `onTimer`.
- **`adminauth.json` is shared by PROCESSES, so the CREDENTIAL is the file's and
  the SESSIONS are the serving bridge's** (#1039). `bridge admin reset-password`
  beside a running public bridge was silently undone: the bridge kept verifying
  the password it loaded at start, and its next write of the file (a login, the
  30 s activity flush, a logout, the shutdown flush) put the old hash back,
  because `persist()` wrote this process's copy of the whole file. The restart
  the command advised was one such write, and the hosted control plane's
  `bridge-tenant passwd` restarts the tenant straight after the reset. Every
  decision about the credential (a login, a ticket's account at mint and at
  redeem, "is there an account", the reset's username check) and every write now
  re-reads the file (`readStoreFile`). A file that cannot be READ decides
  nothing (Verify refuses: this process's copy is the password rotated away
  from) and is written over by nothing, the change staying pending; a MISSING
  file is no credential, and a session write does not recreate it. **The file's,
  never "the newer passwordChangedAt"**: every credential write is synchronous
  and adopted only once its rename lands, so memory is never ahead of the file,
  and a timestamp would keep memory over a restored backup. A session write puts
  down the set in memory; a credential write that keeps the sessions puts down
  the set it finds in the file AT THE WRITE, never the one read at open, since
  reset-password waits at a prompt in between. **One re-read is not enough**:
  staging costs a write and an fsync (milliseconds, tens on a cloud disk), so
  EVERY write goes through
  `commitLocked`, which re-reads the file just before its rename and rebuilds
  from a fresh read if it changed at all, byte for byte. Comparing the
  credential alone covered the running bridge's writes and missed
  reset-password's, whose rename dropped a login or brought back a logout
  committed while it staged (CodeRabbit on #1039). The rename itself (on
  Windows, with its retries) is what remains. A kernel lock (flock / LockFileEx)
  would close that, and the "stale lockfile" reason this file gives for
  declining interprocess locks is about lockFILES: the kernel drops those locks
  with the process. **A rotation ends the sessions by default, and a restart
  does not** (they persist, #800): the next bullet. reset-password said a
  restart ended them until 2026-09-27. **`tokens.json` had the same window** (a
  `bridge pair` beside a running bridge) and closes it the same way, #1043,
  under Config, settings and process lifecycle. A new file that more than one
  process writes needs the re-read before its rename from the start, or better,
  a layout in which no process rewrites a record another wrote: the login
  tickets had the same window and no re-read, and are one file per ticket since
  2026-09-28 (two bullets down).
- **A sign-out reaches the running bridge as a MARKER in `adminauth.json`,
  never as an emptied session set** (2026-09-27). The running bridge holds the
  sessions in memory and writes them back (a login, the 30 s activity flush, a
  logout, the shutdown flush), so a file that merely lost them ends nothing.
  Until this change nothing could end a session another browser held: a
  rotation kept them (a test pinned that as operator-friendly), a restart
  reloaded them, and a console signed in with a leaked password outlived the
  rotation for up to the 7-day cap, while 1-bit.app's troubleshooting page said
  a reset invalidated sessions immediately. Now `bridge admin reset-password`
  signs every console out by default (`--keep-sessions` keeps them), `bridge
  admin sign-out-everywhere` does it with the password kept, and the Devices
  page's "Sign out all other sessions" (public mode) keeps the browser that
  asked; that one runs in the process holding the sessions and needs no
  marker, and a write of it that fails is REPORTED (`saved: false`, not a
  failure: the sessions are refused already, and a restart before the next
  write would sign them back in). A process ending sessions it does not hold moves `sessionsRevokedAt`
  in the same CAS write, and **every writer carries the file's marker over**,
  the `--keep-sessions` rotation included. **The marker is an EVENT, never a
  filter on `IssuedAt`**: a store that reads a marker it has not taken ends
  every session it holds, BEFORE it builds anything it writes
  (`persistSessionsLocked` builds its set inside the commit closure for exactly
  this), and keeps every session made after that read whatever the clocks say.
  A filter would end every login after a clock stepped back until the clock
  passed the marker; and a session held at the read predates the sign-out even
  with a later `IssuedAt`, since its login read the file before the marker
  landed. `load()` takes the file's marker as already taken, or every start
  after a sign-out signs its own sessions out at its first read. **Every
  sign-out MOVES the marker** (`nextSignOut`, one nanosecond past the last when
  the clock does not put now after it): two sign-outs inside one tick of a
  coarse clock, or across a clock stepped back, otherwise end nothing on a
  bridge that took the first. **A session check reads the file whenever a
  stat says it changed** (`refreshIfChangedLocked`: size, mtime AND
  `os.SameFile`, since every write here is a rename and so a new inode whatever
  its size; a commit records its own file's stamp so the bridge never reads
  back its own write), so a signed-out console is refused at its NEXT REQUEST,
  not at the bridge's next write. Measured: 2 µs, against a 22 µs read at one
  session and 1.75 ms at the 1,024 cap. **A check that cannot read the file
  REFUSES** (`ErrStoreUnreadable`, a 503 from the middleware), keeps the
  session, records no stamp (a chmod or chown that fixes the file changes none
  of the three), and logs at most once a `sessionFlushInterval`: the middleware
  adds no line of its own, #1039's debounce concern. **The opposite of
  `tokens.json`'s choice (#1047), deliberately**: refusing there unpairs every
  device over a permissions mistake, while here logins already refuse in that
  state (#1039) and the unread file may hold a sign-out; don't make either
  match the other. A MISSING file still ends
  nothing. **A rotation or sign-out CONFIRMS its write** (`commitAndConfirm`,
  CodeRabbit on #1044): a running bridge's write that passed its own check
  just before the command's rename lands after it, carrying back exactly the
  credential and marker the command replaced (within `RenameWithRetry`'s
  750 ms budget on Windows, where a scanner forces the retries). The command
  re-reads after that budget and a margin (about a second), and commits
  again only when the file is back to exactly what it replaced: a newer
  write carries something else, and the naive "not what I wrote" rule makes
  two concurrent rotations undo each other until they give up. Only a writer
  stalled past the settle between its check and its rename gets through; a
  kernel lock would close that, and #1039 declined one. Open: a bridge still
  running the OLD binary drops the unknown field and writes its sessions
  back, so restart first, then sign out. A `sudo bridge admin
  reset-password` left a root-owned file the service could not read, which
  refused every console request until it was chowned; since #1048 the
  write keeps the file's owner (the KeepOwner bullet under Config, settings
  and process lifecycle).
- **A console login ticket is PERSISTED, because the two halves are different
  PROCESSES, and each ticket is a FILE OF ITS OWN, because both processes write
  tickets** (2026-09-28). `bridge admin login-link` mints and the serving bridge
  redeems, so an in-memory map is invisible to the redeemer and the feature
  never works — it shipped that way, and every unit test passed because each
  minted and redeemed inside one process. Until 2026-09-28 every live ticket
  shared `adminauth-tickets.json`, which each process rewrote whole from its own
  earlier read, and `Store.mu` reaches neither from the other: a mint whose read
  predated a redemption renamed the SPENT ticket back into place (redeemable
  again for up to `MaxLoginTicketTTL`), a redemption whose read predated a mint
  dropped the new ticket, and of two overlapping mints one was lost (all three
  red against the old code with the test hooks at the same points:
  `TestAMintCannotRestoreATicketSpentDuringItsWrite`,
  `TestARedemptionCannotDropATicketMintedDuringIt`,
  `TestTwoInterleavedMintsBothLand`). **Not a re-read before the rename**, the
  cure #1039 and #1043 gave `adminauth.json` and `tokens.json`: it narrows the
  window and cannot close it, since on Windows the rename itself retries for up
  to 750 ms. Now each ticket is `adminauth-ticket-<sha256 hex>.json`, 0600,
  beside the store: a mint creates its own file (staged, `KeepOwner`, synced,
  renamed) and a redemption removes its own, so NO process rewrites a ticket it
  did not mint, and each step is atomic alone. The disk still holds only the
  hash, as the NAME, so it gains no credential it did not already hold, and
  single use plus the lifetime still come from the removal on presentation,
  BEFORE judging expiry and account, which also decides between two
  redemptions: one removal wins. **A redemption touches its own file and
  nothing else**, in the order read, credential, remove, judge: a miss opens
  one name that is not there and WRITES NOTHING (the unauthenticated branch,
  under the mutex every console request takes), and expired or damaged files
  are pruned by a MINT, an operator's act. **A ticket whose file cannot be
  read is a 500 (`ticket_store_unavailable`), never the stale-link page**, and
  stays unspent: the shared-file reader turned every read error into "no
  tickets", against the handler's own docblock. **`ticketAbsent` is the one
  "not there" rule** for a read and a removal alike: ENOENT everywhere, and on
  Windows `ERROR_ACCESS_DENIED` too, which is what every open of a file with a
  pending delete answers (a prune racing an antivirus scanner's handle, the
  window `RenameWithRetry` exists for); only a spent or expired ticket is ever
  removed, so that file was on its way out. The cost: on Windows a genuine ACL
  fault on one ticket file reads as a stale link, where POSIX answers 500. It
  takes the GOOS as a parameter so its table runs on every CI leg. A mint at
  `maxLiveTickets` (32) is REFUSED and evicts nothing (every file is a link
  somebody may hold), counts only REGULAR files (a directory named like a
  ticket failed its read, counted as possibly live, and 32 of them refused
  every mint), and the first mint removes the old shared file. Cost of
  the layout change: links minted by the old binary in the ten minutes before
  an upgrade stop working, and a rollback loses the new binary's the same way.
  No interprocess lock, and none is needed now: nothing is left that two
  processes both rewrite. `SameSite=Strict` is NOT the usual magic-link trap
  here — the app opens the URL itself, and a navigation with no initiator is
  same-site (verified in a real browser, not reasoned about).
- **A login link is REDEEMED ON THE POST of its interstitial, never on the GET
  — a link preview is a navigation-shaped GET no header guard can tell from the
  click** (2026-09-12, the hosted uploader link: every link the phone shared
  landed on `/login?link=stale`). The HEAD-405 and the declared-prefetch-403
  only cover probers that SAY what they are; iOS's share sheet, Messages on
  both ends, Slack and mail clients draw their preview by loading the URL
  through a real browser engine, which declares `Sec-Fetch-Mode: navigate`
  exactly like the human's click that follows — so redeem-on-GET spent the
  ticket while the share sheet was still opening. `GET /login/ticket` now
  renders `login_ticket.html`, a one-button page with NO script (a previewer
  runs scripts; it does not click), and `POST /login/ticket` — an EMPTY body
  with the ticket in the action's query, the one bodiless shape `csrfGuard`
  admits without a Content-Type check — is what redeems. **The GET consults no
  store**: a ticket that never existed gets the same page as a live one, so
  it is neither the "is this link live" oracle the miss branch used to be nor
  a file read under the console's mutex; a bare `?t=` is sent to
  `/login?link=stale` because a button that can only fail is not a page. The
  URL shape is unchanged, so `bridge admin login-link` and the control
  plane's `console-link` need nothing — the operator's link just gained a
  Continue. Negative control: putting the redeem back on the GET turns exactly
  eight of the fourteen ticket tests red (`TestOpeningTheLinkDoesNotSpendIt`
  drives three navigation-shaped previews before the click).
- **The interstitial is served under `Referrer-Policy: strict-origin`, NEVER
  `no-referrer` — a form POST from a `no-referrer` page carries
  `Origin: null`, and the CSRF guard refuses the Continue button** (2026-09-12,
  the second field report on the uploader link, one build after #909: Continue
  answered with the 36-byte `admin refused: cross-origin request`, which Safari
  on iOS saved as `ticket.txt`; macOS the same; reproduced with curl against the
  live tenant — `Origin: null` gets that body, the page's real origin redeems).
  The Fetch standard's "append a request `Origin` header" serializes a non-GET,
  non-`cors` request's origin as `null` under `no-referrer` — a plain form
  navigation is exactly that shape. The console's own requests never show it
  because `fetch()` runs in `cors` mode, which the rule exempts, and the login
  form serves `same-origin`; **no test saw it because `redeemTicket` sent no
  `Origin` at all and `csrfGuard` checks the header only when present**, while
  the leak test pinned the very `no-referrer` that broke the button. Header AND
  `<meta name="referrer">` must agree (the meta is parsed later and is what the
  document keeps) — `loginTicketReferrerPolicy` is the one constant.
  `strict-origin` keeps what `no-referrer` was chosen for and more: the Referer
  carries the origin alone, never the address holding the ticket, same-origin
  subresources included, and it nulls the Origin only on an https→http
  downgrade. `TestLoginTicketRedeemsUnderTheOriginABrowserSends` reads the
  policy the GET serves, derives the Origin the spec says a browser sends
  under it, and submits that — a regression is red by construction. **The
  transferable rule: a test that exercises a form POST must send the `Origin`
  a browser would derive from the served policy, never omit it.** Negative
  control: `no-referrer` on both header and meta turns exactly the two policy
  tests red.
- **`POST /v1/pairing/requests` HAS a per-IP token bucket (burst 5, one per
  5 s, since #133) beside the bridge-wide pending cap.** This bullet said
  the opposite for over a year — "no per-IP rate cap, double-NAT puts every
  LAN device behind one address" — while PROTOCOL.md documented the 429 and
  `pairing.go` enforced it, and `TestPairingCreateQueueFull`'s docblock
  carried the same false claim. The BURST is what makes double-NAT
  survivable: a fumbling re-tap never reaches it, and a script is bounded
  below the 16-pending queue it would otherwise fill alone. Don't remove
  the limiter to match the old prose. The 6-digit code is drawn from
  `crypto/rand`.
- **A pairing link carries a one-time `code`, and the device keeps the token
  the code redeems for, never the link's** (#1052, the 2026-09-23 audit's
  H1). The console's QR and deep link carried the device's long-lived bearer
  token, so anything that saw the URL (a screenshot, a clipboard manager, a
  link preview) held the credential until someone revoked it. Every shipped
  app refuses a link without `token=`, so the link keeps it and gains
  `code=` (`internal/pairingcode`: 32 random bytes, single-use, 10 minutes,
  one live code per token, held as a SHA-256; `Issue` drops the token's old
  code BEFORE drawing the new one, so a failed issue after a console
  rotation still ends the old QR's code, which would otherwise rotate the
  token again for whoever holds that QR). `POST /v1/pairing/redeem`
  trades the code by ROTATING the token it names: a fresh secret for the
  same record, with the link's token dead in the same commit. So a copy of
  the link is dead once the real device has paired, and a copy redeemed
  first makes the real device's redemption fail where the user sees it.
  **One store, in the serving process**: the console issues
  (`admin.Deps.PairingCodes`) and the v1 API redeems, both in `bridge
  serve`, which is what lets the codes live in memory (a restart costs a
  fresh QR). `bridge pair`, another process, issues none, because a code
  it minted could never be redeemed. No package test can see that wiring:
  `TestServeRedeemsThePairingLinksCode` boots the real serve, and both
  halves' negative controls turn only it red. **Take before judging**, the
  login ticket's rule: the code is deleted before its age is read.
  **Every refusal is the same 410** (unknown, used, expired, token revoked
  or expired), and the route shares `POST /v1/pairing/requests`' per-IP
  limiter. `Rotate` keeps `ExpiresAt`, so an expired token is refused
  rather than handed over to 401 on its first request. **A client that
  understands `code` never falls back to the link's token on a refusal**:
  a copy redeemed first has killed it already, and a device paired with it
  would keep the secret the exchange exists to replace. **A code is bound to
  the token's hash at issue and redeems only while the token still carries
  it** (backlog B160, 2026-10-15). `bridge token rotate` is another process
  and cannot drop the serving process's code the way a console rotation
  does, and redeeming the old QR's code after it rotated the token AGAIN:
  the QR's holder (the leak the operator rotated for) got a working secret,
  and the one the CLI printed stopped validating. The console passes the
  record its Mint or Rotate returned (`pairingCodeFor(tok)`), `Take` hands
  the hash back, and the redemption rotates through
  `auth.Store.RotateIfHash`, which judges the token inside the commit (the
  build, run again on a fresh read when a sibling commits first); its
  refusal is the same 410. **Don't move the check before the commit**: a
  `Get` then `Rotate` lets a sibling's rotation land in the commit window
  (`TestRotateIfHashSeesARotationThatLandsInItsCommitWindow`). **Don't bind
  a code to anything a write changes without a rotation** (`LastUsedAt`, an
  expiry): those leave the secret, and the code, as they were
  (`TestPairingRedeemSurvivesChangesThatKeepTheSecret`).
- **The login ticket is refused by SHAPE before anything else looks at it**
  (base64url, at most 64 bytes; a minted one is 43) — on the GET so an
  unbounded query is never echoed into the page, on the POST so it never
  reaches the hash or the file read under the console's mutex. The refusal
  is the same 302 every unusable ticket gets. The interstitial's referrer
  META is injected from `loginTicketReferrerPolicy`, never typed — a
  literal is how header and meta both came to say `no-referrer`. A failed
  session mint after a successful redeem is a STALE link: the ticket is
  spent by then. **`url.QueryEscape` before templating was proposed and is
  wrong**: html/template already URL-escapes that attribute context, so it
  would double-encode; bound the SHAPE instead. (#918)
- **`AllowAndReserve` callers must NOT also call `RecordFailure`** — the
  reservation IS the failure count. Check-then-act across two lock acquisitions
  let concurrent logins all pass the ceiling.
- **The TLS cert is sticky and rotation is warn-only.** iOS pins the SHA-256 at
  pairing, so auto-rotation silently breaks every paired device; `certDuration`
  stays ≤397 days (Apple ATS rejects at handshake, before pinning runs).
- **A warning that only fires at SERVE time arrives after the damage.** The
  SAN-staleness check ran once inside `LoadOrGenerateWithOptions` and nowhere
  else, so a data directory carried to another host reported `[ok] tls-cert
  present` in `bridge doctor` and only said `cert SANs are stale` once the
  bridge was up — by which point devices have pinned a cert that fails TLS for
  every Tailscale and custom-endpoint URL, and the fix costs a re-pair of each.
  `bridge doctor`'s `tls-cert-sans` runs the SAME comparison
  (`servertls.InspectSANCoverage`) against the SAME want-set BEFORE the first
  start. **That want-set is `cmd/bridge`'s `certSANOptions`, the one gather all
  four cert paths use** — serve, `init`, `cert rotate`, doctor; they were four
  copies of three lines, and the copy that drifts is the one that calls a cert
  fine when a rotation would mint something different
  (`TestCertSANOptionsIsWhatEveryCertPathMints` pins it structurally). The
  doctor's cert checks read `cfg.TLSCertPath` too, via `doctor.certPaths` —
  resolved from `DataDir` alone they graded a cert nobody serves. Expiry is on
  the `tls-cert` line against the exported `servertls.ExpiryWarningWindow`, the
  startup warning's own threshold — **compared as an exact duration, never
  `DaysUntilExpiry`**, which truncates toward zero, so a day count would warn at
  30d23h while `logIfExpiringSoon` stays quiet. The fail/warn split is **"can
  `bridge serve` start"**: a pair it cannot LOAD fails (partial, unparseable, or
  mismatched — `VerifyKeyPair` is `LoadX509KeyPair`, serve's own call, and an
  interrupted two-rename rotate leaves a new cert with the old key, which
  `Inspect` grades as a clean 396 days); an expiring, expired or NOT-YET-VALID
  cert loads, so it warns about the clients. **The validity window has a near
  end** — `LoadX509KeyPair` ignores dates, and the mint allows one hour of skew
  (`NotBefore: now-1h`), so a host whose clock was further ahead at mint time (a
  NUC or Pi with no RTC, pre-NTP) leaves a future `NotBefore` that reads as a
  comfortable year of life left; `logIfExpiringSoon` carries the same arm, and
  the hint says CHECK THE CLOCK FIRST because rotating against a wrong clock
  mints another bad cert. **Every surface that grades the window grades BOTH
  ends** — the console tile, `bridge cert info` (human AND `--json`, where the
  verdict is `notYetValid` beside `expired`/`expiringSoon`) and `bridge cert
  rotate`'s preamble all stayed on `DaysUntilExpiry` and read a cert starting
  in 30 days as `(426 days)`, unbadged. The prose is
  ONE const, `servertls.NotYetValidRemediation`, pinned by a test on all three;
  it is deliberately NOT `RotationRemediation` with a lead-in, because this is
  the one band where rotating FIRST mints a second wrong cert — which is why
  `bridge cert rotate`'s preamble carries it and carries NO other band (an
  expiring cert is why the operator is there). The startup log keeps its own
  shorter wording on purpose (structured, sized like its expiry sibling) and
  the console keeps a badge plus a date, because rotating from a browser is
  exactly what this state must not do. **A permission failure reading the 0600 key is NOT
  a finding** — on the public-mode layout the operator is not the service user,
  and that is a fact about the doctor run, the same reason `Validate()` does not
  stat the roots. **Every cert READER skips to the first CERTIFICATE block**
  (`decodeCertificatePEM`, shared by `Inspect`, `fingerprintFromPEM` and
  `InspectSANCoverage`) because `LoadX509KeyPair` does: a key-first or
  `Bag Attributes` PEM loads at serve time and read as "unreadable" on both
  doctor lines. `tls-cert-sans` skips on a managed
  bridge (the control plane owns rotation and the reader has no shell);
  **expiry does not** — `TestManagedReportsExpiryButNotStaleSANs`
  pins the split, which was prose in a docblock and nothing else. `--fix` does
  NOT rotate: that invalidates every pin and is an operator decision with a
  device in hand.
  **Rotation is advised only for a certificate this bridge minted**
  (2026-10-13, backlog B232). `MintedByBridge` is true when the
  certificate's own public key verifies its signature
  (`Certificate.CheckSignature` on `RawTBSCertificate`).
  `GenerateWithOptions` passes the same template as parent and leaf to
  `CreateCertificate`, so a pair minted here verifies that way. A CA leaf
  does not. `CheckSignatureFrom` rejects a version-3 certificate with
  `BasicConstraintsValid` and `IsCA` false, and the mint sets both, so
  that call fails on every certificate this bridge creates.
  `LoadOrGenerateWithOptions` only knows the files existed: a wildcard a
  control plane placed and a previous mint both exist. Managed is a
  posture, and the doctor's managed skip stays because the reader has no
  shell; it is not the startup signal, because a managed bridge can still
  hold a stale pair this process minted. `logIfSANsStale` and the
  unmanaged doctor path both stay quiet when `MintedByBridge` is false.
  DNS coverage honors one leftmost label (RFC 6125 §6.4.3):
  `*.cloud.1-bit.app` covers `abc.cloud.1-bit.app`, and it covers neither
  the apex, nor `a.b.cloud.1-bit.app`, nor a partial label (`a*.example`).
  The want-set always adds `localhost` and the host name, which that
  wildcard does not cover, so the minted check is what silences the
  warning. A stale self-signed pair after a host move or a new custom
  endpoint still warns on both surfaces.
  (`TestAWildcardCertTheBridgeDidNotMintLogsNoRotationWarning`,
  `TestAStaleSelfSignedCertStillLogsTheRotationWarning`,
  `TestALeftmostWildcardCoversOneLabel`,
  `TestAPartialLabelWildcardCoversNothing`,
  `TestACertTheBridgeDidNotMintIsNotToldToRotate`.)
- **…and an address serve holds back is not stale, and a loopback bridge
  that advertises no LAN IPv4 address is warned** (2026-10-18, backlog
  B457; option B, the owner's decision). Serve does not advertise an IPv6
  address of the interface walk that a certificate it minted does not
  name (the bullet under **The wire contract**), so `certSANOptions`
  marks the walk's IPv6 addresses (`GenerateOptions.CertGatedIPs`, from
  `advertise.CertGatedIPs`: not Tailscale's, not one a `customEndpoints`
  entry names, never an IPv4 one), and `InspectSANCoverage` moves one a
  minted certificate lacks from `MissingIPs` to `HeldBackIPs`. `Covered`
  ignores those, so `logIfSANsStale` stays quiet for them, and
  `tls-cert-sans` stays ok, counts only the addresses advertised, and
  names each held-back one with `RotationRemediation`. An IPv4 address
  the certificate lacks is advertised, so it is missing and warns, as a
  missing Tailscale, custom or host-name entry does. A CA leaf holds
  nothing back. **The mint still covers them**: `ExtraIPs` keeps every
  walk address, which is what lets a rotation advertise one again.
  **`tls-cert-sans` also warns when a loopback bridge advertises no LAN
  IPv4 address at all**: no private IPv4 address from its interfaces or
  `customEndpoints` that it does not hold back. A phone on a VPN route
  cannot use the `.local` name, and the app has no IPv4 address to hand
  a DLNA renderer. The warning keeps everything the comparison said, a
  held-back address included, and does not wait for a certificate.
  `doctor.Deps.PublicMode` skips it for a public-mode bridge, which
  advertises no walk; `bridge doctor` and `bridge init`'s preflight set
  it from `cfg.IsPublic()`, as they set `Managed`. **Don't ask a
  public-mode bridge for a LAN IPv4 address, and don't set the posture in
  one builder of the doctor's Deps without the other.**
  (`TestAGatedAddressAMintedCertDoesNotNameIsHeldBackNotMissing`,
  `TestACertTheBridgeDidNotMintHoldsNothingBack`,
  `TestNamedIPsReadsTheServedCertificate`,
  `TestCheckTLSCertSANs_AHeldBackAddressIsNotStale`,
  `TestCheckTLSCertSANs_NoLANIPv4AdvertisedWarns`,
  `TestCheckTLSCertSANs_APublicBridgeIsNotAskedForALANIPv4`,
  `TestTheDoctorAsksOnlyALoopbackBridgeForALANIPv4`.)
- **The pairing QR carries an address a phone can dial, and a name that
  left the want-set stops the stale-SAN warning** (2026-10-13, backlog
  B189, B199, B229). `certSANOptions` takes `advertise.CertHostname`.
  `InspectSANCoverage` reports names in the want-set the certificate
  lacks, so a container id that left the want-set is no longer missing:
  the startup warning and `tls-cert-sans` go quiet for it. A certificate
  that still lacks a custom host still warns. `MintedByBridge` stays in
  the gate. `localhost` stays on the certificate. In loopback mode
  `loopbackHostOnly` accepts only a loopback Host, so the pairing
  handler never sees the published-port host a browser used, and that
  Host is not written into the QR. When the hostname is the generated
  container id and `customEndpoints` is empty, `defaultBridgeURL`
  returns empty. The devices page, a refused mint, a refused rotate, and
  `bridge doctor`'s `pairing-url` line share
  `advertise.PairingNeedsEndpoint`. The first custom endpoint whose host
  is not loopback becomes the primary. A failed hostname read on a host
  that is not a detected container still uses localhost, the
  same-machine path. A phone already paired to the old container-id URL
  keeps that saved primary until the share is edited or the phone is
  paired again.
  (`TestCertHostnameOmitsAGeneratedContainerName`,
  `TestAWantSetThatDroppedAContainerNameIsCovered`,
  `TestAMissingCustomHostStillWarnsAfterTheContainerNameLeft`,
  `TestDefaultBridgeURLDoesNotBakeAGeneratedContainerName`,
  `TestAContainerMintRefusesAnUndialableURL`,
  `TestTheDevicesPageSaysToSetCustomEndpoints`,
  `TestPairingURLWarnsWhenTheContainerHasNoDialAddress`,
  `TestAContainerHostnameLeavesTheWantSetAndTheDoctor`.)
- **The pairing QR advertises the SERVED cert**, resolved by SNI —
  `FingerprintForServerName` mirrors `Get`'s routing rather than delegating, so
  **every freshness and validity gate in `Get` must be restated there**; losing
  one made the QR advertise a fingerprint the listener never presents.
- **The pairing QR's `urls=` and the console's "Reachable endpoints" panel
  read `admin.Deps.Endpoints` — `api.(*Server).ReachableEndpoints`, the ONE
  enumeration `/v1/health` serves — never a second `advertise.Endpoints`
  walk.** PR #269 moved the Tailscale append out of the advertise package
  into the api layer and left both admin consumers on the old walk, so from
  May to September the QR baked no Tailscale fallback (while `buildPairURL`'s
  docblock promised one) and dropped `customEndpoints`, and the panel could
  never show the row its own prose said to look for — a loopback cli-mode
  bridge whose health advertised `.ts.net` + `100.x` + `fd7a:…` paired the
  phone with `.local` and the LAN IP alone. **Nil is NO list, not the old
  walk**: on a host without Tailscale that walk is byte-identical to health,
  which is how the first boot control stayed green with the wiring line
  deleted; the QR falls back to the primary (which always pairs) and the
  panel to its empty state. `TestServeBakesHealthEndpointsIntoThePairingQR`
  boots the real `serve` and requires `POST /api/tokens` to bake exactly
  `[primary] + /v1/health.endpoints`. Public mode keeps its own explicit-
  `:443` synthesis in `pairAlternates` on purpose (the iOS 7788-default bug)
  and never consults the provider. (#936)
- **`atlas.harvestBaseUrl` pins the host a credential POST may set** — the body
  carries the base URL, so whoever sets it chooses where bios come from, and
  those render as an attacker-chosen "Read more on …" link. A pin binds in every
  mode; unpinned is refused in demo and still allowed off-demo. Both sides of
  the comparison must go through `baseurl.CanonicalHTTPS` (the wire side
  through `baseurl.CredentialBase`, which the harvest state store holds too),
  or a one-sided reduction turns a correct pin into a mismatch that fails
  closed. The reduction must be a fixed point, since the pin is reduced twice
  (backlog B97, under **Enrichment**).
- **`fs.Resolve`'s final containment check is the PRIMARY defense on Windows**,
  not belt-and-braces: both guards above it are slash-based, so a backslash
  traversal passes them untouched and only `filepath.Join` + the prefix check
  catches it.
- **HSTS is public-mode + TLS only** — pinning it for `localhost` poisons that
  hostname in the operator's browser for every other local service.
- **A log line names a library file library-relative, and the privacy page
  says exactly where the code does otherwise** (#1055, the v0.2.1 logging
  audit). A file-API failure on a library file logs through
  `writeFileErrorLog` (the client's library-relative path, and the error
  without the `*os.PathError`'s absolute path), and an extractor names its
  file with `trackLogPath` (the track's library-relative path, or the base
  name). The audit found the page promising more than the code did, mostly
  from before v0.2.0; the leaks were fixed and the rest (the startup banner's
  roots, the older scanner and extractor error lines, rendition and waveform
  paths when adopted or removed, the bridge's own files in fault lines, info
  lines naming a track) is now what the page describes. **A new log line
  that names a library file uses the relative path; one that cannot is a
  page change in the same release.** The 0.2.2 audit (2026-10-30, B604, of
  v0.2.1..e242ce2c) found the same leak by another road, and peers: an
  error beside a relative path that names the absolute one (the MP4 walks'
  `*fs.PathError`, scanner.md; one reading of such an error for every
  package, `fsutil.ErrorWithoutPath`), an operator's directory in a disk
  probe's error and a no-room refusal (jobs.md, B273's bullet), the config
  file in a console 500 (admin-console.md), the peer in the DLNA listener's
  refusal and both socket ends in a strict SSDP reply's (dlna-upnp.md), and
  a client in the loopback route's panic line (output.md). Its second round
  (2026-10-31, Codex on the integrated branch) found the same client in the
  MediaServer's and the console's plain-HTTP panic lines (dlna-upnp.md,
  admin-console.md; every `http.Server` in the module takes its `ErrorLog`
  from handshakelog now, `TestEveryServeHTTPServerRedactsPeerAddresses`), and
  a path in the console's other error bodies: the backups, the certificate
  tile, a root add or remove, the token writes and the compaction
  (admin-console.md). Peers the rules let a line name stay: the
  ContentDirectory's `remoteAddr`, OpenHome's refused source (openhome.md),
  the SSDP Debug lines and the GENA and discovery refusals by source
  (dlna-upnp.md), and the console's failed and rate-limited logins (the next
  bullet). **A disk probe's line names the directory it probes by its role,
  and a console error body names no path: the log keeps the error whole,
  the bridge's own files in fault lines, and a success body may show a path
  the console was configured with.**
- **The console's failed and rate-limited login lines name the source
  address, at Warn: a security log, and the console's only lines that name
  a client** (2026-10-31, B604's second round, which kept them rather than
  redact them; Codex on the integrated branch asked for either). `apiLogin`
  logs `ip`, `adminauth.ExtractClientIP`'s answer, which is the limiter's
  own key: the proxy's `X-Real-IP`, or the right-most valid
  `X-Forwarded-For`, where `deployment.adminTLSTerminatedByProxy` says a
  proxy terminates TLS, and the connection's host otherwise, canonical
  (`canonicalIP`) and without a port, so a header's own spelling never
  reaches the line; beside it the username, at most 64 bytes
  (`maxLoginUsernameLen`), and never the password. The console has a login
  only in public mode, where that address is what an operator or fail2ban
  blocks. **Don't log the connection's address and port, a forwarded
  header's text or the password there, and don't add a console line that
  names a client without a bullet here.**
  `TestAConsoleLoginRefusalLogsItsSourceAndNothingElseOfTheRequest` pins it.
- **A 5xx body carries a fixed message, never an error's text** (2026-10-16,
  backlog B425). `GET /v1/search` answered a store or FTS failure with the
  error as its message (`search tracks_fts (served): sql: database is
  closed`, an FTS syntax fragment, a database path) and logged nothing. It
  goes through `writeErrorLog` now: the error is logged at Error with the
  500, and the body says "the bridge couldn't search the library". **A 5xx
  with a cause goes through `writeErrorLog` or `writeFileErrorLog`, or logs
  the error itself and sends a fixed message**; a 4xx may say what the
  client can fix. The upscale folder walk's two 500s still carry
  `redactWalkErr`'s text (an `*os.PathError`'s `op: errno`, its path
  dropped), as #185 chose (#1076 copied it for the root's walk), and were
  left. **Both log through `writeFileErrorLog`** (2026-10-16, backlog
  B435): the error redacted the same way, with the folder the client
  named. The root's walk logged nothing, and the other only the walk's own
  line for an error its callback saw
  (`TestAFolderWalkThatCannotStartLogsItsError`,
  `TestAFolderWalkThatFailsPartWayLogsItsError`; the root's case needs
  `walkableRootForTest`, since no fixture makes `WalkableRoot` fail once
  the resolve has stat'ed the root). The keystroke path's return on a
  cancelled request is B364's and is unchanged
  (`TestASearchStoreFailureAnswersAFixedMessage`,
  `TestASearchStoreFailureThroughTheRouterSendsAFixedMessage`).
- **A search that runs past its route's own 5 s deadline answers that 500,
  never an empty 200** (2026-10-16, backlog B434). The keystroke path's
  early return took `context.DeadlineExceeded` as well as a cancel, and the
  only deadline on the request is the route's `withCtxTimeout`, so a search
  still running at 5 s wrote nothing: net/http sent `200` with an empty
  body, which a client reads as no results, and the log had only an access
  line at Info with status 0. **Only the client's own abort returns without
  writing** (`clientWentAway`: a cancel whose request context is the one
  cancelled): a deadline is the server's (B364's rule) and takes
  `writeErrorLog`, at Error with the 500 and the fixed message, and so does
  a cancel from inside the search while the request is still live (the
  review on #1270, `TestASearchCancelWhileTheRequestIsLiveAnswers500`). A client abort is still that
  silent return, and its access line is still Info at status 0, since
  nothing is written; it never reaches `writeErrorLog`, so it is not
  B364's 499 (`TestASearchThatOutlivesItsDeadlineAnswers500`, which waits
  out the real 5 s, `TestASearchPastItsDeadlineThroughTheRealStoreAnswers500`,
  `TestAClientAbortDuringSearchStaysInfo`).
- **CodeQL's `go/log-injection` is a false positive BY CONSTRUCTION and will
  regenerate.** Both slog handlers quote the value and escape `\n`/`\r`, every
  flagged site passes a structured attribute, and `internal/` contains no
  `log.Printf` at all. Verify empirically in ten lines rather than from memory;
  dismiss on those grounds rather than re-deriving them. Read the dismiss
  comment before re-opening any alert in this repo.
