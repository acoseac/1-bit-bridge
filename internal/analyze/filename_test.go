package analyze

import (
	"strings"
	"testing"
)

func TestSafeAnalysisFilenameClean(t *testing.T) {
	if got := safeAnalysisFilename("01 Track.flac"); got != "01 Track.flac"+waveformExt {
		t.Fatalf("got %q", got)
	}
}

func TestSafeAnalysisFilenameFATCharsSanitizedAndDisambiguated(t *testing.T) {
	got := safeAnalysisFilename(`Track: A? B.flac`)
	if !strings.HasSuffix(got, waveformExt) {
		t.Fatalf("missing suffix: %q", got)
	}
	if strings.ContainsAny(got, `:*?"<>|\`) {
		t.Fatalf("illegal chars remain: %q", got)
	}
	// Two raw inputs that sanitize identically must stay distinct on disk.
	a := safeAnalysisFilename("Track:A.flac")
	b := safeAnalysisFilename("Track*A.flac")
	if a == b {
		t.Fatalf("collision: both produced %q", a)
	}
}

func TestSafeAnalysisFilenameLongFitsCap(t *testing.T) {
	long := strings.Repeat("x", 300) + ".flac"
	got := safeAnalysisFilename(long)
	if len(got)+len(analysisTmpSuffix) > 255 {
		t.Fatalf("basename + tmp suffix overflows 255: len(got)=%d", len(got))
	}
	if !strings.HasSuffix(got, waveformExt) {
		t.Fatalf("missing suffix: %q", got)
	}
}

func TestSafeAnalysisFilenameUTF8MiddleTruncateSafe(t *testing.T) {
	// A long multi-byte name must middle-truncate on rune boundaries
	// (no corrupted UTF-8) and still fit the cap.
	long := strings.Repeat("Dvořák ", 60) + ".flac"
	got := safeAnalysisFilename(long)
	if len(got)+len(analysisTmpSuffix) > 255 {
		t.Fatalf("overflow: len(got)=%d", len(got))
	}
	if !strings.HasSuffix(got, waveformExt) {
		t.Fatalf("missing suffix: %q", got)
	}
	if !utf8ValidStr(got) {
		t.Fatalf("produced invalid UTF-8: %q", got)
	}
}

func utf8ValidStr(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
