package analyze

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// The filename helpers below are a deliberate twin of the unexported
// set in internal/transcode (safeVariantFilename / sanitiseForFAT /
// truncateUTF8AtMost / truncateUTF8FromEnd). Kept duplicated rather
// than shared because the only alternatives — exporting them from
// transcode or introducing a third import path for a ~90-line helper —
// aren't worth the coupling. **Mirror any future change across both
// copies.** The one divergence: this builds `<base>.waveform.bin`
// where transcode builds `<base>.<variantID>.flac`.

const (
	waveformExt       = ".waveform.bin"
	analysisTmpSuffix = ".tmp"
)

// safeAnalysisFilename builds the waveform sidecar basename for the
// source-path-mirrored layout: `<srcBase>.waveform.bin`, with srcBase
// optionally middle-truncated + SHA8-suffixed when the full filename
// would exceed 255 bytes (the ext4 / NTFS / exFAT / encrypted-overlay
// basename cap, minus the atomic-rename `.tmp` suffix). FAT-illegal
// characters (`: * ? " < > | \`) are deterministically replaced with
// `_`; any name that needed rewriting OR overflowed falls through to a
// raw-bytes SHA8 disambiguator so two distinct sources can never
// collide on disk.
func safeAnalysisFilename(srcBase string) string {
	// 255 minus the atomic-rename temp suffix RunAnalysis appends, so a
	// basename at the cap doesn't make the `<sidecar>.tmp` write target
	// overflow ENAMETOOLONG.
	const fsBasenameCap = 255 - len(analysisTmpSuffix)
	raw := srcBase
	sanitized := sanitiseForFAT(srcBase)

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
		// Pathological: the suffix alone consumes the budget. Fully
		// hash-named filename — loses the source-mirror property but
		// guarantees a valid name. Dead under realistic configs.
		return fmt.Sprintf("v.%s%s", sha8, suffix)
	}
	if len(sanitized) <= budget {
		return sanitized + suffix
	}
	// Middle-truncate: keep head + ".." + tail, UTF-8-safe.
	half := (budget - 2) / 2
	if half < 1 {
		return truncateUTF8AtMost(sanitized, budget) + suffix
	}
	head := truncateUTF8AtMost(sanitized, half)
	tail := truncateUTF8FromEnd(sanitized, half)
	return head + ".." + tail + suffix
}

// truncateUTF8AtMost returns the longest prefix of s whose byte length
// is ≤ maxBytes, ending on a rune boundary.
func truncateUTF8AtMost(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		end = i
	}
	return s[:end]
}

// truncateUTF8FromEnd returns the longest suffix of s whose byte length
// is ≤ maxBytes, starting on a rune boundary.
func truncateUTF8FromEnd(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s)
	for i := range s {
		if len(s)-i <= maxBytes {
			start = i
			break
		}
	}
	return s[start:]
}

// sanitiseForFAT replaces characters FAT-family filesystems reject
// (`: * ? " < > | \`) with `_`, deterministically. Lazy-allocates: a
// clean basename (the common case) returns s unchanged. Operates on a
// basename only — forward slash is already split off by the caller.
func sanitiseForFAT(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ':', '*', '?', '"', '<', '>', '|', '\\':
			out := []byte(s)
			out[i] = '_'
			for j := i + 1; j < len(out); j++ {
				switch out[j] {
				case ':', '*', '?', '"', '<', '>', '|', '\\':
					out[j] = '_'
				}
			}
			return string(out)
		}
	}
	return s
}
