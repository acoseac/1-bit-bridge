package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// decodeErrorResponse pulls ErrorResponse out of a recorder body. Tiny
// helper to keep the test cases readable.
func decodeErrorResponse(t *testing.T, body []byte) ErrorResponse {
	t.Helper()
	var er ErrorResponse
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatalf("unmarshal ErrorResponse: %v (body=%q)", err, body)
	}
	return er
}

func TestWriteErrorLog_RecordsErrAndReturnsSanitizedMessage(t *testing.T) {
	// Capture slog records to make sure the underlying err lands in the log.
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	writeErrorLog(rr, req, http.StatusInternalServerError, "internal",
		"the bridge encountered an internal error",
		errors.New("sql: column foo not found"))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Error != "internal" {
		t.Errorf("code = %q, want internal", er.Error)
	}
	if er.Message != "the bridge encountered an internal error" {
		t.Errorf("message = %q, want sanitized literal", er.Message)
	}
	if strings.Contains(rr.Body.String(), "sql:") {
		t.Errorf("raw err must NOT appear in response body: %q", rr.Body.String())
	}
	if !strings.Contains(buf.String(), "sql: column foo not found") {
		t.Errorf("raw err must appear in server log, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), `code=internal`) {
		t.Errorf("code attr missing from server log, got %q", buf.String())
	}
}

func TestWriteErrorLog_NilErrIsValid(t *testing.T) {
	// Some callers use the helper for state-check failures where there's
	// no Go error to attach. The wire body must still be the sanitized
	// message; the log line is silent on the nil-err case.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	writeErrorLog(rr, req, http.StatusBadRequest, "bad_request", "missing field foo", nil)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Message != "missing field foo" {
		t.Errorf("message = %q", er.Message)
	}
}

func TestWriteResolveError_TypedSentinelsReturnStableMessages(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "ErrBadPath",
			err:        bridgefs.ErrBadPath,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
			wantMsg:    bridgefs.ErrBadPath.Error(),
		},
		{
			name:       "ErrUnknownRoot",
			err:        bridgefs.ErrUnknownRoot,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
			wantMsg:    bridgefs.ErrUnknownRoot.Error(),
		},
		{
			name:       "ErrNotFound",
			err:        bridgefs.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
			wantMsg:    bridgefs.ErrNotFound.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			if !writeResolveError(rr, req, tc.err) {
				t.Fatal("writeResolveError returned false for non-nil err")
			}
			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			er := decodeErrorResponse(t, rr.Body.Bytes())
			if er.Error != tc.wantCode {
				t.Errorf("code = %q, want %q", er.Error, tc.wantCode)
			}
			if er.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", er.Message, tc.wantMsg)
			}
		})
	}
}

func TestWriteResolveError_DefaultBranchSanitizes(t *testing.T) {
	// An unknown resolver error must not leak its text. The diagnostic
	// detail goes to the server log; the wire body is generic.
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	leaky := errors.New("readonly: /Users/operator/Music/private-stash refused")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	if !writeResolveError(rr, req, leaky) {
		t.Fatal("returned false for non-nil err")
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Error != "internal" {
		t.Errorf("code = %q, want internal", er.Error)
	}
	if strings.Contains(er.Message, "/Users") || strings.Contains(er.Message, "private-stash") {
		t.Errorf("filesystem path leaked into wire body: %q", er.Message)
	}
	if !strings.Contains(buf.String(), "private-stash") {
		t.Errorf("raw err should land in server log, got %q", buf.String())
	}
}

func TestWriteResolveError_NilIsNoOp(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	if writeResolveError(rr, req, nil) {
		t.Error("writeResolveError should return false for nil err")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("no err should leave status at 200, got %d", rr.Code)
	}
}
