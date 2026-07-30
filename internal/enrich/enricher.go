package enrich

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/lrucache"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
)

var logger = logging.Component("enricher")

// mbidValidPattern matches a MusicBrainz UUID. An embedded
// MUSICBRAINZ_ALBUMID tag is attacker-influenceable — it comes straight
// from the file's tags via stringOf (which only trims) and flows into the
// artwork-cache FILE PATH (ArtworkCachePath -> filepath.Join) and outbound
// CAA/Atlas URLs, so it MUST be validated before use. The /v1/artwork read
// handler already guards the identical value with the same UUID shape
// (api/artwork.go mbidPattern) "to prevent traversal and filesystem abuse";
// this mirrors that guard on the enricher write side. Kept independent of
// the api package on purpose — the dependency direction is api -> enrich,
// so enrich must not import api.
var mbidValidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isValidMBID reports whether s is a well-formed MusicBrainz UUID.
func isValidMBID(s string) bool { return mbidValidPattern.MatchString(s) }

// maxLoggedValueLen bounds an untrusted value written to logs so a hostile
// tag can't flood the log. slog already quotes + escapes control chars in the
// value, so this only handles length.
const maxLoggedValueLen = 96

// truncateForLog bounds s to maxLoggedValueLen bytes at a UTF-8 rune boundary
// (never emitting a split sequence) with a marker appended when it was cut.
func truncateForLog(s string) string {
	if len(s) <= maxLoggedValueLen {
		return s
	}
	cut := maxLoggedValueLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...(truncated)"
}

// Enricher is a long-running worker that pulls un-enriched tracks from
// the manifest store, looks them up against MusicBrainz / Deezer, caches
// artwork locally, and writes the enriched data back.
type Enricher struct {
	store  *manifest.Store
	mb     *MusicBrainzClient
	caa    *CoverArtClient
	itunes *ITunesClient // optional; nil disables the iTunes fallback
	deezer *DeezerClient

	// premiumCovers, when set, is tried BEFORE the CAA chain in
	// ensureArtworkCached — an authenticated Atlas fetch that yields the
	// cross-source premium canonical (Qobuz/Tidal-grade) when one exists,
	// caching it under the same path the CAA chain would use so /v1/artwork
	// serves it with zero iOS change. Wired at startup (cmd/bridge) only when
	// the Phase-H harvest credential store is available; nil = CAA-only
	// enrichment (the default).
	premiumCovers PremiumCoverFetcher

	// acoustic is the fingerprint verdict source. Nil (the default) means
	// acoustic fingerprinting is off, exactly like a nil premiumCovers.
	acoustic AcousticLookup

	// CacheDir is the root where the cached JPEGs live. Album covers go
	// in <CacheDir>/<mbid>-<size>.jpg (see ArtworkCachePath); artist
	// images go in <CacheDir>/artist-<mbid>.jpg (see ArtistImagePath).
	//
	// **Same-partition requirement (best-effort, fallback-safe)**:
	// `linkOrCopy` uses `os.Link` to deduplicate identical JPEG
	// payloads across MBID + name-keyed entries — hard links only
	// work within one filesystem partition. The atomic-write
	// helper uses `os.CreateTemp(filepath.Dir(path), …)` so the
	// temp file is co-located with the final destination ON THE
	// SAME PARTITION by construction; that part is structural.
	// What's NOT structural: a future operator mounting
	// `<CacheDir>` itself across two partitions (e.g. symlinking
	// `<CacheDir>/portraits` to a different volume from
	// `<CacheDir>/covers`) would break the link path silently —
	// the writeArtworkAtomic copy fallback covers that case
	// cleanly, but operators benefit from knowing the design
	// expects a single-partition layout.
	CacheDir string

	// MBMinInterval is the minimum gap between MusicBrainz requests.
	// DERIVED from the MusicBrainzClient's base URL at construction, not a
	// fixed value: PublicMBMinInterval (1.1s, headroom over MB's anonymous
	// 1/s limit) against musicbrainz.org, SelfHostedMinInterval (150ms)
	// against an operator's own mirror. See pacing.go.
	MBMinInterval time.Duration
	// CAAMinInterval is the minimum gap between Cover Art Archive
	// requests. Derived the same way from the CoverArtClient's base:
	// PublicCAAMinInterval (500ms — CAA is more forgiving than MB but we
	// stay polite) against coverartarchive.org, SelfHostedMinInterval
	// against a mirror.
	CAAMinInterval time.Duration
	// ITunesMinInterval is the minimum gap between iTunes Search API
	// requests. Apple's unwritten convention is ~20 req/min; 3s sits
	// well under that. Only consulted when CAA misses and an iTunes
	// client was passed to NewEnricher.
	ITunesMinInterval time.Duration
	// DeezerMinInterval is the minimum gap between Deezer requests.
	// Deezer's unauth limit is ~50 req / 5s; 120ms is well under that.
	DeezerMinInterval time.Duration

	// BatchLimit is the maximum number of un-enriched tracks processed
	// per wakeup. Keeps the worker responsive to cancellation.
	BatchLimit int

	// PollInterval is how long to wait between empty-batch checks.
	PollInterval time.Duration

	// albumCache memoizes (artist, album) → albumResolution so tracks
	// on the same album share a single MB round-trip. Bounded LRU
	// (capacity albumCacheCap) so a long-running enricher on a
	// multi-decade library can't grow the working set indefinitely.
	// Stores both the release MBID and — when MB returned one — the
	// release-group MBID so the CAA release-group fallback doesn't
	// need a second MB lookup.
	albumCache *lrucache.Cache[string, albumResolution]

	// releaseGroupCache memoizes releaseMBID → releaseGroupMBID for
	// the embedded-MBID path where no SearchRelease happened. Keyed
	// on the release MBID alone; negative results cache as the empty
	// string so sibling tracks don't re-query a release that
	// genuinely has no release-group association. Bounded at
	// releaseGroupCacheCap.
	releaseGroupCache *lrucache.Cache[string, string]

	// artistCache memoizes artist-name → ArtistMBID so sibling tracks
	// by the same artist share the lookup + image-fetch. Bounded at
	// artistCacheCap.
	artistCache *lrucache.Cache[string, string]

	// deezerNegCache remembers "Deezer had no portrait" per artist
	// MBID so sibling tracks don't each re-query. Lookup is
	// presence-only via `Has` (a sibling-track read doesn't promote
	// the negative entry to MRU and outlive a positive Deezer
	// re-fetch). Bounded at deezerNegCacheCap — highest-fanout of
	// the four because every artist Deezer doesn't have ends up here.
	deezerNegCache *lrucache.Cache[string, struct{}]

	// progress counters exposed via ScanState.
	done    atomic.Int64
	skipped atomic.Int64

	// skipReasons counts markSkipped calls per bounded reason key. The
	// keys are the skipReason* constants ONLY — never a formatted error
	// string, which would make the map unbounded in cardinality (a
	// per-host MB error message would mint a fresh key each time). The
	// variable detail rides the log line instead.
	skipReasonsMu sync.Mutex
	skipReasons   map[string]int64

	// caaFallbackHits counts how often the release-group fallback salvaged
	// an artwork fetch that the release-level lookup missed. Exposed as a
	// plain counter today; may surface on the admin stats page in v1.2.
	caaFallbackHits atomic.Int64

	// itunesFallbackHits counts how often the iTunes Search fallback
	// salvaged an artwork fetch that both CAA paths (release + release-
	// group) missed. Same exposure plan as caaFallbackHits.
	itunesFallbackHits atomic.Int64
}

// albumResolution bundles the release + release-group MBIDs that
// SearchRelease returns. Stored in albumCache so sibling tracks share
// both values.
type albumResolution struct {
	ReleaseMBID      string
	ReleaseGroupMBID string
}

// Cache size ceilings for the four enricher memoization caches.
// Sized to comfortably cover a 50k-track library on a Pi-class host:
// at ~200 B per entry the worst case is ~10 MB per cache (~40 MB
// aggregate), well below process headroom and finite for the
// long-running enricher loop. Pre-allocated map capacity at
// construction so the bulk-ingestion phase of a 50k-track scan
// doesn't trigger Go map bucket resizing mid-flight.
const (
	albumCacheCap        = 50_000
	releaseGroupCacheCap = 50_000
	artistCacheCap       = 50_000
	deezerNegCacheCap    = 100_000
)

// NewEnricher wires a store + MB/CAA/Deezer clients + cache dir into a
// worker. Sensible defaults applied if the numeric fields are zero.
// Deezer can be nil — artist-image lookup is simply skipped in that
// case.
func NewEnricher(store *manifest.Store, mb *MusicBrainzClient, caa *CoverArtClient, deezer *DeezerClient, cacheDir string) *Enricher {
	// The MB/CAA pacing comes FROM the clients, so it tracks the host each is
	// actually pointed at (see pacing.go). A nil client keeps the public
	// interval — it is only reachable from tests, and the safe direction for a
	// missing client is the slow one. iTunes and Deezer are always the public
	// third-party services, so their intervals stay fixed here.
	mbInterval, caaInterval := PublicMBMinInterval, PublicCAAMinInterval
	if mb != nil {
		mbInterval = mb.MinInterval()
	}
	if caa != nil {
		caaInterval = caa.MinInterval()
	}
	e := &Enricher{
		store:             store,
		mb:                mb,
		caa:               caa,
		deezer:            deezer,
		CacheDir:          cacheDir,
		MBMinInterval:     mbInterval,
		CAAMinInterval:    caaInterval,
		ITunesMinInterval: 3 * time.Second,
		DeezerMinInterval: 120 * time.Millisecond,
		BatchLimit:        100,
		PollInterval:      15 * time.Second,
		albumCache:        lrucache.New[string, albumResolution](albumCacheCap),
		releaseGroupCache: lrucache.New[string, string](releaseGroupCacheCap),
		artistCache:       lrucache.New[string, string](artistCacheCap),
		deezerNegCache:    lrucache.New[string, struct{}](deezerNegCacheCap),
	}
	return e
}

// WithITunes attaches an iTunes Search client to the enricher. Used as
// a fallback artwork source when CAA returns 404 — most mainstream
// releases that CAA misses are present in iTunes, so this raises the
// artwork hit rate without changing the wire shape (the fetched bytes
// still cache under the MB-derived release MBID, and `/v1/artwork/{mbid}`
// continues to serve them transparently). Returns the receiver for
// fluent setup: `NewEnricher(...).WithITunes(itc)`.
func (e *Enricher) WithITunes(itc *ITunesClient) *Enricher {
	e.itunes = itc
	return e
}

// WithPremiumCovers attaches an authenticated Atlas premium-cover fetcher
// (Phase B). When set, ensureArtworkCached tries it BEFORE the CAA chain, so a
// release with a cross-source premium canonical (Qobuz/Tidal) caches the
// hi-res cover under the same MBID-keyed path /v1/artwork already serves —
// zero iOS change. Leave unset for CAA-only enrichment (the default). Returns
// the receiver for fluent setup.
func (e *Enricher) WithPremiumCovers(f PremiumCoverFetcher) *Enricher {
	e.premiumCovers = f
	return e
}

// Run loops until ctx is done, pulling un-enriched tracks and processing
// them in waves. Errors on individual tracks are logged but don't stop
// the loop.
func (e *Enricher) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		batch, err := e.store.UnenrichedTracks(ctx, e.BatchLimit)
		if err != nil {
			logger.Error("list unenriched", "err", err)
			if !sleepCtx(ctx, e.PollInterval) {
				return
			}
			continue
		}
		if len(batch) == 0 {
			if !sleepCtx(ctx, e.PollInterval) {
				return
			}
			continue
		}
		for i := range batch {
			if ctx.Err() != nil {
				return
			}
			e.enrichOne(ctx, &batch[i])
		}
		// Unconditional breather between batches.
		//
		// The MB/CAA pacers are NOT a substitute: they only sleep when a
		// network call is actually made, so a batch whose rows all resolve
		// from the LRU caches (or bail on an early check) completes in
		// milliseconds and immediately re-queries UnenrichedTracks. At the
		// old fixed 1.1s MB pace that was invisible; against a self-hosted
		// mirror at SelfHostedMinInterval it is not, and a batch that keeps
		// failing WITHOUT stamping enriched_at (the transient-error path)
		// would spin the DB as fast as the loop can turn.
		if !sleepCtx(ctx, interBatchPause) {
			return
		}
	}
}

// interBatchPause is the floor on how often Run may re-poll the store. Small
// enough to be invisible against real enrichment work, large enough that a
// zero-progress batch cannot hot-spin.
const interBatchPause = 50 * time.Millisecond

// Done returns the number of tracks processed by this Enricher so far
// (the count resets when the process restarts; it's not persisted).
func (e *Enricher) Done() int64 { return e.done.Load() }

func (e *Enricher) enrichOne(ctx context.Context, t *manifest.Track) {
	// NOTE the ordering: the MBID scrub below runs BEFORE the
	// no-artist/album early return. It used to run after, so a file with a
	// crafted `musicbrainz_albumid` AND a blank artist or album tag was
	// markSkipped (which persists tags_json) with the hostile value intact
	// — and that persisted value is exactly what the booklet-cache writer
	// later consumed. Scrubbing first means every row that reaches storage
	// has been through the same validation (2026-07-20 review, F30).
	//
	// Defense-in-depth: reject a non-UUID embedded album MBID before it can
	// reach ArtworkCachePath / the CAA+Atlas URL builders. A crafted tag like
	// "../../evil" would otherwise escape cacheDir when writeArtworkAtomicStream
	// MkdirAll+renames. Treating it as ABSENT lets the track fall through to the
	// normal name-based search path below — identical to a file that carried no
	// MBID tag at all, so there is no new match-quality risk (that path already
	// exists). See mbidValidPattern.
	if t.MusicBrainzAlbumID != "" && !isValidMBID(t.MusicBrainzAlbumID) {
		// Bound the untrusted tag value before logging: the length cap defends
		// against log flooding from a hostile tag, and slog's text + JSON
		// handlers already escape control chars / newlines in string values
		// (CWE-117 log-injection defense). Pass the raw truncated value rather
		// than a pre-quoted %q form so the handler quotes exactly once, not
		// twice. (Gemini #491)
		logger.Warn("ignoring non-UUID embedded album MBID", "path", t.Path, "value", truncateForLog(t.MusicBrainzAlbumID))
		t.MusicBrainzAlbumID = ""
	}

	// Skip tracks that have no artist+album info to search by. Mark them
	// done anyway so we don't poll them forever. Runs AFTER the scrub above
	// so the persisted row can't retain an unvalidated MBID.
	if t.Artist == "" || t.Album == "" {
		// Nothing to search MusicBrainz by — this is the population acoustic
		// fingerprinting exists for, so consult it before giving up.
		if m, ok := e.applyAcousticFallback(t); ok {
			// The fingerprint supplied an artist; fall through so the artist
			// image, the album ladder and MarkEnriched all run as normal.
			// Deliberately NOT a separate write path: everything downstream
			// treats a fingerprint-derived artist exactly like a tag-derived
			// one, which is what keeps the fallback from growing its own
			// half-parallel copy of enrichOne.
			e.enrichWithRecoveredArtist(ctx, t, m)
			return
		}
		e.markSkipped(ctx, t, skipReasonNoSearchTerms, "no artist/album to search by")
		return
	}

	// If the file already carried an MBID, we don't need to search — just
	// try to grab artwork for it. The release-group MBID for the CAA
	// fallback gets resolved lazily in `ensureArtworkCached` only if the
	// release-level fetch misses, so files that hit on the first try
	// don't pay the extra MB round-trip.
	albumMBID := t.MusicBrainzAlbumID
	var rgMBID string
	if albumMBID == "" {
		// Cache by (artist, album) so sibling tracks on the same album
		// share one MB call.
		key := cacheKey(t.Artist, t.Album)
		if res, ok := e.albumCache.Get(key); ok {
			metrics.RecordMBCache("album", true)
			albumMBID = res.ReleaseMBID
			rgMBID = res.ReleaseGroupMBID
		} else {
			metrics.RecordMBCache("album", false)
			// Honor ctx during the pacer so SIGTERM doesn't sit
			// blocked for up to MBMinInterval per in-flight track.
			// `enrichOne` is void; bare return on shutdown matches the
			// existing `if ctx.Err() != nil { return }` shape below.
			res, err := e.searchReleaseWithFallbacks(ctx, t)
			if err != nil {
				// Shutdown cancellation looks like an MB error; don't
				// poison the skipped-cache or mark the track so a
				// future run can retry it normally.
				if ctx.Err() != nil {
					return
				}
				logger.Error("MB search", "artist", t.Artist, "album", t.Album, "err", err)
				if IsTransient(err) {
					// Network blip, 5xx, 429, or timeout — leave
					// `enriched_at` at 0 so the next pass picks
					// this track up again. PR #N: pre-fix, a 30-
					// second MusicBrainz outage permanently
					// poisoned every track currently being enriched
					// because every error went straight to
					// `markSkipped`. Persistent failures (404, JSON
					// decode, schema drift) still skip — those are
					// guaranteed to fail every retry and would loop
					// the worker indefinitely.
					//
					// CRITICAL: do NOT populate albumCache on a
					// transient error (qodo + gemini bot review on
					// PR #N). If we cached an empty resolution
					// here, the next iteration of the worker loop
					// would see a cache hit, fall into the
					// "albumMBID == \"\"" branch below, and call
					// `markSkipped(t, "no MB match")` — defeating
					// the entire transient-retry mechanism. Sibling
					// tracks on the same album DO re-query MB on
					// the next pass, but the per-process MB pacer
					// (1.1s) keeps the load polite. Acceptable
					// trade-off; preserving retry-ability is the
					// load-bearing contract this PR adds.
					return
				}
				// Persistent failure: cache the empty resolution so
				// sibling tracks on the same album don't re-hammer
				// MB with the same guaranteed-fail query (e.g.
				// schema drift, decode error). The cache is
				// process-local; restart clears it.
				e.albumCache.Set(key, albumResolution{})
				e.markSkipped(ctx, t, skipReasonMBError, err.Error())
				return
			}
			resolution := albumResolution{}
			// Validate the SEARCH RESULT, not just the embedded tag. Both
			// feed ArtworkCachePath / the CAA+Atlas URL builders, and the
			// MB base URL is operator-configurable (musicbrainz / atlas /
			// custom), so a hostile or misconfigured endpoint is a real
			// input channel — the tag-side scrub above does not cover it.
			// A rejected value is treated as "no match", which is the same
			// state a genuine miss produces (2026-07-20 review, F30).
			// Rejections are logged, matching resolveReleaseGroupMBID's
			// existing warning. Silence here made the two paths asymmetric
			// and, given the stated threat model (operator-configurable
			// endpoint), removed the only signal that an upstream is
			// serving garbage — the track would just look like an ordinary
			// no-match. Bounded + raw-valued per the CWE-117 note above.
			if res != nil {
				if isValidMBID(res.MBID) {
					resolution.ReleaseMBID = res.MBID
				} else if res.MBID != "" {
					logger.Warn("ignoring non-UUID release MBID from search",
						"artist", t.Artist, "album", t.Album, "value", truncateForLog(res.MBID))
				}
				if isValidMBID(res.ReleaseGroupMBID) {
					resolution.ReleaseGroupMBID = res.ReleaseGroupMBID
				} else if res.ReleaseGroupMBID != "" {
					logger.Warn("ignoring non-UUID release-group MBID from search",
						"artist", t.Artist, "album", t.Album, "value", truncateForLog(res.ReleaseGroupMBID))
				}
			}
			albumMBID = resolution.ReleaseMBID
			rgMBID = resolution.ReleaseGroupMBID
			e.albumCache.Set(key, resolution)
		}
		// Propagate to the track whether we hit cache or searched fresh.
		if albumMBID != "" {
			t.MusicBrainzAlbumID = albumMBID
		}
	}

	// Without a MusicBrainz match we'd normally markSkipped and bail
	// — but the scanner may have already stamped t.ArtworkMBID with a
	// `local-<sha256>` sentinel pulled from embedded ID3 APIC art or
	// a folder-level cover.jpg. That's exactly the obscure-album case
	// this fallback was designed to cover (no MB record but the user
	// curated artwork locally). Falling through here lets
	// resolveArtist below still fetch the artist image, and the
	// MarkEnriched at the bottom stamps the track done so the worker
	// doesn't loop on it. Without this relaxation, the local-artwork
	// feature would silently fail to fix the very case it targets.
	if albumMBID == "" && !strings.HasPrefix(t.ArtworkMBID, "local-") {
		// Resolve the ARTIST before giving up on the album. The two halves are
		// independent — the artist search is a different, much more reliable
		// query (30-190ms and a far smaller candidate space than the release
		// search) — and this branch is not rare: a release-search miss used to
		// cost the track its artist MBID and artist image too, purely as
		// collateral. On the production library that was roughly half the
		// tracks losing an artist portrait for a reason unrelated to the artist.
		//
		// Same error contract as the success path below: a TRANSIENT MB failure
		// returns non-nil, and we return WITHOUT stamping so the worker retries
		// this track on the next batch. Persistent misses return nil and fall
		// through to markSkipped, which persists whatever resolveArtist did
		// manage to set.
		if err := e.resolveArtist(ctx, t); err != nil {
			return
		}
		// The text ladder ran clean and found nothing. Before stamping, see
		// whether the audio itself identifies the artist — the tags may be
		// wrong rather than merely obscure.
		// Consulted even when the artist ALREADY resolved: a track can have a
		// good artist tag and a junk album one ("CD 01"), in which case the
		// text ladder has nothing to search the album by and the fingerprint's
		// release-group hint is the only new information available. That is a
		// large population — on the test host, over a thousand of the
		// release-missing tracks sit under a generic disc folder.
		// applyAcousticFallback will not overwrite an artist the text path
		// already accepted.
		if m, ok := e.applyAcousticFallback(t); ok {
			e.enrichWithRecoveredArtist(ctx, t, m)
			return
		}
		reason := skipReasonNoMBMatch
		if e.acoustic != nil {
			reason = skipReasonNoFingerprintMatch
		}
		e.markSkipped(ctx, t, reason, "no acceptable release candidate")
		return
	}

	// Fetch and cache 500px front cover. If the file already exists, we
	// skip the network round-trip entirely. On a release-level CAA miss,
	// `ensureArtworkCached` will lazily resolve + try the release-group
	// fallback, then iTunes (if configured) as a last resort.
	//
	// Local-artwork bypass: when the scanner has already stamped a
	// `local-<sha256>` ArtworkMBID, the cache file is on disk under
	// that sentinel and we skip BOTH the CAA round-trip AND iTunes
	// fallback. Treating the locally-curated bytes as authoritative is
	// the explicit V1 contract — embedded APIC + folder.jpg outrank
	// any remote source. Note the doubled-up safety: ensureArtworkCached
	// can only be reached when albumMBID != "" (see the bailout above)
	// AND when ArtworkMBID lacks the `local-` prefix, so an
	// `ensureArtworkCached("", ...)` call shape is structurally
	// impossible from this site.
	if !strings.HasPrefix(t.ArtworkMBID, "local-") {
		if cached, err := e.ensureArtworkCached(ctx, albumMBID, rgMBID, t.Artist, t.Album, 500); err != nil {
			// Suppress the Error log on a clean shutdown: ensureArtworkCached
			// returns ctx.Err() when a pacing sleepCtx trips on cancel, which
			// isn't a real artwork failure. Mirrors the ctx guard the album /
			// artist SearchRelease / SearchArtist paths already carry.
			if ctx.Err() == nil {
				logger.Error("artwork", "mbid", albumMBID, "err", err)
			}
			// Artwork miss isn't fatal — mark enriched so we don't retry
			// every 15 seconds. A future background pass can re-try.
		} else if cached {
			t.ArtworkMBID = albumMBID
		}
	}

	// Resolve artist MBID + fetch artist image (Deezer fallback). A
	// TRANSIENT MusicBrainz SearchArtist error propagates here so we skip
	// MarkEnriched and the worker retries this track on the next batch —
	// mirrors the album SearchRelease path. Persistent errors and any
	// Deezer/CAA image-fetch miss return nil (already logged inside).
	if err := e.resolveArtist(ctx, t); err != nil {
		return
	}

	if err := e.store.MarkEnriched(ctx, t); err != nil {
		logger.Error("mark enriched", "path", t.Path, "err", err)
		return
	}
	e.done.Add(1)
}

// resolveArtist fills in t.ArtistMBID and ensures the artist image is
// cached locally. Best-effort: missing Deezer or missing MBID is not a
// failure. Sibling tracks by the same artist share one round-trip each.
func (e *Enricher) resolveArtist(ctx context.Context, t *manifest.Track) error {
	if t.Artist == "" {
		return nil
	}
	key := "artist\x00" + t.Artist
	var artistMBID string
	if cached, ok := e.artistCache.Get(key); ok {
		metrics.RecordMBCache("artist", true)
		artistMBID = cached
	} else {
		metrics.RecordMBCache("artist", false)
		// Pacing now lives inside searchArtistWithFallbacks, which paces
		// EVERY rung — the politeness contract is per-request, and a
		// ladder that paced only its first rung would burst.
		res, err := e.searchArtistWithFallbacks(ctx, t)
		if err != nil {
			// Don't cache transient errors session-wide — a network
			// blip would otherwise block sibling-track retries until
			// process restart. Matches the album-path behavior.
			if ctx.Err() == nil {
				logger.Error("MB artist search", "artist", t.Artist, "err", err)
			}
			// Shutdown/cancellation: propagate the ctx error so enrichOne
			// skips MarkEnriched (mirrors the album SearchRelease path) — no
			// spurious "mark enriched: context canceled" log, and the track
			// retries on the next run. Checked BEFORE IsTransient because a
			// cancel surfaces as context.Canceled, which IsTransient treats
			// as non-transient (→ would otherwise fall through to nil).
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Transient MB error (5xx / 429 / timeout / conn-refused):
			// propagate so enrichOne skips MarkEnriched and the worker
			// retries this track on the next batch when MB recovers —
			// mirrors the album SearchRelease invariant. Persistent errors
			// (404 / decode) fall through to nil so the track is stamped
			// enriched and doesn't spin.
			if IsTransient(err) {
				return err
			}
			// Persistent failure (4xx / decode): cache the empty
			// resolution so sibling tracks by the same artist don't
			// re-hammer the upstream with the same guaranteed-fail
			// query. Mirrors the album path's `albumCache.Set(key,
			// albumResolution{})` on its persistent branch — the artist
			// path was the one that never got it.
			//
			// The asymmetry was measurable: an Atlas-backed bridge
			// re-enriching a 19k-track library logged 106 identical
			// HTTP 400s for a single 2-character artist name (one per
			// track, ~2.5 min of enricher time at the 1.1s pacer) while
			// the album path, for the same class of rejection, logged 7.
			//
			// This is NOT the "no match" case the comment below refuses
			// to cache: a persistent error means the upstream declined
			// to evaluate the query at all, and the same input will be
			// declined identically for the life of the process. An
			// upstream fixed mid-session needs a restart (or the
			// operator's "Retry missing") to be picked up — the same
			// contract the album path already accepts.
			e.artistCache.Set(key, "")
			return nil
		}
		// Only positively cache non-empty MBIDs. Storing the empty string
		// for a "no match" result would session-cache the miss and block
		// sibling-track retries after metadata changes or upstream mismatches
		// — the exact stale behaviour PR #13's review flagged. Transient
		// errors are already handled by the early-return above (no cache
		// write), so this branch is strictly about positive hits. The
		// persistent-error branch above DOES cache "" — a rejected query is
		// not a no-match.
		//
		// The artist LADDER does not change this. It was tempting to start
		// caching no-matches once a miss could cost several rungs instead
		// of one, but measured on the 300 unresolved artists of the
		// production library the ladder generates 1 rung for 79 of them and
		// 2 for 214 — a 1.47x request multiplier on a cold pass, shrinking
		// as artists resolve and get positively cached. That is not worth
		// trading away a documented invariant. See buildArtistLadder.
		//
		// isValidMBID, not just != "": res.MBID lands in ArtistImagePath's
		// filepath.Join as the leading component. Same rationale as the
		// release-side validation above (F30).
		if res != nil && isValidMBID(res.MBID) {
			artistMBID = res.MBID
			e.artistCache.Set(key, artistMBID)
		}
	}
	if artistMBID != "" {
		t.ArtistMBID = artistMBID
	}
	// Fetch Deezer image (the only source in v1 for artist portraits).
	// Cache file is keyed by artist MBID; name-keyed caching is not
	// implemented today — see /v1/artist-image for the MBID-only API.
	//
	// The portrait fetch is tier-2, like album artwork: any failure is
	// logged + absorbed (return nil) so an image miss never blocks the
	// already-resolved ArtistMBID from being committed via MarkEnriched.
	// Only the tier-1 SearchArtist transient case above propagates.
	if e.deezer == nil || artistMBID == "" {
		return nil
	}
	// Negative-cache Deezer misses so sibling tracks by the same artist
	// don't each re-query Deezer for a portrait the API doesn't have.
	if e.deezerNegCache.Has(artistMBID) {
		return nil
	}
	found, err := e.ensureArtistImageCached(ctx, artistMBID, t.Artist)
	if err != nil {
		// Skip the Error log when the failure is just shutdown cancellation
		// (ensureArtistImageCached returns ctx.Err() from its pacing
		// sleepCtx). Mirrors the album/artist tier-1 ctx guard.
		if ctx.Err() == nil {
			logger.Error("artist image", "artist", t.Artist, "mbid", artistMBID, "err", err)
		}
		return nil
	}
	if !found {
		e.deezerNegCache.Set(artistMBID, struct{}{})
	}
	return nil
}

// ensureArtistImageCached downloads and caches the artist's Deezer
// portrait. Returns (true, nil) if a file exists on disk after the
// call at the MBID-keyed path (which the /v1/artist-image handler
// reads). Pre-cached files are a no-op hit.
//
// Dedup strategy (v1.1, see plan §3): the canonical image is stored at
// a name-hashed path (`artist-name-<sha256>.jpg`), and the MBID-keyed
// path the handler reads is a hardlink into the canonical file. Two
// MBIDs pointing to the same artist name collapse into one Deezer
// fetch and one image payload on disk — the second MBID just creates
// another hardlink. Avoids the "alternate-entity twin cache" that
// would otherwise grow with MB's alternate-name MBIDs for popular
// artists (e.g. Nirvana UK vs Nirvana US).
//
// Hardlink fallback: on filesystems without hardlink support (should
// be none for our target deployments — macOS, Linux, NTFS on Windows —
// but doctored ZFS datasets with `xattr=sa` and similar edge cases can
// refuse `os.Link`), we fall back to a file copy. The dedup on network
// (no second Deezer call) is preserved either way.
func (e *Enricher) ensureArtistImageCached(ctx context.Context, mbid, artistName string) (bool, error) {
	mbidPath := ArtistImagePath(e.CacheDir, mbid)
	if _, err := os.Stat(mbidPath); err == nil {
		return true, nil
	}
	// Name-hashed canonical path — populated once per unique artist
	// name regardless of how many MBIDs we see for them.
	namePath := ArtistImagePathByName(e.CacheDir, artistName)
	if _, err := os.Stat(namePath); err == nil {
		// Canonical already exists from a prior MBID for the same
		// artist name (or a prior session). Link and return without a
		// Deezer fetch.
		if err := linkOrCopy(namePath, mbidPath); err != nil {
			return false, fmt.Errorf("link canonical %q → mbid %q: %w", namePath, mbidPath, err)
		}
		return true, nil
	}
	if !sleepCtx(ctx, e.DeezerMinInterval) {
		return false, ctx.Err()
	}
	imgURL, err := e.deezer.SearchArtist(ctx, artistName)
	if err != nil {
		return false, err
	}
	if imgURL == "" {
		return false, nil
	}
	// Deezer image URLs are on their own CDN; second GET happens after
	// a second DeezerMinInterval pause.
	if !sleepCtx(ctx, e.DeezerMinInterval) {
		return false, ctx.Err()
	}
	data, err := e.deezer.FetchImage(ctx, imgURL)
	if err != nil {
		return false, err
	}
	if err := writeArtworkAtomic(namePath, data); err != nil {
		return false, err
	}
	if err := linkOrCopy(namePath, mbidPath); err != nil {
		return false, fmt.Errorf("link mbid path %q → %q: %w", mbidPath, namePath, err)
	}
	return true, nil
}

// ArtistImagePath returns the on-disk cache path for an artist's image,
// keyed by artist MBID. Exposed so the /v1/artist-image handler reads
// from the same location the enricher writes. Under the v1.1 dedup
// strategy this path is a hardlink into the name-hashed canonical
// file (see ArtistImagePathByName); serving behaviour is unchanged.
func ArtistImagePath(cacheDir, mbid string) string {
	return filepath.Join(cacheDir, fmt.Sprintf("artist-%s.jpg", mbid))
}

// CachedArtistImageMBIDs enumerates the artist MBIDs that have a cached
// image on disk — the `artist-<mbid>.jpg` files ArtistImagePath writes —
// as a lowercase-keyed set. One os.ReadDir over the cache dir; the strict
// UUID check on the middle segment naturally excludes the name-hashed
// canonical files (`artist-name-<sha256>.jpg`, see ArtistImagePathByName)
// and the `<mbid>-<size>.jpg` album covers sharing the directory. A
// missing cache dir is "no images yet", not an error (fresh install).
// Used by the admin dashboard's artist-image coverage stats via the
// Deps.ArtistImageMBIDs closure — called behind a 60s TTL cache there,
// so the directory read is off any hot path.
func CachedArtistImageMBIDs(cacheDir string) (map[string]struct{}, error) {
	if cacheDir == "" {
		// An unconfigured dir must not fall through to os.ReadDir(""), which
		// would enumerate the process's working directory (Gemini on PR #495).
		return map[string]struct{}{}, nil
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "artist-") || !strings.HasSuffix(name, ".jpg") {
			continue
		}
		mbid := strings.TrimSuffix(strings.TrimPrefix(name, "artist-"), ".jpg")
		if !isValidMBID(mbid) {
			continue
		}
		out[strings.ToLower(mbid)] = struct{}{}
	}
	return out, nil
}

// artistCaser is shared across the (parallel) enrichment workers.
// cases.Fold() returns a Caser explicitly documented as "stateless and
// safe to use concurrently by multiple goroutines," so one package-level
// instance replaces the per-call construction the previous code did.
var artistCaser = cases.Fold()

// ArtistImagePathByName returns the canonical on-disk cache path for
// an artist's image, keyed by a SHA-256 of the NFC-normalized,
// whitespace-trimmed, case-folded artist name. Matches iOS's
// `MetadataNormalizer.artistID` semantics so both sides key the same
// canonical bytes for the same human-readable artist name.
//
// Uses `cases.Fold()` rather than `strings.ToLower()` for locale-aware
// caseless matching. Critically, the Turkish/Azerbaijani dotted I
// (`İ` U+0130) Unicode-default-folds to `i` + combining dot above
// (`̇`), which is still distinct from plain ASCII `i` after NFC.
// To collapse `İSTANBUL` and `istanbul` to the same key (the user's
// real-world concern when tag writers across different locales emit
// either form) we strip the combining-dot-above codepoint after the
// fold. It only appears as a fold artifact in the dotted-I case;
// other accents (combining acute U+0301 on Beyoncé's é, etc.) use
// different combining marks and are preserved.
//
// `cases.Fold` is somewhat more CPU-intensive than `ToLower`, but the
// shared caser (artistCaser) is built once at package scope and runs
// only once per artist during enrichment — never in a tight parsing
// loop — so the cost is negligible.
//
// Collisions: two distinct artists with the same display name ("Nirvana"
// UK vs Nirvana US) collapse to the same file. iOS already collapses
// them in its library model via the same normalization rules, so the
// UX is consistent end-to-end.
func ArtistImagePathByName(cacheDir, artistName string) string {
	folded := artistCaser.String(strings.TrimSpace(artistName))
	folded = strings.ReplaceAll(folded, "\u0307", "")
	normalized := norm.NFC.String(folded)
	sum := sha256.Sum256([]byte(normalized))
	return filepath.Join(cacheDir, fmt.Sprintf("artist-name-%s.jpg", hex.EncodeToString(sum[:])))
}

// linkOrCopy creates a hardlink from src to dst; falls back to a file
// copy if the filesystem refuses the link. The dedup goal only
// requires one Deezer fetch per artist name; the on-disk dedup is a
// bonus that collapses to "same bytes duplicated" on non-link-capable
// storage.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	// Fallback — copy the bytes via os.ReadFile + atomic rename.
	// Artist portraits are small (< 1 MB typical) so reading into
	// memory is fine; the atomic-write pattern via writeArtworkAtomic
	// keeps concurrent readers from seeing a torn file.
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeArtworkAtomic(dst, data)
}

// Skip reasons. BOUNDED SET — these are the only values that may reach
// markSkipped's `reason` argument or the skipReasons map. A formatted
// error string must never be used as a key (see the skipReasons field).
const (
	// skipReasonNoSearchTerms — the track has no artist or no album tag,
	// so there is nothing to search MusicBrainz by. Only a tag fix or
	// acoustic fingerprinting can recover these.
	skipReasonNoSearchTerms = "no_search_terms"
	// skipReasonNoMBMatch — the query ladder ran clean and MusicBrainz
	// returned nothing acceptable. This is the bucket that shrinks when
	// matching or query shapes improve.
	skipReasonNoMBMatch = "no_mb_match"
	// skipReasonMBError — a PERSISTENT MusicBrainz error (4xx, decode).
	// Transient errors return without stamping and never land here.
	skipReasonMBError = "mb_error"
	// skipReasonNoFingerprintMatch — the text ladder found nothing AND the
	// acoustic fallback had no verdict for this track (not fingerprinted yet,
	// refused by the gate, or unknown to AcoustID). Distinct from
	// no_mb_match so the two populations can be told apart: no_mb_match
	// shrinks when matching improves, this one shrinks when fingerprint
	// coverage does.
	skipReasonNoFingerprintMatch = "no_fingerprint_match"
)

// markSkipped stamps enriched_at so the worker doesn't retry the same
// unsearchable track forever.
//
// `reason` must be one of the skipReason* constants — it keys the
// bounded skipReasons map. `detail` carries the variable part (an error
// string, the query that came back empty) and rides the log line only.
func (e *Enricher) markSkipped(ctx context.Context, t *manifest.Track, reason, detail string) {
	if err := e.store.MarkEnriched(ctx, t); err != nil {
		logger.Error("mark skipped", "path", t.Path, "err", err)
	}
	e.skipped.Add(1)
	e.skipReasonsMu.Lock()
	if e.skipReasons == nil {
		e.skipReasons = make(map[string]int64, 4)
	}
	e.skipReasons[reason]++
	e.skipReasonsMu.Unlock()
	// Info, not Warn: on a library with genuinely untagged files this is
	// the expected steady state, and a Warn-level line per track would
	// train operators to ignore the log.
	logger.Info("enrichment skipped", "path", t.Path, "reason", reason,
		"detail", detail, "artist", t.Artist, "album", t.Album)
}

// SkipReasons returns a copy of the per-reason skip counts accumulated
// since process start (not persisted). Keys are the skipReason*
// constants.
func (e *Enricher) SkipReasons() map[string]int64 {
	e.skipReasonsMu.Lock()
	defer e.skipReasonsMu.Unlock()
	out := make(map[string]int64, len(e.skipReasons))
	for k, v := range e.skipReasons {
		out[k] = v
	}
	return out
}

// ensureArtworkCached fetches (mbid, size) cover bytes from CAA and
// writes them to disk. Returns (true, nil) on hit, (false, errNotFound)
// if neither the release nor the release-group has a front cover,
// (false, err) for other errors. A file already present on disk is a
// hit without a network call.
//
// On a release-level CAA 404, falls back to the release-group front
// cover if a release-group MBID is available (passed in from the
// SearchRelease result, or lazily resolved via ReleaseGroupMBID for
// the embedded-MBID path). The fallback artwork is written to the same
// on-disk path keyed by the RELEASE MBID, so iOS's existing
// `/v1/artwork/{releaseMBID}` request flow serves it transparently —
// no protocol change required.
func (e *Enricher) ensureArtworkCached(ctx context.Context, mbid, rgMBID, artist, album string, size int) (bool, error) {
	path := ArtworkCachePath(e.CacheDir, mbid, size)
	if _, err := os.Stat(path); err == nil {
		return true, nil
	}
	// Phase B: try the authenticated Atlas premium cover first. A hit caches
	// the cross-source premium canonical (Qobuz/Tidal-grade) under the same
	// path the CAA chain would use, so /v1/artwork serves it with zero iOS
	// change. A miss (no credential / 404 / token rejected / Atlas hiccup)
	// falls through to the public CAA chain below. No extra pacing here — the
	// per-track MB cadence already spaces these calls, and TryCache replaces
	// (not adds to) the CAA request on a hit. premiumCovers is nil unless the
	// harvest credential store was wired at startup.
	if e.premiumCovers != nil && e.premiumCovers.TryCache(ctx, path, mbid, size) {
		return true, nil
	}
	if !sleepCtx(ctx, e.CAAMinInterval) { // pace
		return false, ctx.Err()
	}
	body, err := e.caa.FetchReleaseFrontStream(ctx, mbid, size)
	if err == nil {
		// Stream straight to disk so the JPEG body never lands in RAM.
		// Memory bound is ~32 KB (io.Copy default buffer) regardless
		// of image size — was up to 20 MB per concurrent fetch under
		// the buffered Fetch path. Pi-class hosts running fresh-library
		// enrichment now stay bounded under a few MB peak even with
		// multiple in-flight fetches.
		// Scope-bound defer: body is closed when this closure returns
		// (right after the write) — panic-safe, and it never holds the
		// connection across the release-group / iTunes fallbacks below,
		// which a bare function-scoped defer would if a future edit
		// dropped one of the early returns.
		werr := func() error {
			defer body.Close()
			return writeArtworkAtomicStream(path, body, MaxCoverArtBytes)
		}()
		if werr != nil {
			return false, werr
		}
		return true, nil
	}
	// Release-level miss — try release-group fallback. Other errors
	// (network, rate-limit) bubble up unchanged.
	if !IsNotFound(err) {
		return false, err
	}
	rgMBID, rgErr := e.resolveReleaseGroupMBID(ctx, mbid, rgMBID)
	if rgErr != nil {
		// Logging the resolve error but returning the original
		// release-level not-found so callers stamp the "no artwork"
		// state consistently with pre-fallback behaviour.
		logger.Error("release-group lookup", "mbid", mbid, "err", rgErr)
		// Don't bail yet — iTunes may still have the album by name,
		// and a transient MB lookup error shouldn't block the iTunes
		// fallback below.
	} else if rgMBID != "" {
		if !sleepCtx(ctx, e.CAAMinInterval) { // pace the second CAA call
			return false, ctx.Err()
		}
		rgBody, rgFetchErr := e.caa.FetchReleaseGroupFrontStream(ctx, rgMBID, size)
		if rgFetchErr == nil {
			werr := func() error {
				defer rgBody.Close()
				return writeArtworkAtomicStream(path, rgBody, MaxCoverArtBytes)
			}()
			if werr != nil {
				return false, werr
			}
			e.caaFallbackHits.Add(1)
			return true, nil
		}
		if !IsNotFound(rgFetchErr) {
			// Real error from CAA — bubble up rather than masking
			// behind iTunes. Original release-level errNotFound
			// shape is gone here, but a non-404 from CAA is itself
			// signal worth surfacing.
			return false, rgFetchErr
		}
		// rgFetchErr is errNotFound — fall through to iTunes.
	}
	// iTunes last-resort fallback. Only meaningful when we have a
	// human-readable (artist, album) pair to query — the bridge falls
	// back to MB-derived names if the local tags were empty, but on
	// rare untagged-everything releases there's nothing to ask iTunes
	// for. Caches under the same MBID-keyed path so iOS's existing
	// /v1/artwork/{mbid} URL serves it transparently.
	if e.itunes != nil && artist != "" && album != "" {
		itBody, itErr := e.fetchITunesArtwork(ctx, artist, album)
		if itErr == nil {
			// Stream straight to disk — same shape as the CAA branches
			// above. Body close + size cap (MaxCoverArtBytes) live
			// inside writeArtworkAtomicStream + io.LimitReader; the
			// iTunes path now inherits the ~32 KB peak-RAM profile of
			// the CAA fetches rather than buffering the whole image.
			werr := func() error {
				defer itBody.Close()
				return writeArtworkAtomicStream(path, itBody, MaxCoverArtBytes)
			}()
			if werr != nil {
				return false, werr
			}
			e.itunesFallbackHits.Add(1)
			return true, nil
		}
		if !IsNotFound(itErr) && ctx.Err() == nil {
			// Log iTunes errors but don't fail the whole call —
			// the original release-level errNotFound is the more
			// useful signal for the caller. The ctx.Err() guard
			// suppresses the spurious Error on a clean shutdown, where
			// fetchITunesArtwork returns ctx.Err() from its pacing sleepCtx.
			logger.Error("iTunes fallback", "artist", artist, "album", album, "err", itErr)
		}
	}
	return false, err
}

// fetchITunesArtwork is the rate-paced iTunes search + artwork
// download helper. Sleeps `ITunesMinInterval` between the two
// network round-trips — search returns metadata in ~300 ms; the
// 600x600 image fetch is on Apple's CDN with no public rate limit
// but the second sleep keeps us courteous.
//
// Returns errNotFound (compatible with `IsNotFound`) when iTunes had
// nothing for (artist, album). All other errors bubble up unchanged.
// Returns an io.ReadCloser the caller MUST close — the body streams
// straight to disk in `ensureArtworkCached` via `writeArtworkAtomicStream`.
func (e *Enricher) fetchITunesArtwork(ctx context.Context, artist, album string) (io.ReadCloser, error) {
	// Use the ctx-aware `sleepCtx` helper rather than `time.Sleep` so
	// shutdown / cancellation isn't blocked by up to ~2× ITunesMinInterval
	// (default 6s) per in-flight iTunes call. Matches the pacing pattern
	// used in `Run` for the empty-batch poll.
	if !sleepCtx(ctx, e.ITunesMinInterval) {
		return nil, ctx.Err()
	}
	hit, err := e.itunes.SearchAlbum(ctx, artist, album)
	if err != nil {
		return nil, err
	}
	if hit == nil {
		return nil, errNotFound
	}
	if !sleepCtx(ctx, e.ITunesMinInterval) {
		return nil, ctx.Err()
	}
	return e.itunes.FetchArtwork(ctx, hit)
}

// resolveReleaseGroupMBID returns the release-group MBID for a release,
// checking the hinted value first (from SearchRelease) and falling back
// to a targeted MB lookup only when none was provided. Caches the
// lookup result so sibling tracks on the same release share it.
// Negative results (release with no release-group) cache as "" so we
// don't re-query.
func (e *Enricher) resolveReleaseGroupMBID(ctx context.Context, releaseMBID, hint string) (string, error) {
	if hint != "" {
		return hint, nil
	}
	if cached, ok := e.releaseGroupCache.Get(releaseMBID); ok {
		metrics.RecordMBCache("release_group", true)
		return cached, nil
	}
	metrics.RecordMBCache("release_group", false)
	if !sleepCtx(ctx, e.MBMinInterval) { // pace
		return "", ctx.Err()
	}
	rg, err := e.mb.ReleaseGroupMBID(ctx, releaseMBID)
	if err != nil {
		// Negative-cache a PERSISTENT failure as "" so sibling tracks on the
		// same release — all sharing releaseMBID via the album cache — don't
		// each re-issue the identical guaranteed-fail lookup, every one paced
		// at MBMinInterval. "Persistent" is the full non-transient set, not
		// just 404: a JSON-decode / schema-drift / persistent-4xx error will
		// fail every retry too, so caching it stops the sibling re-hammer.
		// Transient failures (network blip, 5xx, 429, timeout) are LEFT
		// uncached so the next enrichment pass retries them. The ctx.Err()
		// guard is load-bearing: IsTransient treats context.Canceled as
		// non-transient, so without it a shutdown cancel would poison the
		// cache for a release we never actually looked up. This mirrors the
		// transient-vs-persistent split SearchRelease already makes in
		// enrichOne (ctx-cancel first, then IsTransient, then cache-empty).
		if !IsTransient(err) && ctx.Err() == nil {
			e.releaseGroupCache.Set(releaseMBID, "")
		}
		return "", err
	}
	// Validate before caching: this value is interpolated into the CAA
	// release-group URL, so an upstream returning a path-bearing id would
	// steer the fetch. Same rationale as the release/artist search results
	// (F30); an invalid value is cached as "" (a definitive miss) so sibling
	// tracks don't re-query the same bad answer.
	if !isValidMBID(rg) {
		if rg != "" {
			logger.Warn("ignoring non-UUID release-group MBID from upstream",
				"releaseMBID", releaseMBID, "value", truncateForLog(rg))
		}
		e.releaseGroupCache.Set(releaseMBID, "")
		return "", nil
	}
	e.releaseGroupCache.Set(releaseMBID, rg)
	return rg, nil
}

// ArtworkCachePath returns the canonical on-disk path for an (mbid, size)
// cached image. Exposed so the /v1/artwork handler can read from the same
// location.
func ArtworkCachePath(cacheDir, mbid string, size int) string {
	return filepath.Join(cacheDir, fmt.Sprintf("%s-%d.jpg", mbid, size))
}

// writeArtworkAtomicStream streams bytes from src to path via tmp-file
// + rename, capping the read at max+1 bytes to detect oversized inputs
// without buffering the whole stream. Memory bound is ~32 KB
// (io.Copy's default buffer) regardless of input size, vs.
// writeArtworkAtomic's []byte which holds the entire body in RAM.
//
// On rename collision (race against a sibling enricher / scanner) the
// existing file is verified for byte-equivalence by SHA-256 hash
// compare. A SHA-256 of the streamed bytes is computed inline (via
// io.MultiWriter) so the streaming property is preserved — neither
// the streamed content nor the on-disk file is buffered in full
// memory. Pre-fix accepted on size-match alone; that contradicted
// the existing writeArtworkAtomic rationale (size-only is insufficient
// because two different images can share a byte length, and a partial
// write from a prior crash on the destination could leave correct-
// sized but corrupt content) (qodo correctness review on PR #123).
//
// Caller is responsible for closing src when it's an io.ReadCloser
// (HTTP body); the helper signature is io.Reader so a buffered byte
// slice can also be tested via bytes.NewReader without a fake closer.
//
// `maxBytes` parameter (not `max`) so it doesn't shadow Go 1.21's
// builtin `max` (CodeRabbit nit on PR #123).
func writeArtworkAtomicStream(path string, src io.Reader, maxBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Belt-and-braces chmod: MkdirAll is umask-affected. 0o700 has no
	// group/other bits to mask, so on every Unix `os.MkdirAll` lands
	// at exactly 0o700 regardless of umask, but the explicit Chmod
	// future-proofs against either a refactor that loosens the
	// requested mode OR the historical behavior on non-Unix.
	// Mirror of the auth/config setup pattern (qodo on PR #123).
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil && !os.IsNotExist(err) {
		// Don't fail on a chmod error for a cache directory — the
		// directory creation itself succeeded; this is a best-effort
		// hardening pass.
		logger.Debug("artwork cache chmod hardening", "dir", filepath.Dir(path), "err", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".caa-*.jpg.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup && tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()

	// io.MultiWriter tees every byte to disk AND a SHA-256 hash sink
	// in a single pass. The hash is consumed only on rename-collision
	// for byte-equivalence verification — fast path pays one hash
	// (~1 GB/s on Apple Silicon) for a multi-MB image, well below
	// network fetch latency.
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(src, maxBytes+1))
	if err != nil {
		return err
	}
	if n > maxBytes {
		return fmt.Errorf("artwork exceeds %d-byte limit", maxBytes)
	}
	// Refuse zero-byte writes. The buffered `writeArtworkAtomic` /
	// pre-streaming iTunes path implicitly guarded this via a
	// `len(itData) > 0` check at the call site; the streaming
	// refactor (PR #143) dropped that, so a 200 OK with an empty
	// body would land a 0-byte file on disk that ensureArtworkCached
	// then treats as a permanent cache hit (the existence check is
	// `os.Stat(path) == nil` with no size validation). Refusing
	// here benefits all streaming callers (CAA release, CAA
	// release-group, iTunes) — an empty 200 from any of them is
	// equally bogus (qodo bot review on PR #143).
	if n == 0 {
		return fmt.Errorf("artwork body was empty")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	streamedHash := hasher.Sum(nil)

	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		// Race / locking window: existing destination present. Verify
		// byte-equivalence via streaming SHA-256 over the existing file
		// — neither side is buffered in full memory. Size mismatch
		// short-circuits before the read.
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() != n {
			return err
		}
		existingHash, hashErr := hashFile(path)
		if hashErr != nil || !bytes.Equal(existingHash, streamedHash) {
			return err
		}
		// Existing file is byte-equivalent — accept the rename failure
		// and let the deferred tmp cleanup remove our orphan. Caller
		// gets a successful result, same as a normal rename.
		return nil
	}
	cleanup = false
	return nil
}

// hashFile streams the file at path through SHA-256 and returns the
// digest. Used by writeArtworkAtomicStream's rename-collision path
// to verify an existing destination is byte-equivalent to the
// streamed write — the streaming property of the helper depends on
// neither side ever loading the full content into memory.
func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// writeArtworkAtomic is the buffered atomic-write helper for
// enricher-side artwork cache writes. Routes through the shared
// `internal/atomicwrite.WriteBytes` helper so the tmp-file +
// fsync + rename-with-retry + byte-equality fallback contract
// stays defined in exactly one place. The `.caa-*.jpg.tmp` prefix
// is preserved as the diagnostic shape: a stale tmp left on disk
// after a crash tells operators the enricher (CAA / iTunes /
// Deezer fetch) was the writer — not the scanner-side path
// (which uses `.scan-*.jpg.tmp`).
//
// concurrent reader never sees a torn file.
//
// Cache directory perms are 0o700 (owner-only) — application-owned
// caches shouldn't be world-readable on POSIX. Mirrors the
// scanner-side `writeArtworkAtomicScan` so whichever writer touches
// the dir first creates it at the same mode. Upgrades from prior
// 0o755 deployments are accepted: existing dirs keep their mode
// until a clean install / rmdir; new dirs land at 0o700.
//
// **Prefer `writeArtworkAtomicStream` for new code.** The buffered
// shape exists for the byte-equivalence collision check on the
// scanner-side path; the enricher should stream straight from HTTP.
func writeArtworkAtomic(path string, data []byte) error {
	return atomicwrite.WriteBytes(path, data, ".caa-*.jpg.tmp")
}

func cacheKey(artist, album string) string { return artist + "\x00" + album }

// albumEditionSuffixRE matches a trailing parenthesised or bracketed qualifier —
// "(Deluxe Version)", "(Expanded Edition)", "(Remastered)", "(2020 Deluxe)",
// "(Original Motion Picture Soundtrack)".
//
// Deliberately generic rather than a keyword list: the qualifiers in the wild
// are endless ("Super Deluxe", "Remastered Hi-Res Version", "Japanese edition"),
// and a list would need extending forever. Anchored to the END, so a title that
// OPENS with a parenthetical — "(I Can't Get No) Satisfaction" — is untouched.
var albumEditionSuffixRE = regexp.MustCompile(`\s*[\(\[][^\(\)\[\]]*[\)\]]\s*$`)

// stripAlbumEditionSuffix removes one trailing edition qualifier, returning ""
// when there is nothing to strip or stripping would empty the title.
func stripAlbumEditionSuffix(album string) string {
	out := strings.TrimSpace(albumEditionSuffixRE.ReplaceAllString(album, ""))
	if out == "" || out == strings.TrimSpace(album) {
		return ""
	}
	return out
}

// searchReleaseWithFallbacks resolves an album, retrying with a broader query
// when the tags as written don't match MusicBrainz.
//
// The first attempt is what the enricher has always done: the track's OWN
// artist plus the album title verbatim. Two things make that miss on
// well-formed libraries:
//
//   - COMPILATIONS. A release credited to "Various Artists" can never match a
//     track whose artist is "Bon Jovi", so every track on every compilation
//     fails — permanently, since a miss stamps enriched_at. AlbumArtist is what
//     the release is actually credited to. The same holds wherever the
//     per-track artist is narrower than the release credit, and for files whose
//     artist tag is junk: "Load" by "[ME] Load [145412591] [1996]" misses,
//     "Load" by "Metallica" resolves.
//   - EDITION SUFFIXES. "Goats Head Soup (2020 Deluxe)" is not a MusicBrainz
//     release title; "Goats Head Soup" is.
//
// Measured on 180 sampled albums the production bridge had given up on: 67
// recovered by the artist retry, 25 by the suffix retry, 7 needed both —
// 99 of 180 (55%).
//
// Only reached on a cache MISS, and the caller caches the outcome under the
// original (artist, album) key, so an album pays these at most once per process
// no matter how many tracks it has.
//
// ERROR SEMANTICS ARE UNCHANGED. A fallback runs only after a clean (nil, nil)
// "no plausible match". Any error — transient or persistent — returns
// immediately, exactly as the single-attempt version did, so the caller's
// transient-retry and negative-cache contracts still hold.
func (e *Enricher) searchReleaseWithFallbacks(ctx context.Context, t *manifest.Track) (*SearchResult, error) {
	// Shapes, order, dedup and cap all live in buildReleaseLadder — see
	// its docblock for why the order is strictly additive.
	attempts := buildReleaseLadder(t.Artist, t.AlbumArtist, t.Album)

	for i, a := range attempts {
		// Pace every attempt, not just the first — these are real upstream
		// calls and the politeness contract is per-request.
		if !sleepCtx(ctx, e.MBMinInterval) {
			return nil, ctx.Err()
		}
		res, err := e.mb.SearchRelease(ctx, a.artist, a.album)
		if err != nil {
			return nil, err
		}
		if res != nil {
			if i > 0 {
				logger.Info("MB search matched on a fallback query",
					"path", t.Path, "attempt", i,
					"searchedArtist", a.artist, "searchedAlbum", a.album)
			}
			return res, nil
		}
	}
	return nil, nil
}

// sleepCtx sleeps for d or until ctx is done. Returns true if the sleep
// completed normally.
//
// Uses `time.NewTimer` + `Stop` rather than `time.After` so that on
// the cancellation path the underlying timer is released immediately
// instead of remaining in the runtime's timer heap until d elapses
// (qodo bot review on PR #140). Matters more now that the helper is
// invoked from six additional rate-pacing call sites with intervals
// up to several seconds — a SIGTERM mid-pace would otherwise leave a
// pending timer per in-flight enrichOne call until the original
// duration elapsed. Fast-path d <= 0 returns true without scheduling.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		// Stop returns false if the timer already fired or was stopped;
		// in the "already fired" race we drain the channel so the
		// runtime can reclaim the timer cleanly.
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		return false
	}
}
