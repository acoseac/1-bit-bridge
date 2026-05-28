package dlna

import (
	"encoding/binary"
	"net/http"
	"strconv"
)

// -----------------------------------------------------------------------------
// PCM silence asset for the post-pause decoder-reset flush
// -----------------------------------------------------------------------------

// SilenceWAVSampleRateHz / SilenceWAVBitsPerSample / SilenceWAVChannels /
// SilenceWAVDurationSeconds are the canonical PCM parameters for the
// silence asset served at `GET /dlna/silence.wav`. Surfaced as public
// constants so iOS's DIDL-Lite builder can populate matching
// `<res sampleFrequency>` / `bitsPerSample` / `nrAudioChannels` /
// `duration` attributes without hardcoding a second copy.
//
// 16-bit / 44.1 kHz / stereo / 1 second was chosen to:
//   - match the canonical "Red Book CD" PCM shape every renderer supports
//     natively (universal sink coverage — even minimal MPD builds accept it)
//   - keep the payload tiny (~176 KB) so the bridge can `embed` it via
//     a runtime generator without bloating the binary
//   - give the renderer enough time (1 s) to complete a DSD→PCM hardware
//     clock relock before we tear it down with the trailing `Stop`
const (
	SilenceWAVSampleRateHz    = 44100
	SilenceWAVBitsPerSample   = 16
	SilenceWAVChannels        = 2
	SilenceWAVDurationSeconds = 1.0
)

// SilenceWAVPath is the static URL path the silence-flush asset is
// mounted at on the bridge's DLNA HTTP server. iOS dispatches
// `SetAVTransportURI` against `<bridge-base>/dlna/silence.wav` as
// part of the post-pause decoder-reset chain. The path is constant
// (not parameterized by share / track) — there's only ever one
// silence asset per bridge instance.
const SilenceWAVPath = "/dlna/silence.wav"

// silenceWAVBytes builds the WAV payload from the parameters above. Done
// once at package init via `silenceWAVCache` so the actual handler hot
// path is just `w.Write(cached)`. Pure function so tests can verify the
// payload independently of the HTTP plumbing.
//
// **WAV header layout** (44 bytes, little-endian per the Microsoft
// RIFF spec):
//
//	"RIFF" + chunkSize(4)              — RIFF container
//	"WAVE"                              — format identifier
//	"fmt " + 16(4) + 1(2) + ch(2) + sr(4) + br(4) + ba(2) + bps(2)
//	                                    — fmt sub-chunk (PCM, 16 bytes)
//	"data" + dataSize(4) + <samples>    — data sub-chunk
//
// where `br = sr * ch * (bps/8)` (byte rate) and `ba = ch * (bps/8)`
// (block align). For 44.1 / 16 / 2: br=176400, ba=4. Sample payload is
// `sr * ch * (bps/8) * duration` zero bytes (true digital silence).
func silenceWAVBytes() []byte {
	sampleRate := uint32(SilenceWAVSampleRateHz)
	channels := uint16(SilenceWAVChannels)
	bitsPerSample := uint16(SilenceWAVBitsPerSample)
	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := uint32(float64(byteRate) * SilenceWAVDurationSeconds)
	// RIFF chunkSize is total file size minus the 8 bytes of the
	// RIFF header itself.
	riffChunkSize := uint32(36) + dataSize

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], riffChunkSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // fmt chunk size for PCM
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // audio format: 1 = PCM
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataSize)
	// Sample payload — `make` already zero-initialises the slice tail,
	// so the data region is true digital silence with no further work.
	return buf
}

// silenceWAVCache holds the pre-built payload so the hot path never
// re-allocates. Package-scope `var` initialised at first reference;
// `silenceWAVBytes()` is deterministic so the cached value is stable.
var silenceWAVCache = silenceWAVBytes()

// SilenceWAVHandler serves the pre-built silence WAV at
// `GET /dlna/silence.wav`. Used by iOS's post-pause decoder-reset
// flush (PR-pending Mirror-PR pair) — see `SilenceWAVPath` docblock.
//
// **Public + unauthed** by design, same posture as `/dlna/file/{trackID}`.
// The asset is non-sensitive (1 second of digital silence) and renderers
// fetch it without bearer auth, so requiring auth would break the
// silence-flush chain. CORS / OPTIONS / Range support is not implemented
// — renderers fetch the whole asset in a single GET (the 176 KB payload
// is below every renderer's HTTP buffer threshold).
//
// `Cache-Control: no-store` is deliberate so a renderer can't cache the
// silence asset across bridge restarts — the asset is generated at
// package init but a future PR that tweaks the duration / sample rate
// would otherwise leave clients serving the old shape.
func SilenceWAVHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(len(silenceWAVCache)))
		w.Header().Set("Cache-Control", "no-store")
		// `transferMode.dlna.org: Streaming` matches what `FileHandler`
		// sets for `/dlna/file/{trackID}` — strict renderers verify
		// this header for audio payloads even on short fetches.
		w.Header().Set("transferMode.dlna.org", "Streaming")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(silenceWAVCache)
	}
}
