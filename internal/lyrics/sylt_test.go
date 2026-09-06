package lyrics

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// syltBody mirrors the iOS test helper: encoding · "eng" · time format ·
// content type · empty descriptor · (text NUL u32BE)*.
func syltBody(encoding, timeFormat, contentType byte, entries []SYLTEntry) []byte {
	b := []byte{encoding, 'e', 'n', 'g', timeFormat, contentType}
	switch encoding {
	case 1, 2:
		b = append(b, 0, 0)
	default:
		b = append(b, 0)
	}
	for _, e := range entries {
		switch encoding {
		case 1:
			b = append(b, 0xFF, 0xFE) // a per-entry BOM (the trap)
			for _, u := range utf16.Encode([]rune(e.Text)) {
				b = append(b, byte(u), byte(u>>8))
			}
			b = append(b, 0, 0)
		case 2:
			for _, u := range utf16.Encode([]rune(e.Text)) {
				b = append(b, byte(u>>8), byte(u))
			}
			b = append(b, 0, 0)
		default:
			b = append(b, []byte(e.Text)...)
			b = append(b, 0)
		}
		var ts [4]byte
		binary.BigEndian.PutUint32(ts[:], uint32(e.Millis))
		b = append(b, ts[:]...)
	}
	return b
}

func TestSYLTLineMarkersAndTiming(t *testing.T) {
	s, ok := ParseSYLT(syltBody(3, 2, 1, []SYLTEntry{{0, ""}, {1000, "First line"}, {2500, "\nSecond line"}}))
	if !ok || s.Language != "eng" {
		t.Fatalf("parse: %+v %v", s, ok)
	}
	body, word := ToLRC(s)
	if word || body != "[00:01.000]First line\n[00:02.500]Second line" {
		t.Fatalf("leading-newline style:\n%q word=%v", body, word)
	}
	body, _ = ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{1000, "First\n"}, {2000, "Second\n"}})))
	if body != "[00:01.000]First\n[00:02.000]Second" {
		t.Fatalf("trailing-newline style:\n%q", body)
	}
	body, _ = ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{1000, "First"}, {2000, "\r\nSecond"}, {2500, "\r\n"}, {3000, "Third\r\n"}})))
	if body != "[00:01.000]First\n[00:02.000]Second\n[00:03.000]Third" {
		t.Fatalf("CRLF markers:\n%q", body)
	}
}

func TestSYLTSyllablesBecomeEnhancedWordTags(t *testing.T) {
	body, word := ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{1000, "Hel"}, {1200, "lo "}, {1500, "world"}, {3000, "\nSe"}, {3300, "cond"}})))
	want := "[00:01.000]<00:01.000>Hel<00:01.200>lo <00:01.500>world\n[00:03.000]<00:03.000>Se<00:03.300>cond"
	if !word || body != want {
		t.Fatalf("enhanced LRC:\n got %q\nwant %q", body, want)
	}
}

func TestSYLTMarkerlessEntries(t *testing.T) {
	// Seconds apart, no markers: one whole line per entry (Mp3tag-style).
	body, word := ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{1000, "First line"}, {4000, "Second line"}, {7500, "Third line"}})))
	if word || body != "[00:01.000]First line\n[00:04.000]Second line\n[00:07.500]Third line" {
		t.Fatalf("whole-line detection:\n%q", body)
	}
	// Sub-second syllables with no marker anywhere: ONE word-timed line.
	body, word = ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{1000, "Hel"}, {1200, "lo "}, {1500, "world"}})))
	if !word || body != "[00:01.000]<00:01.000>Hel<00:01.200>lo <00:01.500>world" {
		t.Fatalf("syllables stay one line:\n%q", body)
	}
}

func TestSYLTDropsMPEGFramesAndOtherContentTypes(t *testing.T) {
	if _, ok := ParseSYLT(syltBody(3, 1, 1, []SYLTEntry{{10, "x"}})); ok {
		t.Fatal("MPEG-frame timing must be dropped")
	}
	if _, ok := ParseSYLT(syltBody(3, 2, 3, []SYLTEntry{{10, "x"}})); ok {
		t.Fatal("content type 3 must be dropped")
	}
	if _, ok := ParseSYLT(syltBody(3, 2, 2, []SYLTEntry{{10, "x"}})); !ok {
		t.Fatal("transcription (2) is accepted")
	}
	if _, ok := ParseSYLT([]byte{3, 'e', 'n', 'g', 2}); ok {
		t.Fatal("truncated body")
	}
}

func TestSYLTUTF16PerEntryBOMAndUnsortedEntries(t *testing.T) {
	s := mustParse(t, syltBody(1, 2, 1, []SYLTEntry{{5000, "\nLate"}, {1000, "\nEarly"}}))
	if s.Entries[0].Text != "\nEarly" || s.Entries[1].Text != "\nLate" {
		t.Fatalf("stable sort + BOM strip: %+v", s.Entries)
	}
	body, _ := ToLRC(s)
	if body != "[00:01.000]Early\n[00:05.000]Late" {
		t.Fatalf("%q", body)
	}
	be := mustParse(t, syltBody(2, 2, 1, []SYLTEntry{{1000, "Ünïcödé"}}))
	if be.Entries[0].Text != "Ünïcödé" {
		t.Fatalf("UTF-16BE: %q", be.Entries[0].Text)
	}
}

func TestSYLTDoubledMarkersNeverLeakARawNewline(t *testing.T) {
	body, _ := ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{1000, "\n\nFirst\n\n"}, {2000, "\nSec\nond\r\n"}})))
	if body != "[00:01.000]First\n[00:02.000]Sec ond" {
		t.Fatalf("doubled / interior markers:\n%q", body)
	}
	if _, ok := ParseSYLT([]byte{3, 'e', 'n', 'g', 2, 1, 'x', 'y'}); ok {
		t.Fatal("a body with no terminator anywhere is malformed")
	}
}

func TestSYLTClearEventAndEmptyLeadingEntry(t *testing.T) {
	body, _ := ToLRC(mustParse(t, syltBody(3, 2, 1, []SYLTEntry{{0, ""}, {1000, "\nA"}, {2000, ""}, {9000, "\nB"}})))
	if body != "[00:01.000]A\n[00:02.000]\n[00:09.000]B" {
		t.Fatalf("clear event renders as an empty timed line:\n%q", body)
	}
}

func mustParse(t *testing.T, body []byte) SYLT {
	t.Helper()
	s, ok := ParseSYLT(body)
	if !ok {
		t.Fatal("parse failed")
	}
	return s
}

// TestLRCTimeClampsToTheParseableRange pins that every timestamp lrcTime emits
// stays inside the ONE shape lineTag — and the iOS LRCParser it mirrors — will
// match. ParseSYLT reads a raw uint32 of milliseconds from an untrusted frame,
// so 1,193 hours is reachable; unclamped that rendered a four-digit minute
// field matching neither lineTag nor hoursTag, inside a document syltCandidate
// stamps synced regardless, and the phone dropped the line.
func TestLRCTimeClampsToTheParseableRange(t *testing.T) {
	for _, ms := range []int64{-1, 0, 1, 999, 59_999, 60_000, 3_600_000,
		59_999_999, 60_000_000, 359_999_999, 4_294_967_295} {
		line := "[" + lrcTime(ms) + "]lyric"
		if !lineTag.MatchString(line) && !hoursTag.MatchString(line) {
			t.Errorf("lrcTime(%d) rendered %q, which neither LRC regex accepts", ms, line)
		}
	}
	if got := lrcTime(4_294_967_295); got != "999:59.999" {
		t.Errorf("uint32 max should clamp to the end of the range, got %q", got)
	}
	if got := lrcTime(-5); got != "00:00.000" {
		t.Errorf("negative should floor at zero, got %q", got)
	}
	// Everything inside the range is untouched — the clamp must not rewrite
	// the rendering of legitimately long tracks.
	if got := lrcTime(90 * 60 * 1000); got != "90:00.000" {
		t.Errorf("a 90-minute timestamp was rewritten: %q", got)
	}
}
