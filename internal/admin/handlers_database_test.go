package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
