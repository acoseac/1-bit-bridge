package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// mbReleaseHit is a valid MB release-search response so enrichOne's album
// resolution succeeds and control reaches resolveArtist.
const mbReleaseHit = `{"releases":[{"id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`

// newArtistErrEnricher builds an Enricher whose MB stub serves a release hit
// but responds to the artist-search endpoint (`/artist/…`) with artistStatus.
// The CAA stub returns a valid JPEG so the release-level artwork fetch hits
// and enrichOne proceeds straight to resolveArtist without any RG/iTunes
// fallback traffic. Rate pacing is zeroed so the synchronous enrichOne call
// doesn't sleep.
func newArtistErrEnricher(t *testing.T, artistStatus int) (*Enricher, *manifest.Store, *manifest.Track) {
	t.Helper()
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/artist") {
			// No Retry-After header, so get() returns the status
			// immediately rather than pacing.
			w.WriteHeader(artistStatus)
			return
		}
		io.WriteString(w, mbReleaseHit)
	}))
	t.Cleanup(mbSrv.Close)
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0xFF, 0xD8, 0xFF}) // minimal valid JPEG SOI
	}))
	t.Cleanup(caaSrv.Close)

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tr := &manifest.Track{Path: "x.flac", Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album"}
	if err := store.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.DeezerMinInterval = 0
	return e, store, tr
}

// TestResolveArtist_TransientSearchArtistErrorLeavesTrackUnenriched locks the
// finding-9 fix: a TRANSIENT MusicBrainz SearchArtist error (5xx) must NOT
// stamp enriched_at, so the worker retries the track on the next batch when MB
// recovers — mirroring the album SearchRelease invariant. Pre-fix, resolveArtist
// swallowed the error and enrichOne stamped the track enriched, permanently
// losing its ArtistMBID/portrait over a brief blip.
func TestResolveArtist_TransientSearchArtistErrorLeavesTrackUnenriched(t *testing.T) {
	e, store, tr := newArtistErrEnricher(t, http.StatusServiceUnavailable)

	e.enrichOne(context.Background(), tr)

	remaining, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("track stamped enriched despite a transient SearchArtist error; UnenrichedTracks=%d, want 1", len(remaining))
	}
}

// TestResolveArtist_PersistentSearchArtistErrorStampsEnriched is the
// complementary guard: a PERSISTENT SearchArtist error (404) must stamp
// enriched_at so the worker doesn't spin on it forever — only transient errors
// retry.
func TestResolveArtist_PersistentSearchArtistErrorStampsEnriched(t *testing.T) {
	e, store, tr := newArtistErrEnricher(t, http.StatusNotFound)

	e.enrichOne(context.Background(), tr)

	remaining, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("track left unenriched after a persistent SearchArtist error; UnenrichedTracks=%d, want 0", len(remaining))
	}
}
