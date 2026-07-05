package analyze

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

const (
	waveformExt       = ".waveform.bin"
	analysisTmpSuffix = ".tmp"
)

// safeAnalysisFilename builds the waveform sidecar basename for the
// source-path-mirrored layout: `<srcBase>.waveform.bin`, with srcBase
// optionally middle-truncated + SHA8-suffixed when the full filename
// would exceed 255 bytes (the ext4 / NTFS / exFAT / encrypted-overlay
// basename cap, minus the atomic-rename `.tmp` suffix). FAT-illegal
// characters are deterministically replaced with `_`; any name that
// needed rewriting OR overflowed falls through to a raw-bytes SHA8
// disambiguator so two distinct sources can never collide on disk.
//
// The same shape as internal/transcode's `safeVariantFilename`; the
// FAT-sanitize + UTF-8-truncate primitives are shared via internal/fsutil.
func safeAnalysisFilename(srcBase string) string {
	// 255 minus the atomic-rename temp suffix RunAnalysis appends, so a
	// basename at the cap doesn't make the `<sidecar>.tmp` write target
	// overflow ENAMETOOLONG.
	const fsBasenameCap = 255 - len(analysisTmpSuffix)
	raw := srcBase
	sanitized := fsutil.SanitiseForFAT(srcBase)

	// Clean path: name didn't trip FAT sanitization AND fits the cap.
	// The `sanitized == raw` clause ensures any name that DID get
	// rewritten falls through to the SHA8 path, so two raw inputs that
	// sanitize identically (`Track:A` + `Track*A`) stay distinct.
	if sanitized == raw {
		candidate := sanitized + waveformExt
		if len(candidate) <= fsBasenameCap {
			return candidate
		}
	}
	// Disambiguation path — hash the RAW (pre-sanitization) bytes.
	sum := sha256.Sum256([]byte(raw))
	sha8 := hex.EncodeToString(sum[:])[:8]
	suffix := fmt.Sprintf("~%s%s", sha8, waveformExt)
	budget := fsBasenameCap - len(suffix)
	if budget < 8 {
		// Pathological: the suffix alone consumes the budget. Emit a
		// fully-hashed name — `suffix` already carries the extension, so
		// use waveformExt (not `suffix`) to avoid a doubled `~<sha8>`.
		// Matches the transcode safeVariantFilename twin.
		return fmt.Sprintf("v.%s%s", sha8, waveformExt)
	}
	if len(sanitized) <= budget {
		return sanitized + suffix
	}
	// Middle-truncate: keep head + ".." + tail, UTF-8-safe.
	half := (budget - 2) / 2
	if half < 1 {
		return fsutil.TruncateUTF8AtMost(sanitized, budget) + suffix
	}
	// On an odd budget, give the leftover byte to the head so
	// `head + ".." + tail` uses the full budget instead of dropping a
	// byte of naming context. Matches the transcode safeVariantFilename twin.
	head := fsutil.TruncateUTF8AtMost(sanitized, budget-2-half)
	tail := fsutil.TruncateUTF8FromEnd(sanitized, half)
	return head + ".." + tail + suffix
}
