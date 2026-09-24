package atlasharvest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The harvest client ticks on runServe's scanCtx, and a shutdown that
// cancels it mid-tick is a tick that STOPPED, not one that failed
// (ctxerr.WithoutCancellation). The tests below cancel inside a request (the
// fake Atlas cancels and holds it) or inside a store write (a fake store that
// cancels and answers with the cancellation, as the real one does when a
// shutdown lands in it). Each has a twin in which the same step fails on a
// live context and is still reported.

const (
	msgTickError      = "atlasharvest.tick_error"
	msgRefetchFailed  = "atlasharvest.cover_refetch_failed"
	msgGCFailed       = "atlasharvest.booklet_gc_failed"
	msgFetchList      = "atlasharvest.booklet_fetch_list"
	msgMarkUnavail    = "atlasharvest.booklet_mark_unavailable"
	msgClearTag       = "atlasharvest.booklet_clear_tag"
	msgMarkFetched    = "atlasharvest.booklet_mark_fetched"
	msgStampFailed    = "atlaslyrics.stamp_failed"
	cancelRelease     = "44444444-4444-4444-8444-444444444444"
	cancelArtistMBID  = "55555555-5555-4555-8555-555555555555"
	injectedStoreFail = "injected store failure"
)

// TestATickStoppedByShutdownReportsNothingAndStopsThere: the shutdown lands
// in the submit leg's request. Nothing is reported, and the tick ends there:
// the poll and booklet legs after it are not started on the cancelled
// context (one warn line each, before), which the booklet store's untouched
// universe listing shows.
func TestATickStoppedByShutdownReportsNothingAndStopsThere(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := atlasFake(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		holdUntilAbandoned(r)
	})
	booklets := &stoppingBooklets{fakeBookletSink: newFakeBookletSink()}
	c := dueClient(t, srv.URL)
	c.Booklets = booklets
	c.BookletFiles = newFakeBookletFiles()
	// The booklet leg is due, so an untouched universe listing below means
	// the tick never started it, not that it had nothing to do (Gemini API
	// review, #1003).
	if !bookletsCheckDue(c.State.Snapshot(), c.now(), c.submitInterval()) {
		t.Fatal("precondition: the booklet check is not due, so this test would pass having shown nothing")
	}

	rec := loggingtest.Record(t)
	c.tick(ctx)

	mustNotReport(t, rec, msgTickError, msgFetchList)
	if got := booklets.universeCalls.Load(); got != 0 {
		t.Errorf("the booklet leg started after the tick was stopped (%d universe listing(s))", got)
	}
}

// TestATickWhoseRequestTimesOutStillReportsIt is the twin, and the reason the
// LOOP's context is the one asked: every request runs on a per-request
// timeout derived from it, and a request that ran out of time failed. The
// loop is live, so the timeout is reported.
func TestATickWhoseRequestTimesOutStillReportsIt(t *testing.T) {
	srv := atlasFake(t, func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		holdUntilAbandoned(r) // the submit, until its own deadline
	})
	c := dueClient(t, srv.URL)
	c.RequestTimeout = 50 * time.Millisecond

	rec := loggingtest.Record(t)
	c.tick(context.Background())

	mustReportOnce(t, rec, msgTickError)
}

// TestACoverRefetchStoppedByShutdownReportsNothing: the shutdown lands in a
// premium cover refetch. Nothing is reported, and the cover stays pending
// with no attempt burned.
func TestACoverRefetchStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := coverClient(t, refetcherFunc(func(ctx context.Context, _ string) (bool, error) {
		cancel()
		return false, fmt.Errorf("atlas premium cover: %w", ctx.Err())
	}))

	rec := loggingtest.Record(t)
	c.refreshCovers(ctx)

	mustNotReport(t, rec, msgRefetchFailed)
	if got, ok := c.State.PendingCoversSnapshot()[cancelRelease]; !ok || got != 0 {
		t.Errorf("pending attempts for the cover = %d (pending %v), want 0 and still pending", got, ok)
	}
}

// TestACoverRefetchThatFailsIsStillReported is the twin.
func TestACoverRefetchThatFailsIsStillReported(t *testing.T) {
	c := coverClient(t, refetcherFunc(func(context.Context, string) (bool, error) {
		return false, errors.New("atlas premium cover: http 502")
	}))

	rec := loggingtest.Record(t)
	c.refreshCovers(context.Background())

	mustReportOnce(t, rec, msgRefetchFailed)
}

// TestABookletStepStoppedByShutdownReportsNothing covers the booklet legs'
// store writes, one per case: the orphan GC, the fetch listing, the two
// writes after Atlas says a booklet is gone (the shutdown landing in each),
// and the one after a booklet lands. In each, the named write is where the
// shutdown lands.
func TestABookletStepStoppedByShutdownReportsNothing(t *testing.T) {
	for _, tc := range bookletCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := loggingtest.Record(t)
			tc.run(t, ctx, &stoppingBooklets{fakeBookletSink: tc.sink(), at: tc.at, cancel: cancel})
			mustNotReport(t, rec, tc.msgs...)
		})
	}
}

// TestABookletStepThatFailsIsStillReported is the twin: the same write fails
// on a live context, and each line it owns is reported once.
func TestABookletStepThatFailsIsStillReported(t *testing.T) {
	for _, tc := range bookletCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			tc.run(t, context.Background(), &stoppingBooklets{fakeBookletSink: tc.sink(), at: tc.at,
				fail: errors.New(injectedStoreFail)})
			mustReportOnce(t, rec, tc.twinMsgs...)
		})
	}
}

// bookletCases are the booklet steps TestABookletStep... drive.
var bookletCases = []struct {
	name     string
	at       string   // the write that stops, or fails
	msgs     []string // not reported when it stops
	twinMsgs []string // reported once each when it fails
	sink     func() *fakeBookletSink
	run      func(t *testing.T, ctx context.Context, s *stoppingBooklets)
}{
	{
		name: "the orphan GC", at: "DeleteBookletsNotIn",
		msgs: []string{msgGCFailed}, twinMsgs: []string{msgGCFailed},
		sink: newFakeBookletSink,
		run: func(t *testing.T, ctx context.Context, s *stoppingBooklets) {
			bookletClient(t, "", s).gcBooklets(ctx, []string{cancelRelease})
		},
	},
	{
		name: "the fetch listing", at: "BookletsToFetch",
		msgs: []string{msgFetchList}, twinMsgs: []string{msgFetchList},
		sink: newFakeBookletSink,
		run: func(t *testing.T, ctx context.Context, s *stoppingBooklets) {
			_ = bookletClient(t, "", s).fetchBooklets(ctx)
		},
	},
	{
		// Atlas answers 404 for the PDF: the row is marked unavailable, then
		// its wire tag is cleared. The shutdown lands in the first write, so
		// the second runs on the cancelled context too.
		name: "a booklet gone upstream", at: "MarkBookletUnavailable",
		msgs: []string{msgMarkUnavail, msgClearTag}, twinMsgs: []string{msgMarkUnavail},
		sink: withOneToFetch,
		run: func(t *testing.T, ctx context.Context, s *stoppingBooklets) {
			srv := atlasFake(t, http.NotFound)
			_ = bookletClient(t, srv.URL, s).fetchBooklets(ctx)
		},
	},
	{
		// The same 404, with the shutdown landing in the tag clear.
		name: "a booklet gone upstream, in its tag clear", at: "SetBookletTagAndBumpIndex",
		msgs: []string{msgClearTag}, twinMsgs: []string{msgClearTag},
		sink: withOneToFetch,
		run: func(t *testing.T, ctx context.Context, s *stoppingBooklets) {
			srv := atlasFake(t, http.NotFound)
			_ = bookletClient(t, srv.URL, s).fetchBooklets(ctx)
		},
	},
	{
		name: "a booklet that landed", at: "MarkBookletFetched",
		msgs: []string{msgMarkFetched}, twinMsgs: []string{msgMarkFetched},
		sink: withOneToFetch,
		run: func(t *testing.T, ctx context.Context, s *stoppingBooklets) {
			srv := atlasFake(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/pdf")
				_, _ = w.Write([]byte("%PDF-1.4 booklet"))
			})
			_ = bookletClient(t, srv.URL, s).fetchBooklets(ctx)
		},
	},
}

// TestALyricsStampStoppedByShutdownReportsNothing: the shutdown lands in the
// attempt stamp.
func TestALyricsStampStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{Lyrics: &stoppingLyrics{fakeLyricsSink: newFakeSink(), cancel: cancel}}

	rec := loggingtest.Record(t)
	c.stamp(ctx, LyricsCandidate{Path: "a.flac"}, cancelArtistMBID, statusUnavailable, time.Hour)

	mustNotReport(t, rec, msgStampFailed)
}

// TestALyricsStampThatFailsIsStillReported is the twin.
func TestALyricsStampThatFailsIsStillReported(t *testing.T) {
	c := &Client{Lyrics: &stoppingLyrics{fakeLyricsSink: newFakeSink(), fail: errors.New(injectedStoreFail)}}

	rec := loggingtest.Record(t)
	c.stamp(context.Background(), LyricsCandidate{Path: "a.flac"}, cancelArtistMBID, statusUnavailable, time.Hour)

	mustReportOnce(t, rec, msgStampFailed)
}

// stoppingBooklets is a booklet store that behaves as the real one does on a
// cancelled context, answering every call with the cancellation, and whose
// write named `at` is where a shutdown lands: it cancels the pass and
// answers with the cancellation. With fail set, that write fails with it on
// a live context instead. It counts universe listings, which is how a test
// sees whether a booklet leg started.
type stoppingBooklets struct {
	*fakeBookletSink
	at            string
	cancel        context.CancelFunc
	fail          error
	universeCalls atomic.Int32
}

// step answers for one call: the cancellation on a cancelled context, the
// stop (or the failure) at `at`, and nil otherwise, so the fake handles it.
func (s *stoppingBooklets) step(ctx context.Context, method string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if method != s.at {
		return nil
	}
	if s.fail != nil {
		return s.fail
	}
	s.cancel()
	return ctx.Err()
}

func (s *stoppingBooklets) DistinctAlbumReleaseMBIDs(ctx context.Context) ([]string, error) {
	s.universeCalls.Add(1)
	if err := s.step(ctx, "DistinctAlbumReleaseMBIDs"); err != nil {
		return nil, err
	}
	return s.fakeBookletSink.DistinctAlbumReleaseMBIDs(ctx)
}

func (s *stoppingBooklets) BookletsToFetch(ctx context.Context, limit, maxAttempts int) ([]BookletFetchItem, error) {
	if err := s.step(ctx, "BookletsToFetch"); err != nil {
		return nil, err
	}
	return s.fakeBookletSink.BookletsToFetch(ctx, limit, maxAttempts)
}

func (s *stoppingBooklets) MarkBookletFetched(ctx context.Context, mbid string) error {
	if err := s.step(ctx, "MarkBookletFetched"); err != nil {
		return err
	}
	return s.fakeBookletSink.MarkBookletFetched(ctx, mbid)
}

func (s *stoppingBooklets) MarkBookletUnavailable(ctx context.Context, mbid string) error {
	if err := s.step(ctx, "MarkBookletUnavailable"); err != nil {
		return err
	}
	return s.fakeBookletSink.MarkBookletUnavailable(ctx, mbid)
}

func (s *stoppingBooklets) SetBookletTagAndBumpIndex(ctx context.Context, mbid, tag string) (int64, error) {
	if err := s.step(ctx, "SetBookletTagAndBumpIndex"); err != nil {
		return 0, err
	}
	return s.fakeBookletSink.SetBookletTagAndBumpIndex(ctx, mbid, tag)
}

func (s *stoppingBooklets) DeleteBookletsNotIn(ctx context.Context, universe []string) ([]string, error) {
	if err := s.step(ctx, "DeleteBookletsNotIn"); err != nil {
		return nil, err
	}
	return s.fakeBookletSink.DeleteBookletsNotIn(ctx, universe)
}

// stoppingLyrics is stoppingBooklets for the lyrics sink's attempt stamp.
type stoppingLyrics struct {
	*fakeLyricsSink
	cancel context.CancelFunc
	fail   error
}

func (s *stoppingLyrics) MarkAtlasLyricsAttempt(ctx context.Context, path, albumMBID, trackMBID,
	resolvedMBID, status string, nextAttemptAt int64) error {
	if s.fail != nil {
		return s.fail
	}
	s.cancel()
	return ctx.Err()
}

// refetcherFunc adapts a function to CoverRefetcher.
type refetcherFunc func(ctx context.Context, releaseMBID string) (bool, error)

func (f refetcherFunc) RefetchPremium(ctx context.Context, releaseMBID string) (bool, error) {
	return f(ctx, releaseMBID)
}

// withOneToFetch is a booklet store with one booklet waiting to download.
func withOneToFetch() *fakeBookletSink {
	s := newFakeBookletSink()
	s.toFetch = []BookletFetchItem{{ReleaseMBID: cancelRelease}}
	return s
}

// atlasFake is an Atlas whose every request h answers.
func atlasFake(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// holdUntilAbandoned holds a request until the client gives up on it, as a
// shutdown or a request timeout does. Bounded, so a client that never gives
// up fails the test rather than hanging it. It reads the body first: net/http
// notices a client hanging up only once the request body is consumed, so a
// POST held unread would sit out the whole bound.
func holdUntilAbandoned(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// dueClient is a client against atlasURL whose submit and booklet check are
// both due, with one artist to submit and an empty results page to poll.
func dueClient(t *testing.T, atlasURL string) *Client {
	t.Helper()
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	if err := state.SetCredential("test-token", atlasURL, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{cancelArtistMBID}}, Sink: &fakeSink{}}
}

// bookletClient is bookletTestClient over a store that stops.
func bookletClient(t *testing.T, atlasURL string, s *stoppingBooklets) *Client {
	t.Helper()
	c := bookletTestClient(t, atlasURL, s.fakeBookletSink, newFakeBookletFiles())
	c.Booklets = s
	return c
}

// coverClient is a client with one release pending a premium cover.
func coverClient(t *testing.T, r CoverRefetcher) *Client {
	t.Helper()
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	if err := state.AddPendingCovers([]string{cancelRelease}); err != nil {
		t.Fatal(err)
	}
	return &Client{State: state, Refetcher: r}
}

// mustNotReport fails the test for each msg logged at Warn or above.
func mustNotReport(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 0 {
			t.Errorf("a step that shutdown stopped reported %q:\n%s", m, strings.Join(got, "\n"))
		}
	}
}

// mustReportOnce fails the test for each msg not logged exactly once at
// Warn or above.
func mustReportOnce(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 1 {
			t.Errorf("a failure on a live context logged %q %d times, want 1:\n%s", m, len(got), strings.Join(got, "\n"))
		}
	}
}
