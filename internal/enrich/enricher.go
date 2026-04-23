package enrich

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Enricher is a long-running worker that pulls un-enriched tracks from
// the manifest store, looks them up against MusicBrainz / Deezer, caches
// artwork locally, and writes the enriched data back.
type Enricher struct {
	store  *manifest.Store
	mb     *MusicBrainzClient
	caa    *CoverArtClient
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
	// DeezerMinInterval is the minimum gap between Deezer requests.
	// Deezer's unauth limit is ~50 req / 5s; 120ms is well under that.
	DeezerMinInterval time.Duration

	// BatchLimit is the maximum number of un-enriched tracks processed
	// per wakeup. Keeps the worker responsive to cancellation.
	BatchLimit int

	// PollInterval is how long to wait between empty-batch checks.
	PollInterval time.Duration

	// albumCache memoizes (artist, album) → ArtworkMBID so tracks on the
	// same album share a single MB round-trip. In-memory, lives as long
	// as the Enricher.
	albumCache sync.Map

	// artistCache memoizes artist-name → ArtistMBID so sibling tracks by
	// the same artist share the lookup + image-fetch.
	artistCache sync.Map

	// deezerNegCache remembers "Deezer had no portrait" per artist MBID
	// so sibling tracks don't each re-query. Populated with struct{}{}.
	deezerNegCache sync.Map

	// progress counters exposed via ScanState.
	done    atomic.Int64
	skipped atomic.Int64
}

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
		DeezerMinInterval: 120 * time.Millisecond,
		BatchLimit:        100,
		PollInterval:      15 * time.Second,
	}
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
			log.Printf("enricher: list unenriched: %v", err)
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
	// try to grab artwork for it.
	albumMBID := t.MusicBrainzAlbumID
	if albumMBID == "" {
		// Cache by (artist, album) so sibling tracks on the same album
		// share one MB call.
		key := cacheKey(t.Artist, t.Album)
		if cached, ok := e.albumCache.Load(key); ok {
			albumMBID = cached.(string)
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
				log.Printf("enricher: MB search %q / %q: %v", t.Artist, t.Album, err)
				// Cache the failure as an empty MBID so sibling tracks
				// on the same album don't re-hammer MB with the same
				// query and hit the same error. This matters for
				// persistent decode errors (e.g. schema drift) where
				// every retry is guaranteed to fail — without the
				// cache, the worker loops forever on an N-track album.
				// Successful searches populate the same cache entry
				// with a real MBID the next pass over.
				e.albumCache.Store(key, "")
				e.markSkipped(t, fmt.Sprintf("MB error: %v", err))
				return
			}
			if res != nil {
				albumMBID = res.MBID
			}
			e.albumCache.Store(key, albumMBID)
		}
		// Propagate to the track whether we hit cache or searched fresh.
		if albumMBID != "" {
			t.MusicBrainzAlbumID = albumMBID
		}
	}

	if albumMBID == "" {
		e.markSkipped(t, "no MB match")
		return
	}

	// Fetch and cache 500px front cover. If the file already exists, we
	// skip the network round-trip entirely.
	if cached, err := e.ensureArtworkCached(ctx, albumMBID, 500); err != nil {
		log.Printf("enricher: artwork %s: %v", albumMBID, err)
		// Artwork miss isn't fatal — mark enriched so we don't retry
		// every 15 seconds. A future background pass can re-try.
	} else if cached {
		t.ArtworkMBID = albumMBID
	}

	// Resolve artist MBID + fetch artist image (Deezer fallback).
	e.resolveArtist(ctx, t)

	if err := e.store.MarkEnriched(t); err != nil {
		log.Printf("enricher: mark enriched %q: %v", t.Path, err)
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
	if cached, ok := e.artistCache.Load(key); ok {
		artistMBID = cached.(string)
	} else {
		time.Sleep(e.MBMinInterval)
		res, err := e.mb.SearchArtist(ctx, t.Artist)
		if err != nil {
			// Don't cache transient errors session-wide — a network
			// blip would otherwise block sibling-track retries until
			// process restart. Matches the album-path behavior.
			if ctx.Err() == nil {
				log.Printf("enricher: MB artist search %q: %v", t.Artist, err)
			}
			return
		}
		if res != nil {
			artistMBID = res.MBID
		}
		e.artistCache.Store(key, artistMBID)
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
	if _, miss := e.deezerNegCache.Load(artistMBID); miss {
		return
	}
	found, err := e.ensureArtistImageCached(ctx, artistMBID, t.Artist)
	if err != nil {
		log.Printf("enricher: artist image %q (%s): %v", t.Artist, artistMBID, err)
		return
	}
	if !found {
		e.deezerNegCache.Store(artistMBID, struct{}{})
	}
}

// ensureArtistImageCached downloads and caches the artist's Deezer
// portrait at <CacheDir>/artist-<mbid>.jpg. Returns (true, nil) if a file
// exists on disk after the call. Pre-cached files are a no-op hit.
func (e *Enricher) ensureArtistImageCached(ctx context.Context, mbid, artistName string) (bool, error) {
	path := ArtistImagePath(e.CacheDir, mbid)
	if _, err := os.Stat(path); err == nil {
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
	if err := writeArtworkAtomic(path, data); err != nil {
		return false, err
	}
	return true, nil
}

// ArtistImagePath returns the on-disk cache path for an artist's image,
// keyed by artist MBID. Exposed so the /v1/artist-image handler reads
// from the same location the enricher writes.
func ArtistImagePath(cacheDir, mbid string) string {
	return filepath.Join(cacheDir, fmt.Sprintf("artist-%s.jpg", mbid))
}

// markSkipped stamps enriched_at so the worker doesn't retry the same
// unsearchable track forever.
func (e *Enricher) markSkipped(t *manifest.Track, reason string) {
	_ = reason // kept for future logging/observability
	if err := e.store.MarkEnriched(t); err != nil {
		log.Printf("enricher: mark skipped %q: %v", t.Path, err)
	}
	e.skipped.Add(1)
}

// ensureArtworkCached fetches (mbid, size) cover bytes from CAA and
// writes them to disk. Returns (true, nil) on hit, (false, errNotFound)
// if CAA has no cover, (false, err) for other errors. A file already
// present on disk is a hit without a network call.
func (e *Enricher) ensureArtworkCached(ctx context.Context, mbid string, size int) (bool, error) {
	path := ArtworkCachePath(e.CacheDir, mbid, size)
	if _, err := os.Stat(path); err == nil {
		return true, nil
	}
	time.Sleep(e.CAAMinInterval) // pace
	data, err := e.caa.FetchReleaseFront(ctx, mbid, size)
	if err != nil {
		return false, err
	}
	if err := writeArtworkAtomic(path, data); err != nil {
		return false, err
	}
	return true, nil
}

// ArtworkCachePath returns the canonical on-disk path for an (mbid, size)
// cached image. Exposed so the /v1/artwork handler can read from the same
// location.
func ArtworkCachePath(cacheDir, mbid string, size int) string {
	return filepath.Join(cacheDir, fmt.Sprintf("%s-%d.jpg", mbid, size))
}

// writeArtworkAtomic writes bytes to path via tmp-file + rename so a
// concurrent reader never sees a torn file.
func writeArtworkAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
