package enrich

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A portrait's lookup bound to the artist it was taken for, one backoff for
// every path that asks Deezer for a portrait, a lookup that waits recorded
// for the batch whatever made it wait, an answer that ends a name told apart
// from one that settles the artist, a pending answer that ends, and the wait
// between turns woken by a switch that came back while a batch was refused
// (backlog B761, its second round).

// enrichQueued runs one ordinary pass over the rows that wait for the
// enricher, as a batch does.
func enrichQueued(t *testing.T, e *Enricher, store *manifest.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		e.enrichOne(ctx, &rows[i])
	}
}

// pendingRowsOf is each pending row as the batch reads it, by path.
func pendingRowsOf(t *testing.T, store *manifest.Store) map[string]manifest.PendingPortrait {
	t.Helper()
	rows, err := store.PendingPortraits(context.Background(), "", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]manifest.PendingPortrait, len(rows))
	for _, r := range rows {
		out[r.Path] = r
	}
	return out
}

// requirePendingPortrait fails unless the batch reads path as waiting for
// mbid's portrait by query.
func requirePendingPortrait(t *testing.T, store *manifest.Store, path, mbid, query string) {
	t.Helper()
	r, ok := pendingRowsOf(t, store)[path]
	if !ok {
		t.Fatalf("%s waits for no portrait, want %s's by %q", path, mbid, query)
	}
	if r.ArtistMBID != mbid || r.Query != query {
		t.Fatalf("%s waits for %s's portrait by %q, want %s's by %q", path, r.ArtistMBID, r.Query, mbid, query)
	}
}

// searchesFor is how many times Deezer's artist search was asked for name.
func searchesFor(u *countingUpstream, name string) int {
	n := 0
	for _, r := range u.requests() {
		rest, ok := strings.CutPrefix(r, "GET /search/artist?")
		if !ok {
			continue
		}
		if q, err := url.ParseQuery(rest); err == nil && q.Get("q") == name {
			n++
		}
	}
	return n
}

// searches is how many times Deezer's artist search was asked at all.
func searches(u *countingUpstream) int {
	n := 0
	for _, r := range u.requests() {
		if strings.HasPrefix(r, "GET /search/artist") {
			n++
		}
	}
	return n
}

// TestAReEnrichmentThatResolvesAnotherArtistNeverAsksByTheFirstName: a
// lookup Deezer's switch refused is kept for the first artist MusicBrainz
// named; after a restart, "Retry missing" re-enriches the row, MusicBrainz
// now names a second artist, and the second artist's portrait fails. The row
// then waits for the second artist's portrait by the second artist's name,
// and the batch fetches that, never the first artist's face under the second
// artist's MBID. On main the failed attempt kept the first artist's name,
// which the batch paired with the second artist's MBID.
func TestAReEnrichmentThatResolvesAnotherArtistNeverAsksByTheFirstName(t *testing.T) {
	const firstMBID, secondMBID = "a1a1a1a1-0761-4761-8761-0000000000a1", "b2b2b2b2-0761-4761-8761-0000000000b2"
	var second, secondFails atomic.Bool
	ups := newSwitchUpstreams(t)
	ups.mb = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/artist") {
			http.NotFound(w, r)
			return
		}
		id, name := firstMBID, "Name A"
		if second.Load() {
			id, name = secondMBID, "Name B"
		}
		fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":%q}]}`, id, name)
	})
	jpegA, jpegB := []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x0A}, []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x0B}
	ups.deezer = newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/artist"):
			q := r.URL.Query().Get("q")
			if q == "Name B" && secondFails.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			picture := "/a.jpg"
			if q == "Name B" {
				picture = "/b.jpg"
			}
			fmt.Fprintf(w, `{"data":[{"id":1,"name":%q,"picture_xl":%q,"picture_big":""}]}`, q, u.srv.URL+picture)
		case r.URL.Path == "/a.jpg":
			_, _ = w.Write(jpegA)
		case r.URL.Path == "/b.jpg":
			_, _ = w.Write(jpegB)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	f := newRestartFixture(t, ups)
	first, store := f.open(t)
	const p = "Artist/Album/01.flac"
	tr := manifest.Track{Path: p, Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album",
		MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("d", 64)}
	if err := store.UpsertTrack(ctx, &tr); err != nil {
		t.Fatal(err)
	}
	first.WithDeezerEnabled(func() bool { return false })
	enrichQueued(t, first, store)
	requirePendingPortrait(t, store, p, firstMBID, "Name A")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, store := f.open(t)
	now := time.Unix(1_800_000_000, 0)
	restarted.clock = func() time.Time { return now }
	second.Store(true)
	secondFails.Store(true)
	if n, err := store.ResetEnrichedByArtistMBIDs(ctx, []string{firstMBID}); err != nil || n != 1 {
		t.Fatalf("Retry missing re-queued %d (%v), want the row", n, err)
	}
	enrichQueued(t, restarted, store)
	if got := searchesFor(ups.deezer, "Name B"); got != 1 {
		t.Fatalf("the second artist's portrait was asked for %d time(s) by the re-enrichment, want once", got)
	}
	requirePendingPortrait(t, store, p, secondMBID, "Name B")

	secondFails.Store(false)
	now = now.Add(portraitRetryMost)
	restarted.runPortraits(ctx)
	got, err := os.ReadFile(ArtistImagePath(f.artworkDir, secondMBID))
	if err != nil {
		t.Fatalf("the second artist's portrait: %v", err)
	}
	if !bytes.Equal(got, jpegB) {
		t.Errorf("the second artist's portrait is %x, want its own picture %x (the first artist's is %x)", got, jpegB, jpegA)
	}
	if n := searchesFor(ups.deezer, "Name A"); n != 0 {
		t.Errorf("Deezer was asked by the first artist's name %d time(s): it was never on while that artist was the row's", n)
	}
}

// TestTheBackoffHoldsEveryPathThatAsksForAPortrait: once an artist's portrait
// lookup by its name has failed, neither a sibling track's enrichment nor
// "Retry missing" asks Deezer for it again before its retry is due; both rows
// then wait for the batch, which asks once when it is.
func TestTheBackoffHoldsEveryPathThatAsksForAPortrait(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(string) bool { return failing.Load() })
	e, store, artworkDir := switchEnricher(t, ups)
	now := time.Unix(1_800_000_000, 0)
	e.clock = func() time.Time { return now }
	ctx := context.Background()
	first := switchTracks[0]
	first.ModTime = time.Now()
	if err := store.UpsertTrack(ctx, &first); err != nil {
		t.Fatal(err)
	}
	enrichQueued(t, e, store)
	if got := searches(ups.deezer); got != 1 {
		t.Fatalf("the first track's portrait was asked for %d time(s), want once", got)
	}

	sibling := manifest.Track{Path: "Artist/Second Album/01.flac", Size: 1, ModTime: time.Now(), Artist: "Artist",
		Album: "Second Album", MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("9", 64)}
	if err := store.UpsertTrack(ctx, &sibling); err != nil {
		t.Fatal(err)
	}
	enrichQueued(t, e, store)
	if got := searches(ups.deezer); got != 1 {
		t.Fatalf("a sibling's enrichment asked Deezer during the artist's backoff: %d search(es), want the one that failed", got)
	}
	if n, err := store.ResetEnrichedByArtistMBIDs(ctx, []string{switchArtistMBID}); err != nil || n != 2 {
		t.Fatalf("Retry missing re-queued %d (%v), want both rows", n, err)
	}
	enrichQueued(t, e, store)
	if got := searches(ups.deezer); got != 1 {
		t.Fatalf("Retry missing asked Deezer during the artist's backoff: %d search(es), want the one that failed", got)
	}
	for _, p := range []string{first.Path, sibling.Path} {
		requirePendingPortrait(t, store, p, switchArtistMBID, "Artist")
	}

	failing.Store(false)
	now = now.Add(portraitRetryFirst)
	for i := 0; i < 3; i++ {
		e.runPortraits(ctx)
		enrichQueued(t, e, store)
	}
	if got := searches(ups.deezer); got != 2 {
		t.Errorf("Deezer was searched %d time(s) once the retry was due, want once more", got)
	}
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Fatal("no portrait once the retry was due")
	}
	if got := pendingRowsOf(t, store); len(got) != 0 {
		t.Errorf("rows still wait once the portrait arrived: %v", got)
	}
}

// TestAFailedAttemptWaitsForTheBatchAndIsFetchedOnceItsBackoffEnds: a pass
// whose portrait lookup failed, with no lookup the switch refused before it,
// leaves the row waiting for the portrait by the name it asked, through the
// ordinary pass and the fingerprint's alike, and the batch fetches it once the
// retry is due. On main the failure was in memory alone: the row was stamped
// with nothing kept, and no queue came back to it.
func TestAFailedAttemptWaitsForTheBatchAndIsFetchedOnceItsBackoffEnds(t *testing.T) {
	for _, path := range []string{"the ordinary pass", "the fingerprint path"} {
		t.Run(path, func(t *testing.T) {
			var failing atomic.Bool
			failing.Store(true)
			ups := newSwitchUpstreams(t)
			ups.deezer = failingSearchDeezer(t, func(string) bool { return failing.Load() })
			e, store, artworkDir := switchEnricher(t, ups)
			now := time.Unix(1_800_000_000, 0)
			e.clock = func() time.Time { return now }
			ctx := context.Background()
			p, mbid, name := switchTracks[0].Path, switchArtistMBID, "Artist"
			tr := switchTracks[0]
			if path == "the fingerprint path" {
				p, mbid, name = "Unknown/01.flac", recoveredArtistMBID, "Recovered"
				tr = manifest.Track{Path: p, Size: 1}
				e.WithAcousticFallback(fixedAcoustic{p: {ArtistMBID: recoveredArtistMBID, ArtistName: "Recovered"}})
			}
			tr.ModTime = time.Now()
			if err := store.UpsertTrack(ctx, &tr); err != nil {
				t.Fatal(err)
			}
			enrichQueued(t, e, store)
			if left := unenrichedPaths(t, store); len(left) != 0 {
				t.Fatalf("the pass left %v unstamped", left)
			}
			if got := searchesFor(ups.deezer, name); got != 1 {
				t.Fatalf("the portrait was asked for %d time(s) by %q, want once", got, name)
			}
			requirePendingPortrait(t, store, p, mbid, name)

			failing.Store(false)
			now = now.Add(portraitRetryFirst)
			e.runPortraits(ctx)
			if !fileExists(ArtistImagePath(artworkDir, mbid)) {
				t.Fatal("no portrait once the retry was due")
			}
			if got := pendingRowsOf(t, store); len(got) != 0 {
				t.Errorf("rows still wait once the portrait arrived: %v", got)
			}
		})
	}
}

// TestEachPortraitFailureAnswersTheRoutesProbeAsItWillGo: one attempt of
// the batch at a waiting row's portrait, for each way it can fail, and what
// the route's probe then answers: a failure that waiting may mend (a 503,
// Deezer unreachable, no artwork cache to write to) keeps the row and
// answers pending; an answer that will not change for the name it was asked
// by (a picture on a host the client refuses, an answer that is no image, no
// such artist, a picture that is gone) drops the row's query and answers
// no_image. On main the first two of those answers kept the row, pending for
// good.
func TestEachPortraitFailureAnswersTheRoutesProbeAsItWillGo(t *testing.T) {
	type answer func(u *countingUpstream, w http.ResponseWriter, r *http.Request)
	picture := func(on func(u *countingUpstream) string) answer {
		return func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/search/artist"):
				fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q,"picture_big":""}]}`, on(u))
			case r.URL.Path == "/page.jpg":
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html><body>Not a picture</body></html>"))
			default:
				http.NotFound(w, r)
			}
		}
	}
	for _, c := range []struct {
		name        string
		deezer      answer
		unreachable bool
		noCache     bool
		waits       bool
	}{
		{name: "a 503", deezer: func(_ *countingUpstream, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, waits: true},
		{name: "Deezer unreachable", unreachable: true, waits: true},
		{name: "no artwork cache to write to", noCache: true, waits: true},
		{name: "a picture on a host the client refuses", deezer: picture(func(*countingUpstream) string {
			return "http://refused.invalid/picture.jpg"
		})},
		{name: "an answer that is no image", deezer: picture(func(u *countingUpstream) string { return u.srv.URL + "/page.jpg" })},
		{name: "no such artist", deezer: func(_ *countingUpstream, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		}},
		{name: "a picture that is gone", deezer: picture(func(u *countingUpstream) string { return u.srv.URL + "/gone.jpg" })},
	} {
		t.Run(c.name, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			if c.deezer != nil {
				ups.deezer = newCountingUpstream(t, c.deezer)
			}
			if c.unreachable {
				ups.deezer.srv.Close()
			}
			e, store, _ := switchEnricher(t, ups)
			if c.noCache {
				e.CacheDir = ""
			}
			e.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }
			ctx := context.Background()
			const p = "Artist/Album/01.flac"
			stampPending(t, store, p, switchArtistMBID, "Artist")
			e.runPortraits(ctx)
			_, kept := pendingRowsOf(t, store)[p]
			pending, err := store.ArtistMBIDPortraitPending(ctx, switchArtistMBID)
			if err != nil {
				t.Fatal(err)
			}
			if kept != c.waits || pending != c.waits {
				t.Errorf("after the failure the row is kept: %v, and the route's probe answers pending: %v; want %v and %v",
					kept, pending, c.waits, c.waits)
			}
		})
	}
}

// TestANameThatFailsForGoodLeavesTheArtistsOtherNameToBeAsked: two names for
// one artist's portrait, the first answered with a picture on a host the
// client refuses, which ends that name, and the second with the portrait:
// whether both are on one page of the batch or on two, the second is asked
// and the portrait arrives. On main the batch grouped the artist's rows under
// the first name alone, and a failure of that name put off the whole artist.
func TestANameThatFailsForGoodLeavesTheArtistsOtherNameToBeAsked(t *testing.T) {
	for _, c := range []struct {
		name  string
		limit int
	}{{"on one page", 10}, {"on two pages", 1}} {
		t.Run(c.name, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			ups.deezer = newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/search/artist"):
					host := u.srv.URL
					if r.URL.Query().Get("q") == "First" {
						host = "http://refused.invalid"
					}
					fmt.Fprintf(w, `{"data":[{"id":1,"name":%q,"picture_xl":%q,"picture_big":""}]}`,
						r.URL.Query().Get("q"), host+"/picture.jpg")
				case r.URL.Path == "/picture.jpg":
					_, _ = w.Write(switchJPEG)
				default:
					http.NotFound(w, r)
				}
			})
			e, store, artworkDir := switchEnricher(t, ups)
			e.PortraitBatchLimit = c.limit
			e.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }
			ctx := context.Background()
			stampPending(t, store, "A/Album/01.flac", switchArtistMBID, "First")
			stampPending(t, store, "B/Album/01.flac", switchArtistMBID, "Second")
			for i := 0; i < 3; i++ {
				e.runPortraits(ctx)
			}
			if got := searchesFor(ups.deezer, "Second"); got != 1 {
				t.Errorf("the second name was asked %d time(s), want once", got)
			}
			if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
				t.Fatal("no portrait: the first name's failure put off the second")
			}
			if got := pendingRowsOf(t, store); len(got) != 0 {
				t.Errorf("rows still wait once the portrait arrived: %v", got)
			}
		})
	}
}

// TestAnExhaustedPortraitStaysSoAcrossAReopenAndALaterSuccessServesIt: once
// an artist's portrait lookup has failed PortraitPendingAttempts times the
// route's probe answers no longer pending, and that, and the backoff, outlive
// a restart: the enricher of the next process asks nothing before the retry
// is due. The batch still asks when it is, and a success then caches the
// portrait, settles the row and forgets the failures. On main the probe
// answered pending for good, and a restart forgot the backoff.
func TestAnExhaustedPortraitStaysSoAcrossAReopenAndALaterSuccessServesIt(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	ups := newSwitchUpstreams(t)
	ups.deezer = failingSearchDeezer(t, func(string) bool { return failing.Load() })
	ctx := context.Background()
	f := newRestartFixture(t, ups)
	first, store := f.open(t)
	now := time.Unix(1_800_000_000, 0)
	first.clock = func() time.Time { return now }
	const p = "Artist/Album/01.flac"
	stampPending(t, store, p, switchArtistMBID, "Artist")
	for i := 0; i < manifest.PortraitPendingAttempts; i++ {
		if i > 0 {
			now = now.Add(portraitRetryMost)
		}
		first.runPortraits(ctx)
	}
	asked := searches(ups.deezer)
	if asked != manifest.PortraitPendingAttempts {
		t.Fatalf("Deezer was asked %d time(s), want %d failed attempts", asked, manifest.PortraitPendingAttempts)
	}
	if pending, err := store.ArtistMBIDPortraitPending(ctx, switchArtistMBID); err != nil || pending {
		t.Fatalf("after %d failed attempts the route's probe answers pending: %v (%v)", asked, pending, err)
	}
	requirePendingPortrait(t, store, p, switchArtistMBID, "Artist")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, store := f.open(t)
	restarted.clock = func() time.Time { return now }
	restarted.runPortraits(ctx)
	if got := searches(ups.deezer); got != asked {
		t.Errorf("the restarted enricher asked Deezer %d time(s) before the retry was due", got-asked)
	}
	if pending, err := store.ArtistMBIDPortraitPending(ctx, switchArtistMBID); err != nil || pending {
		t.Errorf("after the restart the route's probe answers pending: %v (%v)", pending, err)
	}

	failing.Store(false)
	now = now.Add(portraitRetryMost)
	restarted.runPortraits(ctx)
	if !fileExists(ArtistImagePath(f.artworkDir, switchArtistMBID)) {
		t.Fatal("no portrait once Deezer answered")
	}
	if got := pendingRowsOf(t, store); len(got) != 0 {
		t.Errorf("rows still wait once the portrait arrived: %v", got)
	}
	if got, err := store.PortraitAttemptsOf(ctx, switchArtistMBID, "Artist"); err != nil || got.Failures != 0 {
		t.Errorf("the failures outlived the portrait: %+v (%v)", got, err)
	}
}

// TestADeezerPulseThatRefusesThePortraitBatchStillWakesTheLoop: Deezer
// answers off once, to the watch of the batch's search on the wire, and on
// from then on: the batch is refused, and the enricher, idle for an hour,
// takes the portrait up within a turn of the switch's interval. On main the
// wait compared the switch with what it was before the batch, on, saw it on,
// and slept the hour out.
func TestADeezerPulseThatRefusesThePortraitBatchStillWakesTheLoop(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	e.PollInterval = time.Hour
	e.switchEvery = 20 * time.Millisecond
	stampPending(t, store, "Artist/Album/01.flac", switchArtistMBID, "Artist")
	var pulse atomic.Bool
	e.WithDeezerEnabled(func() bool { return !pulse.CompareAndSwap(true, false) })
	searching, cutOff := make(chan struct{}), make(chan struct{})
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
	runEnricher(t, e)
	select {
	case <-searching:
	case <-time.After(5 * time.Second):
		t.Fatal("the batch's search was never sent")
	}
	pulse.Store(true)
	select {
	case <-cutOff:
	case <-time.After(10 * time.Second):
		t.Fatal("the search was not cut off by the switch's one off answer")
	}
	if pulse.Load() {
		t.Fatal("the switch's off answer was never asked for")
	}
	waitFor(t, func() bool { return fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) },
		"the portrait once the switch is on again, with the enricher idle for an hour")
}

// TestAnEnrichmentPulseThatRefusesTheBatchsListingStillWakesTheLoop:
// enrichment answers off once, to the ordinary batch's own switch as it
// begins (the second time it is asked, after Run's own ask), and on from then
// on: the batch's listing is refused, and the enricher, idle for an hour,
// enriches the waiting rows within a turn of the switch's interval. Deezer is
// off, so the portrait batch, which a refused batch skips and whose switch
// then wakes the wait on its own, takes no part: the wake is the ordinary
// batch's refusal alone. On main the wait compared the switch with what Run
// saw before the batch, on, and slept the hour out.
func TestAnEnrichmentPulseThatRefusesTheBatchsListingStillWakesTheLoop(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	e.PollInterval = time.Hour
	e.switchEvery = 20 * time.Millisecond
	e.WithDeezerEnabled(func() bool { return false })
	seedSwitchTracks(t, store)
	var asks atomic.Int64
	// The rows that wait at the third ask, the first after the refused
	// batch, or -1 where they could not be read: the enricher's goroutine
	// asks, so it reports, and the test's goroutine judges.
	waitingAtTheThirdAsk := make(chan int, 1)
	e.WithEnabled(func() bool {
		switch asks.Add(1) {
		case 2:
			return false
		case 3:
			rows, err := store.UnenrichedTracks(context.Background(), 100)
			if err != nil {
				waitingAtTheThirdAsk <- -1
			} else {
				waitingAtTheThirdAsk <- len(rows)
			}
		}
		return true
	})
	runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 },
		"the waiting rows once enrichment is on again, with the enricher idle for an hour")
	select {
	case n := <-waitingAtTheThirdAsk:
		if n != len(switchTracks) {
			t.Fatalf("%d row(s) waited after the first batch, want all %d: the switch's off answer did not refuse its listing",
				n, len(switchTracks))
		}
	default:
		t.Fatal("the switch was asked fewer than three times")
	}
}
