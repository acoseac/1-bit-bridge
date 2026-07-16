// Codec vocabulary helpers shared across packages. Track.Codec is the
// scanner-stamped canonical upper-case codec string (FLAC / ALAC / WAV
// / AIFF / AAC / MP3 / OGG / OPUS / WMA / DSF / DFF; "" for legacy
// pre-codec rows and unreadable containers) — this file owns the
// predicates over that vocabulary so consumers can't drift.
package manifest

import "strings"

// IsLossyCodec reports whether codec identifies a LOSSY encode
// (MP3 / AAC / OGG / OPUS / WMA). Case-insensitive and whitespace-
// tolerant, matching the scanner's stamping conventions.
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
	case "MP3", "AAC", "OGG", "OPUS", "WMA":
		return true
	}
	return false
}
