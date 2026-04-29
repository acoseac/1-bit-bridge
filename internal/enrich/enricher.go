package enrich

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/lrucache"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

var logger = logging.Component("enricher")

// Enricher is a long-running worker that pulls un-enriched tracks from
// the manifest store, looks them up against MusicBrainz / Deezer, caches
// artwork locally, and writes the enriched data back.
type Enricher struct {
	store  *manifest.Store
	mb     *MusicBrainzClient
	caa    *CoverArtClient
	itunes *ITunesClient // optional; nil disables the iTunes fallback
	deezer *DeezerClient

	// CacheDir is the root where the cached JPEGs live. Album covers go
	// in <CacheDir>/<mbid>-<size>.jpg (see ArtworkCachePath); artist
	// images go in <CacheDir>/artist-<mbid>.jpg (see ArtistImagePath).
	CacheDir string

	// MBMinInterval is the minimum gap between MusicBrainz requests. MB's
	// anonymous rate limit is 1/s; 1.1s gives us headroom.
	MBMinInterval time.Duration
	// CAAMinInterval is the minimum gap between Cover Art Archive
	// requests. CAA is more forgiving but we stay polite.
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
	e := &Enricher{
		store:             store,
		mb:                mb,
		caa:               caa,
		deezer:            deezer,
		CacheDir:          cacheDir,
		MBMinInterval:     1100 * time.Millisecond,
		CAAMinInterval:    500 * time.Millisecond,
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

// Run loops until ctx is done, pulling un-enriched tracks and processing
// them in waves. Errors on individual tracks are logged but don't stop
// the loop.
func (e *Enricher) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		batch, err := e.store.UnenrichedTracks(e.BatchLimit)
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
	}
}

// Done returns the number of tracks processed by this Enricher so far
// (the count resets when the process restarts; it's not persisted).
func (e *Enricher) Done() int64 { return e.done.Load() }

func (e *Enricher) enrichOne(ctx context.Context, t *manifest.Track) {
	// Skip tracks that have no artist+album info to search by. Mark them
	// done anyway so we don't poll them forever.
	if t.Artist == "" || t.Album == "" {
		e.markSkipped(t, "no artist/album to search by")
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
			albumMBID = res.ReleaseMBID
			rgMBID = res.ReleaseGroupMBID
		} else {
			time.Sleep(e.MBMinInterval) // pace
			res, err := e.mb.SearchRelease(ctx, t.Artist, t.Album)
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
				e.markSkipped(t, fmt.Sprintf("MB error: %v", err))
				return
			}
			resolution := albumResolution{}
			if res != nil {
				resolution.ReleaseMBID = res.MBID
				resolution.ReleaseGroupMBID = res.ReleaseGroupMBID
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
		e.markSkipped(t, "no MB match")
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
			logger.Error("artwork", "mbid", albumMBID, "err", err)
			// Artwork miss isn't fatal — mark enriched so we don't retry
			// every 15 seconds. A future background pass can re-try.
		} else if cached {
			t.ArtworkMBID = albumMBID
		}
	}

	// Resolve artist MBID + fetch artist image (Deezer fallback).
	e.resolveArtist(ctx, t)

	if err := e.store.MarkEnriched(t); err != nil {
		logger.Error("mark enriched", "path", t.Path, "err", err)
		return
	}
	e.done.Add(1)
}

// resolveArtist fills in t.ArtistMBID and ensures the artist image is
// cached locally. Best-effort: missing Deezer or missing MBID is not a
// failure. Sibling tracks by the same artist share one round-trip each.
func (e *Enricher) resolveArtist(ctx context.Context, t *manifest.Track) {
	if t.Artist == "" {
		return
	}
	key := "artist\x00" + t.Artist
	var artistMBID string
	if cached, ok := e.artistCache.Get(key); ok {
		artistMBID = cached
	} else {
		time.Sleep(e.MBMinInterval)
		res, err := e.mb.SearchArtist(ctx, t.Artist)
		if err != nil {
			// Don't cache transient errors session-wide — a network
			// blip would otherwise block sibling-track retries until
			// process restart. Matches the album-path behavior.
			if ctx.Err() == nil {
				logger.Error("MB artist search", "artist", t.Artist, "err", err)
			}
			return
		}
		// Only positively cache non-empty MBIDs. Storing the empty string
		// for a "no match" result would session-cache the miss and block
		// sibling-track retries after metadata changes or upstream mismatches
		// — the exact stale behaviour PR #13's review flagged. Transient
		// errors are already handled by the early-return above (no cache
		// write), so this branch is strictly about positive hits.
		if res != nil && res.MBID != "" {
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
	if e.deezer == nil || artistMBID == "" {
		return
	}
	// Negative-cache Deezer misses so sibling tracks by the same artist
	// don't each re-query Deezer for a portrait the API doesn't have.
	if e.deezerNegCache.Has(artistMBID) {
		return
	}
	found, err := e.ensureArtistImageCached(ctx, artistMBID, t.Artist)
	if err != nil {
		logger.Error("artist image", "artist", t.Artist, "mbid", artistMBID, "err", err)
		return
	}
	if !found {
		e.deezerNegCache.Set(artistMBID, struct{}{})
	}
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
	time.Sleep(e.DeezerMinInterval)
	imgURL, err := e.deezer.SearchArtist(ctx, artistName)
	if err != nil {
		return false, err
	}
	if imgURL == "" {
		return false, nil
	}
	// Deezer image URLs are on their own CDN; second GET happens after
	// a second DeezerMinInterval pause.
	time.Sleep(e.DeezerMinInterval)
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

// ArtistImagePathByName returns the canonical on-disk cache path for
// an artist's image, keyed by a SHA-256 of the NFC-normalized,
// whitespace-trimmed, lowercased artist name. Matches iOS's
// `MetadataNormalizer.artistID` semantics so both sides key the same
// canonical bytes for the same human-readable artist name.
//
// Collisions: two distinct artists with the same display name ("Nirvana"
// UK vs Nirvana US) collapse to the same file. iOS already collapses
// them in its library model via the same normalization rules, so the
// UX is consistent end-to-end.
func ArtistImagePathByName(cacheDir, artistName string) string {
	normalized := norm.NFC.String(strings.ToLower(strings.TrimSpace(artistName)))
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

// markSkipped stamps enriched_at so the worker doesn't retry the same
// unsearchable track forever.
func (e *Enricher) markSkipped(t *manifest.Track, reason string) {
	_ = reason // kept for future logging/observability
	if err := e.store.MarkEnriched(t); err != nil {
		logger.Error("mark skipped", "path", t.Path, "err", err)
	}
	e.skipped.Add(1)
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
	time.Sleep(e.CAAMinInterval) // pace
	data, err := e.caa.FetchReleaseFront(ctx, mbid, size)
	if err == nil {
		if werr := writeArtworkAtomic(path, data); werr != nil {
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
		time.Sleep(e.CAAMinInterval) // pace the second CAA call
		rgData, rgFetchErr := e.caa.FetchReleaseGroupFront(ctx, rgMBID, size)
		if rgFetchErr == nil {
			if werr := writeArtworkAtomic(path, rgData); werr != nil {
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
		if itData, itErr := e.fetchITunesArtwork(ctx, artist, album); itErr == nil && len(itData) > 0 {
			if werr := writeArtworkAtomic(path, itData); werr != nil {
				return false, werr
			}
			e.itunesFallbackHits.Add(1)
			return true, nil
		} else if itErr != nil && !IsNotFound(itErr) {
			// Log iTunes errors but don't fail the whole call —
			// the original release-level errNotFound is the more
			// useful signal for the caller.
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
func (e *Enricher) fetchITunesArtwork(ctx context.Context, artist, album string) ([]byte, error) {
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
		return cached, nil
	}
	time.Sleep(e.MBMinInterval) // pace
	rg, err := e.mb.ReleaseGroupMBID(ctx, releaseMBID)
	if err != nil {
		// Do not negative-cache on error — transient network failures
		// should retry on the next enrichment pass.
		return "", err
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

// writeArtworkAtomic writes bytes to path via tmp-file + rename so a
// concurrent reader never sees a torn file.
//
// Cache directory perms are 0o700 (owner-only) — application-owned
// caches shouldn't be world-readable on POSIX. Mirrors the
// scanner-side `writeArtworkAtomicScan` so whichever writer touches
// the dir first creates it at the same mode. Upgrades from prior
// 0o755 deployments are accepted: existing dirs keep their mode
// until a clean install / rmdir; new dirs land at 0o700.
func writeArtworkAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".caa-*.jpg.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

func cacheKey(artist, album string) string { return artist + "\x00" + album }

// sleepCtx sleeps for d or until ctx is done. Returns true if the sleep
// completed normally.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
