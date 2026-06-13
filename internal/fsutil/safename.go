package fsutil

import "unicode/utf8"

// Filesystem-safe filename + UTF-8 string helpers shared by the sidecar
// producers (internal/transcode variant FLACs, internal/analyze waveform
// `.bin`s). These were duplicated in both packages; consolidated here so
// there's a single canonical copy.

// SanitiseForFAT replaces characters that FAT-family filesystems
// (FAT32 / exFAT / NTFS) reject in filenames with `_`, deterministically
// (so re-runs of the same source produce identical output — DB sidecar-
// path lookups depend on it). Operates on a single path SEGMENT
// (basename); forward slash isn't included because the caller has
// already split the path. Backslash IS included (cross-OS rip tools
// sometimes embed it in one segment).
//
// Lazy allocation: a clean basename (>95% of a typical library) is
// returned unchanged with zero allocations; only the first illegal byte
// materialises a mutable copy. The bad set is all ASCII, so the byte
// loop is safe on UTF-8 input (multi-byte runes start ≥ 0x80 and never
// collide with the ASCII bad chars).
func SanitiseForFAT(s string) string {
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

// TruncateUTF8AtMost returns the longest prefix of s whose byte length is
// ≤ maxBytes, ending on a rune boundary (never slices mid-rune).
func TruncateUTF8AtMost(s string, maxBytes int) string {
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

// TruncateUTF8FromEnd returns the longest suffix of s whose byte length
// is ≤ maxBytes, starting on a rune boundary.
func TruncateUTF8FromEnd(s string, maxBytes int) string {
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

// TrimPartialTrailingRune removes at most utf8.UTFMax-1 trailing bytes
// when a byte-slice cut split a multi-byte rune, in O(1). A string that
// still ends with genuinely invalid bytes after that is returned as-is
// (interior garbage is left for the JSON encoder to render as U+FFFD).
// Used when truncating subprocess stderr at a fixed byte budget.
func TrimPartialTrailingRune(s string) string {
	for i := 0; i < utf8.UTFMax-1 && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}
