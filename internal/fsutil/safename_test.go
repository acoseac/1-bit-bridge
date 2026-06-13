package fsutil

import (
	"strings"
	"testing"
)

// sanitiseForFATSink defeats dead-store elimination so the alloc guard
// below actually exercises SanitiseForFAT.
var sanitiseForFATSink string

// TestSanitiseForFATCleanInputZeroAlloc pins the fast-path: a basename
// with no FAT-illegal bytes returns the original string with ZERO heap
// allocations (the `sanitized == raw` fast-path in the sidecar filename
// builders depends on it across a 50k-track scan). Moved here from
// internal/transcode when the helper was consolidated.
func TestSanitiseForFATCleanInputZeroAlloc(t *testing.T) {
	const clean = "01 Love Letters - Diana Krall.flac"
	if got := SanitiseForFAT(clean); got != clean {
		t.Fatalf("clean input mutated: got %q want %q", got, clean)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		sanitiseForFATSink = SanitiseForFAT(clean)
	}); allocs != 0 {
		t.Errorf("SanitiseForFAT(clean) allocated %.0f times, want 0", allocs)
	}
	// Dirty input substitutes every FAT-illegal byte with '_'.
	if got := SanitiseForFAT(`a:b*c?.flac`); got != "a_b_c_.flac" {
		t.Errorf("dirty input: got %q want %q", got, "a_b_c_.flac")
	}
	// Backslash is in the bad set; multi-byte runes are untouched.
	if got := SanitiseForFAT(`Dvořák<1>.flac`); got != "Dvořák_1_.flac" {
		t.Errorf("mixed input: got %q", got)
	}
}

func TestTruncateUTF8Helpers(t *testing.T) {
	s := strings.Repeat("Dvořák ", 20) // multi-byte
	for _, max := range []int{0, 1, 3, 7, 50, len(s), len(s) + 10} {
		head := TruncateUTF8AtMost(s, max)
		if len(head) > max && max >= 0 {
			t.Errorf("AtMost(%d) len=%d > max", max, len(head))
		}
		if !strings.HasPrefix(s, head) {
			t.Errorf("AtMost(%d)=%q not a prefix", max, head)
		}
		if strings.ContainsRune(head, '�') {
			t.Errorf("AtMost(%d)=%q sliced mid-rune", max, head)
		}
		tail := TruncateUTF8FromEnd(s, max)
		if len(tail) > max && max >= 0 {
			t.Errorf("FromEnd(%d) len=%d > max", max, len(tail))
		}
		if !strings.HasSuffix(s, tail) {
			t.Errorf("FromEnd(%d)=%q not a suffix", max, tail)
		}
		if strings.ContainsRune(tail, '�') {
			t.Errorf("FromEnd(%d)=%q sliced mid-rune", max, tail)
		}
	}
}

func TestTrimPartialTrailingRune(t *testing.T) {
	// A valid string is returned unchanged.
	if got := TrimPartialTrailingRune("hello"); got != "hello" {
		t.Errorf("valid: got %q", got)
	}
	// "café" is c a f é(2 bytes); cutting the last byte leaves a partial
	// rune that should be trimmed back to "caf".
	full := "café"
	cut := full[:len(full)-1] // drops the last byte of é
	if got := TrimPartialTrailingRune(cut); got != "caf" {
		t.Errorf("partial trailing rune: got %q want %q", got, "caf")
	}
	if got := TrimPartialTrailingRune(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}
