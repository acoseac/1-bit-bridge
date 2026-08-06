package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestArtistImageQueryName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		res      *SearchResult
		matched  string
		tagged   string
		want     string
		whyIfBad string
	}{
		{
			name:    "canonical name wins",
			res:     &SearchResult{MBID: "m", Title: "Rachel Podger"},
			matched: "Rachel Podger",
			tagged:  "Rachel Podger, Conductor, Brecon Baroque, Ensemble",
			want:    "Rachel Podger",
		},
		{
			name:    "canonical wins even over the rung",
			res:     &SearchResult{MBID: "m", Title: "Zdob și Zdub"},
			matched: "Zdob si Zdub",
			tagged:  "Zdob si Zdub",
			want:    "Zdob și Zdub",
		},
		{
			name:    "falls back to the matched rung when MB sends no name",
			res:     &SearchResult{MBID: "m", Title: ""},
			matched: "Rachel Podger",
			tagged:  "Rachel Podger, Conductor, Brecon Baroque, Ensemble",
			want:    "Rachel Podger",
		},
		{
			name:    "blank-only title is not a name",
			res:     &SearchResult{MBID: "m", Title: "   "},
			matched: "Ennio Morricone",
			tagged:  "Ennio Morricone; Solisti e Orchestre del Cinema Italiano",
			want:    "Ennio Morricone",
		},
		{
			name:    "falls back to the tag when nothing else is known",
			res:     nil,
			matched: "",
			tagged:  "Some Artist",
			want:    "Some Artist",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := artistImageQueryName(tc.res, tc.matched, tc.tagged); got != tc.want {
				t.Errorf("artistImageQueryName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveArtistFetchesPortraitByCanonicalName is the F4 regression gate.
//
// The artist ladder NARROWS a multi-credit tag before querying: "Rachel
// Podger, Conductor, Brecon Baroque, Ensemble" (34 tracks on the production
// library) resolves through its role-truncated rung, so the MBID names
// Rachel Podger while the tag names four entities. resolveArtist then read
// only res.MBID and handed Deezer the ORIGINAL tag.
//
// That is not merely a worse query. pickDeezerArtist tries exact name, then
// either-direction substring — under which a candidate literally named
// "Brecon Baroque" IS contained in the tag and wins — and failing both
// returns list[0] unconditionally. Whatever comes back is hardlinked to
// Rachel Podger's MBID and served by /v1/artist-image from then on. Before
// the ladder existed this could not happen: MusicBrainz got the same string,
// so a multi-credit tag got no MBID and Deezer was never reached.
func TestResolveArtistFetchesPortraitByCanonicalName(t *testing.T) {
	const (
		tag       = "Rachel Podger, Conductor, Brecon Baroque, Ensemble"
		canonical = "Rachel Podger"
		mbid      = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)

	// MB answers only the role-truncated rung, exactly as production does.
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.QueryUnescape(r.URL.Query().Get("query"))
		if strings.Contains(q, "Conductor") {
			io.WriteString(w, `{"artists":[]}`)
			return
		}
		io.WriteString(w, `{"artists":[{"id":"`+mbid+`","score":100,"name":"`+canonical+`"}]}`)
	}))
	defer mbSrv.Close()

	var mu sync.Mutex
	var deezerQueries []string
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "search") {
			mu.Lock()
			deezerQueries = append(deezerQueries, r.URL.Query().Get("q"))
			mu.Unlock()
			// The hazard in miniature: an ensemble named in the tag but
			// NOT the artist the MBID belongs to. pickDeezerArtist's
			// substring arm accepts it against the full tag.
			io.WriteString(w, `{"data":[
				{"id":1,"name":"Brecon Baroque","picture_xl":"`+r.Host+`/wrong.jpg","picture_big":""},
				{"id":2,"name":"Rachel Podger","picture_xl":"`+r.Host+`/right.jpg","picture_big":""}
			]}`)
			return
		}
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer deezerSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil), nil,
		NewDeezerClient(deezerSrv.URL, "t", nil), filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.DeezerMinInterval = 0

	tr := &manifest.Track{Path: "x.flac", Size: 1, ModTime: time.Now(), Artist: tag}
	if err := e.resolveArtist(context.Background(), tr); err != nil {
		t.Fatalf("resolveArtist: %v", err)
	}
	if tr.ArtistMBID != mbid {
		t.Fatalf("ArtistMBID = %q, want the ladder-resolved %q", tr.ArtistMBID, mbid)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deezerQueries) != 1 {
		t.Fatalf("Deezer search queries = %q, want exactly one", deezerQueries)
	}
	if deezerQueries[0] != canonical {
		t.Errorf("Deezer searched %q; the portrait is cached under the MBID for %q, "+
			"so the name handed to Deezer must describe the SAME entity — the raw "+
			"tag names four", deezerQueries[0], canonical)
	}
}

// TestFetchRecoveredArtistImageSkipsWhenTheMBIDIsNotTheFingerprintsIs the
// acoustic-side twin. applyAcousticFallback deliberately does NOT overwrite
// an artist the text path already resolved, so this function could be
// reached pairing a TEXT-resolved MBID with the fingerprint's name — the
// same mismatch, inverted.
func TestFetchRecoveredArtistImageSkipsWhenTheMBIDIsNotTheFingerprints(t *testing.T) {
	var mu sync.Mutex
	var calls int
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		io.WriteString(w, `{"data":[{"id":1,"name":"Whoever","picture_xl":"`+r.Host+`/x.jpg","picture_big":""}]}`)
	}))
	defer deezerSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	e := NewEnricher(store, nil, nil, NewDeezerClient(deezerSrv.URL, "t", nil),
		filepath.Join(dir, "artwork"))
	e.DeezerMinInterval = 0

	textMBID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	fpMBID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	// The text path owns the MBID; the fingerprint names a different entity.
	tr := &manifest.Track{Path: "x.flac", ArtistMBID: textMBID}
	e.fetchRecoveredArtistImage(context.Background(), tr,
		AcousticMatch{ArtistMBID: fpMBID, ArtistName: "Somebody Else"})
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("Deezer called %d times; a name that does not describe the MBID it "+
			"will be hardlinked under must not be searched", got)
	}

	// The genuine recovery path — MBID and name from the same match — still
	// fetches.
	tr2 := &manifest.Track{Path: "y.flac", ArtistMBID: fpMBID}
	e.fetchRecoveredArtistImage(context.Background(), tr2,
		AcousticMatch{ArtistMBID: fpMBID, ArtistName: "Somebody Else"})
	mu.Lock()
	got = calls
	mu.Unlock()
	if got == 0 {
		t.Error("the recovered-artist path must still fetch the portrait")
	}
}
