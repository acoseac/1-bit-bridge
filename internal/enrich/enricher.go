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
// the manifest store, looks them up against MusicBrainz, caches front
// covers from Cover Art Archive, and writes the enriched data back.
type Enricher struct {
	store *manifest.Store
	mb    *MusicBrainzClient
	caa   *CoverArtClient

	// CacheDir is the root where the artwork byte cache lives, one file
	// per (mbid, size) tuple. Created on demand.
	CacheDir string

	// MBMinInterval is the minimum gap between MusicBrainz requests. MB's
	// anonymous rate limit is 1/s; 1.1s gives us headroom.
	MBMinInterval time.Duration
	// CAAMinInterval is the minimum gap between Cover Art Archive
	// requests. CAA is more forgiving but we stay polite.
	CAAMinInterval time.Duration

	// BatchLimit is the maximum number of un-enriched tracks processed
	// per wakeup. Keeps the worker responsive to cancellation.
	BatchLimit int

	// PollInterval is how long to wait between empty-batch checks.
	PollInterval time.Duration

	// albumCache memoizes (artist, album) → ArtworkMBID so tracks on the
	// same album share a single MB round-trip. In-memory, lives as long
	// as the Enricher.
	albumCache sync.Map

	// progress counters exposed via ScanState.
	done    atomic.Int64
	skipped atomic.Int64
}

// NewEnricher wires a store + MB/CAA clients + cache dir into a worker.
// Sensible defaults applied if the numeric fields are zero.
func NewEnricher(store *manifest.Store, mb *MusicBrainzClient, caa *CoverArtClient, cacheDir string) *Enricher {
	e := &Enricher{
		store:          store,
		mb:             mb,
		caa:            caa,
		CacheDir:       cacheDir,
		MBMinInterval:  1100 * time.Millisecond,
		CAAMinInterval: 500 * time.Millisecond,
		BatchLimit:     100,
		PollInterval:   15 * time.Second,
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
				log.Printf("enricher: MB search %q / %q: %v", t.Artist, t.Album, err)
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

	if err := e.store.MarkEnriched(t); err != nil {
		log.Printf("enricher: mark enriched %q: %v", t.Path, err)
		return
	}
	e.done.Add(1)
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
