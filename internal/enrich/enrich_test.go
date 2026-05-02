package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// --- MusicBrainz client ---

func TestMusicBrainzSearchReleaseHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/release/") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{
			"releases": [
				{"id": "11111111-1111-4111-8111-111111111111", "score": 100, "title": "Blue Train", "artist-credit":[{"name":"John Coltrane"}]},
				{"id": "22222222-2222-4222-8222-222222222222", "score": 85,  "title": "Blue Train (Reissue)", "artist-credit":[{"name":"John Coltrane"}]}
			]
		}`)
	}))
	defer srv.Close()

	c := NewMusicBrainzClient(srv.URL, "test-agent", nil)
	res, err := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("no result")
	}
	if res.MBID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("picked wrong release: %s", res.MBID)
	}
}

// TestMusicBrainzDecodeRealResponseShape locks in the wire shape the
// public MB API actually sends — specifically, `release-group` is an
// OBJECT ({id, title, primary-type}), not a bare string. The initial
// implementation had `ReleaseGroupID string` which silently failed
// every search against prod. This fixture is a trimmed version of a
// real live search response; if MB changes the shape in a
// backward-incompatible way, this test fails loudly instead of
// silently breaking every enrichment in the field.
func TestMusicBrainzDecodeRealResponseShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"created": "2024-01-01T00:00:00.000Z",
			"count": 1,
			"offset": 0,
			"releases": [
				{
					"id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
					"score": 100,
					"title": "Blue Train",
					"status": "Official",
					"artist-credit": [
						{"name": "John Coltrane", "artist": {"id": "bbbb", "name": "John Coltrane"}}
					],
					"release-group": {
						"id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
						"title": "Blue Train",
						"primary-type": "Album"
					},
					"country": "US",
					"date": "1958",
					"track-count": 5
				}
			]
		}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, err := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if err != nil {
		t.Fatalf("decode real MB response: %v", err)
	}
	if res == nil || res.MBID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Errorf("got %+v", res)
	}
}

func TestMusicBrainzSearchReleaseNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases": []}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, err := c.SearchRelease(context.Background(), "Unknown", "Artist")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("unexpected match: %+v", res)
	}
}

func TestMusicBrainzSearchReleaseScoreThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"x","score":50,"title":"Blue Train","artist-credit":[{"name":"John Coltrane"}]}]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, _ := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if res != nil {
		t.Errorf("low-score release was not filtered: %+v", res)
	}
}

func TestMusicBrainzSearchReleaseIgnoresTitleMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"x","score":100,"title":"Totally Different Album","artist-credit":[{"name":"John Coltrane"}]}]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, _ := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if res != nil {
		t.Errorf("title-mismatch release leaked through: %+v", res)
	}
}

func TestMusicBrainzSendsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, `{"releases":[]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "1-bit-bridge/0.0.1 (+https://example.com)", nil)
	c.SearchRelease(context.Background(), "A", "B")
	if !strings.Contains(gotUA, "1-bit-bridge") {
		t.Errorf("UA not set: %q", gotUA)
	}
}

func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"0", 0},
		{"1", 1 * time.Second},
		{"30", 30 * time.Second},
		{"  120  ", 2 * time.Minute},
	}
	for _, c := range cases {
		got := parseRetryAfter(c.header, now)
		if got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	// RFC 7231 IMF-fixdate, 90s in the future.
	header := "Sat, 25 Apr 2026 12:01:30 GMT"
	got := parseRetryAfter(header, now)
	if got < 89*time.Second || got > 91*time.Second {
		t.Errorf("parseRetryAfter(HTTP-date) = %v, want ~90s", got)
	}
}

func TestParseRetryAfterPastDateReturnsZero(t *testing.T) {
	// Past HTTP-date → don't wait at all.
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter("Sat, 25 Apr 2026 11:00:00 GMT", now)
	if got != 0 {
		t.Errorf("past Retry-After should be 0, got %v", got)
	}
}

func TestParseRetryAfterMalformedReturnsZero(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	for _, h := range []string{"", "garbage", "-5", "2.5", "soon"} {
		if got := parseRetryAfter(h, now); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", h, got)
		}
	}
}

func TestParseRetryAfterCappedAtOneHour(t *testing.T) {
	// A hostile or misconfigured upstream telling us to wait a day
	// shouldn't park the enricher for a day.
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter("86400", now) // 24h
	if got != time.Hour {
		t.Errorf("parseRetryAfter(86400s) = %v, want capped at 1h", got)
	}
}

func TestParseRetryAfterOverflowSafe(t *testing.T) {
	// `time.Duration(secs) * time.Second` overflows int64 nanoseconds
	// for `secs` near 2^33 — the cap MUST apply in the seconds domain
	// before the multiplication, otherwise a hostile upstream sending
	// a huge value silently bypasses the 1h ceiling.
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []string{
		"9999999999",          // 10 digits, well past 2^31
		"99999999999",         // 11 digits, past 2^33
		"9223372036854775807", // int64 max
	}
	for _, h := range cases {
		got := parseRetryAfter(h, now)
		if got != time.Hour {
			t.Errorf("parseRetryAfter(%q) = %v, want capped at 1h (no overflow)", h, got)
		}
	}
}

func TestParseRetryAfterBeyondInt64ClampsToCap(t *testing.T) {
	// `strconv.ParseInt(_, 10, 64)` returns ErrRange for values that
	// don't fit in int64. The previous behaviour was to fall through
	// to 0 — defeating the cap entirely for hostile / misconfigured
	// upstreams. Now the parser detects the range error and clamps
	// to maxRetryAfter for non-negative inputs (negative-overflow
	// still falls through to 0, like other malformed inputs).
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"99999999999999999999999", time.Hour}, // 23 digits, way past int64
		{"9223372036854775808", time.Hour},     // exactly int64 max + 1
		{"-99999999999999999999999", 0},        // negative, malformed
	}
	for _, c := range cases {
		got := parseRetryAfter(c.header, now)
		if got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestMusicBrainz429HonorsRetryAfter(t *testing.T) {
	// Server returns 429 with Retry-After: 1 (second). Client should
	// sleep ~1s and then return an error. We verify both: (a) the
	// error returns, (b) the call took at least the advised duration.
	const advisedSeconds = "1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", advisedSeconds)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"too many requests"}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	start := time.Now()
	_, err := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	// Must have waited at least the advised duration before returning.
	if elapsed < 950*time.Millisecond {
		t.Errorf("did not honor Retry-After: returned in %v (want >=1s)", elapsed)
	}
}

func TestMusicBrainz429RespectsContextCancellation(t *testing.T) {
	// Server advises a long wait; client should bail when ctx is cancelled
	// rather than parking the goroutine for the full duration.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60") // 1 minute
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.SearchRelease(ctx, "X", "Y")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after ctx cancel, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("did not honor ctx cancel during Retry-After wait: returned in %v", elapsed)
	}
}

func TestMusicBrainzSearchArtistPrefersExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"artists":[
			{"id":"fuzzy","score":95,"name":"Coltrane"},
			{"id":"exact","score":90,"name":"John Coltrane"}
		]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, _ := c.SearchArtist(context.Background(), "John Coltrane")
	if res == nil || res.MBID != "exact" {
		t.Errorf("picked %+v, wanted exact name match", res)
	}
}

// --- iTunes Search client ---

func TestITunesSearchAlbumHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("entity") != "album" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{
			"resultCount": 1,
			"results": [
				{
					"wrapperType": "collection",
					"collectionId": 1234567890,
					"collectionName": "Blue Train",
					"artistName": "John Coltrane",
					"artworkUrl100": "https://is1-ssl.mzstatic.com/image/.../100x100bb.jpg"
				}
			]
		}`)
	}))
	defer srv.Close()
	c := NewITunesClient(srv.URL, "test", nil)
	hit, err := c.SearchAlbum(context.Background(), "John Coltrane", "Blue Train")
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("no hit")
	}
	if hit.CollectionID != 1234567890 {
		t.Errorf("CollectionID = %d", hit.CollectionID)
	}
	if !strings.Contains(hit.ArtworkURL100, "100x100bb") {
		t.Errorf("artwork URL didn't carry the 100x100 marker: %q", hit.ArtworkURL100)
	}
}

func TestITunesSearchAlbumZeroResultsIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"resultCount": 0, "results": []}`)
	}))
	defer srv.Close()
	c := NewITunesClient(srv.URL, "test", nil)
	hit, err := c.SearchAlbum(context.Background(), "Nobody", "Nothing")
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got hit=%v err=%v", hit, err)
	}
}

func TestITunesSearchAlbumRejectsObviousMisses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"resultCount": 1,
			"results": [
				{
					"wrapperType": "collection",
					"collectionId": 99,
					"collectionName": "Black Diamond",
					"artistName": "John Coltrane",
					"artworkUrl100": "https://is1-ssl.mzstatic.com/image/.../100x100bb.jpg"
				}
			]
		}`)
	}))
	defer srv.Close()
	c := NewITunesClient(srv.URL, "test", nil)
	hit, _ := c.SearchAlbum(context.Background(), "John Coltrane", "Blue Train")
	if hit != nil {
		t.Errorf("hard mismatch leaked through: %+v", hit)
	}
}

func TestITunesSearchAlbumAcceptsDeluxeSuffix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"resultCount": 1,
			"results": [
				{
					"wrapperType": "collection",
					"collectionId": 1234567890,
					"collectionName": "Blue Train (Deluxe Edition)",
					"artistName": "John Coltrane",
					"artworkUrl100": "https://is1-ssl.mzstatic.com/image/.../100x100bb.jpg"
				}
			]
		}`)
	}))
	defer srv.Close()
	c := NewITunesClient(srv.URL, "test", nil)
	hit, err := c.SearchAlbum(context.Background(), "John Coltrane", "Blue Train")
	if err != nil || hit == nil {
		t.Fatalf("deluxe-suffix should still match: %v / %v", hit, err)
	}
}

func TestITunesFetchArtworkUpscalesURL(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer srv.Close()
	c := NewITunesClient("unused", "test", nil)
	a := &ITunesAlbum{
		CollectionID:  1,
		ArtworkURL100: srv.URL + "/path/to/100x100bb.jpg",
	}
	data, err := c.FetchArtwork(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestedPath, "/600x600bb.") {
		t.Errorf("upscale missing — requested %q", requestedPath)
	}
	if string(data) != string([]byte{0xFF, 0xD8, 0xFF}) {
		t.Errorf("payload = %x", data)
	}
}

func TestITunesFetchArtwork404ReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewITunesClient("unused", "test", nil)
	_, err := c.FetchArtwork(context.Background(), &ITunesAlbum{
		CollectionID:  1,
		ArtworkURL100: srv.URL + "/100x100bb.jpg",
	})
	if !IsNotFound(err) {
		t.Errorf("want IsNotFound, got %v", err)
	}
}

func TestITunesFetchArtworkHonorsRetryAfter(t *testing.T) {
	// Same shape as TestITunesHonorsRetryAfter but exercising the
	// artwork CDN path. FetchArtwork previously returned immediately
	// on non-200 (including 429/503) without honoring Retry-After,
	// unlike SearchAlbum's `get()` JSON path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewITunesClient("unused", "test", nil)
	a := &ITunesAlbum{
		CollectionID:  1,
		ArtworkURL100: srv.URL + "/100x100bb.jpg",
	}
	start := time.Now()
	_, err := c.FetchArtwork(context.Background(), a)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if elapsed < 950*time.Millisecond {
		t.Errorf("FetchArtwork did not honor Retry-After: returned in %v", elapsed)
	}
}

func TestITunesHonorsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()
	c := NewITunesClient(srv.URL, "test", nil)
	start := time.Now()
	_, err := c.SearchAlbum(context.Background(), "John Coltrane", "Blue Train")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if elapsed < 950*time.Millisecond {
		t.Errorf("Retry-After not honored: returned in %v", elapsed)
	}
}

// --- Cover Art client ---

func TestCoverArtFetchFrontReturnsBytes(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF} // JPEG SOI
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(want)
	}))
	defer srv.Close()
	c := NewCoverArtClient(srv.URL, "test", nil)
	got, err := c.FetchReleaseFront(context.Background(), "11111111-1111-4111-8111-111111111111", 500)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("got %x, want %x", got, want)
	}
}

func TestCoverArtFetchFront404ReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewCoverArtClient(srv.URL, "test", nil)
	_, err := c.FetchReleaseFront(context.Background(), "11111111-1111-4111-8111-111111111111", 500)
	if !IsNotFound(err) {
		t.Errorf("want IsNotFound, got %v", err)
	}
}

func TestCoverArtSizeValidation(t *testing.T) {
	c := NewCoverArtClient("http://unused", "test", nil)
	_, err := c.FetchReleaseFront(context.Background(), "valid-mbid", 42)
	if err == nil || !strings.Contains(err.Error(), "unsupported size") {
		t.Errorf("bad-size error = %v", err)
	}
}

func TestParseSize(t *testing.T) {
	got, err := ParseSize("")
	if err != nil || got != 500 {
		t.Errorf("ParseSize('') = %d, %v", got, err)
	}
	got, err = ParseSize("250")
	if err != nil || got != 250 {
		t.Errorf("ParseSize('250') = %d, %v", got, err)
	}
	if _, err := ParseSize("42"); err == nil {
		t.Error("unsupported size should error")
	}
	if _, err := ParseSize("not-a-number"); err == nil {
		t.Error("non-integer should error")
	}
}

// --- Enricher end-to-end ---

func TestEnricherProcessesTracksEndToEnd(t *testing.T) {
	// Mock MusicBrainz.
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
	}))
	defer mbSrv.Close()

	// Mock Cover Art Archive — returns a fake JPEG for the expected MBID.
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed two un-enriched tracks on the same album.
	for _, path := range []string{"Artist/Album/01.flac", "Artist/Album/02.flac"} {
		store.UpsertTrack(&manifest.Track{
			Path:    path,
			Size:    100,
			ModTime: time.Now(),
			Artist:  "Artist",
			Album:   "Album",
			Title:   filepath.Base(path),
		})
	}

	e := NewEnricher(
		store,
		NewMusicBrainzClient(mbSrv.URL, "test", nil),
		NewCoverArtClient(caaSrv.URL, "test", nil),
		nil, // no Deezer in this test — artist-image fallback path tested separately
		filepath.Join(dir, "artwork"),
	)
	e.MBMinInterval = 0 // no pacing in tests
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.Run(ctx)

	// Wait for both to be enriched.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.Done() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := e.Done(); got < 2 {
		t.Fatalf("enriched %d, want 2 (skipped=%d)", got, e.skipped.Load())
	}

	// Verify both tracks have ArtworkMBID set.
	all, _ := store.ListTracks(nil)
	for _, tr := range all {
		if tr.ArtworkMBID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Errorf("%s ArtworkMBID = %q", tr.Path, tr.ArtworkMBID)
		}
		if tr.MusicBrainzAlbumID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Errorf("%s MusicBrainzAlbumID = %q", tr.Path, tr.MusicBrainzAlbumID)
		}
	}

	// Artwork cached on disk.
	wantPath := ArtworkCachePath(filepath.Join(dir, "artwork"),
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 500)
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("artwork not cached at %q: %v", wantPath, err)
	}
}

func TestEnricherDeduplicatesAlbumLookups(t *testing.T) {
	// Three tracks on the same album should result in exactly one MB
	// release search — the enricher's (artist, album) cache kicks in
	// for the second and third. We count only /release/ hits so the
	// artist-resolution path doesn't muddy the signal.
	var mbReleaseCalls int
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			mbReleaseCalls++
			io.WriteString(w, `{"releases":[{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
		case strings.Contains(r.URL.Path, "/artist/"):
			io.WriteString(w, `{"artists":[]}`)
		}
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	for _, p := range []string{"a.flac", "b.flac", "c.flac"} {
		store.UpsertTrack(&manifest.Track{
			Path: p, Size: 1, ModTime: time.Now(),
			Artist: "Artist", Album: "Album",
		})
	}

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() < 3 {
		t.Fatalf("only enriched %d of 3", e.Done())
	}
	if mbReleaseCalls != 1 {
		t.Errorf("MB /release/ called %d times, want 1 (sibling-track dedup broken)", mbReleaseCalls)
	}
}

func TestEnricherSkipsUnsearchableTracks(t *testing.T) {
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	// Track with no artist/album → can't be searched.
	store.UpsertTrack(&manifest.Track{Path: "orphan.flac", Size: 1, ModTime: time.Now()})

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && e.skipped.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.skipped.Load() == 0 {
		t.Error("unsearchable track never marked skipped")
	}

	// Track's enriched_at should be non-zero so the worker doesn't spin.
	remaining, _ := store.UnenrichedTracks(100)
	if len(remaining) != 0 {
		t.Errorf("skipped track still un-enriched: %+v", remaining)
	}
}

func TestEnricherSkipsNetworkCallIfCoverAlreadyCached(t *testing.T) {
	var caaCalls int
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaCalls++
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	// Pre-populate the cache file as if a previous run already cached it.
	os.MkdirAll(artDir, 0o755)
	preCached := ArtworkCachePath(artDir, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", 500)
	os.WriteFile(preCached, []byte("cached"), 0o644)

	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	store.UpsertTrack(&manifest.Track{Path: "x.flac", Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album"})

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, artDir)
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("track never enriched")
	}
	if caaCalls != 0 {
		t.Errorf("CAA called %d times despite pre-cached file", caaCalls)
	}
}

// TestEnricherSkipsArtworkFetchForLocalMBID is the contract test for
// the local-artwork bypass added in the v1.2 batch. When the scanner
// has stamped t.ArtworkMBID with a `local-<sha256>` sentinel (because
// it found embedded ID3 APIC art or a folder-level cover.jpg), the
// enricher must:
//
//  1. NOT call Cover Art Archive (the local bytes are authoritative).
//  2. NOT call iTunes (same reason — no remote source overrides
//     user-curated artwork).
//  3. Preserve the local- value through MarkEnriched (no overwrite).
//  4. Still mark the track enriched so the worker doesn't loop on it.
//
// The MB album search is allowed to run — V1 scope is "skip
// ensureArtworkCached only" per the plan; tightening that is a
// follow-up. We don't assert the MB call count to keep the test
// focused on the load-bearing invariants above.
func TestEnricherSkipsArtworkFetchForLocalMBID(t *testing.T) {
	const localMBID = "local-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Counters live across goroutines (HTTP handler vs. main test) —
	// atomic.Int32 keeps `go test -race` clean. Pre-fix the int
	// variant tripped the race detector on the read path even when
	// no writes happened (CodeRabbit Minor on c506922).
	var caaCalls atomic.Int32
	var itunesCalls atomic.Int32
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return an MB hit so albumMBID resolution succeeds — the
		// bailout we added to enrichOne handles albumMBID == "" with a
		// local-prefix anyway, but a passing MB makes this test
		// independent of that secondary fallthrough.
		io.WriteString(w, `{"releases":[{"id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaCalls.Add(1)
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()
	itunesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		itunesCalls.Add(1)
		// Reply with an empty hit set — the assertion is purely on
		// the call-count, but a sane response keeps logs quiet.
		io.WriteString(w, `{"resultCount":0,"results":[]}`)
	}))
	defer itunesSrv.Close()

	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	os.MkdirAll(artDir, 0o755)
	// Pre-stage the local-<hash>-500.jpg the scanner would have
	// written. The enricher MUST NOT touch this file — we don't
	// assert mtime preservation here (the bypass guard simply skips
	// ensureArtworkCached), but if the bypass regressed the existing
	// stat-and-replace path inside ensureArtworkCached would leave
	// the original on disk anyway. The CAA call-count assertion is
	// the load-bearing check.
	if err := os.WriteFile(filepath.Join(artDir, localMBID+"-500.jpg"),
		[]byte("scanner-curated"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	// Track arrives with ArtworkMBID already stamped — scanner side.
	tr := &manifest.Track{
		Path: "x.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
		ArtworkMBID: localMBID,
	}
	if err := store.UpsertTrack(tr); err != nil {
		t.Fatal(err)
	}

	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		nil, artDir,
	).WithITunes(NewITunesClient(itunesSrv.URL, "t", nil))
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("track never enriched (the bypass should still mark it done)")
	}
	if got := caaCalls.Load(); got != 0 {
		t.Errorf("CAA called %d times for local-prefix track; want 0", got)
	}
	if got := itunesCalls.Load(); got != 0 {
		t.Errorf("iTunes called %d times for local-prefix track; want 0", got)
	}

	// Round-trip the track from storage to confirm ArtworkMBID
	// survived MarkEnriched without being overwritten by the
	// enricher's UUID-stamping branch.
	got, err := store.GetTrack("x.flac")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("track missing from store after enrichment")
	}
	if got.ArtworkMBID != localMBID {
		t.Errorf("ArtworkMBID = %q, want %q (local- value must survive)", got.ArtworkMBID, localMBID)
	}
}

// TestEnricherFallthroughForLocalPrefixWithoutMBMatch covers the
// obscure-album case the v1.2 bypass was designed for: track has
// local-prefix ArtworkMBID, MusicBrainz returns no match. The
// pre-fix enricher would early-return at the `albumMBID == ""`
// bailout and skip resolveArtist entirely — silently breaking the
// exact case the local-art feature targets. Post-fix, the bailout
// allows the local-prefix to fall through; resolveArtist runs;
// MarkEnriched stamps the track done.
func TestEnricherFallthroughForLocalPrefixWithoutMBMatch(t *testing.T) {
	const localMBID = "local-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// atomic.Int32 — see sibling TestEnricherSkipsArtworkFetchForLocalMBID
	// for the race-detector rationale.
	var caaCalls atomic.Int32
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty MB result — this is the obscure-album case.
		io.WriteString(w, `{"releases":[]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaCalls.Add(1)
		w.WriteHeader(404)
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	os.MkdirAll(artDir, 0o755)
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	tr := &manifest.Track{
		Path: "y.flac", Size: 1, ModTime: time.Now(),
		Artist: "ObscureArtist", Album: "RareAlbum",
		ArtworkMBID: localMBID,
	}
	if err := store.UpsertTrack(tr); err != nil {
		t.Fatal(err)
	}

	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		nil, artDir,
	)
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("local-prefix track without MB match was never enriched (bailout regression)")
	}
	if got := caaCalls.Load(); got != 0 {
		t.Errorf("CAA called %d times despite albumMBID==\"\" + local prefix; want 0", got)
	}
	got, _ := store.GetTrack("y.flac")
	if got == nil || got.ArtworkMBID != localMBID {
		t.Errorf("ArtworkMBID lost on local-prefix obscure-album path (got %v)", got)
	}
}

func TestEscapeLucene(t *testing.T) {
	got := escapeLucene(`title "with" specials [and] brackets`)
	for _, want := range []string{`\"`, `\[`, `\]`} {
		if !strings.Contains(got, want) {
			t.Errorf("escaped %q missing %q", got, want)
		}
	}
}

func TestArtworkCachePathFormat(t *testing.T) {
	got := ArtworkCachePath("/x", "aaaa", 500)
	want := filepath.Join("/x", "aaaa-500.jpg")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestArtistImagePathByNameNormalizes locks in the normalization
// contract that dedup depends on: lower + trim + NFC. Two distinct
// capitalizations / whitespace variants / NFC-vs-NFD forms of the
// same artist name MUST collapse to the same on-disk path.
func TestArtistImagePathByNameNormalizes(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"case difference", "John Coltrane", "JOHN COLTRANE"},
		{"trailing whitespace", "John Coltrane", "  John Coltrane  "},
		// NFC "é" (U+00E9) vs NFD "é" (U+0065 U+0301) — macOS HFS+ and
		// some tag writers produce different forms for the same name.
		// Normalization must collapse them.
		{"NFC vs NFD", "Beyonc\u00e9", "Beyonce\u0301"},
		// Turkish dotted capital I (U+0130) folds to "i\u0307" via
		// Unicode case-fold; lowercased with `strings.ToLower` it
		// would stay as `i\u0307stanbul` (ASCII `i` + combining dot
		// above), still distinct from plain `istanbul` after
		// NFC normalization. `cases.Fold` collapses both to the
		// same key.
		{"Turkish dotted I", "\u0130STANBUL", "istanbul"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pa := ArtistImagePathByName("/x", tc.a)
			pb := ArtistImagePathByName("/x", tc.b)
			if pa != pb {
				t.Errorf("%q and %q mapped to different paths:\n  %s\n  %s", tc.a, tc.b, pa, pb)
			}
		})
	}
}

// TestMusicBrainzReleaseGroupMBIDLookup verifies the targeted
// `/release/{mbid}?inc=release-groups` query path used by the CAA
// release-group fallback when a track carried an embedded release MBID
// (so no SearchRelease ran to populate the association).
func TestMusicBrainzReleaseGroupMBIDLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"rel","release-group":{"id":"rg-xyz","title":"Whatever","primary-type":"Album"}}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	got, err := c.ReleaseGroupMBID(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if got != "rg-xyz" {
		t.Errorf("got %q, want rg-xyz", got)
	}
}
