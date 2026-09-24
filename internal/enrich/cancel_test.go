package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The enricher runs on runServe's scanCtx, and a shutdown that cancels it
// mid-track is a pass that STOPPED, not one that failed
// (ctxerr.WithoutCancellation). Most tests below cancel from inside an
// upstream request: the fake server's handler cancels the pass's context and
// holds the request until the client gives up on it, which is exactly what a
// shutdown does to a fetch in flight. Each has a twin that makes the same
// step fail on a live context, so a genuine failure is still reported.

const (
	msgListUnenriched  = "list unenriched"
	msgMarkEnriched    = "mark enriched"
	msgMarkSkipped     = "mark skipped"
	msgReleaseGroup    = "release-group lookup"
	msgPremiumFetch    = "atlas premium cover fetch"
	msgPremiumWrite    = "atlas premium cover write"
	cancelArtistMBID   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cancelReleaseMBID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cancelArtistResult = `{"artists":[{"id":"` + cancelArtistMBID + `","score":100,"name":"Artist"}]}`
)

// TestAnEnricherStoppedWhileListingReportsNothing: the pass's context is
// cancelled right after Run's own check, so the listing query fails with the
// cancellation.
func TestAnEnricherStoppedWhileListingReportsNothing(t *testing.T) {
	e, _ := newOfflineEnricher(t, nil)
	e.PollInterval = time.Hour
	ctx := cancelledAfterItsCheck(t)

	rec := loggingtest.Record(t)
	runUntilReturn(t, e, ctx)

	mustNotReport(t, rec, msgListUnenriched)
}

// TestAnEnricherWhoseListingFailsStillReportsIt is the twin: a listing that
// fails on a live context is reported.
func TestAnEnricherWhoseListingFailsStillReportsIt(t *testing.T) {
	e, store := newOfflineEnricher(t, nil)
	_ = store.Close()
	e.PollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())

	rec := loggingtest.Record(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	waitFor(t, func() bool { return len(rec.Failures(msgListUnenriched)) > 0 }, "the failed listing to be reported")
	cancel()
	<-done
}

// TestAStampStoppedByShutdownReportsNothing: a track whose artwork is local
// and whose album MBID is embedded goes straight to the artist, and the
// shutdown lands in the portrait fetch. That fetch is tier-2 and absorbed, so
// the pass reaches the stamp on the cancelled context.
func TestAStampStoppedByShutdownReportsNothing(t *testing.T) {
	e, store, ctx := enricherStoppedInThePortraitFetch(t, true)
	track := stampableTrack(t, store)

	rec := loggingtest.Record(t)
	e.enrichOne(ctx, &track)

	mustNotReport(t, rec, msgMarkEnriched)
	if got := e.Done(); got != 0 {
		t.Errorf("a stamp the shutdown stopped counted %d track(s) done", got)
	}
	mustStillBeUnenriched(t, store, track.Path)
}

// TestAStampThatFailsIsStillReported is the twin: the stamp fails on a live
// context, and is reported.
func TestAStampThatFailsIsStillReported(t *testing.T) {
	e, store, _ := enricherStoppedInThePortraitFetch(t, false)
	track := stampableTrack(t, store)
	_ = store.Close()

	rec := loggingtest.Record(t)
	e.enrichOne(context.Background(), &track)

	mustReportOnce(t, rec, msgMarkEnriched)
}

// TestASkipStoppedByShutdownRecordsNothing is question 2 of the survey. A
// track MusicBrainz has no release for goes on to the artist, the shutdown
// lands in the portrait fetch, and the pass reaches markSkipped on the
// cancelled context. Nothing was skipped: the stamp never happened, so the
// track is not counted, not logged as "enrichment skipped", and not reported
// as a failed stamp. It stays at enriched_at = 0 for the next run.
func TestASkipStoppedByShutdownRecordsNothing(t *testing.T) {
	e, store, ctx := enricherStoppedInThePortraitFetch(t, true)
	track := skippableTrack(t, store)

	rec := loggingtest.Record(t)
	e.enrichOne(ctx, &track)

	mustNotReport(t, rec, msgMarkSkipped)
	if got := e.skipped.Load(); got != 0 {
		t.Errorf("a skip the shutdown stopped was counted: skipped = %d", got)
	}
	if got := e.SkipReasons(); len(got) != 0 {
		t.Errorf("a skip the shutdown stopped was attributed a reason: %v", got)
	}
	mustStillBeUnenriched(t, store, track.Path)
}

// TestASkipThatFailsIsStillReportedAndCounted is the twin, and pins that a
// genuine stamp failure keeps today's behaviour: reported, and still counted
// under its reason.
func TestASkipThatFailsIsStillReportedAndCounted(t *testing.T) {
	e, store, _ := enricherStoppedInThePortraitFetch(t, false)
	track := skippableTrack(t, store)
	_ = store.Close()

	rec := loggingtest.Record(t)
	e.enrichOne(context.Background(), &track)

	mustReportOnce(t, rec, msgMarkSkipped)
	if got := e.SkipReasons()[skipReasonNoMBMatch]; got != 1 {
		t.Errorf("%s = %d after a genuine stamp failure, want 1", skipReasonNoMBMatch, got)
	}
}

// TestAReleaseGroupLookupStoppedByShutdownReportsNothing: an embedded album
// MBID whose release has no CAA cover falls back to the release-group, and
// the shutdown lands in that MusicBrainz lookup.
func TestAReleaseGroupLookupStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, store := newOfflineEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/release/"+cancelReleaseMBID) {
			cancelAndHold(cancel, r)
			return
		}
		_, _ = io.WriteString(w, cancelArtistResult)
	})
	tuneForTest(e)
	track := releaseGroupTrack(t, store)

	rec := loggingtest.Record(t)
	e.enrichOne(ctx, &track)

	mustNotReport(t, rec, msgReleaseGroup, msgMarkEnriched)
}

// TestAReleaseGroupLookupThatFailsIsStillReported is the twin.
func TestAReleaseGroupLookupThatFailsIsStillReported(t *testing.T) {
	e, store := newOfflineEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/release/"+cancelReleaseMBID) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad request"}`)
			return
		}
		_, _ = io.WriteString(w, cancelArtistResult)
	})
	tuneForTest(e)
	track := releaseGroupTrack(t, store)

	rec := loggingtest.Record(t)
	e.enrichOne(context.Background(), &track)

	mustReportOnce(t, rec, msgReleaseGroup)
}

// TestAPremiumCoverFetchStoppedByShutdownReportsNothing: the shutdown lands
// while the authenticated cover request is in flight.
func TestAPremiumCoverFetchStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancelAndHold(cancel, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: srv.URL, ok: true}, "ua", nil, dir)

	rec := loggingtest.Record(t)
	if f.TryCache(ctx, filepath.Join(dir, cancelReleaseMBID+"-500.jpg"), cancelReleaseMBID, 500) {
		t.Fatal("a fetch the shutdown stopped reported a cached cover")
	}
	mustNotReport(t, rec, msgPremiumFetch, msgPremiumWrite)
}

// TestAPremiumCoverFetchThatFailsIsStillReported is the twin: the upstream
// is unreachable on a live context.
func TestAPremiumCoverFetchThatFailsIsStillReported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens there now
	dir := t.TempDir()
	f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: base, ok: true}, "ua", nil, dir)

	rec := loggingtest.Record(t)
	f.TryCache(context.Background(), filepath.Join(dir, cancelReleaseMBID+"-500.jpg"), cancelReleaseMBID, 500)
	mustReportOnce(t, rec, msgPremiumFetch)
}

// TestAPremiumCoverWriteStoppedByShutdownReportsNothing: the upstream has
// answered and started the body, and the shutdown lands while the write is
// streaming it to disk. The cancel comes from the body's first read, so the
// request has certainly returned: a cancel from the handler instead races the
// client reading the headers, and lands in the fetch about as often.
func TestAPremiumCoverWriteStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(premiumJPEG)
		w.(http.Flusher).Flush()
		select { // the rest of the body never comes; the client gives up first
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	client := &http.Client{Transport: cancelOnFirstRead{rt: http.DefaultTransport, cancel: cancel}}
	f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: srv.URL, ok: true}, "ua", client, dir)

	rec := loggingtest.Record(t)
	if f.TryCache(ctx, filepath.Join(dir, cancelReleaseMBID+"-500.jpg"), cancelReleaseMBID, 500) {
		t.Fatal("a write the shutdown stopped reported a cached cover")
	}
	mustNotReport(t, rec, msgPremiumFetch, msgPremiumWrite)
}

// TestAPremiumCoverWriteThatFailsIsStillReported is the twin: the upstream
// answers 200 with a body that is not an image, which the write refuses.
func TestAPremiumCoverWriteThatFailsIsStillReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>not a cover</html>")
	}))
	defer srv.Close()
	dir := t.TempDir()
	f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: srv.URL, ok: true}, "ua", nil, dir)

	rec := loggingtest.Record(t)
	f.TryCache(context.Background(), filepath.Join(dir, cancelReleaseMBID+"-500.jpg"), cancelReleaseMBID, 500)
	mustReportOnce(t, rec, msgPremiumWrite)
}

// enricherStoppedInThePortraitFetch builds an enricher whose MusicBrainz
// knows the artist and no release, and whose Deezer, when stop is set,
// cancels the returned context from inside the portrait search. Unset, the
// search answers that Deezer has no such artist.
func enricherStoppedInThePortraitFetch(t *testing.T, stop bool) (*Enricher, *manifest.Store, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e, store := newOfflineEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/artist") {
			_, _ = io.WriteString(w, cancelArtistResult)
			return
		}
		_, _ = io.WriteString(w, `{"releases":[]}`)
	})
	deezer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stop {
			cancelAndHold(cancel, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(deezer.Close)
	e.deezer = NewDeezerClient(deezer.URL, "t", nil)
	tuneForTest(e)
	return e, store, ctx
}

// tuneForTest drops the upstream pacing: the upstreams are local fakes.
func tuneForTest(e *Enricher) {
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.DeezerMinInterval = 0
}

// stampableTrack stores and returns a track that reaches stampEnriched: an
// embedded album MBID, so no release search, and local artwork, so no cover
// fetch.
func stampableTrack(t *testing.T, store *manifest.Store) manifest.Track {
	t.Helper()
	return storeCancelTrack(t, store, manifest.Track{
		Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
		MusicBrainzAlbumID: cancelReleaseMBID, ArtworkMBID: "local-" + strings.Repeat("a", 64),
	})
}

// skippableTrack stores and returns a track MusicBrainz has no release for,
// with no local artwork, so the pass ends in markSkipped.
func skippableTrack(t *testing.T, store *manifest.Store) manifest.Track {
	t.Helper()
	return storeCancelTrack(t, store, manifest.Track{
		Path: "Artist/Album/02.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
	})
}

// releaseGroupTrack stores and returns a track with an embedded album MBID
// and no cover, so the artwork fetch falls back to the release-group.
func releaseGroupTrack(t *testing.T, store *manifest.Store) manifest.Track {
	t.Helper()
	return storeCancelTrack(t, store, manifest.Track{
		Path: "Artist/Album/03.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album", MusicBrainzAlbumID: cancelReleaseMBID,
	})
}

func storeCancelTrack(t *testing.T, store *manifest.Store, tr manifest.Track) manifest.Track {
	t.Helper()
	if err := store.UpsertTrack(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

// cancelAndHold cancels the pass's context and holds the request until the
// client abandons it, as a shutdown does to a fetch in flight. Bounded, so a
// client that never gives up fails the test rather than hanging it.
func cancelAndHold(cancel context.CancelFunc, r *http.Request) {
	cancel()
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// cancelledAfterItsCheck returns a context whose first Err answers nil and
// cancels it, so a pass that asks its context once before starting then
// runs on a context shutdown has just cancelled.
func cancelledAfterItsCheck(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &cancelOnFirstErr{Context: ctx, cancel: cancel}
}

// cancelOnFirstErr is the context behind cancelledAfterItsCheck. It embeds
// the context it cancels, which is how every derived context is built.
type cancelOnFirstErr struct {
	context.Context
	cancel context.CancelFunc
	asked  atomic.Bool
}

// Err answers nil the first time, cancelling as it does, and the embedded
// context's answer after that.
func (c *cancelOnFirstErr) Err() error {
	if c.asked.CompareAndSwap(false, true) {
		c.cancel()
		return nil
	}
	return c.Context.Err()
}

// cancelOnFirstRead is a RoundTripper whose responses cancel the pass's
// context on their body's first read, after handing back what that read got.
type cancelOnFirstRead struct {
	rt     http.RoundTripper
	cancel context.CancelFunc
}

// RoundTrip wraps the response body.
func (c cancelOnFirstRead) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &cancellingBody{ReadCloser: resp.Body, cancel: c.cancel}
	return resp, nil
}

// cancellingBody is the body cancelOnFirstRead hands back.
type cancellingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   atomic.Bool
}

// Read reads, and cancels after the first read.
func (b *cancellingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.once.CompareAndSwap(false, true) {
		b.cancel()
	}
	return n, err
}

// runUntilReturn runs the enricher on ctx and waits for Run to return.
func runUntilReturn(t *testing.T, e *Enricher, ctx context.Context) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of its context's cancel")
	}
}

// waitFor polls cond until it holds, failing the test after 5s.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mustStillBeUnenriched fails the test unless path is still waiting for
// the enricher, i.e. nothing stamped it.
func mustStillBeUnenriched(t *testing.T, store *manifest.Store, path string) {
	t.Helper()
	rows, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Path == path {
			return
		}
	}
	t.Errorf("%q was stamped although the stamp was stopped", path)
}

// mustNotReport fails the test for each msg logged at Warn or above.
func mustNotReport(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 0 {
			t.Errorf("a pass that shutdown stopped reported %q:\n%s", m, strings.Join(got, "\n"))
		}
	}
}

// mustReportOnce fails the test for each msg not logged exactly once at Warn
// or above.
func mustReportOnce(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 1 {
			t.Errorf("a failure on a live context logged %q %d times, want 1:\n%s", m, len(got), strings.Join(got, "\n"))
		}
	}
}
