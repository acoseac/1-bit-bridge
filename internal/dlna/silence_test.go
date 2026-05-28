package dlna

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Test_SilenceWAV_HeaderShape pins the 44-byte canonical WAV header
// layout against the constants the handler advertises. A regression
// that drifts e.g. the channel count constant out of sync with the
// header `channels` field would surface here before any renderer
// gets a malformed asset.
func Test_SilenceWAV_HeaderShape(t *testing.T) {
	buf := silenceWAVBytes()
	if len(buf) < 44 {
		t.Fatalf("WAV payload shorter than 44-byte header: got %d", len(buf))
	}
	cases := []struct {
		name string
		want string
		at   int
	}{
		{"RIFF magic", "RIFF", 0},
		{"WAVE format", "WAVE", 8},
		{"fmt sub-chunk", "fmt ", 12},
		{"data sub-chunk", "data", 36},
	}
	for _, tc := range cases {
		got := string(buf[tc.at : tc.at+len(tc.want)])
		if got != tc.want {
			t.Errorf("%s at byte %d: got %q, want %q", tc.name, tc.at, got, tc.want)
		}
	}

	wantChannels := uint16(SilenceWAVChannels)
	if got := binary.LittleEndian.Uint16(buf[22:24]); got != wantChannels {
		t.Errorf("channels: got %d, want %d", got, wantChannels)
	}
	wantSR := uint32(SilenceWAVSampleRateHz)
	if got := binary.LittleEndian.Uint32(buf[24:28]); got != wantSR {
		t.Errorf("sampleRate: got %d, want %d", got, wantSR)
	}
	wantBPS := uint16(SilenceWAVBitsPerSample)
	if got := binary.LittleEndian.Uint16(buf[34:36]); got != wantBPS {
		t.Errorf("bitsPerSample: got %d, want %d", got, wantBPS)
	}
}

// Test_SilenceWAV_DataIsZero pins the actual sample payload as digital
// silence — every byte after the 44-byte header is 0x00. A regression
// that accidentally seeds the buffer with non-zero data (e.g. a dev
// who hard-codes a test tone for debugging and forgets to revert)
// would surface here.
func Test_SilenceWAV_DataIsZero(t *testing.T) {
	buf := silenceWAVBytes()
	for i := 44; i < len(buf); i++ {
		if buf[i] != 0 {
			t.Fatalf("non-zero sample byte at offset %d: 0x%02X (silence asset must be true digital zero)", i, buf[i])
		}
	}
}

// Test_SilenceWAV_PayloadSizeMatchesParams pins the total payload length
// against the duration / rate / depth / channel parameters. Catches a
// duration-constant tweak that forgets to re-derive the data-size header
// field (the renderer would read the WAV header's dataSize and either
// truncate playback or wait for "missing" bytes that never come).
func Test_SilenceWAV_PayloadSizeMatchesParams(t *testing.T) {
	buf := silenceWAVBytes()
	sampleRate := uint32(SilenceWAVSampleRateHz)
	channels := uint32(SilenceWAVChannels)
	bytesPerSample := uint32(SilenceWAVBitsPerSample) / 8
	wantData := uint32(float64(sampleRate*channels*bytesPerSample) * SilenceWAVDurationSeconds)
	wantTotal := 44 + int(wantData)
	if len(buf) != wantTotal {
		t.Errorf("total payload size: got %d, want %d (44 header + %d data)", len(buf), wantTotal, wantData)
	}
	gotData := binary.LittleEndian.Uint32(buf[40:44])
	if gotData != wantData {
		t.Errorf("data sub-chunk size: got %d, want %d", gotData, wantData)
	}
}

// Test_SilenceWAVHandler_GETReturnsPayload pins the happy-path GET:
// 200 OK, correct Content-Type, Content-Length matches actual payload,
// transferMode header present (some strict renderers verify it on
// audio payloads).
func Test_SilenceWAVHandler_GETReturnsPayload(t *testing.T) {
	h := SilenceWAVHandler()
	req := httptest.NewRequest(http.MethodGet, SilenceWAVPath, nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", ct)
	}
	wantLen := strconv.Itoa(len(silenceWAVCache))
	if cl := rec.Header().Get("Content-Length"); cl != wantLen {
		t.Errorf("Content-Length = %q, want %q", cl, wantLen)
	}
	if tm := rec.Header().Get("transferMode.dlna.org"); tm != "Streaming" {
		t.Errorf("transferMode.dlna.org = %q, want Streaming", tm)
	}
	// Cache-Control pin (PR #311 round-1 CodeRabbit): the handler
	// deliberately sets `no-store` so a renderer can't cache the
	// silence asset across bridge restarts. A future tweak to the
	// silence parameters (duration / sample rate) requires the
	// cached payload to invalidate — pin the header here so a
	// regression doesn't silently drop the no-store.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rec.Body.Len() != len(silenceWAVCache) {
		t.Errorf("body length = %d, want %d", rec.Body.Len(), len(silenceWAVCache))
	}
	// Sanity: first 4 bytes must be "RIFF" (matches the header test).
	// `len(got) < 4` short-circuit avoids a panic on `got[:4]` if a
	// future regression returns a truncated body. Per CodeRabbit
	// PR #311 round-1.
	got := rec.Body.Bytes()
	if len(got) < 4 {
		t.Errorf("body shorter than 4 bytes: got %d bytes, want RIFF prefix", len(got))
	} else if string(got[:4]) != "RIFF" {
		t.Errorf("body does not start with RIFF magic; first 4 bytes = %q", string(got[:4]))
	}
}

// Test_SilenceWAVHandler_HEADReturnsZeroLengthWithCorrectHeaders pins
// HEAD-request handling: same headers as GET (so strict renderers can
// probe Content-Length before deciding to fetch) but no body.
func Test_SilenceWAVHandler_HEADReturnsZeroLengthWithCorrectHeaders(t *testing.T) {
	h := SilenceWAVHandler()
	req := httptest.NewRequest(http.MethodHead, SilenceWAVPath, nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", ct)
	}
	wantLen := strconv.Itoa(len(silenceWAVCache))
	if cl := rec.Header().Get("Content-Length"); cl != wantLen {
		t.Errorf("Content-Length = %q, want %q", cl, wantLen)
	}
	// Strict renderers probing via HEAD before fetching expect the
	// same headers as the GET response — pin them here too. Per
	// CodeRabbit PR #311 round-1.
	if tm := rec.Header().Get("transferMode.dlna.org"); tm != "Streaming" {
		t.Errorf("transferMode.dlna.org = %q, want Streaming", tm)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
}

// Test_SilenceWAVHandler_RejectsPOST pins that non-GET/HEAD methods
// return 405 Method Not Allowed. The asset is read-only by definition;
// a misbehaving client that tries to POST to upload silence is a
// configuration error worth surfacing rather than silently 200-OKing.
func Test_SilenceWAVHandler_RejectsPOST(t *testing.T) {
	h := SilenceWAVHandler()
	req := httptest.NewRequest(http.MethodPost, SilenceWAVPath, nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

// Test_SilenceWAV_PathConstantStable pins the URL path string against
// regressions. The iOS-side Mirror PR dispatches against this exact
// path; a rename here without coordinating the iOS side would silently
// 404 every silence-flush dispatch.
func Test_SilenceWAV_PathConstantStable(t *testing.T) {
	if SilenceWAVPath != "/dlna/silence.wav" {
		t.Errorf("SilenceWAVPath = %q, want /dlna/silence.wav (iOS Mirror-PR depends on this exact path)", SilenceWAVPath)
	}
}
