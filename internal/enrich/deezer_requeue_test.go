package enrich

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The portraits enrich.deezerEnabled refused, fetched once it is on (backlog
// B761): the stamp records the name the lookup would have asked by, and the
// portrait batch asks Deezer for it once both switches are on, asking
// MusicBrainz and the Cover Art Archive nothing and resetting no stamp.

// pendingPortraitsOf is each row's pending query, by path.
func pendingPortraitsOf(t *testing.T, store *manifest.Store) map[string]string {
	t.Helper()
	rows, err := store.PendingPortraits(context.Background(), "", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Path] = r.Query
	}
	return out
}

// stampPending stores path as enriched with artist mbid and a portrait the
// switch refused, asked by query: what a pass with Deezer off leaves.
func stampPending(t *testing.T, store *manifest.Store, path, mbid, query string) {
	t.Helper()
	ctx := context.Background()
	tr := manifest.Track{Path: path, Size: 1, ModTime: time.Now(), Artist: query, Album: "Album",
		ArtworkMBID: "local-" + strings.Repeat("c", 64)}
	if err := store.UpsertTrack(ctx, &tr); err != nil {
		t.Fatal(err)
	}
	tr.ArtistMBID = mbid
	tr.PortraitRefused(query)
	if err := store.MarkEnriched(ctx, &tr); err != nil {
		t.Fatal(err)
	}
}

// requirePending fails the test unless path carries query: the premise of a
// case about what becomes of a pending portrait.
func requirePending(t *testing.T, store *manifest.Store, path, query string) {
	t.Helper()
	if q, ok := pendingPortraitsOf(t, store)[path]; !ok || q != query {
		t.Fatalf("%s carries %q (%v), want the refused lookup's %q", path, q, ok, query)
	}
}

// requireNoRequestsSince fails when MusicBrainz, the Cover Art Archive or
// Atlas were asked anything past the counts taken before.
func requireNoRequestsSince(t *testing.T, ups *switchUpstreams, mb, caa, atlas int) {
	t.Helper()
	for _, s := range []struct {
		name   string
		u      *countingUpstream
		before int
	}{{"MusicBrainz", ups.mb, mb}, {"the Cover Art Archive", ups.caa, caa}, {"Atlas", ups.atlas, atlas}} {
		if got := s.u.requests(); len(got) != s.before {
			t.Errorf("%s was asked for a portrait: %d request(s) after the switch, want none:\n%s",
				s.name, len(got)-s.before, strings.Join(got[s.before:], "\n"))
		}
	}
}

// TestARefusedPortraitIsFetchedOnceDeezerIsOn: tracks enriched while
// enrich.deezerEnabled was off carry the name the lookup would have asked by,
// and once it is on the portrait batch fetches the portrait, one search and one
// image for the artist, with no request to MusicBrainz, the Cover Art Archive
// or Atlas and no stamp reset, and settles every row. On main the rows kept no
// portrait until a file changed or "Retry missing" re-ran their enrichment.
func TestARefusedPortraitIsFetchedOnceDeezerIsOn(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	seedSwitchTracks(t, store)
	var deezerOn atomic.Bool
	e.WithDeezerEnabled(deezerOn.Load)
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "both tracks to be enriched with Deezer off")
	if got := ups.deezer.count(); got != 0 {
		t.Fatalf("Deezer was asked %d time(s) while it was off", got)
	}
	pending := pendingPortraitsOf(t, store)
	for _, tr := range switchTracks {
		if q, ok := pending[tr.Path]; !ok || q != "Artist" {
			t.Fatalf("%s: pending query %q (%v), want the refused lookup's %q", tr.Path, q, ok, "Artist")
		}
	}
	mb, caa, atlas := ups.mb.count(), ups.caa.count(), ups.atlas.count()

	deezerOn.Store(true)
	waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) },
		"the portrait once Deezer is on")
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "both rows to be settled")
	if got := unenrichedPaths(t, store); len(got) != 0 {
		t.Errorf("the portrait batch re-queued %d row(s) for enrichment", len(got))
	}
	stop()
	requireNoRequestsSince(t, ups, mb, caa, atlas)
	if got := ups.deezer.requests(); len(got) != 2 || got[0] != "GET /search/artist?q=Artist&limit=5" || got[1] != "GET /picture.jpg" {
		t.Errorf("Deezer was asked %v, want one search for the artist and its picture", got)
	}
}

// restartFixture is switchEnricher with the store's path kept, so a test can
// stop the enricher, close the store and open both again, as a restart does.
type restartFixture struct {
	ups        *switchUpstreams
	dbPath     string
	artworkDir string
}

// newRestartFixture makes the directories and the upstreams.
func newRestartFixture(t *testing.T, ups *switchUpstreams) *restartFixture {
	t.Helper()
	dir := t.TempDir()
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &restartFixture{ups: ups, dbPath: filepath.Join(dir, "bridge.db"), artworkDir: artworkDir}
}

// open is a store over the fixture's database and an enricher over it, as
// switchEnricher builds one, with nothing in its memory.
func (f *restartFixture) open(t *testing.T) (*Enricher, *manifest.Store) {
	t.Helper()
	store, err := manifest.OpenStore(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	deezer := NewDeezerClient(f.ups.deezer.srv.URL, "t", nil, onDeezerSwitches)
	deezer.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	e := NewEnricher(store,
		NewMusicBrainzClient(f.ups.mb.srv.URL, "t", nil, onSwitches),
		NewCoverArtClient(f.ups.caa.srv.URL, "t", nil, onSwitches),
		deezer, f.artworkDir)
	tuneForTest(e)
	e.PollInterval = time.Millisecond
	return e, store
}

// canonicalArtistMB is a MusicBrainz that answers an artist search only for
// the role-truncated rung of podgerTag, with podgerCanonical, the way the
// ladder narrows a multi-credit tag (TestResolveArtistFetchesPortraitByCanonicalName).
const (
	podgerTag       = "Rachel Podger, Conductor, Brecon Baroque, Ensemble"
	podgerCanonical = "Rachel Podger"
)

func canonicalArtistMB(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
	q, _ := url.QueryUnescape(r.URL.Query().Get("query"))
	switch {
	case strings.HasPrefix(r.URL.Path, "/artist") && strings.Contains(q, "Conductor"):
		_, _ = w.Write([]byte(`{"artists":[]}`))
	case strings.HasPrefix(r.URL.Path, "/artist"):
		fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":%q}]}`, switchArtistMBID, podgerCanonical)
	default:
		http.NotFound(w, r)
	}
}

// TestARefusedPortraitSurvivesARestartAndIsAskedByItsCanonicalName: the name
// the refused lookup would have asked by is MusicBrainz's canonical name for
// the artist, never the tag, and it is the store's, so a restart between the
// refusal and the switch keeps it, and the enricher of the next process, with
// none of the first one's memory, asks Deezer by it.
func TestARefusedPortraitSurvivesARestartAndIsAskedByItsCanonicalName(t *testing.T) {
	ups := newSwitchUpstreams(t)
	ups.mb = newCountingUpstream(t, canonicalArtistMB)
	f := newRestartFixture(t, ups)

	first, store := f.open(t)
	const p = "Podger/Album/01.flac"
	tr := manifest.Track{Path: p, Size: 1, ModTime: time.Now(), Artist: podgerTag, Album: "Album",
		MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("d", 64)}
	if err := store.UpsertTrack(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	first.WithDeezerEnabled(func() bool { return false })
	stop := runEnricher(t, first)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "the track to be enriched with Deezer off")
	stop()
	if q := pendingPortraitsOf(t, store)[p]; q != podgerCanonical {
		t.Fatalf("pending query %q, want the canonical %q", q, podgerCanonical)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	second, store := f.open(t)
	mb := ups.mb.count()
	runEnricher(t, second)
	waitFor(t, func() bool { return fileExists(ArtistImagePath(f.artworkDir, switchArtistMBID)) },
		"the portrait after the restart")
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "the row to be settled")
	searches := 0
	for _, r := range ups.deezer.requests() {
		if !strings.HasPrefix(r, "GET /search/artist") {
			continue
		}
		searches++
		if r != "GET /search/artist?q=Rachel+Podger&limit=5" {
			t.Errorf("Deezer was asked %q, want the canonical name %q", r, podgerCanonical)
		}
	}
	if searches != 1 {
		t.Errorf("%d search(es) after the restart, want one", searches)
	}
	if got := ups.mb.count(); got != mb {
		t.Errorf("MusicBrainz was asked %d time(s) after the restart: the name must come from the store", got-mb)
	}
}

// TestTheFingerprintPathsRefusedPortraitIsFetchedOnceDeezerIsOn: a track whose
// artist came from its fingerprint records the refused lookup too, under the
// fingerprint's canonical name, and its portrait arrives once Deezer is on.
func TestTheFingerprintPathsRefusedPortraitIsFetchedOnceDeezerIsOn(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	const untagged = "Unknown/01.flac"
	tr := manifest.Track{Path: untagged, Size: 1, ModTime: time.Now()}
	if err := store.UpsertTrack(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.WithAcousticFallback(fixedAcoustic{untagged: {ArtistMBID: recoveredArtistMBID, ArtistName: "Recovered"}})
	var deezerOn atomic.Bool
	e.WithDeezerEnabled(deezerOn.Load)
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "the track to be enriched with Deezer off")
	if q := pendingPortraitsOf(t, store)[untagged]; q != "Recovered" {
		t.Fatalf("pending query %q, want the fingerprint's %q", q, "Recovered")
	}
	mb, caa, atlas := ups.mb.count(), ups.caa.count(), ups.atlas.count()

	deezerOn.Store(true)
	waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, recoveredArtistMBID)) },
		"the recovered artist's portrait once Deezer is on")
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "the row to be settled")
	stop()
	requireNoRequestsSince(t, ups, mb, caa, atlas)
	if got := ups.deezer.requests(); len(got) == 0 || got[0] != "GET /search/artist?q=Recovered&limit=5" {
		t.Errorf("Deezer was asked %v, want a search for %q first", got, "Recovered")
	}
}

// TestAPortraitTheSwitchCutOffIsFetchedOnceItIsOnAgain: Deezer switched off
// while a portrait's search is on the wire cuts the search off, and the pass
// records the lookup as refused; switched on again, the portrait arrives. The
// same holds for a search the portrait batch itself had on the wire.
func TestAPortraitTheSwitchCutOffIsFetchedOnceItIsOnAgain(t *testing.T) {
	for _, inBatch := range []bool{false, true} {
		name := "in an enrichment pass"
		if inBatch {
			name = "in the portrait batch"
		}
		t.Run(name, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			e, store, artworkDir := switchEnricher(t, ups)
			e.switchEvery = time.Millisecond
			var deezerOn atomic.Bool
			if inBatch {
				stampPending(t, store, "Artist/Album/01.flac", switchArtistMBID, "Artist")
			} else {
				seedSwitchTracks(t, store)
			}
			deezerOn.Store(true)
			e.WithDeezerEnabled(deezerOn.Load)
			searching := make(chan struct{})
			cutOff := make(chan struct{})
			var first sync.Once
			ups.deezer.setHold(func(r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/search/") {
					return
				}
				isFirst := false
				first.Do(func() { isFirst = true })
				if !isFirst {
					return
				}
				close(searching)
				select {
				case <-r.Context().Done():
					close(cutOff)
				case <-time.After(10 * time.Second):
				}
			})
			stop := runEnricher(t, e)
			select {
			case <-searching:
			case <-time.After(5 * time.Second):
				t.Fatal("Deezer's search was never sent")
			}
			deezerOn.Store(false)
			select {
			case <-cutOff:
			case <-time.After(10 * time.Second):
				t.Fatal("the search held at the switch-off was not cut off")
			}
			waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "every row to be stamped")
			waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) > 0 }, "the cut-off lookup to be kept as refused")
			if e.deezerNegCache.Has(switchArtistMBID) {
				t.Fatal("the cut-off search was negative-cached")
			}
			if fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
				t.Fatal("a portrait was cached while Deezer was off")
			}

			deezerOn.Store(true)
			waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) },
				"the portrait once Deezer is on again")
			waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "every row to be settled")
			stop()
		})
	}
}

// TestTurningDeezerOnWakesAnIdleEnricher: the enricher idles between its
// polls, an hour here; turning Deezer on while it idles wakes it, and the
// portrait batch runs with no scan and no poll's end. On main the enricher
// slept its poll through, and asked nothing of Deezer's switch meanwhile.
func TestTurningDeezerOnWakesAnIdleEnricher(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	e.PollInterval = time.Hour
	e.switchEvery = time.Millisecond
	seedSwitchTracks(t, store)
	var deezerOn atomic.Bool
	var deezerAsked atomic.Int64
	e.WithDeezerEnabled(func() bool {
		deezerAsked.Add(1)
		return deezerOn.Load()
	})
	runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 && len(pendingPortraitsOf(t, store)) == 2 },
		"both tracks to be enriched with Deezer off")
	from := deezerAsked.Load()
	waitFor(t, func() bool { return deezerAsked.Load() >= from+5 }, "the idle enricher to ask Deezer's switch")

	deezerOn.Store(true)
	waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) },
		"the portrait once Deezer is on, with the enricher idle")
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "both rows to be settled")
}

// TestOneFetchServesAnAlbumsTwelveRows: twelve rows of one artist, read five
// at a time, cost Deezer one search and one image, and all twelve are settled:
// the rows of a later page find the portrait in the cache.
func TestOneFetchServesAnAlbumsTwelveRows(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	e.PortraitBatchLimit = 5
	for i := 1; i <= 12; i++ {
		stampPending(t, store, fmt.Sprintf("Artist/Album/%02d.flac", i), switchArtistMBID, "Artist")
	}
	if got := len(pendingPortraitsOf(t, store)); got != 12 {
		t.Fatalf("%d pending rows seeded, want 12", got)
	}
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "all twelve rows to be settled")
	stop()
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Error("no portrait cached")
	}
	if got := ups.deezer.requests(); len(got) != 2 {
		t.Errorf("Deezer was asked %d time(s) for one artist's twelve rows, want one search and one image:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// TestAPortraitAlreadyCachedIsSettledWithNoRequest: a portrait in the cache
// by the time Deezer is on settles its rows with no request.
func TestAPortraitAlreadyCachedIsSettledWithNoRequest(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	stampPending(t, store, "Artist/Album/01.flac", switchArtistMBID, "Artist")
	requirePending(t, store, "Artist/Album/01.flac", "Artist")
	if err := os.WriteFile(ArtistImagePath(artworkDir, switchArtistMBID), switchJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "the row to be settled")
	stop()
	if got := ups.deezer.requests(); len(got) != 0 {
		t.Errorf("Deezer was asked for a portrait in the cache:\n%s", strings.Join(got, "\n"))
	}
}

// TestATerminalMissIsSettledAndNotAskedAgain: Deezer's answer that it has no
// such artist settles the rows, and nothing asks it again however many turns
// the enricher takes.
func TestATerminalMissIsSettledAndNotAskedAgain(t *testing.T) {
	ups := newSwitchUpstreams(t)
	ups.deezer = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	e, store, _ := switchEnricher(t, ups)
	stampPending(t, store, "Artist/Album/01.flac", switchArtistMBID, "Artist")
	stampPending(t, store, "Artist/Album/02.flac", switchArtistMBID, "Artist")
	requirePending(t, store, "Artist/Album/02.flac", "Artist")
	var deezerAsked atomic.Int64
	e.WithDeezerEnabled(func() bool {
		deezerAsked.Add(1)
		return true
	})
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(pendingPortraitsOf(t, store)) == 0 }, "both rows to be settled by the miss")
	from := deezerAsked.Load()
	waitFor(t, func() bool { return deezerAsked.Load() >= from+20 }, "twenty more turns of the enricher")
	stop()
	if got := ups.deezer.requests(); len(got) != 1 {
		t.Errorf("Deezer was asked %d time(s), want the one search whose miss settled both rows:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// failingSearchDeezer is a Deezer whose search answers 503 while fail says so,
// and otherwise a picture, as newSwitchUpstreams' does.
func failingSearchDeezer(t *testing.T, fail func(name string) bool) *countingUpstream {
	t.Helper()
	return newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/artist"):
			if fail(r.URL.Query().Get("q")) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":1,"name":%q,"picture_xl":%q,"picture_big":""}]}`,
				r.URL.Query().Get("q"), u.srv.URL+"/picture.jpg")
		case r.URL.Path == "/picture.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(switchJPEG)
		default:
			http.NotFound(w, r)
		}
	})
}

// TestATransientFailureKeepsThePortraitAndPacesItsRetries: a 503 keeps the
// row's query, and its artist is asked again only once its retry is due, not
// at every turn; once Deezer answers, the portrait settles the row.
func TestATransientFailureKeepsThePortraitAndPacesItsRetries(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(string) bool { return failing.Load() })
	e, store, artworkDir := switchEnricher(t, ups)
	now := time.Unix(1_800_000_000, 0)
	e.clock = func() time.Time { return now }
	const p = "Artist/Album/01.flac"
	stampPending(t, store, p, switchArtistMBID, "Artist")
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		e.runPortraits(ctx)
	}
	if got := ups.deezer.count(); got != 1 {
		t.Fatalf("Deezer was asked %d time(s) in ten turns at one instant, want one", got)
	}
	if q := pendingPortraitsOf(t, store)[p]; q != "Artist" {
		t.Fatalf("a transient failure left the query %q, want it kept", q)
	}

	now = now.Add(portraitRetryFirst)
	for i := 0; i < 10; i++ {
		e.runPortraits(ctx)
	}
	if got := ups.deezer.count(); got != 2 {
		t.Fatalf("Deezer was asked %d time(s) once the retry was due, want two", got)
	}
	if q := pendingPortraitsOf(t, store)[p]; q != "Artist" {
		t.Fatalf("a second transient failure left the query %q, want it kept", q)
	}

	failing.Store(false)
	now = now.Add(portraitRetryFirst)
	for i := 0; i < 10; i++ {
		e.runPortraits(ctx)
	}
	if got := ups.deezer.count(); got != 2 {
		t.Fatalf("Deezer was asked %d time(s) before the doubled retry was due, want two", got)
	}
	now = now.Add(portraitRetryFirst)
	e.runPortraits(ctx)
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Fatal("no portrait once Deezer answered")
	}
	if _, ok := pendingPortraitsOf(t, store)[p]; ok {
		t.Error("the row is still pending once its portrait arrived")
	}
}

// TestAPassThatFindsNoArtistDueRestsAndReadsNothing: once a pass has found
// every pending artist waiting for its retry, the batch reads nothing until
// the earliest of those retries, however many turns come meanwhile; at that
// retry it reads again.
func TestAPassThatFindsNoArtistDueRestsAndReadsNothing(t *testing.T) {
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(string) bool { return true })
	e, store, _ := switchEnricher(t, ups)
	now := time.Unix(1_800_000_000, 0)
	e.clock = func() time.Time { return now }
	stampPending(t, store, "Artist/Album/01.flac", switchArtistMBID, "Artist")
	requirePending(t, store, "Artist/Album/01.flac", "Artist")
	ctx := context.Background()
	e.runPortraits(ctx) // asks, and fails
	e.runPortraits(ctx) // finds the artist waiting: the batch rests
	if got := ups.deezer.count(); got != 1 {
		t.Fatalf("Deezer was asked %d time(s), want the one failed search", got)
	}

	rec := loggingtest.Record(t)
	_ = store.Close() // any read of the store now fails, and the batch says so
	for i := 0; i < 5; i++ {
		e.runPortraits(ctx)
	}
	if got := rec.Failures("list pending portraits"); len(got) != 0 {
		t.Fatalf("the resting batch read the store:\n%s", strings.Join(got, "\n"))
	}
	now = now.Add(portraitRetryFirst)
	e.runPortraits(ctx)
	if got := rec.Failures("list pending portraits"); len(got) != 1 {
		t.Errorf("the batch read the store %d time(s) once the retry was due, want once", len(got))
	}
}

// TestARefusalKeptWhileTheBatchRestsEndsTheRest: a pass that keeps a refused
// lookup while the batch rests makes its row due at once: the next turn reads
// it and fetches its portrait, though the failing artist the rest waits for
// is still not due.
func TestARefusalKeptWhileTheBatchRestsEndsTheRest(t *testing.T) {
	const failingMBID = "99999999-9999-4999-8999-999999999999"
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(name string) bool { return name == "Failing" })
	e, store, artworkDir := switchEnricher(t, ups)
	now := time.Unix(1_800_000_000, 0)
	e.clock = func() time.Time { return now }
	var deezerOn atomic.Bool
	deezerOn.Store(true)
	e.WithDeezerEnabled(deezerOn.Load)
	stampPending(t, store, "A Failing/Album/01.flac", failingMBID, "Failing")
	ctx := context.Background()
	e.runPortraits(ctx) // asks, and fails
	e.runPortraits(ctx) // finds the artist waiting: the batch rests

	deezerOn.Store(false)
	tr := manifest.Track{Path: "B Artist/Album/01.flac", Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album",
		MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("f", 64)}
	if err := store.UpsertTrack(ctx, &tr); err != nil {
		t.Fatal(err)
	}
	rows, err := store.UnenrichedTracks(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("UnenrichedTracks = %d rows (%v), want the new one", len(rows), err)
	}
	e.enrichOne(ctx, &rows[0])
	requirePending(t, store, "B Artist/Album/01.flac", "Artist")

	deezerOn.Store(true)
	e.runPortraits(ctx)
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Fatal("the refused lookup kept during the rest waited for the rest to end")
	}
	if _, ok := pendingPortraitsOf(t, store)["B Artist/Album/01.flac"]; ok {
		t.Error("the fetched portrait's row is still pending")
	}
	failed := 0
	for _, r := range ups.deezer.requests() {
		if r == "GET /search/artist?q=Failing&limit=5" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("the failing artist was asked %d time(s), want once: its retry is not due", failed)
	}
}

// TestAQueryOutlivesRetryMissingAndATransientFailure: a refusal, then "Retry
// missing", then the re-enrichment's portrait failing transiently, then
// Deezer recovering: the query survives every step until the portrait arrives.
func TestAQueryOutlivesRetryMissingAndATransientFailure(t *testing.T) {
	var failing atomic.Bool
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(string) bool { return failing.Load() })
	e, store, artworkDir := switchEnricher(t, ups)
	now := time.Unix(1_800_000_000, 0)
	e.clock = func() time.Time { return now }
	seedSwitchTracks(t, store)
	var deezerOn atomic.Bool
	e.WithDeezerEnabled(deezerOn.Load)
	ctx := context.Background()

	for e.runBatch(ctx) != e.PollInterval {
	}
	requireQueries(t, store, "Artist", "the refusal")

	// "Retry missing" hands the rows back to the ordinary pass, whose own
	// attempt asks for the portrait; the batch reads none of them meanwhile.
	if _, err := store.ResetEnrichedByArtistMBIDs(ctx, []string{switchArtistMBID}); err != nil {
		t.Fatal(err)
	}
	if got := len(unenrichedPaths(t, store)); got != len(switchTracks) {
		t.Fatalf("%d row(s) back in the queue after Retry missing, want %d", got, len(switchTracks))
	}

	failing.Store(true)
	deezerOn.Store(true)
	for e.runBatch(ctx) != e.PollInterval {
	}
	if len(unenrichedPaths(t, store)) != 0 {
		t.Fatal("the re-enrichment left rows unstamped")
	}
	if ups.deezer.count() == 0 {
		t.Fatal("the re-enrichment never asked Deezer")
	}
	requireQueries(t, store, "Artist", "Retry missing and the re-enrichment's transient failure")
	e.runPortraits(ctx)
	requireQueries(t, store, "Artist", "the portrait batch's turn within the failure's pacing")

	failing.Store(false)
	now = now.Add(portraitRetryMost)
	e.runPortraits(ctx)
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Fatal("no portrait once Deezer recovered")
	}
	if got := pendingPortraitsOf(t, store); len(got) != 0 {
		t.Errorf("rows still pending once the portrait arrived: %v", got)
	}
}

// requireQueries fails unless every seeded switch track carries query.
func requireQueries(t *testing.T, store *manifest.Store, query, after string) {
	t.Helper()
	pending := pendingPortraitsOf(t, store)
	for _, tr := range switchTracks {
		if q, ok := pending[tr.Path]; !ok || q != query {
			t.Fatalf("after %s, %s carries %q (%v), want %q", after, tr.Path, q, ok, query)
		}
	}
}

// TestFailingRowsAtTheHeadDoNotStarveTheRest: with more pending rows than a
// batch reads, and the first in the order failing, the rows behind them are
// still fetched, and the failing artist is not asked at every turn.
func TestFailingRowsAtTheHeadDoNotStarveTheRest(t *testing.T) {
	const failingMBID = "66666666-6666-4666-8666-666666666666"
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(name string) bool { return name == "Failing" })
	e, store, artworkDir := switchEnricher(t, ups)
	e.PortraitBatchLimit = 2
	stampPending(t, store, "A Failing/Album/01.flac", failingMBID, "Failing")
	stampPending(t, store, "A Failing/Album/02.flac", failingMBID, "Failing")
	stampPending(t, store, "A Failing/Album/03.flac", failingMBID, "Failing")
	stampPending(t, store, "B Artist/Album/01.flac", switchArtistMBID, "Artist")
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) },
		"the portrait of the row behind the failing ones")
	waitFor(t, func() bool { _, ok := pendingPortraitsOf(t, store)["B Artist/Album/01.flac"]; return !ok },
		"the row behind the failing ones to be settled")
	stop()
	failed := 0
	for _, r := range ups.deezer.requests() {
		if r == "GET /search/artist?q=Failing&limit=5" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("the failing artist was asked %d time(s), want once within its retry's pacing", failed)
	}
	if got := len(pendingPortraitsOf(t, store)); got != 3 {
		t.Errorf("%d rows pending, want the failing artist's three kept", got)
	}
}

// TestPortraitsAndOrdinaryWorkBothProgress: while the ordinary queue never
// empties, a batch of one row and one portrait batch take each turn, so the
// pending portrait is fetched long before the queue drains, and the queue
// drains too.
func TestPortraitsAndOrdinaryWorkBothProgress(t *testing.T) {
	const waitingMBID = "77777777-7777-4777-8777-777777777777"
	ups := newSwitchUpstreams(t)
	ups.mb = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":%q}]}`, switchArtistMBID, artistQueryName(r))
	})
	e, store, artworkDir := switchEnricher(t, ups)
	e.BatchLimit = 1
	stampPending(t, store, "Waiting/Album/01.flac", waitingMBID, "Waiting")
	const queued = 30
	for i := 0; i < queued; i++ {
		tr := manifest.Track{Path: fmt.Sprintf("Queue/Album/%02d.flac", i), Size: 1, ModTime: time.Now(),
			Artist: fmt.Sprintf("Queued %02d", i), Album: "Album", MusicBrainzAlbumID: switchReleaseMBID,
			ArtworkMBID: "local-" + strings.Repeat("e", 64)}
		if err := store.UpsertTrack(context.Background(), &tr); err != nil {
			t.Fatal(err)
		}
	}
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, waitingMBID)) },
		"the pending portrait while the queue is long")
	if left := len(unenrichedPaths(t, store)); left == 0 {
		t.Errorf("the queue of %d rows had drained before the pending portrait arrived", queued)
	}
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "the queue to drain")
	stop()
	if _, ok := pendingPortraitsOf(t, store)["Waiting/Album/01.flac"]; ok {
		t.Error("the fetched portrait's row is still pending")
	}
}

// artistQueryName is the artist name a MusicBrainz artist search asks for:
// its query is `artist:"<name>"`.
func artistQueryName(r *http.Request) string {
	q := r.URL.Query().Get("query")
	q = strings.TrimPrefix(q, `artist:"`)
	if i := strings.Index(q, `"`); i >= 0 {
		q = q[:i]
	}
	return q
}

// TestAStaleClearLeavesTheReplacementsQuery: the portrait batch fetched a
// portrait and, before its clear, the row was retagged to another artist and
// enriched again with Deezer off, so it carries a query of its own; the clear
// the batch then makes is of the row it read, which is gone, and leaves the
// replacement's query.
func TestAStaleClearLeavesTheReplacementsQuery(t *testing.T) {
	const otherMBID = "88888888-8888-4888-8888-888888888888"
	ups := newSwitchUpstreams(t)
	ups.mb = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		id := switchArtistMBID
		if artistQueryName(r) == "Other" {
			id = otherMBID
		}
		if strings.HasPrefix(r.URL.Path, "/artist") {
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":%q}]}`, id, artistQueryName(r))
			return
		}
		http.NotFound(w, r)
	})
	e, store, artworkDir := switchEnricher(t, ups)
	const p = "Artist/Album/01.flac"
	stampPending(t, store, p, switchArtistMBID, "Artist")

	// The batch parks at each clear: the first is the stale one; the second
	// is the next batch's, which reaches its clear only if the replacement's
	// query outlived the first.
	var clears atomic.Int64
	parked := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	e.beforePortraitClear = func() {
		clears.Add(1)
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
	}
	runEnricher(t, e)
	// Registered after runEnricher's stop, so it runs first: a batch parked
	// at its clear is let go before Run is asked to return.
	t.Cleanup(func() { close(release) })
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the portrait batch never reached its clear")
	}
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Fatal("the batch parked before its portrait was cached")
	}

	ctx := context.Background()
	retagged := manifest.Track{Path: p, Size: 2, ModTime: time.Now(), Artist: "Other", Album: "Album",
		MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("c", 64)}
	if err := store.UpsertTrack(ctx, &retagged); err != nil {
		t.Fatal(err)
	}
	offline, _, _ := switchEnricherOver(t, ups, store, artworkDir)
	offline.WithDeezerEnabled(func() bool { return false })
	rows, err := store.UnenrichedTracks(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("UnenrichedTracks = %d rows (%v), want the retagged one", len(rows), err)
	}
	offline.enrichOne(ctx, &rows[0])
	if q := pendingPortraitsOf(t, store)[p]; q != "Other" {
		t.Fatalf("the replacement carries %q, want its own refused lookup's %q", q, "Other")
	}

	release <- struct{}{}
	waitFor(t, func() bool {
		_, pending := pendingPortraitsOf(t, store)[p]
		return clears.Load() >= 2 || !pending
	}, "the stale clear to be made and the next batch to reach its own")
	if q, ok := pendingPortraitsOf(t, store)[p]; !ok || q != "Other" {
		t.Errorf("the stale clear left the replacement's query %q (%v), want %q kept", q, ok, "Other")
	}
}

// switchEnricherOver is switchEnricher over a store and an artwork directory
// that already exist.
func switchEnricherOver(t *testing.T, ups *switchUpstreams, store *manifest.Store, artworkDir string) (*Enricher, *manifest.Store, string) {
	t.Helper()
	deezer := NewDeezerClient(ups.deezer.srv.URL, "t", nil, onDeezerSwitches)
	deezer.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	e := NewEnricher(store,
		NewMusicBrainzClient(ups.mb.srv.URL, "t", nil, onSwitches),
		NewCoverArtClient(ups.caa.srv.URL, "t", nil, onSwitches),
		deezer, artworkDir)
	tuneForTest(e)
	e.PollInterval = time.Millisecond
	return e, store, artworkDir
}
