package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// slowSearcher is a searcher whose query runs until its context ends and
// then fails the way the store's does: the SQLite driver answers a query
// its context interrupts with the context's error, and the store wraps
// that under its prefix (backlog B434). One search per searcher: started
// is closed when it begins, and ended receives how its context ended.
type slowSearcher struct {
	stubSearcher
	started chan struct{}
	ended   chan slowSearchEnd
}

// slowSearchEnd is how a slowSearcher's context ended: its error, and
// whether it carried a deadline.
type slowSearchEnd struct {
	err         error
	hadDeadline bool
}

// slowSearchCap ends a search whose context never ends, so a route that
// lost its deadline fails the test instead of hanging it.
const slowSearchCap = 30 * time.Second

func newSlowSearcher() *slowSearcher {
	return &slowSearcher{
		stubSearcher: stubSearcher{available: true},
		started:      make(chan struct{}),
		ended:        make(chan slowSearchEnd, 1),
	}
}

func (s *slowSearcher) SearchServedTracks(ctx context.Context, _ string, _ int) ([]manifest.TrackHit, error) {
	close(s.started)
	_, hadDeadline := ctx.Deadline()
	timer := time.NewTimer(slowSearchCap)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		s.ended <- slowSearchEnd{hadDeadline: hadDeadline}
		return nil, errors.New("slowSearcher: the search's context never ended")
	}
	s.ended <- slowSearchEnd{err: ctx.Err(), hadDeadline: hadDeadline}
	return nil, fmt.Errorf("search tracks_fts (served): %w", ctx.Err())
}

// newSearchRouter is the API router (Handler()) over mp, and a token it
// accepts.
func newSearchRouter(t *testing.T, mp ManifestProvider) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("search-client")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "Test"}
	return New(cfg, tokens, mp, "fp").Handler(), raw
}

// searchWith sends GET /v1/search?q=xtal through router with token, under
// ctx, and returns the recorded answer.
func searchWith(ctx context.Context, router http.Handler, token string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=xtal", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(rr, req)
	return rr
}

// newRealSearchProvider is the Provider cmd/bridge builds, over a real
// store holding one track, Xtal, and that store. It skips where the SQLite
// driver has no FTS5.
func newRealSearchProvider(t *testing.T) (*manifest.Provider, *manifest.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Registered after TempDir, so it runs first. A test may close the
	// store itself: a second Close is nil.
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if ok, _ := store.SearchAvailable(ctx); !ok {
		t.Skip("FTS5 not compiled into this driver build")
	}
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "Aphex Twin/SAW/01 Xtal.flac", Title: "Xtal",
		Artist: "Aphex Twin", Album: "Selected Ambient Works", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return manifest.NewProvider(store, manifest.NewScanner([]string{lib}, store, "")), store
}

// assertSearchDeadlineIsAServerFailure checks the answer and the log of a
// search that ran out its deadline: one request-failed line at Error with
// the 500 and the error, the access line at Error at 500, and a 500
// `internal` body with the fixed message and none of the error's text.
func assertSearchDeadlineIsAServerFailure(t *testing.T, rr *httptest.ResponseRecorder, logged string) {
	t.Helper()
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%q", rr.Code, rr.Body.String())
	}
	failed := linesContaining(logged, `msg="request failed"`)
	if len(failed) != 1 {
		t.Errorf("want one request-failed line, got %d; log:\n%s", len(failed), logged)
	} else {
		for _, want := range []string{"level=ERROR", "status=500", "code=internal", "context deadline exceeded"} {
			if !strings.Contains(failed[0], want) {
				t.Errorf("request-failed line lacks %q: %s", want, failed[0])
			}
		}
	}
	access := linesContaining(logged, "msg=http ")
	if len(access) != 1 {
		t.Errorf("want one access line, got %d; log:\n%s", len(access), logged)
	} else if !strings.Contains(access[0], "level=ERROR") || !strings.Contains(access[0], "status=500") {
		t.Errorf("access line is not Error at 500: %s", access[0])
	}
	if rr.Body.Len() == 0 {
		t.Fatal("the body is empty, which a client reads as no results")
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Error != "internal" || er.Message != searchFailedMessage {
		t.Errorf("body = {%q, %q}, want {internal, %q}", er.Error, er.Message, searchFailedMessage)
	}
	for _, leak := range []string{"tracks_fts", "deadline", "context"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Errorf("the 500 body carries %q from the error's text: %s", leak, rr.Body.String())
		}
	}
}

// TestASearchThatOutlivesItsDeadlineAnswers500 is backlog B434 through the
// router. GET /v1/search runs under the route's own 5 s deadline
// (withCtxTimeout), and a search still running then fails with the
// context's error under the store's prefix. The deadline is the server's,
// so by B364's rule the failure is Error and 500. The handler returned
// without writing for it, as it does for a cancel, and net/http sent an
// empty 200, which a client reads as no results. The test waits out the
// route's real deadline, so it takes 5 s.
func TestASearchThatOutlivesItsDeadlineAnswers500(t *testing.T) {
	buf := withTestSlog(t)
	slow := newSlowSearcher()
	router, token := newSearchRouter(t, slow)

	rr := searchWith(context.Background(), router, token)

	select {
	case end := <-slow.ended:
		if !end.hadDeadline {
			t.Fatal("the search's context carried no deadline; the route sets one")
		}
		if !errors.Is(end.err, context.DeadlineExceeded) {
			t.Fatalf("the search's context ended with %v, want the route's deadline", end.err)
		}
	default:
		t.Fatalf("the search never ran: status %d, body %q", rr.Code, rr.Body.String())
	}
	assertSearchDeadlineIsAServerFailure(t, rr, buf.String())
}

// TestASearchPastItsDeadlineThroughTheRealStoreAnswers500 is backlog B434
// against the store cmd/bridge serves from: the router over the real
// Provider and store, and a request whose deadline has passed when the
// search runs. database/sql answers a query on an expired context with the
// context's error, as the SQLite driver answers one its deadline
// interrupts, and the store wraps it under its prefix. The same search
// with time to spare finds the track.
func TestASearchPastItsDeadlineThroughTheRealStoreAnswers500(t *testing.T) {
	buf := withTestSlog(t)
	provider, _ := newRealSearchProvider(t)
	router, token := newSearchRouter(t, provider)

	if rr := searchWith(context.Background(), router, token); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Xtal") {
		t.Fatalf("search with time to spare: status %d, body %s; want 200 with the seeded track", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	buf.Reset()
	rr := searchWith(ctx, router, token)
	assertSearchDeadlineIsAServerFailure(t, rr, buf.String())
}

// TestASearchCancelWhileTheRequestIsLiveAnswers500 pins the other half of
// the search's silent return (the review on #1270): it is for the client's
// own abort only. A context.Canceled from inside the search while the
// request's context is still live is the server's failure, so by B364's
// rule it is Error and 500 with the fixed message, never an empty 200.
func TestASearchCancelWhileTheRequestIsLiveAnswers500(t *testing.T) {
	buf := withTestSlog(t)
	stub := &stubSearcher{available: true,
		err: fmt.Errorf("search tracks_fts (served): %w", context.Canceled)}
	router, token := newSearchRouter(t, stub)

	rr := searchWith(context.Background(), router, token)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rr.Code, rr.Body.String())
	}
	logged := buf.String()
	failed := linesContaining(logged, `msg="request failed"`)
	if len(failed) != 1 {
		t.Fatalf("want one request-failed line, got %d; log:\n%s", len(failed), logged)
	}
	for _, want := range []string{"level=ERROR", "status=500", "code=internal", "context canceled"} {
		if !strings.Contains(failed[0], want) {
			t.Errorf("request-failed line lacks %q: %s", want, failed[0])
		}
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Error != "internal" || er.Message != searchFailedMessage {
		t.Errorf("body = {%q, %q}, want {internal, %q}", er.Error, er.Message, searchFailedMessage)
	}
	for _, leak := range []string{"tracks_fts", "canceled", "context"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Errorf("the 500 body carries %q from the error's text: %s", leak, rr.Body.String())
		}
	}
}

// TestAClientAbortDuringSearchStaysInfo pins what B434 leaves alone. A
// client that keeps typing abandons the search in flight, which cancels
// the request's context, and the handler returns without writing: no
// request-failed line, nothing at Error, and an access line at Info. Its
// status is 0, which the access line records for a handler that wrote
// nothing; the abort never reaches writeErrorLog, so it is not B364's 499.
func TestAClientAbortDuringSearchStaysInfo(t *testing.T) {
	buf := withTestSlog(t)
	slow := newSlowSearcher()
	router, token := newSearchRouter(t, slow)
	// The access line is written after the router returns. finished is
	// closed only then, so the test reads a settled log.
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		close(finished)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/search?q=xtal", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		select {
		case <-slow.started:
			cancel()
		case <-ctx.Done():
		}
	}()
	resp, doErr := http.DefaultClient.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the search did not finish after the client canceled")
	}
	if doErr == nil {
		t.Fatal("client Do returned nil; the cancel did not reach the request")
	}
	select {
	case end := <-slow.ended:
		if !errors.Is(end.err, context.Canceled) {
			t.Fatalf("the search's context ended with %v, want the client's cancel", end.err)
		}
	default:
		t.Fatal("the search never ran")
	}

	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("a client abort logged at ERROR:\n%s", logged)
	}
	if failed := linesContaining(logged, `msg="request failed"`); len(failed) != 0 {
		t.Errorf("a client abort wrote a request-failed line: %q", failed)
	}
	access := linesContaining(logged, "msg=http ")
	if len(access) != 1 {
		t.Fatalf("want one access line, got %d; log:\n%s", len(access), logged)
	}
	if !strings.Contains(access[0], "level=INFO") || !strings.Contains(access[0], "status=0 ") {
		t.Errorf("access line is not Info at status 0: %s", access[0])
	}
}
