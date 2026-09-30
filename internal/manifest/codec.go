// Codec vocabulary helpers shared across packages. Track.Codec is the
// scanner-stamped canonical upper-case codec string (FLAC / ALAC / WAV
// / AIFF / AAC / MP3 / OGG / OPUS / WMA / DSF / DFF; the compressed
// AIFF-C and WAV encodings ULAW / ALAW / IMA4 / ADPCM / GSM / MP2 and
// "AIFC", an AIFF-C whose compression the bridge does not know, since
// ExtractorVersion 21; "" for legacy pre-codec rows and unreadable
// containers) — this file owns the predicates over that vocabulary so
// consumers can't drift.
package manifest

import "strings"

// IsLossyCodec reports whether codec identifies a LOSSY encode
// (MP3 / AAC / OGG / OPUS / WMA, and since ExtractorVersion 21 the
// compressed AIFF-C and WAV encodings: G.711 ULAW / ALAW, IMA4 and
// ADPCM, GSM, MP2; backlog B124, B154). Case-insensitive and whitespace-
// tolerant, matching the scanner's stamping conventions. "AIFC" (a
// compression the bridge does not know) is not in it: the extractor
// gives such a row no depth, which keeps it out of upscaling anyway.
//
// This is the single source of truth for the upscale lossy gate —
// transcode.Coordinator.Submit's candidate walk, the cmd/bridge
// single-track enqueuer, the admin projection walk, and the admin
// tile badge all call it, and upscaleEligibleSQL in eligibility.go
// carries the same set as a SQL NOT IN mirror (change both
// together; the admin lockstep tests pin the agreement). Upscaling a
// lossy source adds no fidelity — sox would just resample decoded
// lossy audio into a FLAC several times the size — and PROTOCOL.md
// has always documented /v1/upscale's eligibility gate as "PCM".
//
// An EMPTY codec returns false — deliberately, and UNLIKE the
// extractors' canSetBitsPerSample ALLOWLIST (see extractors.go):
// that gate answers "does BitsPerSample carry meaning?" where
// failing open on "" re-admits a container-width regression, while
// THIS gate answers "is upscaling pointless?" where codec-unknown-
// but-geometry-known legacy rows must stay eligible (the pre-gate
// behavior; the rate/bits geometry gate still protects them).
func IsLossyCodec(codec string) bool {
	switch strings.ToUpper(strings.TrimSpace(codec)) {
	case "MP3", "AAC", "OGG", "OPUS", "WMA", "ULAW", "ALAW", "IMA4", "ADPCM", "GSM", "MP2":
		return true
	}
	return false
}
