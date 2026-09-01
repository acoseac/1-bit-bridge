package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestDatabaseCompactReclaimsAndReports(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	req := httptest.NewRequest("POST", "/api/database/compact", nil)
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got databaseCompactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if got.BeforeBytes <= 0 || got.AfterBytes <= 0 {
		t.Errorf("implausible sizes: %+v", got)
	}
	if got.ReclaimedBytes != got.BeforeBytes-got.AfterBytes {
		t.Errorf("ReclaimedBytes = %d, want before-after = %d",
			got.ReclaimedBytes, got.BeforeBytes-got.AfterBytes)
	}
}

// TestDatabaseCompactRefusedDuringScan pins the guard. A vacuum takes an
// exclusive lock; a scan takes s.mu for every batch flush, so overlapping
// them serialises the scan behind the whole vacuum.
func TestDatabaseCompactRefusedDuringScan(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	// Drive the scanner's in-flight counter directly: standing up a real
	// long-running scan to hold the window open is far more fixture than
	// the assertion needs, and the counter IS the predicate.
	s.deps.Scanner.MarkScanInFlightForTests(true)
	defer s.deps.Scanner.MarkScanInFlightForTests(false)

	req := httptest.NewRequest("POST", "/api/database/compact", nil)
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while a scan is in flight; body=%s", rr.Code, rr.Body.String())
	}
}

// TestDiagnosticsCarriesDatabaseSize pins the visibility half: an
// operator cannot sensibly decide whether to compact a file whose size
// they have never seen.
func TestDiagnosticsCarriesDatabaseSize(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	dreq := httptest.NewRequest("GET", "/api/diagnostics", nil)
	dreq.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, dreq)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	v, ok := body["databaseBytes"]
	if !ok {
		t.Fatal("diagnostics response carries no databaseBytes")
	}
	if n, _ := v.(float64); n <= 0 {
		t.Errorf("databaseBytes = %v, want a real file size", v)
	}
	if _, ok := body["databaseReclaimableBytes"]; !ok {
		t.Error("diagnostics response carries no databaseReclaimableBytes")
	}
}

// TestDiagnosticsCarriesRetentionCounts pins the visibility half of the
// retention work. The knobs default to off, so the numbers are the only
// thing that turns "keep everything" from an inherited default into a
// decision.
func TestDiagnosticsCarriesRetentionCounts(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()

	// Seed one registration and two events, one of them older.
	if err := s.deps.Manifest.UpsertDeviceRegistration(ctx, "dev", "tok", "Phone"); err != nil {
		t.Fatal(err)
	}
	oldest := time.Now().Add(-72 * time.Hour)
	if err := s.deps.Manifest.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
		{DeviceToken: "dev", Path: "a.flac", StartedAt: oldest.UnixNano(), DurationUsed: 30},
		{DeviceToken: "dev", Path: "b.flac", StartedAt: time.Now().UnixNano(), DurationUsed: 30},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/diagnostics", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got, _ := body["playbackHistoryRows"].(float64); got != 2 {
		t.Errorf("playbackHistoryRows = %v, want 2", body["playbackHistoryRows"])
	}
	if got, _ := body["deviceRegistrationRows"].(float64); got != 1 {
		t.Errorf("deviceRegistrationRows = %v, want 1", body["deviceRegistrationRows"])
	}
	ts, _ := body["oldestPlaybackStartedAt"].(string)
	if ts == "" {
		t.Fatal("oldestPlaybackStartedAt missing; the operator cannot see how far back the table goes")
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("oldestPlaybackStartedAt %q is not RFC3339: %v", ts, err)
	}
	if d := parsed.Sub(oldest).Abs(); d > time.Second {
		t.Errorf("oldestPlaybackStartedAt = %v, want ~%v", parsed, oldest)
	}
}

// An EMPTY history table must omit the timestamp rather than emit a zero
// one — the same trap the enrichmentProgress.lastEnrichedAt pointer
// exists for: a client would parse 0001-01-01 as a real, very old date.
func TestDiagnosticsOmitsTheOldestEventWhenHistoryIsEmpty(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/diagnostics", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["oldestPlaybackStartedAt"]; present {
		t.Errorf("oldestPlaybackStartedAt present on an empty table: %v",
			body["oldestPlaybackStartedAt"])
	}
}
