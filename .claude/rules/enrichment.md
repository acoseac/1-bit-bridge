---
paths:
  - "internal/enrich/**"
  - "internal/acoustid/**"
  - "internal/atlasharvest/**"
  - "internal/baseurl/**"
  - "internal/authredirect/**"
  - "internal/lrucache/**"
  - "internal/dupes/**"
  - "internal/manifest/dupe_*.go"
  - "internal/manifest/scanner_dupes*.go"
  - "internal/manifest/acoustid_store*.go"
  - "internal/manifest/atlas_meta*.go"
  - "internal/manifest/artwork_*.go"
  - "internal/manifest/booklets*.go"
  - "internal/manifest/library_meta*.go"
  - "cmd/bridge/enrichment*.go"
  - "cmd/bridge/fingerprint*.go"
  - "cmd/bridge/duplicates*.go"
  - "internal/admin/handlers_duplicates*.go"
  - "internal/admin/handlers_enrichment_misses*.go"
  - "internal/api/atlas_*.go"
  - "internal/api/artwork*.go"
  - "internal/api/booklet*.go"
  - "internal/manifest/store.go"
---
# Enrichment, artwork, fingerprinting and duplicates

MusicBrainz, Cover Art Archive, Deezer and iTunes, the Atlas harvest and its credential, artwork caching, AcoustID fingerprinting, and the duplicates partition and the served set.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Enrichment — MusicBrainz, Atlas, artwork, fingerprinting

- **Relaxations belong in the QUERY; strictness belongs in the ACCEPTANCE.**
  Every dangerous idea in this area is dangerous because it sits in the wrong
  bucket. Stripping a leading "The" as a *fold rule* silently accepts any
  `The X`/`X` pair anywhere; as a *query rung* it issues a fresh request and
  still demands the result fold-equal.
- **`internal/enrich/matchfold.go` is COMPARISON-TIME ONLY** and must never be
  persisted, hashed into a filename, used as a cache key, or put in SQL. **Do
  not unify it with its three siblings** — `unicodeLowerScalar` backs functional
  indexes (changing it needs a migration), `ArtistImagePathByName` hashes into a
  FILENAME (unifying orphans every cached portrait), and `normTitle` is
  deliberately weak because its pass REWRITES TAGS. `internal/dupes` and
  `librarycat`'s genre fold are the fourth and fifth members of that family.
- **Never split a head credit on `&`, a bare comma, ` with ` or ` vs `** — all
  appear inside real artist names. `pickBestArtist` validates against the query
  that was SENT, not the original tag, and `foldName` erases commas, so
  `Peter, Paul` matches an unrelated `Peter Paul` at 100 and 186 tracks get the
  wrong MBID. The acceptance layer cannot catch that even in principle; **never
  generating the query is the only defence.**
- **Classify errors before caching or stamping.** Transient (5xx, 429, timeouts,
  ECONNRESET/ECONNREFUSED/ENETUNREACH/EHOSTUNREACH) must NOT `markSkipped` and
  must NOT negative-cache — a 30-second outage would otherwise poison every
  in-flight track permanently. The HTTP-code parser must be structural
  (`HasPrefix` + `Atoi`), never a substring match, or a 4xx whose *body* mentions
  "HTTP 503" retries forever. A clean artist no-match is still NOT cached.
- **The log level never decides retry or stamp** (2026-10-14, backlog B191).
  `IsTransient` decides the MusicBrainz search paths: a transient error is
  left unenriched for the next batch and is not negative-cached; a persistent
  error is stamped. Cover Art Archive, iTunes and Deezer artwork and portrait
  paths stamp either way; they do not consult `IsTransient`.
  `upstreamReports` is the one logger, one streak per upstream. A transient
  failure warns once when that streak starts, stays quiet while it continues,
  and the next answer from that upstream is one Info line (`upstream answered`,
  attribute `upstream`, the count of failures it covered). There is no later
  Error escalation. MusicBrainz artist search, release search, release-group
  lookup and the fingerprint artist search share `musicbrainz`. Cover Art
  Archive, iTunes, Deezer and Atlas each have their own. A clean 404
  (`errNotFound`) is Debug, once per track at most: the process log level is
  Info, so Debug stays out of the journal an operator scans, and a library of
  missing covers would otherwise be an Info line per track. A 404 is an
  answer, so it ends an open streak before that Debug line. A 429 and a 5xx
  are one status class, `transientHTTPStatus`, and `IsTransient` reads it
  through `HTTPStatus` (`*httpError`, and a Cover Art Archive, iTunes or
  Deezer sentence wrapped by `carryTransientStatus`). Every client notes an
  answer through `noteSettledAnswer`, which skips that class, so a run of
  503s is one Warn. A decode error and a 4xx other than 404 and 429 stay
  Error, and the client notes that answer, which ends the streak. Atlas 5xx
  is the same streak through `noteBusy`. A disk or cache hit does not end a
  streak. Atlas and iTunes clean misses stay silent, as they were, and still
  end an open streak. `report` ends a streak only for a nil error. A DNS
  failure, a TLS certificate failure and a base that could not be used
  produced no response, so they leave the streak open; the next real answer
  is the Info line. `NewEnricher` sets `reports` and points the clients at
  it. A nil field is an Enricher built field by field: it keeps no streak,
  and each transient failure warns, because a map created on first use would
  open a streak the clients were never pointed at. The recovery line names
  the upstream. It carries no URL and no credential (the B69 rule). Local
  store errors stay Error.
  (`TestABusyUpstreamWarnsOnceAndAnswersAtInfo`,
  `TestABusyMusicBrainzRetriesAndACoverMissStillStamps`,
  `TestACleanMissIsDebugAndStillStamps`,
  `TestAnUnexpectedUpstreamFailureStaysErrorAndStamps`,
  `TestAtlasBusyWarnsOnceThenAnswers`,
  `TestARunOfTransientStatusesWarnsOnce`,
  `TestANilReportsFieldKeepsNoStreak`,
  `TestATransientStreakSurvivesANonResponse`.)
- **A stamp a shutdown stopped records nothing**: no `mark skipped` count, no
  `enrichment skipped` line, and the row stays `enriched_at = 0`. The stop is
  at `stampEnriched` / `markSkipped`, never at the fetches that absorb their
  own errors. The rule is under **The CLI and the serve wiring**. (#1001)
  **Nor does a stamp over a row that changed since the batch read it**
  (`manifest.ErrTrackChanged`: it writes nothing, and the next batch reads
  the row again; the compare-and-set bullet under **Scanner**, B187).
- **Pacing derives from the client's base URL** (`minIntervalForBase`,
  fail-safe to the public interval, dot-anchored suffix match) — public MB is
  1.1s and self-hosted is 150ms — **not zero**, because Atlas's own per-IP tier
  gate is shared with the harvest client. The invariant is enforced by
  construction rather than by a code path remembering; don't put a fixed interval
  back in `NewEnricher`. `Enricher.Run` needs its
  own inter-batch pause too: the pacer only fires when a network call is made.
- **A configured base URL's user information travels in a header, never in a
  request URL** (backlog B69). `enrich.musicbrainzBaseURL` and
  `coverArtBaseURL` may carry a mirror's credential (`<MIRROR_BASE_URL_WITH_USERINFO>`,
  or a token written as the user name), and net/http both sends it as Basic
  auth and names the request URL in every error it returns, through a
  `stripPassword` that masks a PASSWORD and nothing else. So a token written
  as the user name reached the journal whole on every transport failure:
  the `MB search`, `MB artist search`, `release-group lookup` and `artwork`
  Error lines, the `enrichment skipped` line's `detail`, and the premium
  fetch's Warn (measured over a mirror whose certificate this process does
  not trust, a persistent failure, and over a refused connect). The skip
  REASON was never affected: `markSkipped` counts one of the fixed
  `skipReason*` keys and the error rides only that log line, so "no stored
  skip reason" is about the line, not a column. No RoundTripper can stop it,
  because the client builds the error from the request's own URL; so
  `baseEndpoint` (baseauth.go) cuts a base in two, the ROOT every request URL
  is built from and the user information, which `newRequest` sends with
  `SetBasicAuth` (the user alone as `user:`), byte for byte the header
  net/http built from the URL: `TestABaseURLsCredentialReachesTheMirrorAsBasicAuth`
  takes its reference from net/http on every run. Five rules keep it whole.
  **Every request built from a configured base goes through `newRequest`**,
  and `TestEveryRequestThisPackageBuildsComesFromAListedBuilder` lists every
  function in the package that calls `http.NewRequest*` (or the package-level
  `Get`, `Post`…), so the next client cannot skip it: a builder is listed
  with the reason its URL is a public constant, or is not built. **A request
  resolves its base ONCE and carries the credential with the root**
  (`resolveBase` returns the `baseEndpoint`; never keep the root alone): a
  live base can change between two reads, and a credential read apart from the
  URL sends one mirror's to another's host
  (`TestALiveBaseThatChangesBetweenReadsNeverPairsACredentialWithAnotherHost`,
  whose provider answers a different mirror on every call). **A base that is
  not an absolute http(s) URL with a host is an error naming none of it**,
  never a fallback and never net/url's own `parse "…"` error, which quotes the
  value with no mask at all; config refuses such a value
  (`normalizeBaseURL`), so this is a backstop. **The root ends in no
  slash**: `newRequest` joins a path that begins with one, so the parser trims
  a base's trailing slashes (config, a live value and the premium fetch's
  stored base were trimmed already; a base handed straight to a constructor
  was not, and requested `/ws/2//release/…`,
  `TestABaseWithTrailingSlashesRequestsNoDoubleSlashPath`). **The header
  follows a redirect by net/http's rule for an explicit Authorization
  header, and only while the scheme stays https**: to the same host and its
  subdomains, never to another domain
  (`TestABaseURLsCredentialFollowsARedirectOnlyWhereNetHTTPSendsAnAuthorizationHeader`),
  where the URL form followed a relative Location only. That rule compares
  host names and not the scheme, so it also carried the header from an https
  request onto a plain-http hop on the same host, in cleartext, where the URL
  form's absolute Location carried none. `guardRedirects` (baseauth.go, a
  delegate to `authredirect.Guard` since backlog B133) is the
  `CheckRedirect` of every client that sends a credential: it strips the
  header from a hop that is not https when the request began on https, judged
  per hop, because net/http copies the header from the FIRST request onto
  every hop and a cleartext hop's own redirect would have it back. **Strip,
  never refuse**: a request with no credential follows a downgrade as it
  always has, and a mirror that insists on the credential at the plain hop
  answers with the 401 it always did, where a refusal is an error
  `IsTransient` does not know, stamped persistent all the same. The guard
  keeps a caller's own policy and net/http's limit of ten
  (a client that sets a policy loses that default), and copies the caller's
  client (`TestNoCredentialFollowsARedirectFromHTTPSToACleartextHop`,
  `TestARequestWithNoCredentialStillFollowsARedirectToPlainHTTP` and
  `TestTheCredentialGuardKeepsACallersRedirectPolicy`, each over all three
  clients). A constructor of a client that calls `newRequest` builds it over
  the guard, and
  `TestEveryClientThatSendsACredentialIsBuiltWithTheRedirectGuard` lists them.
  The Atlas premium
  cover fetch builds its request the same way and sends the bearer token
  alone, which the guard withholds from a hop that leaves https as well: its
  stored base carries no user information (`baseurl.CredentialBase` refuses
  it when it is provisioned, and the harvest state store drops one a hand
  edit left in its file: the next bullet), and the fetch goes through the
  parser all the same, for any other credential source. iTunes and Deezer
  take no operator URL.
  `TestNoRequestErrorNamesABaseURLsCredential` drives every request either
  client makes against three ways for a mirror to fail and five ways to write a
  credential, and `TestNoLogLineOrSkipDetailCarriesABaseURLsCredential` runs
  the real enricher and searches every line it logs, at every level, without
  regard to case.
- **…and the harvest credential's stored base is `scheme://host` or
  nothing, decided at the STORE** (backlog B97, and the stored half of B49).
  `atlasharvest.StateStore` kept whatever `atlas-harvest.json` held, and the
  harvest client's submit and poll, the booklet check and fetch, and the
  lyrics tier build every request URL from it. Measured on main with the
  real `serve` over a hand-edited file: `WARN atlasharvest.tick_error
  phase=poll error="Get \"https://s3cret-Pw@127.0.0.1:1/v1/atlas/harvest/results?…\":
  … connection refused"`, a token written as the user name, whole, on every
  tick. A path, a query or a fragment reached the same errors; a base
  written without a scheme (`user:pw@host`) failed `unsupported protocol
  scheme` with the URL quoted whole and lowercased (search without regard to
  case); plain http sent `Authorization: Bearer <token>` in the clear; and a
  port with no host, stored before #1074's check, was dialled on this
  machine (a TCP connect; the TLS handshake then fails for want of a server
  name, so the token never left). **Fix it at the store, never per
  consumer**: #1091's `parseBaseEndpoint` covered the premium fetch alone,
  and two readers of one stored value disagreed about what they tolerate.
  `OpenStateStore` reduces a loaded base with `baseurl.CredentialBase`, the
  reduction `POST /v1/atlas-harvest/credential` stores; a base with no such
  form is DROPPED with the credential held against it (token and expiry;
  the sync position stays, as `Clear` keeps it), the drop is WRITTEN BACK so
  neither stays on disk (a harvest-off revoke must not answer 204 over a
  credential still in the file), and one `atlasharvest.state.base_refused`
  Warn names the file and no part of the value. A write-back that fails
  fails the open, and `serve` runs without the harvest, as for any state
  file it cannot open. `SetCredential` refuses such a base with an error
  naming none of it, BEFORE it touches anything (a changed base resets the
  cursor). A base that reduces (a trailing slash, `:443` or an empty port,
  an uppercase scheme, space) is held reduced and keeps its credential.
  **One reduction,
  in `internal/baseurl`**: `CanonicalHTTPS` (the pin's), `NamesHost` and
  `CredentialBase` (the canonical form, when it names a host and any port it
  names is 1-65535: `url.Parse` checks a port's digits, not its range, so
  `https://atlas.example:99999` was stored and failed every dial), imported
  by config, the handler and the store, since `internal/atlasharvest`
  imports neither config nor enrich; `config.CanonicalHTTPSBase` and
  `BaseURLNamesHost` are gone, so there is no second copy to drift. **Keep
  the host and port tests in `CredentialBase`, out of `CanonicalHTTPS`**:
  the pin goes through the latter, `Validate` refuses a pin that reduces to
  "", and a config that loaded must keep loading (B36's reasoning); such a
  pin keeps its canonical form and matches no credential. **The
  reduction is a fixed point** (`TestTheReductions` reduces every answer
  again): the pin is reduced twice, by config and again by
  `WithAtlasHarvest`, and the store reduces at every open. `https://:443`
  reduced to `https://` and then to "", so a pin written that way left a
  non-demo bridge UNPINNED (measured with main's binary: a paired device's
  credential for `https://attacker.example` answered 200 and was stored);
  it stays itself now and matches nothing, as a port and no host should.
  **The harvest tests' fake Atlases are TLS servers** (`httptest.NewTLSServer`,
  the client given `srv.Client()`), since the store holds an https base or
  none: one that seeds an http base fails at `SetCredential`, and one that
  ignored that error would pass having shown nothing
  (`TestClientTokenRejectedClearsCredential` did, `_ = state.SetCredential`).
  `TestAStoredBaseThatIsNotSchemeAndHostIsNeverUsed` (eleven shapes, each
  searched for the secret without regard to case in every line at every
  level, and a positive control over a base in the stored form that must
  connect and log),
  `TestSetCredentialRefusesABaseThatIsNotSchemeAndHost`,
  `TestTheStoreHoldsABaseInItsCanonicalForm`,
  `TestARevokeLeavesNoStoredBaseBehind`,
  `TestOpeningAStoreThatCannotDropItsBaseFails` and
  `TestAPinOfAPortAndNoHostStaysAPinAsServeWiresIt`.
- **…and the harvest client is built over the enrich clients' redirect guard,
  `authredirect.Guard`** (backlog B133, found by the B97 session). Every
  harvest request (submit, poll, the booklet check and PDF, the lyrics tier)
  sets its bearer token as an explicit Authorization header, and
  `defaultHarvestHTTPClient` had no `CheckRedirect`, so an https Atlas
  answering with a redirect to http on its own host had the token sent in
  the clear: #1091's round-2 finding, fixed then for the enrich clients
  alone, because their guard lived in `internal/enrich`, which
  `internal/atlasharvest` must not import (measured: a TLS fake Atlas 307ing
  to a plain server on 127.0.0.1, and the plain hop saw `Bearer <token>` on
  all three legs tested). The guard moved to `internal/authredirect`;
  enrich's `guardRedirects` is a delegate, so its population test
  (`TestEveryClientThatSendsACredentialIsBuiltWithTheRedirectGuard`) still
  finds each constructor's call by name, and `Client.httpClient` builds the
  default client and any client a caller hands in over the guard (a copy,
  never the caller's own). **A client that sends a credential is built over
  this guard, whatever package it lives in**: the population test sees
  enrich alone. `TestNoHarvestRequestCarriesItsTokenOntoACleartextHop`,
  `TestTheDefaultHarvestClientDropsTheTokenLeavingHTTPS`, and
  `internal/authredirect`'s own two tests. B97's log entry named B133 and no
  more while it was unfixed (the rule in the backlog section above).
- **A tick's results cursor and last-submit stamp land only on the base they were computed
  against** (backlog B169, 2026-10-15). `SetCredential` to another base resets
  `ResultCursor` and `LastSubmitAt`, but a results page or a library submit still in flight
  against the old base landed after it: the poll's `SetCursor` gave the new base the old
  base's cursor, so the new base's results below it were never fetched, and the submit's
  `SetLastSubmit` held the new base's first library submit for a whole submit interval
  (6 h). Both setters now take the base the tick's snapshot read and do nothing when the
  store holds another (`TestAResultsPageFromTheOldBaseLeavesTheNewBasesCursorAlone`,
  `TestASubmitToTheOldBaseLeavesTheNewBasesFirstSubmitDue`). **Don't give either setter a
  form without the base**: the "Retry missing" reset passes the base it reads, and a switch
  in between has already zeroed the stamp it resets.
- **A release-search miss must not cost the track its artist resolution** — the
  two halves are independent and the artist search is the cheap reliable one.
- **`ResetEnrichedMisses` tests THREE arms — artwork, artist AND release MBID.**
  `artworkMBID` is not a proxy: it also carries the scanner's `local-<sha256>`
  sentinel, so 6,801 of 8,945 affected rows were invisible to the two-arm form
  while the handler reported success.
- **A fingerprint identifies AUDIO, so `acoustid.Decision` has nowhere to put a
  release or artwork MBID** — one recording sits under many release groups
  precisely because they contain the same audio. Reach an album MBID only by
  running the existing text ladder with the recovered artist NAME. This is what
  bounds a wrong answer to a wrong portrait rather than a wrong album identity,
  cover, booklet and grouping.
- **Fingerprint suppression is keyed on BOTH inputs and TTL'd**, and the two
  marker kinds are cleared by ONE statement while being READ separately — a
  retry that cleared one and not the other makes the button silently do nothing
  for half the population. Only `ErrNoMatch` and the tag-contradiction veto
  persist; a lookup error is a fact about the upstream and must not sideline a
  file. Retry must clear the in-process cache AND the SQLite rows, or everything
  answered this session stays suppressed until a restart.
- **One fpcalc gets its own deadline, and a file that uses it up stays eligible**
  (2026-10-12, backlog B214). `fingerprintOne` called `acoustid.Compute` on the
  sweep context, and `fingerprintAll` waits for its workers with no budget of
  its own, so one fpcalc that never returned held the sweep until shutdown.
  The default pool is one worker, so the next file never started. Each file
  now runs fpcalc under `context.WithTimeout` of `fingerprintDecodeTimeout`
  (10 minutes; a test sets `decodeTimeout`). Measured on fpcalc 1.6.1: the
  default `-length` 120 window of a 192 kHz 24-bit FLAC took 0.39 s, and a
  30-minute FLAC under the same `-length` took 0.11 s, because the decode
  stops at the window. The wait that matters is the read of the whole object
  on a network mount, so a bound near that decode, or near the 120 s of
  audio, would time out a slow read on every sweep. Ten minutes is the cap a
  transcode or analysis job already gives a tool that can spin.
  `exec.CommandContext` kills the direct child. A child in uninterruptible
  sleep still outlives the signal, which is what `sweeperDrainGrace` is for.
  A timeout logs once (`fingerprint: fpcalc timed out`, the library-relative
  path) and is neither cached nor written as a no-match: a hung mount and a
  pathological file look the same from here, and the next sweep offers the
  file again. **The timeout is the per-file deadline, whatever error the
  decode returned** (2026-10-13, backlog B349). `Cmd.Wait` prefers the
  process exit over the context error, so on Windows `TerminateProcess`
  comes back as `exit status 1` and does not wrap `context.DeadlineExceeded`.
  A start still inside `CreateProcess` when the deadline passes — the first
  run of a copied `fpcalc.exe`, which Defender scans — can return that start
  error, or `ErrUnreadable` when `Compute` wraps it while `ctx.Err()` is
  still nil. `noteDecodeErr` asks the file context (`fileBudgetExpired`):
  `Err` is `DeadlineExceeded`, or `Deadline()` is already in the past. The
  timer that sets `Err` can be late on a loaded host. `ErrUnreadable` is
  still cached as a miss when the budget has not expired. A cancelled parent
  stays silent, including when the file deadline has also passed.
  **The stand-in that plays a hung fpcalc blocks on a timer** (2026-10-13,
  backlog B363). A bare `select` with no case parks with nothing queued.
  The runtime aborts that process when no timer is on a processor yet
  (`all goroutines are asleep - deadlock!`, exit status 2). The sweep
  then sees a failed decode while the budget is still ahead, caches
  `ErrUnreadable`, and logs no timeout line. windows-latest gate run
  38385471196 attempt 1 failed `TestAHungFpcalcDoesNotHoldTheSweep` in
  0.78s with an empty timeout list. The hang sleeps, which arms a timer
  before the goroutine parks. The test requires the sweep to have used
  the budget before it requires the timeout line. A decode that ends
  while the budget is still ahead stays a failed decode.
  (`TestAHungFpcalcDoesNotHoldTheSweep`,
  `TestFpcalcBudgetDefaultsToTenMinutes`,
  `TestNoteDecodeErrCachesOnlyAnUnreadableFile`.)
- **The local-artist veto lives in `internal/enrich`, not `internal/acoustid`**
  (import cycle), only ever SUBTRACTS, and its junk-tag list is closed and tiny.
  An all-digits ALBUM title is NOT junk though an all-digits artist is:
  misclassifying an artist removes a witness, misclassifying an album
  substitutes the fingerprint's title for the operator's own.
- **MusicBrainz `release-group` is an OBJECT, not a string** — `{id, title,
  primary-type}`. Mock fixtures must match the live shape.
- **Negative-cache PERSISTENT search failures** (store an empty MBID under the
  `(artist, album)` key) or sibling tracks on the same album re-query the same
  inputs and turn a 1-track failure into an N-track spin loop. Transient failures
  must NOT be cached — see the classification rule above.
- **`Retry-After` is honoured on MB and iTunes 429/503, capped at 1h.** The cap
  applies in the SECONDS domain before multiplying (overflow guard) and on
  `strconv.ErrRange`; without it a hostile or misconfigured upstream parks the
  enricher indefinitely. Pacing sleeps are ctx-aware so shutdown isn't blocked.
- **The enricher caches are bounded LRUs, pre-allocated at full capacity**, not
  `sync.Map` — the unbounded form leaked for the process lifetime on a
  multi-decade library. The Deezer negative cache is presence-only and must not
  promote to MRU, or a stale entry outlives a positive re-fetch.
- **An MBID that reaches a path is validated at the site that BUILDS the path,
  and the artwork write is bounded to its own directory.** `ArtworkCachePath`
  makes the value the LEADING component of a `filepath.Join` (which Cleans) and
  `writeArtworkAtomicStream` then `MkdirAll`s the parent — so a traversing value
  CREATES its way out rather than failing. Every caller validated at entry
  except the pair fed by the ATLAS HARVEST RESULTS PAGE, where the UPSTREAM
  chooses the MBID and the bridge never checks it against what it submitted:
  `atlasCoverRefetcher.RefetchPremium` wrote up to `MaxCoverArtBytes` there and
  the stale-tier loop beside it `os.Remove`d two more. Persisted, too —
  `AddPendingCovers` skipped only `""`, so a hostile value survived restarts and
  came back every tick. Three layers now, each negative-controlled separately:
  shape at ingest (`pollResults`) and at the sink, containment via
  `fsutil.IsUnderAny` inside the write primitive, and an image-signature check on
  the body. **`config.go`'s unpinned-`harvestBaseUrl` note bounds the accepted
  risk at CONTENT INJECTION** ("the bios it returns land in `artist_atlas`") —
  that was written against an incomplete model, and an arbitrary file write is
  outside it. Pin `atlas.harvestBaseUrl` on any bridge with harvest enabled.
- **Artwork is JPEG-only and two-layer verified (MIME *and* the `FF D8 FF` magic
  bytes) ON THE SCANNER'S LOCAL FOLDER-ART PATH — that rule never covered the
  NETWORK path, and this bullet read as though it did.** `internal/manifest` has
  `looksLikeJPEG` + the `folderArtCandidates` sniff; `internal/enrich` had no
  content check of any kind, on ANY of its six write sites (CAA release, CAA
  release-group, iTunes, both premium paths, and the Deezer artist portrait),
  so whatever an upstream returned was stored behind a name served as
  `image/jpeg`: a cover's `*-N.jpg`, which `/v1/artwork` serves, and the
  portrait's `artist-<mbid>.jpg`, which `/v1/artist-image` serves. The write primitive
  (`writeArtworkAtomicStream`) now refuses bytes that are not a recognised
  image. **Every upstream artwork write goes through it**: the Deezer
  portrait kept the buffered `writeArtworkAtomic` until 2026-10-14 (backlog
  B168), so an allowlisted host serving HTML had it stored as the canonical
  and linked to the `artist-<mbid>.jpg` that `/v1/artist-image` serves as
  `image/jpeg` (`TestArtistPortraitIsStoredOnlyWhenItIsAnImage`). The
  buffered writer is left only for `linkOrCopy`'s copy of a canonical that
  already passed. **Both portrait paths are checked inside the cache before
  anything is read, fetched, linked or copied** (`ensureArtistImageCached`):
  the primitive's containment covers the canonical it writes, never the MBID
  path it is then linked or copied to, so an MBID that leaves the cache had
  a portrait hard-linked outside it
  (`TestArtistPortraitPathsStayInsideTheCache`; every caller checks the
  MBID's shape first, so this is the layer under that check). No cache
  directory refuses first, as such (both paths would be relative to the
  working directory; `TestArtistPortraitWithNoCacheDirectoryIsRefusedAsSuch`). Deliberately NOT JPEG-only, though that is what this path's contract
  says: CAA can serve PNG, those covers render today because clients sniff, and
  dropping them inside a security fix buys nothing the arbitrary-payload refusal
  does not already buy. `artwork_scale.go`'s rule — **"a verbatim PNG write would
  put PNG bytes behind an image/jpeg label"**, which is why the SCANNER
  transcodes — still applies here and is still unimplemented on the network
  side; the warn line is what stops that staying invisible. Folder-art
  lookup is single-flighted per directory (a `sync.Once` promise stored with
  `LoadOrStore` — **never compute-then-`LoadOrStore`**, which runs N concurrent
  ReadDir+hash per album under contention) and reset per scan; a reset alone
  never brought a new cover to tracks already indexed, which the folder-art
  key does (the B141 bullet under Scanner). The disc-subfolder
  parent fallback climbs EXACTLY ONE level, gated on an anchored disc-folder
  name and bounded by the absolute library roots.
- **iTunes is a fallback, not a primary source**; artwork cache keys stay
  MB-derived so the wire shape is unchanged.

## Duplicates and the served set

- **`internal/dupes` is a VERBATIM MIRROR of the iOS normalizer** — its output
  must equal the client's partition, so a fix that makes it "better" than the
  client makes it WRONG. Test literals are lifted verbatim from the Swift tests.
- **DSD and PCM are never cross-suppressed**, and `different-audio` (proven
  remasters) never suppresses under any mode. Winner election is a strict total
  order so winners never flap (flapping = `indexed_at` churn).
- **`outranks` has NO availability term** — so enabling cross-source
  suppression would suppress a local rip in favour of a copy on an upstream that
  is powered off half the time. Deciding where availability sits in that ranking
  IS the product decision; don't flip `includeRouted` without making it.
- Tier names are evidence claims: only `identical-audio` may say "reclaimable";
  `inconclusive` is never suppressed.
