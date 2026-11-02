package atlasharvest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

type harvestRoundTrip func(*http.Request) (*http.Response, error)

func (f harvestRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedHTTPBody struct {
	io.Reader
	closed bool
}

func (b *trackedHTTPBody) Close() error { b.closed = true; return nil }

type failedHTTPRead struct{ err error }

func (r failedHTTPRead) Read([]byte) (int, error) { return 0, r.err }

// TestSubmitHTTPClassifiesOnlyAbandonedReads uses an in-memory transport to
// pin the classification independently of TLS scheduling.
func TestSubmitHTTPClassifiesOnlyAbandonedReads(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		status    int
		body      string
		readErr   error
		transport error
		wantStop  bool
		wantAuth  bool
	}{
		{name: "transport EOF after shutdown", cancel: true, transport: io.EOF, wantStop: true},
		{name: "empty answer after shutdown", cancel: true, status: 200, wantStop: true},
		{name: "abandoned body read", cancel: true, status: 200, readErr: io.ErrUnexpectedEOF, wantStop: true},
		{name: "abandoned 401", cancel: true, status: 401, wantStop: true},
		{name: "500 after shutdown", cancel: true, status: 500},
		{name: "complete malformed answer after shutdown", cancel: true, status: 200, body: `{"accepted":`},
		{name: "live 500", status: 500},
		{name: "live malformed answer", status: 200, body: `{"accepted":`},
		{name: "live 401", status: 401, wantAuth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
			if err := state.SetCredential("test-token", "https://atlas.test", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			var body *trackedHTTPBody
			c := &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{cancelArtistMBID}}, HTTP: &http.Client{
				Transport: harvestRoundTrip(func(r *http.Request) (*http.Response, error) {
					if tc.cancel {
						cancel()
					}
					if tc.transport != nil {
						return nil, tc.transport
					}
					var read io.Reader = strings.NewReader(tc.body)
					if tc.readErr != nil {
						read = failedHTTPRead{tc.readErr}
					}
					body = &trackedHTTPBody{Reader: read}
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body, Request: r}, nil
				}),
			}}
			rec := loggingtest.Record(t)
			err := c.submitAll(ctx, state.Snapshot())
			if err == nil {
				t.Fatal("submitAll returned nil, want an error")
			}
			c.handleErr(ctx, "submit", err)
			if got := errors.Is(err, context.Canceled); got != tc.wantStop {
				t.Errorf("shutdown classification = %v, want %v (error %v)", got, tc.wantStop, err)
			}
			if tc.status == http.StatusInternalServerError {
				var status *httpStatusError
				if !errors.As(err, &status) || status.Code != tc.status {
					t.Errorf("HTTP 500 was lost: %v", err)
				}
			}
			if tc.wantStop || tc.wantAuth {
				mustNotReport(t, rec, msgTickError)
			} else {
				mustReportOnce(t, rec, msgTickError)
			}
			if tc.wantAuth {
				mustReportOnce(t, rec, "atlasharvest.token_rejected")
				if got := state.Snapshot().Token; got != "" {
					t.Errorf("live 401 kept token %q", got)
				}
			} else if got := state.Snapshot().Token; got != "test-token" {
				t.Errorf("token was wiped: %q", got)
			}
			if body != nil && !body.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

func TestSubmitTickStopsQuietlyOnAbandonedAnswer(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
			if err := state.SetCredential("test-token", "https://atlas.test", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			c := &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{cancelArtistMBID}}, HTTP: &http.Client{
				Transport: harvestRoundTrip(func(r *http.Request) (*http.Response, error) {
					cancel()
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}),
			}}
			rec := loggingtest.Record(t)
			c.tick(ctx)
			mustNotReport(t, rec, msgTickError, "atlasharvest.token_rejected")
			if got := state.Snapshot().Token; got != "test-token" {
				t.Errorf("abandoned answer wiped token %q", got)
			}
		})
	}
}

func TestSubmitPerRequestTimeoutIsReportedWhileLoopLives(t *testing.T) {
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	if err := state.SetCredential("test-token", "https://atlas.test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c := &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{cancelArtistMBID}}, RequestTimeout: time.Millisecond,
		HTTP: &http.Client{Transport: harvestRoundTrip(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}}
	rec := loggingtest.Record(t)
	loopCtx := context.Background()
	err := c.submitAll(loopCtx, state.Snapshot())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("submit timeout = %v, want deadline exceeded", err)
	}
	c.handleErr(loopCtx, "submit", err)
	mustReportOnce(t, rec, msgTickError)
}

func TestHandleErrKeepsCredentialAfterShutdown(t *testing.T) {
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	if err := state.SetCredential("test-token", "https://atlas.test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := loggingtest.Record(t)
	(&Client{State: state}).handleErr(ctx, "submit", errUnauthorized)
	mustNotReport(t, rec, msgTickError, "atlasharvest.token_rejected")
	if got := state.Snapshot().Token; got != "test-token" {
		t.Errorf("shutdown's 401 wiped credential: %q", got)
	}
}
