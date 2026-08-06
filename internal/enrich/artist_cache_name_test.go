package enrich

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestArtistCacheHitUsesCanonicalNameForDeezer pins the artistCache VALUE
// contract: it stores `<mbid>\x00<canonicalName>`, so a sibling track that
// hits the cache queries Deezer under the name MusicBrainz actually matched
// — not the raw multi-credit tag.
//
// Why this needs its own test rather than riding the existing F4 coverage:
// the F4 fix only touched the cache-MISS path, where `resolveArtist` has
// `res.Title` in hand. On a HIT the function used to fall back to `t.Artist`,
// which is exactly the multi-credit tag F4 exists to keep away from Deezer.
//
// The window is real but narrow, which is why the fixture has to construct
// it deliberately: a cache hit normally means a sibling already cached the
// portrait on disk (so `ensureArtistImageCached` returns before the name is
// read) or Deezer had none (so `deezerNegCache` short-circuits). Neither
// holds when the sibling's IMAGE fetch failed transiently — no file, no
// negative entry — so the retry goes out under whatever name this path
// supplies. `pickDeezerArtist` falls through to `list[0]` unconditionally,
// so a wrong query does not merely miss: it files SOME OTHER artist's
// portrait under this MBID, and `/v1/artist-image/{mbid}` serves it until
// the cache is cleared.
func TestArtistCacheHitUsesCanonicalNameForDeezer(t *testing.T) {
	const (
		taggedArtist   = "Rachel Podger, Conductor"
		canonicalName  = "Rachel Podger"
		artistMBID     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		releaseMBIDVal = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)

	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			fmt.Fprintf(w, `{"releases":[{"id":%q,"score":100,"title":"Album","artist-credit":[{"name":%q}]}]}`,
				releaseMBIDVal, canonicalName)
		case strings.Contains(r.URL.Path, "/artist/"):
			// The ladder queries the role-truncated name; MB answers with
			// its canonical form, which is what must reach Deezer.
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":%q}]}`, artistMBID, canonicalName)
		}
	}))
	defer mbSrv.Close()

	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()

	// Every Deezer artist search is recorded; the IMAGE fetch always fails
	// so no portrait is ever cached and no negative entry is recorded —
	// that is what keeps the name-dependent path live for the second track.
	var mu sync.Mutex
	var searchQueries []string
	var deezerBase string
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/search/artist") {
			mu.Lock()
			searchQueries = append(searchQueries, r.URL.Query().Get("q"))
			mu.Unlock()
			fmt.Fprintf(w, `{"data":[{"id":1,"name":%q,"picture_xl":%q}]}`,
				canonicalName, deezerBase+"/pic.jpg")
			return
		}
		// Transient image failure: nothing lands on disk.
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	deezerBase = deezerSrv.URL
	defer deezerSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Two tracks sharing the artist tag: the first populates artistCache,
	// the second must take the hit path.
	for _, p := range []string{"a.flac", "b.flac"} {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 1, ModTime: time.Now(),
			Artist: taggedArtist, Album: "Album",
		}); err != nil {
			t.Fatal(err)
		}
	}

	deezerClient := NewDeezerClient(deezerSrv.URL, "t", nil)
	deezerClient.http.Transport = rewritingTransport{base: deezerSrv.URL, tr: http.DefaultTransport}
	deezerClient.SetAllowedImageHostsForTest([]string{"127.0.0.1"})

	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		deezerClient,
		filepath.Join(dir, "artwork"))
	e.DeezerMinInterval = 0

	defer startEnricherForTest(e, 5*time.Second)()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && e.Done() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() < 2 {
		t.Fatalf("only enriched %d of 2 tracks", e.Done())
	}

	mu.Lock()
	got := append([]string(nil), searchQueries...)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("Deezer searched %d time(s), want >= 2 — the fixture's failing image "+
			"fetch should leave the second track's lookup live (queries: %q)", len(got), got)
	}
	for i, q := range got {
		if q == taggedArtist {
			t.Errorf("Deezer search #%d used the raw multi-credit tag %q; want the canonical %q "+
				"— the artistCache hit path lost the canonical name",
				i+1, taggedArtist, canonicalName)
		}
	}
}

// TestArtistCacheValueDecodesLegacyBareMBID pins the decode's tolerance for a
// value carrying no NUL separator. Nothing writes that shape today, but the
// decode is the only reader of the cache and a bare MBID must not be
// misparsed into an empty MBID plus a garbage name.
func TestArtistCacheValueDecodesLegacyBareMBID(t *testing.T) {
	const mbid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

	gotMBID, gotName := decodeArtistCacheValue(mbid, "fallback")
	if gotMBID != mbid {
		t.Errorf("mbid = %q, want %q", gotMBID, mbid)
	}
	if gotName != "fallback" {
		t.Errorf("name = %q, want the caller's fallback %q", gotName, "fallback")
	}

	gotMBID, gotName = decodeArtistCacheValue(mbid+"\x00Canonical", "fallback")
	if gotMBID != mbid {
		t.Errorf("encoded mbid = %q, want %q", gotMBID, mbid)
	}
	if gotName != "Canonical" {
		t.Errorf("encoded name = %q, want %q", gotName, "Canonical")
	}

	// An empty name half must not blank the caller's fallback.
	gotMBID, gotName = decodeArtistCacheValue(mbid+"\x00", "fallback")
	if gotMBID != mbid || gotName != "fallback" {
		t.Errorf("empty-name half = (%q, %q), want (%q, %q)", gotMBID, gotName, mbid, "fallback")
	}
}
