package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAClientAbortIsLoggedAsInfoWithStatus499 is the playlist shape: the
// handler's store read uses the request context, and the error goes to
// writeErrorLog at 500. A client that closes the connection cancels that
// context. The access line and the request-failed line both record that
// as a client abort.
func TestAClientAbortIsLoggedAsInfoWithStatus499(t *testing.T) {
	buf := withTestSlog(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	inner := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		err := fmt.Errorf("list playlists: %w", r.Context().Err())
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to list playlists", err)
	}))
	// The access line is written after the handler returns. Close finished
	// only once that write has returned, so the test reads a settled buffer.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		close(finished)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/playlists", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-started
		cancel()
	}()
	resp, doErr := http.DefaultClient.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish after the client canceled")
	}
	if doErr == nil {
		t.Fatal("client Do returned nil; the cancel did not reach the request")
	}

	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("client abort logged at ERROR:\n%s", logged)
	}
	if !strings.Contains(logged, "status=499") {
		t.Errorf("access line status, log:\n%s", logged)
	}
	if strings.Count(logged, "level=INFO") < 2 {
		t.Errorf("want the request-failed line and the access line at INFO:\n%s", logged)
	}
}

func TestAServerDeadlineStaysAServerError(t *testing.T) {
	buf := withTestSlog(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/playlists", nil).WithContext(ctx)
	requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to list playlists", fmt.Errorf("list playlists: %w", r.Context().Err()))
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, log:\n%s", rr.Code, buf.String())
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") || strings.Contains(logged, "status=499") {
		t.Fatalf("a server deadline stays a server error:\n%s", logged)
	}
}

func TestACancelWhileTheRequestIsLiveStaysAServerError(t *testing.T) {
	buf := withTestSlog(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/playlists", nil)
	requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to list playlists", context.Canceled)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, log:\n%s", rr.Code, buf.String())
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") || strings.Contains(logged, "status=499") {
		t.Fatalf("a canceled error on a live request stays a server error:\n%s", logged)
	}
}

func TestAClientAbortThroughWriteFileErrorLogRecords499(t *testing.T) {
	buf := withTestSlog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/download", nil).WithContext(ctx)
	writeFileErrorLog(rr, req, http.StatusInternalServerError, "internal",
		"could not read this file", "Artist/Album/01.flac",
		fmt.Errorf("stat: %w", context.Canceled))
	if rr.Code != statusClientClosedRequest {
		t.Fatalf("status %d, log:\n%s", rr.Code, buf.String())
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Fatalf("client abort logged at ERROR:\n%s", logged)
	}
	if !strings.Contains(logged, "level=INFO") || !strings.Contains(logged, "status=499") {
		t.Fatalf("log:\n%s", logged)
	}
	if !strings.Contains(logged, "path=Artist/Album/01.flac") {
		t.Fatalf("library path missing:\n%s", logged)
	}
}

func TestAClientAbortDoesNotRewriteA4xx(t *testing.T) {
	buf := withTestSlog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/playlists", nil).WithContext(ctx)
	writeErrorLog(rr, req, http.StatusBadRequest, "bad_request", "bad request", context.Canceled)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, log:\n%s", rr.Code, buf.String())
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") || strings.Contains(logged, "status=499") {
		t.Fatalf("a 4xx stays a 4xx:\n%s", logged)
	}
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "status=400") {
		t.Fatalf("log:\n%s", logged)
	}
}
