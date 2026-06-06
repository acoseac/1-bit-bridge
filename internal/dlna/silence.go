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

// -----------------------------------------------------------------------------
// Rate-matched DSD silence assets (DSF) for the DSD pause flush
// -----------------------------------------------------------------------------
//
// Mirror-PR pair with iOS (PR-pending). The PCM `silence.wav` flush above forces
// a DSD→PCM hardware mode switch on the renderer, which on the Chord 2Go +
// Hugo 2 at DSD256 triggers digital-filter limit-cycle oscillation in the
// FPGA pulse-array pipeline (the persistent high-pitched ring users hear on
// pause). Rate-matched DSF silence is the JRiver/HQPlayer approach — keep the
// renderer locked in DSD-N mode by dispatching a pure-DSD silence track at
// the same rate, so the FPGA accumulators drain cleanly while staying at
// their native operating frequency. Once 1.5 s of native DSD silence has
// played, a trailing Stop is benign (FPGA state already zeroed).
//
// Three rates served — covers the canonical DSD family (DSD64 / DSD128 /
// DSD256). DSD512 is out of scope (vanishingly rare in the wild and the 2Go
// itself doesn't accept it).
//
// **DSD silence byte pattern**: `0x69` (binary 01101001). DSF is LSB-first
// per byte (per the iOS-side `DSFPrebufferingSource` invariant); LSB-first
// `0x69` integrates to ~50% bit density across many bytes, which is true
// digital zero in 1-bit PDM — the analog output drives to 0 V naturally.
// `0x96` would be the MSB-first equivalent; we serve LSB-first per the DSF
// container's native bit order.

// DSDRate represents a DSF audio data rate in Hz. Stable string-form keys for
// the URL path / Content-Disposition come from the .pathSuffix() method.
type DSDRate uint32

const (
	// DSDRate64 = 64 × 44.1 kHz = 2,822,400 Hz (Single-rate DSD, SACD native).
	DSDRate64 DSDRate = 2822400
	// DSDRate128 = 128 × 44.1 kHz = 5,644,800 Hz (Double-rate DSD).
	DSDRate128 DSDRate = 5644800
	// DSDRate256 = 256 × 44.1 kHz = 11,289,600 Hz (Quad-rate DSD).
	DSDRate256 DSDRate = 11289600
)

// SilenceDSFDurationSeconds — approximate playback duration of each DSF
// silence asset. Rounded up to the nearest whole 4 KB DSF block per channel
// (the format requires block-aligned data), so the actual file is ~2.00 s.
// 2 s gives the iOS-side dispatch ample headroom for the 1.5 s wait between
// Play and the trailing Stop (which matches the PCM flush's existing tuning).
const SilenceDSFDurationSeconds = 2.0

// silenceDSFBlockSize is the DSF spec's canonical per-channel block size
// (4096 bytes). The audio data section MUST be a whole multiple of this
// per channel; partial blocks are forbidden by the format.
const silenceDSFBlockSize = 4096

// silenceDSFByte is the silence byte repeated throughout the audio data
// region. See the package-level docblock above for why `0x69` is correct
// for LSB-first DSF.
const silenceDSFByte byte = 0x69

// SilenceDSF64Path / SilenceDSF128Path / SilenceDSF256Path are the static
// URL paths the rate-matched DSF silence assets are mounted at. iOS resolves
// the right one per-pause based on the active DSD rate.
const (
	SilenceDSF64Path  = "/dlna/silence-dsd64.dsf"
	SilenceDSF128Path = "/dlna/silence-dsd128.dsf"
	SilenceDSF256Path = "/dlna/silence-dsd256.dsf"
)

// silenceDSFBytes builds a stereo DSF silence payload at the given DSD rate.
// Pure function so tests can verify byte-for-byte without touching HTTP.
//
// **DSF container layout** (per Sony DSF spec, all multi-byte fields
// little-endian):
//
//	DSD chunk (28 bytes):
//	  "DSD " (4) + chunk size 28 (8) + total file size (8) + metadata offset (8)
//	fmt chunk (52 bytes):
//	  "fmt " (4) + chunk size 52 (8) + format version 1 (4) + format ID 0 (4)
//	  + channel type 2 (4, = stereo L/R) + channel num 2 (4) + sample rate (4)
//	  + bits per sample 1 (4) + sample count (8, = total DSD samples per channel
//	  not bytes!) + block size 4096 (4) + reserved 0 (4)
//	data chunk (12 + payload):
//	  "data" (4) + chunk size (8, INCLUDES the 12-byte header per Sony spec)
//	  + audio data
//
// Audio data is block-interleaved: 4096 bytes left, 4096 bytes right,
// repeat. Each block is `silenceDSFByte` (0x69) × 4096.
//
// Metadata offset = 0 (no ID3 tag, as documented by the DSF spec for the
// "no metadata" case).
func silenceDSFBytes(rate DSDRate) []byte {
	// Bytes per second per channel = rate (Hz, which equals bits per second
	// for 1-bit DSD) / 8 bits-per-byte.
	bytesPerSecondPerChannel := uint32(rate) / 8
	// Round UP to the next whole 4 KB block so we satisfy the DSF
	// block-alignment requirement without truncating below the target
	// duration. (Rounding down would EOF before the iOS dispatch's 1.5 s
	// wait window completes.)
	targetBytesPerChannel := uint32(float64(bytesPerSecondPerChannel) * SilenceDSFDurationSeconds)
	blocksPerChannel := (targetBytesPerChannel + silenceDSFBlockSize - 1) / silenceDSFBlockSize
	bytesPerChannel := blocksPerChannel * silenceDSFBlockSize
	const channelCount = 2
	audioBytes := bytesPerChannel * channelCount
	// `data` chunk size INCLUDES its own 12-byte header per the Sony DSF
	// spec — this trips up implementers; if a downstream consumer reads
	// chunk_size - 12 as the audio length, that's correct.
	const dataChunkHeader = 12
	dataChunkSize := uint64(audioBytes) + dataChunkHeader
	const dsdChunkSize = 28
	const fmtChunkSize = 52
	totalFileSize := uint64(dsdChunkSize + fmtChunkSize + dataChunkHeader + audioBytes)
	// Sample count = total 1-bit DSD samples PER CHANNEL (NOT bytes).
	sampleCountPerChannel := uint64(bytesPerChannel) * 8

	buf := make([]byte, totalFileSize)

	// --- DSD chunk (28 bytes) ---
	copy(buf[0:4], "DSD ")
	binary.LittleEndian.PutUint64(buf[4:12], dsdChunkSize)
	binary.LittleEndian.PutUint64(buf[12:20], totalFileSize)
	binary.LittleEndian.PutUint64(buf[20:28], 0) // metadata offset 0 = no ID3 tag

	// --- fmt chunk (52 bytes) ---
	copy(buf[28:32], "fmt ")
	binary.LittleEndian.PutUint64(buf[32:40], fmtChunkSize)
	binary.LittleEndian.PutUint32(buf[40:44], 1) // format version
	binary.LittleEndian.PutUint32(buf[44:48], 0) // format ID (0 = DSD raw)
	binary.LittleEndian.PutUint32(buf[48:52], 2) // channel type (2 = stereo L/R)
	binary.LittleEndian.PutUint32(buf[52:56], channelCount)
	binary.LittleEndian.PutUint32(buf[56:60], uint32(rate))
	binary.LittleEndian.PutUint32(buf[60:64], 1) // bits per sample (DSD = 1)
	binary.LittleEndian.PutUint64(buf[64:72], sampleCountPerChannel)
	binary.LittleEndian.PutUint32(buf[72:76], silenceDSFBlockSize)
	binary.LittleEndian.PutUint32(buf[76:80], 0) // reserved

	// --- data chunk header (12 bytes) ---
	copy(buf[80:84], "data")
	binary.LittleEndian.PutUint64(buf[84:92], dataChunkSize)

	// --- audio data (all 0x69, true DSD digital silence) ---
	// `make` zero-fills, so loop over the audio slice to set 0x69.
	audio := buf[92:]
	for i := range audio {
		audio[i] = silenceDSFByte
	}
	return buf
}

// silenceDSFCache holds pre-built per-rate payloads. Built at package init;
// total cached footprint ~9 MB (1.4 + 2.7 + 5.4 MB). Acceptable on a bridge
// process where the user is typically streaming gigabytes through it.
var (
	silenceDSF64Cache  = silenceDSFBytes(DSDRate64)
	silenceDSF128Cache = silenceDSFBytes(DSDRate128)
	silenceDSF256Cache = silenceDSFBytes(DSDRate256)
)

// SilenceDSFHandler serves the pre-built rate-matched DSF silence for a
// given rate at the matching URL path. Same posture as SilenceWAVHandler:
// public + unauthed (renderers can't speak the bearer-token scheme), no
// Range support (renderers fetch the whole short asset in one GET).
//
// **MIME**: `audio/x-dsf` is the canonical DSF content type and what the
// Chord 2Go advertises in its `GetProtocolInfo` Sink list (confirmed via
// direct SSDP probe 2026-05-28). Strict renderers cross-check this against
// the DIDL `<res protocolInfo>` attribute iOS sends in SetAVTransportURI.
func SilenceDSFHandler(rate DSDRate) http.HandlerFunc {
	var payload []byte
	switch rate {
	case DSDRate64:
		payload = silenceDSF64Cache
	case DSDRate128:
		payload = silenceDSF128Cache
	case DSDRate256:
		payload = silenceDSF256Cache
	default:
		// Unsupported rate — return a handler that 404s. Defensive; the
		// only call sites pass one of the three constants above.
		return func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}
	}
	contentLen := strconv.Itoa(len(payload))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "audio/x-dsf")
		w.Header().Set("Content-Length", contentLen)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("transferMode.dlna.org", "Streaming")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(payload)
	}
}
