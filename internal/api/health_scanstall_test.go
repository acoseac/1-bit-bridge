package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The client-facing half of the stall watchdog. iOS defers every
// incremental sync while `scanState.isScanning` is true, so a scan that
// can never finish (2026-08-29: scanner threads wedged uninterruptibly
// in `fuse_open` on a network mount) turned that optimisation into a
// silent, indefinite sync outage. `/v1/health` must therefore answer
// "should you wait for me?" — not "is a goroutine alive?".

func healthScanState(t *testing.T, mp ManifestProvider) ScanState {
	t.Helper()
	hs, _ := withManifest(t, mp)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.ScanState
}

func TestHealth_ActiveScanIsAdvertisedAsScanning(t *testing.T) {
	st := healthScanState(t, &fakeManifestProvider{isScanning: true})
	if !st.IsScanning {
		t.Error("a healthy in-flight scan must still be advertised — the busy-defer is a real optimisation")
	}
	if st.ScanStalled {
		t.Error("a healthy scan must not be flagged stalled")
	}
}

// THE regression: a wedged scan must not keep clients from syncing.
func TestHealth_StalledScanIsNotAdvertisedAsScanning(t *testing.T) {
	st := healthScanState(t, &fakeManifestProvider{isScanning: true, isScanStalled: true})
	if st.IsScanning {
		t.Error("a stalled scan must report isScanning=false so client syncs resume")
	}
	if !st.ScanStalled {
		t.Error("the stall must still be surfaced — suppressed, not hidden")
	}
}

// The stall flag is meaningless without a scan, and must never be
// emitted on its own — a client seeing scanStalled with no scan would
// be reading a contradiction.
func TestHealth_NoScanNeverReportsStalled(t *testing.T) {
	st := healthScanState(t, &fakeManifestProvider{isScanning: false, isScanStalled: true})
	if st.IsScanning {
		t.Error("no scan running: isScanning must be false")
	}
	if st.ScanStalled {
		t.Error("no scan running: scanStalled must be false even if the scanner says otherwise")
	}
}

// Wire-shape guard: `scanStalled` is additive with omitempty, so a
// bridge with no stall emits nothing new and pre-existing clients
// decode exactly the bytes they did before. ProtocolVersion stays 1.
func TestHealth_ScanStalledOmittedWhenFalse(t *testing.T) {
	hs, _ := withManifest(t, &fakeManifestProvider{isScanning: true})
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "scanStalled") {
		t.Errorf("scanStalled must be omitted when false, got %s", raw)
	}
}
