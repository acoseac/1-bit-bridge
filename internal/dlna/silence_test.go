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

// -----------------------------------------------------------------------------
// Rate-matched DSD silence assets
// -----------------------------------------------------------------------------

// Test_SilenceDSF_HeaderShape pins the DSF container header layout for all
// three rates. A regression that drifts the chunk magic / size fields / fmt
// fields would surface here before any renderer rejects a malformed asset.
//
// DSF layout (Sony spec, all multi-byte fields little-endian):
//
//	[0..28)   DSD chunk: "DSD " + chunkSize(8) + fileSize(8) + metaOffset(8)
//	[28..80)  fmt chunk: "fmt " + chunkSize(8) + ...fmt fields...
//	[80..92)  data header: "data" + chunkSize(8)
//	[92..)    audio data
func Test_SilenceDSF_HeaderShape(t *testing.T) {
	cases := []struct {
		name string
		rate DSDRate
	}{
		{"DSD64", DSDRate64},
		{"DSD128", DSDRate128},
		{"DSD256", DSDRate256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := silenceDSFBytes(tc.rate)
			if len(buf) < 92 {
				t.Fatalf("payload shorter than 92-byte header: got %d", len(buf))
			}
			// Chunk magic bytes.
			if string(buf[0:4]) != "DSD " {
				t.Errorf("DSD chunk magic: got %q, want %q", buf[0:4], "DSD ")
			}
			if string(buf[28:32]) != "fmt " {
				t.Errorf("fmt chunk magic: got %q, want %q", buf[28:32], "fmt ")
			}
			if string(buf[80:84]) != "data" {
				t.Errorf("data chunk magic: got %q, want %q", buf[80:84], "data")
			}
			// Chunk sizes.
			if got := binary.LittleEndian.Uint64(buf[4:12]); got != 28 {
				t.Errorf("DSD chunk size: got %d, want 28", got)
			}
			if got := binary.LittleEndian.Uint64(buf[32:40]); got != 52 {
				t.Errorf("fmt chunk size: got %d, want 52", got)
			}
			// Total file size in DSD chunk must match actual buffer length.
			if got := binary.LittleEndian.Uint64(buf[12:20]); got != uint64(len(buf)) {
				t.Errorf("file size header: got %d, want %d", got, len(buf))
			}
			// Metadata offset = 0 (no ID3 tag).
			if got := binary.LittleEndian.Uint64(buf[20:28]); got != 0 {
				t.Errorf("metadata offset: got %d, want 0", got)
			}
			// fmt fields.
			if got := binary.LittleEndian.Uint32(buf[40:44]); got != 1 {
				t.Errorf("format version: got %d, want 1", got)
			}
			if got := binary.LittleEndian.Uint32(buf[44:48]); got != 0 {
				t.Errorf("format ID: got %d, want 0 (DSD raw)", got)
			}
			if got := binary.LittleEndian.Uint32(buf[48:52]); got != 2 {
				t.Errorf("channel type: got %d, want 2 (stereo L/R)", got)
			}
			if got := binary.LittleEndian.Uint32(buf[52:56]); got != 2 {
				t.Errorf("channel num: got %d, want 2", got)
			}
			if got := binary.LittleEndian.Uint32(buf[56:60]); got != uint32(tc.rate) {
				t.Errorf("sample rate: got %d, want %d", got, tc.rate)
			}
			if got := binary.LittleEndian.Uint32(buf[60:64]); got != 1 {
				t.Errorf("bits per sample: got %d, want 1 (DSD)", got)
			}
			if got := binary.LittleEndian.Uint32(buf[72:76]); got != 4096 {
				t.Errorf("block size: got %d, want 4096", got)
			}
			if got := binary.LittleEndian.Uint32(buf[76:80]); got != 0 {
				t.Errorf("reserved: got %d, want 0", got)
			}
			// data chunk size includes its own 12-byte header per Sony spec.
			audioBytes := uint64(len(buf) - 92)
			wantDataSize := audioBytes + 12
			if got := binary.LittleEndian.Uint64(buf[84:92]); got != wantDataSize {
				t.Errorf("data chunk size: got %d, want %d (audioBytes %d + 12 header)",
					got, wantDataSize, audioBytes)
			}
		})
	}
}

// Test_SilenceDSF_AudioIsDSD_DigitalSilence pins that every byte of the
// audio data region is the LSB-first DSD silence pattern (0x69). 0x69
// integrates to 50% bit density → analog 0 V on a 1-bit PDM DAC; this is
// THE load-bearing audio property of these assets. A regression that
// seeds the buffer with 0x00 (PCM silence) would feed a DC offset into
// the Chord pulse-array DAC — the exact ring this fix is preventing.
func Test_SilenceDSF_AudioIsDSD_DigitalSilence(t *testing.T) {
	for _, rate := range []DSDRate{DSDRate64, DSDRate128, DSDRate256} {
		buf := silenceDSFBytes(rate)
		for i := 92; i < len(buf); i++ {
			if buf[i] != silenceDSFByte {
				t.Fatalf("rate %d: non-0x69 audio byte at offset %d: 0x%02X (DSD silence must be 0x69 LSB-first)",
					rate, i, buf[i])
			}
		}
	}
}

// Test_SilenceDSF_AudioBlockAligned pins that the audio data region is a
// whole multiple of the DSF block size (4096) per channel — the format
// requires it, partial blocks are forbidden.
func Test_SilenceDSF_AudioBlockAligned(t *testing.T) {
	for _, rate := range []DSDRate{DSDRate64, DSDRate128, DSDRate256} {
		buf := silenceDSFBytes(rate)
		audioBytes := len(buf) - 92
		bytesPerChannel := audioBytes / 2
		if bytesPerChannel%silenceDSFBlockSize != 0 {
			t.Errorf("rate %d: bytesPerChannel %d not a multiple of block size %d",
				rate, bytesPerChannel, silenceDSFBlockSize)
		}
		if audioBytes%(silenceDSFBlockSize*2) != 0 {
			t.Errorf("rate %d: audio bytes %d not stereo-block-aligned",
				rate, audioBytes)
		}
	}
}

// Test_SilenceDSF_SampleCountMatchesPayload pins the fmt chunk's
// sampleCount field — it's the total 1-bit samples per channel (NOT bytes).
// Easy to fumble (8× factor); pin it.
func Test_SilenceDSF_SampleCountMatchesPayload(t *testing.T) {
	for _, rate := range []DSDRate{DSDRate64, DSDRate128, DSDRate256} {
		buf := silenceDSFBytes(rate)
		audioBytes := uint64(len(buf) - 92)
		bytesPerChannel := audioBytes / 2
		wantSamples := bytesPerChannel * 8
		got := binary.LittleEndian.Uint64(buf[64:72])
		if got != wantSamples {
			t.Errorf("rate %d: sampleCount = %d, want %d (bytesPerChannel %d × 8)",
				rate, got, wantSamples, bytesPerChannel)
		}
	}
}

// Test_SilenceDSFHandler_GETReturnsPayload pins the happy-path GET for
// each rate: 200 OK, audio/x-dsf MIME, Content-Length matches the cache,
// body starts with the DSF magic.
func Test_SilenceDSFHandler_GETReturnsPayload(t *testing.T) {
	cases := []struct {
		name string
		path string
		rate DSDRate
	}{
		{"DSD64", SilenceDSF64Path, DSDRate64},
		{"DSD128", SilenceDSF128Path, DSDRate128},
		{"DSD256", SilenceDSF256Path, DSDRate256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := SilenceDSFHandler(tc.rate)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "audio/x-dsf" {
				t.Errorf("Content-Type = %q, want audio/x-dsf", ct)
			}
			if tm := rec.Header().Get("transferMode.dlna.org"); tm != "Streaming" {
				t.Errorf("transferMode.dlna.org = %q, want Streaming", tm)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			body := rec.Body.Bytes()
			if len(body) < 4 {
				t.Errorf("body shorter than 4 bytes: got %d, want DSD magic prefix", len(body))
			} else if string(body[:4]) != "DSD " {
				t.Errorf("body does not start with DSD magic; first 4 bytes = %q", body[:4])
			}
			wantLen := strconv.Itoa(len(body))
			if cl := rec.Header().Get("Content-Length"); cl != wantLen {
				t.Errorf("Content-Length = %q, want %q", cl, wantLen)
			}
		})
	}
}

// Test_SilenceDSFHandler_HEADReturnsZeroLength pins HEAD-request handling.
func Test_SilenceDSFHandler_HEADReturnsZeroLength(t *testing.T) {
	h := SilenceDSFHandler(DSDRate256)
	req := httptest.NewRequest(http.MethodHead, SilenceDSF256Path, nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/x-dsf" {
		t.Errorf("Content-Type = %q, want audio/x-dsf", ct)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
}

// Test_SilenceDSFHandler_RejectsPOST pins 405 for non-GET/HEAD methods,
// AND that the response carries the `Allow: GET, HEAD` header per
// RFC 7231 §6.5.5 (a 405 MUST list supported methods). Per Gemini
// Medium on PR #347.
func Test_SilenceDSFHandler_RejectsPOST(t *testing.T) {
	h := SilenceDSFHandler(DSDRate64)
	req := httptest.NewRequest(http.MethodPost, SilenceDSF64Path, nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want %q (RFC 7231 §6.5.5)", got, "GET, HEAD")
	}
}

// Test_SilenceDSFHandler_UnknownRate404s pins the defensive fallback for
// an unsupported rate — the only production call sites pass the three
// constants above, but a future caller error shouldn't crash.
func Test_SilenceDSFHandler_UnknownRate404s(t *testing.T) {
	h := SilenceDSFHandler(DSDRate(99999))
	req := httptest.NewRequest(http.MethodGet, "/dlna/silence-dsd99999.dsf", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown rate status = %d, want 404", rec.Code)
	}
}

// Test_SilenceDSF_PathConstantsStable pins the URL paths against
// regressions — iOS Mirror-PR dispatches against these exact paths;
// a rename without coordinating iOS would silently 404 every dispatch.
func Test_SilenceDSF_PathConstantsStable(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{SilenceDSF64Path, "/dlna/silence-dsd64.dsf"},
		{SilenceDSF128Path, "/dlna/silence-dsd128.dsf"},
		{SilenceDSF256Path, "/dlna/silence-dsd256.dsf"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("path constant: got %q, want %q (iOS Mirror-PR depends on this exact path)",
				tc.got, tc.want)
		}
	}
}

// Test_SilenceDSF_RateConstantsMatchSpec pins the rate values to the DSD
// spec (N × 44.1 kHz). A drift would mean the bridge serves a file whose
// fmt-chunk rate header doesn't match the URL path's claimed rate → iOS
// dispatches to the wrong-rate file → mode switch on the renderer → ring.
func Test_SilenceDSF_RateConstantsMatchSpec(t *testing.T) {
	const baseRate = 44100
	cases := []struct {
		name     string
		got      DSDRate
		multiple int
	}{
		{"DSD64", DSDRate64, 64},
		{"DSD128", DSDRate128, 128},
		{"DSD256", DSDRate256, 256},
	}
	for _, tc := range cases {
		want := DSDRate(tc.multiple * baseRate)
		if tc.got != want {
			t.Errorf("%s: got %d, want %d (= %d × %d Hz)",
				tc.name, tc.got, want, tc.multiple, baseRate)
		}
	}
}
