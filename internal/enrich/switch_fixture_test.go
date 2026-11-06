package enrich

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The fixture behind the tests of enrich.enabled and enrich.deezerEnabled
// (backlog B617): fake MusicBrainz, Cover Art Archive, Deezer and Atlas
// services that count and log every request they answer, and an enricher
// whose clients talk to them and nothing else.

const (
	// switchReleaseMBID is the release the first seeded track embeds, and
	// switchSearchedMBID the one MusicBrainz's release search answers for
	// the second.
	switchReleaseMBID  = "11111111-1111-4111-8111-111111111111"
	switchSearchedMBID = "22222222-2222-4222-8222-222222222222"
	switchGroupMBID    = "33333333-3333-4333-8333-333333333333"
	// switchArtistMBID is the artist MusicBrainz's artist search answers.
	switchArtistMBID = "44444444-4444-4444-8444-444444444444"
)

// switchJPEG is enough of a JPEG for the artwork writes' signature check.
var switchJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE1, 0xDE, 0xAD}

// countingUpstream is one fake service: every request it answers is logged
// as "METHOD /path?query" before hold, when set, decides when it answers.
type countingUpstream struct {
	srv *httptest.Server

	mu   sync.Mutex
	log  []string
	hold func(r *http.Request)
}

// newCountingUpstream starts a fake service that answers with answer.
func newCountingUpstream(t *testing.T, answer func(u *countingUpstream, w http.ResponseWriter, r *http.Request)) *countingUpstream {
	t.Helper()
	u := &countingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.log = append(u.log, r.Method+" "+r.URL.RequestURI())
		hold := u.hold
		u.mu.Unlock()
		if hold != nil {
			hold(r)
			if r.Context().Err() != nil {
				// The client gave up while the request was held: answer
				// nothing (build-ci-tests.md, B526).
				panic(http.ErrAbortHandler)
			}
		}
		answer(u, w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// requests returns what the service was asked, in order.
func (u *countingUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.log...)
}

// count is how many requests the service was asked.
func (u *countingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.log)
}

// setHold has the service call hold before it answers each request.
func (u *countingUpstream) setHold(hold func(r *http.Request)) {
	u.mu.Lock()
	u.hold = hold
	u.mu.Unlock()
}

// switchUpstreams are the four services an enricher asks.
type switchUpstreams struct {
	mb, caa, deezer, atlas *countingUpstream
}

// newSwitchUpstreams starts the four services. MusicBrainz knows one artist
// and one release for any album it is asked about; the Cover Art Archive
// has a front cover for every release; Deezer has a portrait for the
// artist; Atlas has no premium cover, so the Cover Art Archive is asked
// after it.
func newSwitchUpstreams(t *testing.T) *switchUpstreams {
	t.Helper()
	ups := &switchUpstreams{}
	ups.mb = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/artist"):
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, switchArtistMBID)
		case strings.HasPrefix(r.URL.Path, "/release/") && r.URL.Query().Get("query") != "":
			fmt.Fprintf(w, `{"releases":[{"id":%q,"score":100,"title":"Other Album","artist-credit":[{"name":"Artist"}],`+
				`"release-group":{"id":%q,"title":"Other Album","primary-type":"Album"}}]}`, switchSearchedMBID, switchGroupMBID)
		default:
			http.NotFound(w, r)
		}
	})
	ups.caa = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/release/") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(switchJPEG)
			return
		}
		http.NotFound(w, r)
	})
	ups.deezer = newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/artist"):
			fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q,"picture_big":""}]}`, u.srv.URL+"/picture.jpg")
		case r.URL.Path == "/picture.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(switchJPEG)
		default:
			http.NotFound(w, r)
		}
	})
	ups.atlas = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return ups
}

// all is every request the four services were asked, each with the
// service's name in front.
func (ups *switchUpstreams) all() []string {
	var out []string
	for _, s := range []struct {
		name string
		u    *countingUpstream
	}{{"musicbrainz", ups.mb}, {"coverart", ups.caa}, {"deezer", ups.deezer}, {"atlas", ups.atlas}} {
		for _, r := range s.u.requests() {
			out = append(out, s.name+" "+r)
		}
	}
	return out
}

// total is how many requests the four services were asked.
func (ups *switchUpstreams) total() int {
	return ups.mb.count() + ups.caa.count() + ups.deezer.count() + ups.atlas.count()
}

// switchEnricher is an enricher over a store of its own, whose four clients
// talk to ups, with no pacing and a short poll, caching into the artwork
// directory it returns.
func switchEnricher(t *testing.T, ups *switchUpstreams) (*Enricher, *manifest.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	deezer := NewDeezerClient(ups.deezer.srv.URL, "t", nil)
	deezer.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	e := NewEnricher(store,
		NewMusicBrainzClient(ups.mb.srv.URL, "t", nil),
		NewCoverArtClient(ups.caa.srv.URL, "t", nil),
		deezer, artworkDir)
	e.WithPremiumCovers(NewAtlasPremiumFetcher(fakeCred{token: "tok", base: ups.atlas.srv.URL, ok: true}, "t", nil, artworkDir))
	tuneForTest(e)
	e.PollInterval = time.Millisecond
	return e, store, artworkDir
}

// switchTracks are the two tracks seedSwitchTracks stores: one that embeds
// its release, and one whose release MusicBrainz's search finds. Both are
// by the one artist, so the second asks for no portrait the first fetched.
var switchTracks = []manifest.Track{
	{Path: "Artist/Album/01.flac", Size: 1, Artist: "Artist", Album: "Album", MusicBrainzAlbumID: switchReleaseMBID},
	{Path: "Artist/Other Album/01.flac", Size: 1, Artist: "Artist", Album: "Other Album"},
}

// seedSwitchTracks stores switchTracks, each waiting for the enricher.
func seedSwitchTracks(t *testing.T, store *manifest.Store) {
	t.Helper()
	for _, tr := range switchTracks {
		tr.ModTime = time.Now()
		if err := store.UpsertTrack(context.Background(), &tr); err != nil {
			t.Fatal(err)
		}
	}
}

// runEnricher runs e on a goroutine of its own and returns the function that
// stops it and waits for Run to return, which a cleanup calls too.
func runEnricher(t *testing.T, e *Enricher) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not return within 10s of its context's cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// unenrichedPaths is the paths still waiting for the enricher.
func unenrichedPaths(t *testing.T, store *manifest.Store) map[string]bool {
	t.Helper()
	rows, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.Path] = true
	}
	return out
}
