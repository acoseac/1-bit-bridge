package atlasharvest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestSubmitHTTPAnswerAfterShutdown exercises the TLS response race B526
// observed: the server answers only after the submit's loop was cancelled.
// The transport keeps the close window open so the answer is deterministic.
func TestSubmitHTTPAnswerAfterShutdown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		answer     func(http.ResponseWriter)
		shutdown   bool
		wantStatus int
	}{
		{"empty answer after shutdown", func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }, true, 0},
		{"connection closes after shutdown", func(http.ResponseWriter) { panic(http.ErrAbortHandler) }, true, 0},
		{"body read abandoned after shutdown", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, `{"accepted":`)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, true, 0},
		{"401 after shutdown", func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }, true, 0},
		{"500 after shutdown", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, true, http.StatusInternalServerError},
		{"complete malformed body after shutdown", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"accepted":`) }, true, -1},
		{"500 while live", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, false, http.StatusInternalServerError},
		{"malformed body while live", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"accepted":`) }, false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.shutdown {
					cancel()
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
						t.Error("TLS request did not observe the client's hang-up")
						panic(http.ErrAbortHandler)
					}
				}
				tc.answer(w)
			}))
			t.Cleanup(srv.Close)
			c := dueClient(t, srv)
			c.HTTP = racingHangUpRawClient(srv)
			rec := loggingtest.Record(t)

			err := c.submitAll(ctx, c.State.Snapshot())
			if err == nil {
				t.Fatal("submitAll returned nil, want the HTTP result classified")
			}
			c.handleErr(ctx, "submit", err)
			switch tc.wantStatus {
			case 0:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("submitAll error = %v, want shutdown cancellation", err)
				}
				mustNotReport(t, rec, msgTickError, "atlasharvest.token_rejected")
			case -1:
				if errors.Is(err, context.Canceled) {
					t.Errorf("complete malformed body was hidden as shutdown: %v", err)
				}
				mustReportOnce(t, rec, msgTickError)
			default:
				var status *httpStatusError
				if !errors.As(err, &status) || status.Code != tc.wantStatus {
					t.Errorf("submitAll error = %v, want HTTP %d", err, tc.wantStatus)
				}
				mustReportOnce(t, rec, msgTickError)
			}
			if tc.name == "401 after shutdown" {
				if got := c.State.Snapshot().Token; got != "test-token" {
					t.Errorf("abandoned 401 wiped credential: token = %q", got)
				}
			}
		})
	}
}
