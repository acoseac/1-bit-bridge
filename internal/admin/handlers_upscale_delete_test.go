package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubVariantDeleter is the test-only stub the admin handler talks
// to in lieu of the cmd/bridge adapter. Captures the request the
// handler passed in so tests can assert the query → request
// translation, and returns a programmable response/error.
type stubVariantDeleter struct {
	gotReq   AdminVariantDeleteRequest
	gotCalls int
	resp     AdminVariantDeleteResponse
	err      error
}

func (s *stubVariantDeleter) Delete(_ context.Context, req AdminVariantDeleteRequest) (AdminVariantDeleteResponse, error) {
	s.gotCalls++
	s.gotReq = req
	return s.resp, s.err
}

// --- parseVariantDeleteRequest (pure function tests) ---

func TestParseVariantDeleteRequest_unscopedRequiresConfirm(t *testing.T) {
	_, code, _ := parseVariantDeleteRequest(map[string][]string{})
	if code != "bad_request" {
		t.Errorf("unscoped without confirm: got code=%q, want bad_request", code)
	}
}

func TestParseVariantDeleteRequest_confirmSetsAll(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"confirm": {"true"},
	})
	if code != "" {
		t.Errorf("confirm=true rejected: %q", code)
	}
	if !req.All {
		t.Errorf("confirm=true did not set req.All")
	}
	if req.Prefix != "" || req.Path != "" {
		t.Errorf("confirm=true leaked Prefix=%q Path=%q", req.Prefix, req.Path)
	}
}

func TestParseVariantDeleteRequest_prefixSetsField(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Music/Jazz"},
	})
	if code != "" {
		t.Errorf("prefix rejected: %q", code)
	}
	if req.Prefix != "Music/Jazz" {
		t.Errorf("got Prefix=%q, want Music/Jazz", req.Prefix)
	}
	if req.All || req.Path != "" {
		t.Errorf("prefix leaked All=%v Path=%q", req.All, req.Path)
	}
}

func TestParseVariantDeleteRequest_pathSetsField(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"path": {"Music/Jazz/01 - Track.flac"},
	})
	if code != "" {
		t.Errorf("path rejected: %q", code)
	}
	if req.Path != "Music/Jazz/01 - Track.flac" {
		t.Errorf("got Path=%q", req.Path)
	}
}

func TestParseVariantDeleteRequest_prefixAndPathRejected(t *testing.T) {
	_, code, msg := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Music"},
		"path":   {"Music/01.flac"},
	})
	if code != "bad_request" {
		t.Errorf("prefix+path: got code=%q, want bad_request", code)
	}
	if msg == "" {
		t.Errorf("prefix+path: no message provided")
	}
}

func TestParseVariantDeleteRequest_leadingSlashRejected(t *testing.T) {
	_, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"/Music/Jazz"},
	})
	if code != "bad_request" {
		t.Errorf("leading slash: got code=%q, want bad_request", code)
	}
}

func TestParseVariantDeleteRequest_traversalRejected(t *testing.T) {
	_, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Music/../etc"},
	})
	if code != "bad_request" {
		t.Errorf("traversal: got code=%q, want bad_request", code)
	}
}

func TestParseVariantDeleteRequest_backslashRejected(t *testing.T) {
	_, code, _ := parseVariantDeleteRequest(map[string][]string{
		"path": {`Music\01.flac`},
	})
	if code != "bad_request" {
		t.Errorf("backslash: got code=%q, want bad_request", code)
	}
}

func TestParseVariantDeleteRequest_emptyPrefixRejected(t *testing.T) {
	// An explicit `?prefix=` (empty value) is structurally different
	// from no prefix at all — the user clearly intended a prefix
	// shape but forgot the value. Reject so the admin UI surfaces
	// "bad request" instead of silently widening to all-variants.
	_, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {""},
	})
	if code != "bad_request" {
		t.Errorf("empty prefix: got code=%q, want bad_request", code)
	}
}

// --- Kind-narrowing parser tests (PR #276 senior-review fix) ---

func TestParseVariantDeleteRequest_emptyKindPreservesLegacyBehaviour(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Diana Krall"},
	})
	if code != "" {
		t.Fatalf("legacy shape: got code=%q, want empty (success)", code)
	}
	if req.Kind != "" {
		t.Errorf("legacy shape: got kind=%q, want empty", req.Kind)
	}
	if req.Prefix != "Diana Krall" {
		t.Errorf("prefix not preserved: got %q", req.Prefix)
	}
}

func TestParseVariantDeleteRequest_kindUpscaleAccepted(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Diana Krall"},
		"kind":   {"upscale"},
	})
	if code != "" {
		t.Fatalf("kind=upscale: got code=%q, want empty", code)
	}
	if req.Kind != "upscale" {
		t.Errorf("kind: got %q, want upscale", req.Kind)
	}
}

func TestParseVariantDeleteRequest_kindOptimizeAccepted(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Diana Krall"},
		"kind":   {"optimize"},
	})
	if code != "" {
		t.Fatalf("kind=optimize: got code=%q, want empty", code)
	}
	if req.Kind != "optimize" {
		t.Errorf("kind: got %q, want optimize", req.Kind)
	}
}

func TestParseVariantDeleteRequest_kindCaseFoldedToLower(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Diana Krall"},
		"kind":   {"OPTIMIZE"},
	})
	if code != "" {
		t.Fatalf("kind=OPTIMIZE: got code=%q, want empty", code)
	}
	if req.Kind != "optimize" {
		t.Errorf("kind: got %q, want optimize (case folded)", req.Kind)
	}
}

func TestParseVariantDeleteRequest_kindUnknownRejected(t *testing.T) {
	_, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"Diana Krall"},
		"kind":   {"junk"},
	})
	if code != "bad_request" {
		t.Errorf("kind=junk: got code=%q, want bad_request", code)
	}
}

// --- Handler integration tests via the admin Server's Handler() ---

func TestUpscaleVariantsDelete_returns503WhenUnwired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// VariantDeleter is intentionally NOT wired into Deps.
	code := doJSON(t, srv.Handler(),
		"DELETE", "/api/upscale/variants?confirm=true", nil, nil)
	if code != http.StatusServiceUnavailable {
		t.Errorf("unwired deleter: got status=%d, want 503", code)
	}
}

func TestUpscaleVariantsDelete_returns400OnMalformedQuery(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Wire a stub so the handler reaches the parse step instead of
	// short-circuiting on 503.
	srv.deps.VariantDeleter = &stubVariantDeleter{}

	cases := []struct {
		name string
		url  string
	}{
		{"no params", "/api/upscale/variants"},
		{"prefix + path", "/api/upscale/variants?prefix=Music&path=Music/01.flac"},
		{"leading slash", "/api/upscale/variants?prefix=/Music"},
		{"traversal", "/api/upscale/variants?prefix=Music/../etc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code := doJSON(t, srv.Handler(), "DELETE", c.url, nil, nil)
			if code != http.StatusBadRequest {
				t.Errorf("%s: got status=%d, want 400", c.name, code)
			}
		})
	}
}

func TestUpscaleVariantsDelete_translatesUnavailableErrTo503(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.VariantDeleter = &stubVariantDeleter{
		err: ErrAdminVariantDeleterUnavailable,
	}
	code := doJSON(t, srv.Handler(),
		"DELETE", "/api/upscale/variants?confirm=true", nil, nil)
	if code != http.StatusServiceUnavailable {
		t.Errorf("ErrAdminVariantDeleterUnavailable surface: got status=%d, want 503", code)
	}
}

func TestUpscaleVariantsDelete_returns500OnAdapterError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.VariantDeleter = &stubVariantDeleter{
		err: errors.New("sqlite read failed"),
	}
	code := doJSON(t, srv.Handler(),
		"DELETE", "/api/upscale/variants?confirm=true", nil, nil)
	if code != http.StatusInternalServerError {
		t.Errorf("adapter error: got status=%d, want 500", code)
	}
}

func TestUpscaleVariantsDelete_forwardsPrefixToAdapter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &stubVariantDeleter{
		resp: AdminVariantDeleteResponse{
			DeletedCount: 3,
			FreedBytes:   12345,
			DeletedPaths: []string{"Music/Jazz/A.flac", "Music/Jazz/B.flac"},
		},
	}
	srv.deps.VariantDeleter = stub

	var out AdminVariantDeleteResponse
	code := doJSON(t, srv.Handler(),
		"DELETE", "/api/upscale/variants?prefix=Music/Jazz", nil, &out)
	if code != http.StatusOK {
		t.Fatalf("got status=%d, want 200", code)
	}

	if stub.gotCalls != 1 {
		t.Errorf("adapter calls: got %d, want 1", stub.gotCalls)
	}
	if stub.gotReq.Prefix != "Music/Jazz" {
		t.Errorf("forwarded Prefix=%q, want Music/Jazz", stub.gotReq.Prefix)
	}
	if stub.gotReq.All {
		t.Errorf("forwarded All=true on prefix request")
	}
	if out.DeletedCount != 3 || out.FreedBytes != 12345 {
		t.Errorf("response mismatched: got %+v", out)
	}
}

func TestUpscaleVariantsDelete_forwardsAllOnConfirm(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &stubVariantDeleter{
		resp: AdminVariantDeleteResponse{
			DeletedCount: 10,
			FreedBytes:   999_000,
			DeletedPaths: []string{},
		},
	}
	srv.deps.VariantDeleter = stub

	code := doJSON(t, srv.Handler(),
		"DELETE", "/api/upscale/variants?confirm=true", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("got status=%d, want 200", code)
	}
	if !stub.gotReq.All {
		t.Errorf("?confirm=true did not set req.All on the adapter call")
	}
}

func TestUpscaleVariantsDelete_csrfBlocksWrongContentType(t *testing.T) {
	// The CSRF guard (`csrfGuard` middleware) rejects mutating
	// requests with a body whose Content-Type isn't
	// application/json — defense against form-encoded cross-site
	// POSTs reaching the admin from a malicious page. Verify the
	// gate fires for our route too.
	srv, _, _ := newTestServer(t)
	srv.deps.VariantDeleter = &stubVariantDeleter{}

	req := httptest.NewRequest("DELETE",
		"/api/upscale/variants?confirm=true",
		strings.NewReader(`{"_": true}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "text/plain")

	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusUnsupportedMediaType {
		t.Errorf("CSRF wrong content-type: got status=%d, want 415", rw.Code)
	}
}
