package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestVariantFailureRetryClearsScoped pins the operator escape hatch: without
// it a suppressed source waits out the 30-day TTL or needs its mtime touched,
// even after the operator has fixed the actual cause.
func TestVariantFailureRetryClearsScoped(t *testing.T) {
	s, _, _ := newTestServer(t)

	rate, bits, dsd := 96000.0, 24, false
	for _, p := range []string{"Album/01.flac", "Other/01.flac"} {
		if err := s.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 1000, ModTime: time.Unix(0, 1700000000),
			SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC", IsDSD: &dsd,
		}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ { // variantFailureThreshold, unexported in manifest
			if err := s.deps.Manifest.RecordVariantFailure(context.Background(), p, 1000, 1700000000); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := s.deps.Manifest.SuppressedVariantFailureCount(context.Background()); err != nil || n != 2 {
		t.Fatalf("precondition: count=%d err=%v, want 2", n, err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/upscale/failures/retry",
		strings.NewReader(`{"path":"Album"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got variantFailureRetryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cleared != 1 {
		t.Errorf("cleared = %d, want 1 — the retry must be scoped to the requested subtree", got.Cleared)
	}
	// The out-of-scope suppression survives: a scoped retry that cleared the
	// whole library would silently re-open work the operator did not ask for.
	if n, err := s.deps.Manifest.SuppressedVariantFailureCount(context.Background()); err != nil || n != 1 {
		t.Errorf("remaining suppressed = %d err=%v, want 1", n, err)
	}
}

// TestScrubForLog pins the log-sink scrubbing. slog already escapes, so this
// is not closing an exploitable hole — but /api/logs/export parses this log
// back as text on a strict `time= level=` prefix, so control characters in an
// operator-supplied path have no business reaching it.
func TestScrubForLog(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Album/Disc 1", "Album/Disc 1"},
		{"Album\nlevel=ERROR msg=\"forged\"", "Album?level=ERROR msg=\"forged\""},
		{"a\r\nb", "a??b"},
		{"tab\there", "tab?here"},
		{"Édith Piaf/Non, je ne regrette rien", "Édith Piaf/Non, je ne regrette rien"},
	} {
		if got := scrubForLog(tc.in); got != tc.want {
			t.Errorf("scrubForLog(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
