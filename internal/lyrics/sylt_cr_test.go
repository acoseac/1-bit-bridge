package lyrics

import (
	"strings"
	"testing"
)

// A bare carriage return is a newline marker, as it is on the phone.
//
// iOS #1564 (2026-09-03) made `ID3v2Parser` count `\r` beside `\n` and
// `\r\n` in all three places — the whole-line heuristic, the clear-event
// decision and the prefix/suffix strip — because older Mac taggers wrote
// CR-only markers and the marker "stayed in the text". The bridge's
// hasNewlineMarker / splitMarkers are documented as that rule's VERBATIM
// mirror and stayed on `\n` / `\r\n`: `strings.Trim(text, "\r\n")` then
// stripped the CR from the body while `leading` / `trailing` read false, so
// ToLRC merged the next entry onto the open line and the phone got one
// timed line where the file had two.
func TestToLRCTreatsABareCarriageReturnAsAMarker(t *testing.T) {
	cases := []struct {
		name    string
		entries []SYLTEntry
		want    []string
	}{
		{"trailing CR ends a line", []SYLTEntry{{1000, "First line\r"}, {4500, "Second line\r"}},
			[]string{"[00:01.000]First line", "[00:04.500]Second line"}},
		{"leading CR starts a line", []SYLTEntry{{1000, "First line"}, {4500, "\rSecond line"}},
			[]string{"[00:01.000]First line", "[00:04.500]Second line"}},
		{"CR, LF and CRLF markers mix", []SYLTEntry{{1000, "One\r"}, {2000, "Two\n"}, {3000, "Three\r\n"}, {4000, "Four"}},
			[]string{"[00:01.000]One", "[00:02.000]Two", "[00:03.000]Three", "[00:04.000]Four"}},
		// A bare-CR entry is a line break, not a clear event — the same
		// distinction the phone's `carriesMarker` makes.
		{"a bare CR entry is only a line break", []SYLTEntry{{1000, "One"}, {2000, "\r"}, {3000, "Two"}},
			[]string{"[00:01.000]One", "[00:03.000]Two"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ToLRC(SYLT{Entries: c.entries})
			if lines := strings.Split(got, "\n"); strings.Join(lines, "|") != strings.Join(c.want, "|") {
				t.Fatalf("ToLRC =\n%s\nwant\n%s", got, strings.Join(c.want, "\n"))
			}
		})
	}
}

func TestHasNewlineMarkerCountsABareCarriageReturn(t *testing.T) {
	for _, s := range []string{"\rline", "line\r", "\r\nline", "line\r\n", "\nline", "line\n"} {
		if !hasNewlineMarker(s) {
			t.Errorf("hasNewlineMarker(%q) = false", s)
		}
	}
	if hasNewlineMarker("a\rb") {
		t.Error("an interior CR is not a leading/trailing marker")
	}
	// A CR-marked writer is a marker writer, so the whole-line heuristic must
	// not treat its entries as unmarked whole lines.
	entries := []SYLTEntry{{0, "Verse one line\r"}, {5000, "Verse two line\r"}, {10000, "Verse three\r"}}
	if EntriesLookLikeWholeLines(entries) {
		t.Error("CR-marked entries must not read as marker-less whole lines")
	}
	body, leading, trailing := splitMarkers("\rmid\rdle\r")
	if body != "mid dle" || !leading || !trailing {
		t.Errorf("splitMarkers(CR-marked) = %q %v %v", body, leading, trailing)
	}
}
