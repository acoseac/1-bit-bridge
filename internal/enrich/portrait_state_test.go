package enrich

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A miss under one name that leaves the lookup waiting by another, each
// failure timed when it happens, a redirect the client refuses that waits
// until the cap and then ends its name, and a backoff a restart keeps though
// no row waits by its name any more (backlog B761, its third round).

// testClock is a clock a test moves while the enricher reads it, from the
// test's goroutine or a fake upstream's handler.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// namingMB is a MusicBrainz that answers every artist search with mbid and
// the name name gives, and every release search with no release.
func namingMB(t *testing.T, mbid string, name func() string) *countingUpstream {
	t.Helper()
	return newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/artist"):
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":%q}]}`, mbid, name())
		case strings.HasPrefix(r.URL.Path, "/release"):
			_, _ = w.Write([]byte(`{"releases":[]}`))
		default:
			http.NotFound(w, r)
		}
	})
}

// missingDeezer is a Deezer whose artist search answers 503 for a name among
// failing, and that it has no such artist for any other.
func missingDeezer(t *testing.T, failing ...string) *countingUpstream {
	t.Helper()
	return newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/search/artist") {
			http.NotFound(w, r)
			return
		}
		for _, f := range failing {
			if r.URL.Query().Get("q") == f {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
}

// TestAMissUnderAnotherNameKeepsTheLookupThatWaits: an artist's portrait
// lookup waits by MusicBrainz's name for it, and an answer then ends another
// name of the same artist: the fingerprint's, asked later in the same pass,
// or the name a later pass asks by after "Retry missing", a pass that knows
// nothing of the first; or that other name waits too. The row still waits by
// the first name, which the batch then asks by. Until B761's third round the
// later answer settled or replaced the lookup, and the first name was never
// asked again.
func TestAMissUnderAnotherNameKeepsTheLookupThatWaits(t *testing.T) {
	const mbid = switchArtistMBID
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		bFails bool
	}{{"the fingerprint's name misses in the same pass", false}, {"the fingerprint's name waits too", true}} {
		t.Run(c.name, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			ups.mb = namingMB(t, mbid, func() string { return "Artist A" })
			failing := []string{"Artist A"}
			if c.bFails {
				failing = append(failing, "Artist B")
			}
			ups.deezer = missingDeezer(t, failing...)
			e, store, _ := switchEnricher(t, ups)
			e.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }
			const p = "Artist/Album/01.flac"
			e.WithAcousticFallback(fixedAcoustic{p: {ArtistMBID: mbid, ArtistName: "Artist B"}})
			tr := manifest.Track{Path: p, Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album"}
			if err := store.UpsertTrack(ctx, &tr); err != nil {
				t.Fatal(err)
			}
			enrichQueued(t, e, store)
			if left := unenrichedPaths(t, store); len(left) != 0 {
				t.Fatalf("the pass left %v unstamped", left)
			}
			for _, name := range []string{"Artist A", "Artist B"} {
				if got := searchesFor(ups.deezer, name); got != 1 {
					t.Fatalf("Deezer was asked by %q %d time(s), want once: the pass asks by both names", name, got)
				}
			}
			requirePendingPortrait(t, store, p, mbid, "Artist A")
		})
	}

	t.Run("a later pass misses by another name after Retry missing", func(t *testing.T) {
		var renamed atomic.Bool
		ups := newSwitchUpstreams(t)
		ups.mb = namingMB(t, mbid, func() string {
			if renamed.Load() {
				return "Artist B"
			}
			return "Artist A"
		})
		ups.deezer = missingDeezer(t, "Artist A")
		f := newRestartFixture(t, ups)
		first, store := f.open(t)
		first.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }
		const p = "Artist/Album/01.flac"
		tr := manifest.Track{Path: p, Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album",
			MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("d", 64)}
		if err := store.UpsertTrack(ctx, &tr); err != nil {
			t.Fatal(err)
		}
		enrichQueued(t, first, store)
		requirePendingPortrait(t, store, p, mbid, "Artist A")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		restarted, store := f.open(t)
		restarted.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }
		renamed.Store(true)
		if n, err := store.ResetEnrichedByArtistMBIDs(ctx, []string{mbid}); err != nil || n != 1 {
			t.Fatalf("Retry missing re-queued %d (%v), want the row", n, err)
		}
		enrichQueued(t, restarted, store)
		if got := searchesFor(ups.deezer, "Artist B"); got != 1 {
			t.Fatalf("Deezer was asked by the new name %d time(s), want once", got)
		}
		requirePendingPortrait(t, store, p, mbid, "Artist A")
	})
}

// slowDeezer is a Deezer whose artist search moves clock on by step before
// it answers, as a request that takes that long does: 503 for a name fail
// says so, else a picture, which it serves.
func slowDeezer(t *testing.T, clock *testClock, step time.Duration, fail func(name string) bool) *countingUpstream {
	t.Helper()
	return newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/artist"):
			clock.add(step)
			q := r.URL.Query().Get("q")
			if fail(q) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":1,"name":%q,"picture_xl":%q,"picture_big":""}]}`, q, u.srv.URL+"/picture.jpg")
		case r.URL.Path == "/picture.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(switchJPEG)
		default:
			http.NotFound(w, r)
		}
	})
}

// requireLastFailure fails unless the store keeps one failed attempt at
// mbid's portrait by name, at at.
func requireLastFailure(t *testing.T, store *manifest.Store, mbid, name string, at time.Time, why string) {
	t.Helper()
	got, err := store.PortraitAttemptsOf(context.Background(), mbid, name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failures != 1 || !got.LastFailedAt.Equal(at) {
		t.Errorf("the attempts at %q are %+v, want one failure at %v: %s", name, got, at, why)
	}
}

// TestEachFailureOnAPageStartsItsBackoffWhenItHappens: a page of the batch
// whose requests each take three minutes, and two artists failing on it:
// each failure's backoff starts when it happened, the second's three minutes
// after the first's. Until B761's third round every failure on a page was
// timed at the page's start, so a later artist's first steps of backoff were
// spent before it failed.
func TestEachFailureOnAPageStartsItsBackoffWhenItHappens(t *testing.T) {
	const otherMBID = "c3c3c3c3-0761-4761-8761-0000000000c3"
	start := time.Unix(1_800_000_000, 0)
	clock := &testClock{at: start}
	ups := newSwitchUpstreams(t)
	ups.deezer = slowDeezer(t, clock, 3*time.Minute, func(string) bool { return true })
	e, store, _ := switchEnricher(t, ups)
	e.clock = clock.now
	stampPending(t, store, "A First/Album/01.flac", otherMBID, "First")
	stampPending(t, store, "B Second/Album/01.flac", switchArtistMBID, "Second")
	e.runPortraits(context.Background())
	requireLastFailure(t, store, otherMBID, "First", start.Add(3*time.Minute), "the page's first request took three minutes")
	requireLastFailure(t, store, switchArtistMBID, "Second", start.Add(6*time.Minute), "it failed after the page's second request")
}

// TestAnArtistWhoseRetryFallsDueDuringThePageIsAsked: the second artist on a
// page waits for a retry due a minute after its failure, and the page's
// first request takes two minutes, by whose end that retry is due: the page
// asks for the second artist's portrait when it reaches it. Until B761's
// third round the page compared each retry with the time the page began, and
// left the second artist to the next pass.
func TestAnArtistWhoseRetryFallsDueDuringThePageIsAsked(t *testing.T) {
	const firstMBID = "d4d4d4d4-0761-4761-8761-0000000000d4"
	failedAt := time.Unix(1_800_000_000, 0)
	clock := &testClock{at: failedAt.Add(30 * time.Second)}
	ups := newSwitchUpstreams(t)
	ups.deezer = slowDeezer(t, clock, 2*time.Minute, func(name string) bool { return name == "First" })
	e, store, artworkDir := switchEnricher(t, ups)
	e.clock = clock.now
	ctx := context.Background()
	stampPending(t, store, "A First/Album/01.flac", firstMBID, "First")
	stampPending(t, store, "B Second/Album/01.flac", switchArtistMBID, "Second")
	if _, err := store.NotePortraitFailure(ctx, switchArtistMBID, "Second", failedAt); err != nil {
		t.Fatal(err)
	}
	e.runPortraits(ctx)
	if got := searchesFor(ups.deezer, "Second"); got != 1 {
		t.Fatalf("the second artist was asked %d time(s) on the page, want once: its retry fell due at %v, before the page reached it",
			got, failedAt.Add(portraitRetryFirst))
	}
	if !fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Error("no portrait for the second artist")
	}
}

// TestTheBatchRestsFromTheEndOfThePass: a pass finds the one artist waiting
// for a retry hours away and rests portraitRestMost, counted from when the
// pass ended, five minutes after it began: twelve minutes after it began the
// batch still reads nothing, and fifteen minutes after it reads again. Until
// B761's third round the rest counted from the pass's start.
func TestTheBatchRestsFromTheEndOfThePass(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := &testClock{at: start}
	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	e.clock = clock.now
	ctx := context.Background()
	stampPending(t, store, "Artist/Album/01.flac", switchArtistMBID, "Artist")
	for i := 0; i < 10; i++ {
		if _, err := store.NotePortraitFailure(ctx, switchArtistMBID, "Artist", start); err != nil {
			t.Fatal(err)
		}
	}
	// The pass takes five minutes: enrich.enabled, which the pass asks as
	// its read of the waiting rows begins, moves the clock on the first time.
	var slow atomic.Bool
	slow.Store(true)
	e.WithEnabled(func() bool {
		if slow.CompareAndSwap(true, false) {
			clock.add(5 * time.Minute)
		}
		return true
	})
	e.runPortraits(ctx)
	if slow.Load() {
		t.Fatal("the pass never asked enrich.enabled")
	}
	if got := ups.deezer.count(); got != 0 {
		t.Fatalf("Deezer was asked %d time(s): the artist's retry is hours away", got)
	}

	rec := loggingtest.Record(t)
	_ = store.Close() // any read of the store now fails, and the batch says so
	clock.set(start.Add(12 * time.Minute))
	e.runPortraits(ctx)
	if got := rec.Failures("list pending portraits"); len(got) != 0 {
		t.Fatalf("the batch read the store twelve minutes after a pass that began at %v and ended five minutes later:\n%s",
			start, strings.Join(got, "\n"))
	}
	clock.set(start.Add(5*time.Minute + portraitRestMost))
	e.runPortraits(ctx)
	if got := rec.Failures("list pending portraits"); len(got) != 1 {
		t.Errorf("the batch read the store %d time(s) once its rest had ended, want once", len(got))
	}
}

// TestAPassTimesAPortraitFailureWhenItHappens: the ordinary pass's lookup
// (resolveArtist's, through askPortrait) and the fingerprint path's
// (fetchRecoveredArtistImage's, through askPortrait too), each failing after a
// request that takes three minutes: the failure's backoff starts when it
// happened. Until B761's third round it started when the attempt began.
func TestAPassTimesAPortraitFailureWhenItHappens(t *testing.T) {
	for _, path := range []string{"the ordinary pass", "the fingerprint path"} {
		t.Run(path, func(t *testing.T) {
			start := time.Unix(1_800_000_000, 0)
			clock := &testClock{at: start}
			ups := newSwitchUpstreams(t)
			ups.deezer = slowDeezer(t, clock, 3*time.Minute, func(string) bool { return true })
			e, store, _ := switchEnricher(t, ups)
			e.clock = clock.now
			tr, mbid, name := switchTracks[0], switchArtistMBID, "Artist"
			if path == "the fingerprint path" {
				tr, mbid, name = manifest.Track{Path: "Unknown/01.flac", Size: 1}, recoveredArtistMBID, "Recovered"
				e.WithAcousticFallback(fixedAcoustic{tr.Path: {ArtistMBID: recoveredArtistMBID, ArtistName: "Recovered"}})
			}
			tr.ModTime = time.Now()
			if err := store.UpsertTrack(context.Background(), &tr); err != nil {
				t.Fatal(err)
			}
			enrichQueued(t, e, store)
			if got := searchesFor(ups.deezer, name); got != 1 {
				t.Fatalf("the portrait was asked for %d time(s) by %q, want once", got, name)
			}
			requireLastFailure(t, store, mbid, name, start.Add(3*time.Minute), "the request took three minutes")
		})
	}
}

// redirectingDeezer is a Deezer that redirects to a host the client refuses:
// its artist search where search is set, else the picture its search names.
func redirectingDeezer(t *testing.T, search bool) *countingUpstream {
	t.Helper()
	return newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/artist") && search:
			http.Redirect(w, r, "http://refused.invalid/search", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/search/artist"):
			fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q,"picture_big":""}]}`, u.srv.URL+"/picture.jpg")
		case r.URL.Path == "/picture.jpg":
			http.Redirect(w, r, "http://refused.invalid/picture.jpg", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
}

// pictureRequests is how many times the picture at /picture.jpg was asked for.
func pictureRequests(u *countingUpstream) int {
	n := 0
	for _, r := range u.requests() {
		if r == "GET /picture.jpg" {
			n++
		}
	}
	return n
}

// TestARefusedRedirectWaitsUntilTheCapAndThenEndsItsName: the batch's request
// for a waiting portrait, its search or its picture, redirected to a host the
// client refuses: the row waits through each such failure, and the route's
// probe answers pending, until the PortraitPendingAttempts'th in a row, which
// ends the name as an answer no wait changes does, its failures forgotten; a
// restart between the failures keeps their count, and no request goes out
// past the cap. A 503 keeps being asked past it. Until B761's third round the
// first redirect ended the name.
func TestARefusedRedirectWaitsUntilTheCapAndThenEndsItsName(t *testing.T) {
	for _, c := range []struct {
		name   string
		deezer func(t *testing.T) *countingUpstream
		asked  func(u *countingUpstream) int
		ends   bool
	}{
		{"a redirect of the search", func(t *testing.T) *countingUpstream { return redirectingDeezer(t, true) }, searches, true},
		{"a redirect of the picture", func(t *testing.T) *countingUpstream { return redirectingDeezer(t, false) }, pictureRequests, true},
		{"a 503", func(t *testing.T) *countingUpstream {
			return failingSearchDeezer(t, func(string) bool { return true })
		}, searches, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			ups.deezer = c.deezer(t)
			ctx := context.Background()
			f := newRestartFixture(t, ups)
			e, store := f.open(t)
			clock := &testClock{at: time.Unix(1_800_000_000, 0)}
			e.clock = clock.now
			const p = "Artist/Album/01.flac"
			stampPending(t, store, p, switchArtistMBID, "Artist")
			const turns = manifest.PortraitPendingAttempts + 2
			for i := 1; i <= turns; i++ {
				if i > 1 {
					clock.add(portraitRetryMost)
				}
				if i == 3 {
					// A restart between two of the failures.
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					e, store = f.open(t)
					e.clock = clock.now
				}
				e.runPortraits(ctx)
				if !c.ends || i < manifest.PortraitPendingAttempts {
					if _, waits := pendingRowsOf(t, store)[p]; !waits {
						t.Fatalf("after %d failure(s) the row waits for no portrait", i)
					}
					if pending, err := store.ArtistMBIDPortraitPending(ctx, switchArtistMBID); err != nil || pending != (i < manifest.PortraitPendingAttempts) {
						t.Fatalf("after %d failure(s) the route's probe answers pending: %v (%v)", i, pending, err)
					}
				}
			}
			want := turns
			if c.ends {
				want = manifest.PortraitPendingAttempts
			}
			if got := c.asked(ups.deezer); got != want {
				t.Errorf("Deezer was asked %d time(s) in %d turns, each past the retry, want %d", got, turns, want)
			}
			if !c.ends {
				return
			}
			if _, waits := pendingRowsOf(t, store)[p]; waits {
				t.Error("the row still waits once the cap ended its name")
			}
			if a, err := store.PortraitAttemptsOf(ctx, switchArtistMBID, "Artist"); err != nil || a.Failures != 0 {
				t.Errorf("the ended name keeps its failures: %+v (%v)", a, err)
			}
		})
	}
}

// TestTheOrdinaryPassWaitsOnARefusedRedirectUntilTheCap: the ordinary pass's
// portrait lookup redirected to a host the client refuses waits for the
// batch, as a failure does, while its name has failed fewer than
// PortraitPendingAttempts times in a row, and the failure that reaches the
// cap ends the name: the row is stamped with no lookup, and neither a
// sibling's pass nor the batch asks Deezer by it again. Until B761's third
// round the first redirect ended the name.
func TestTheOrdinaryPassWaitsOnARefusedRedirectUntilTheCap(t *testing.T) {
	for _, c := range []struct {
		name  string
		prior int
		waits bool
	}{{"its first failure", 0, true}, {"the failure that reaches the cap", manifest.PortraitPendingAttempts - 1, false}} {
		t.Run(c.name, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			ups.deezer = redirectingDeezer(t, true)
			e, store, _ := switchEnricher(t, ups)
			clock := &testClock{at: time.Unix(1_800_000_000, 0)}
			e.clock = clock.now
			ctx := context.Background()
			for i := 0; i < c.prior; i++ {
				if _, err := store.NotePortraitFailure(ctx, switchArtistMBID, "Artist", clock.now().Add(-portraitRetryMost)); err != nil {
					t.Fatal(err)
				}
			}
			first := switchTracks[0]
			first.ModTime = time.Now()
			if err := store.UpsertTrack(ctx, &first); err != nil {
				t.Fatal(err)
			}
			enrichQueued(t, e, store)
			if got := searches(ups.deezer); got != 1 {
				t.Fatalf("Deezer was asked %d time(s), want once", got)
			}
			if _, waits := pendingRowsOf(t, store)[first.Path]; waits != c.waits {
				t.Fatalf("after the redirect the row waits: %v, want %v", waits, c.waits)
			}
			if pending, err := store.ArtistMBIDPortraitPending(ctx, switchArtistMBID); err != nil || pending != c.waits {
				t.Fatalf("after the redirect the route's probe answers pending: %v (%v), want %v", pending, err, c.waits)
			}
			if c.waits {
				return
			}
			sibling := manifest.Track{Path: "Artist/Second Album/01.flac", Size: 1, ModTime: time.Now(), Artist: "Artist",
				Album: "Second Album", MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("9", 64)}
			if err := store.UpsertTrack(ctx, &sibling); err != nil {
				t.Fatal(err)
			}
			enrichQueued(t, e, store)
			clock.add(portraitRetryMost)
			e.runPortraits(ctx)
			if got := searches(ups.deezer); got != 1 {
				t.Errorf("Deezer was asked %d more time(s) past the cap, want none", got-1)
			}
		})
	}
}

// TestARestartKeepsABackoffNoRowWaitsFor: an artist's portrait lookup fails,
// the row that waited by its name goes back to the enricher, through a
// changed file, whose upsert drops the lookup, or through "Retry missing",
// which keeps it, and the bridge restarts before the retry is due. The
// failure's count and time outlive the restart, the re-enrichment asks
// Deezer nothing before the retry is due, and the batch asks once it is.
// Until B761's third round a restart after a changed file forgot the
// backoff, and the re-enrichment asked at once.
func TestARestartKeepsABackoffNoRowWaitsFor(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		requeue func(t *testing.T, store *manifest.Store, tr manifest.Track)
	}{
		{"after a changed file", func(t *testing.T, store *manifest.Store, tr manifest.Track) {
			tr.Size = 2
			if err := store.UpsertTrack(ctx, &tr); err != nil {
				t.Fatal(err)
			}
			if _, waits := pendingRowsOf(t, store)[tr.Path]; waits {
				t.Fatal("the changed file's upsert kept the lookup: the case needs it dropped")
			}
		}},
		{"after Retry missing", func(t *testing.T, store *manifest.Store, tr manifest.Track) {
			if n, err := store.ResetEnrichedByArtistMBIDs(ctx, []string{switchArtistMBID}); err != nil || n != 1 {
				t.Fatalf("Retry missing re-queued %d (%v), want the row", n, err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var failing atomic.Bool
			failing.Store(true)
			ups := newSwitchUpstreams(t)
			ups.deezer = failingSearchDeezer(t, func(string) bool { return failing.Load() })
			f := newRestartFixture(t, ups)
			e, store := f.open(t)
			// The store's open reads the host's clock, so the enricher's
			// starts beside it.
			failedAt := time.Now().Truncate(time.Second)
			clock := &testClock{at: failedAt}
			e.clock = clock.now
			tr := manifest.Track{Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album",
				MusicBrainzAlbumID: switchReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("d", 64)}
			if err := store.UpsertTrack(ctx, &tr); err != nil {
				t.Fatal(err)
			}
			enrichQueued(t, e, store)
			requirePendingPortrait(t, store, tr.Path, switchArtistMBID, "Artist")
			c.requeue(t, store, tr)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			restarted, store := f.open(t)
			clock.add(30 * time.Second)
			restarted.clock = clock.now
			requireLastFailure(t, store, switchArtistMBID, "Artist", failedAt, "the restart came before the retry is due")
			enrichQueued(t, restarted, store)
			if got := searches(ups.deezer); got != 1 {
				t.Fatalf("the re-enrichment asked Deezer %d time(s) before the retry was due, want none", got-1)
			}
			requirePendingPortrait(t, store, tr.Path, switchArtistMBID, "Artist")

			failing.Store(false)
			clock.set(failedAt.Add(portraitRetryFirst))
			restarted.runPortraits(ctx)
			if got := searches(ups.deezer); got != 2 {
				t.Errorf("Deezer was searched %d time(s) once the retry was due, want once more", got)
			}
			if !fileExists(ArtistImagePath(f.artworkDir, switchArtistMBID)) {
				t.Error("no portrait once the retry was due")
			}
		})
	}
}
