package lyrics

import (
	"fmt"
	"strings"
	"testing"
)

// The truth table is LIFTED VERBATIM from the app's
// `LRCParserTests.test_timedCoverageIsTooSparse_truthTable` (iOS #1759): the
// two constants and every row. A divergence between the two sides is the
// failure this mirror exists to prevent, so the numbers are the app's, not
// re-derived here.
func TestTimedCoverageIsTooSparseTruthTable(t *testing.T) {
	if MinimumTimedLines != 2 {
		t.Fatalf("MinimumTimedLines = %d, the app's LRCParser.minimumTimedLines is 2", MinimumTimedLines)
	}
	if MinimumTimedShare != 0.25 {
		t.Fatalf("MinimumTimedShare = %v, the app's LRCParser.minimumTimedShare is 0.25", MinimumTimedShare)
	}
	cases := []struct {
		timed, untimed int
		want           bool
	}{
		{1, 0, false},
		{0, 5, false},
		{1, 1, true},
		{2, 6, false},
		{2, 7, true},
		{30, 4, false},
	}
	for _, c := range cases {
		if got := TimedCoverageIsTooSparse(c.timed, c.untimed); got != c.want {
			t.Errorf("TimedCoverageIsTooSparse(timed=%d, untimed=%d) = %v, want %v", c.timed, c.untimed, got, c.want)
		}
	}
}

// TimedCoverage counts what the app's parse loop counts: a source line is
// TIMED when it carries at least one line tag and text survives after the
// tags (a clear event — `[00:15.00]` alone — is neither); UNTIMED when it is
// non-blank, carries no tag, and is not one of the LRC ID tags a reader must
// never show. Blank lines count for nothing.
func TestTimedCoverageCountsLikeTheAppsParseLoop(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		timed, untimed int
	}{
		{"one cue in a transcript", "We found love in a hopeless place\nShine a light through an open door\n[04:20.00]Yellow diamonds in the light\nAnd we're standing side by side\nAs your shadow crosses mine", 1, 4},
		{"a real timeline", "[00:10.00]A\n[00:12.00]B\n[00:14.00]C", 3, 0},
		{"blank lines count for nothing", "[00:10.00]A\n\n\nB\n\n", 1, 1},
		{"a clear event is not a timed TEXT line", "[00:12.00]Hi\n[00:15.00]", 1, 0},
		{"known ID tags are consumed, not text", "[ar:Artist]\n[ti:Title]\n[al:Album: Deluxe]\n[offset:+500]\n[00:10.00]A\n[00:12.00]B", 2, 0},
		{"an unknown [key:value] line is a section header, i.e. text", "[Chorus: Rihanna]\nWe found love\nIn a hopeless place", 0, 3},
		{"several tags on one line are ONE source line", "[00:10.00][00:20.00]Refrain\nProse", 1, 1},
		{"a word-tag-only remainder has no text", "[00:10.00]<00:10.50>\nProse", 0, 1},
		{"hours tags count", "[01:02:03.45]late\nProse", 1, 1},
		{"full-width brackets count", "［00:01.00］x\n【00:02.00】y", 2, 0},
	}
	for _, c := range cases {
		timed, untimed := TimedCoverage(c.body)
		if timed != c.timed || untimed != c.untimed {
			t.Errorf("%s: TimedCoverage = (%d, %d), want (%d, %d)", c.name, timed, untimed, c.timed, c.untimed)
		}
	}
}

// The app fixtures, run through the bridge's classifier. LooksLikeLRC stays
// any-line on both sides; the rule sits in the CLASSIFICATION, as the app's
// sits in its parse — both arms of it: `timed.isEmpty` and the sparse share.
func TestTextCandidateRanksASparseTimedBodyAsPlain(t *testing.T) {
	// One `[4:20]` cue in a Genius-style transcript used to yield a `text-lrc`
	// row with `synced: true` — a one-line synced stub, on the bridge's word,
	// outranking a complete plain document in the same file.
	transcript := "We found love in a hopeless place\nShine a light through an open door\n[04:20.00]Yellow diamonds in the light\nAnd we're standing side by side\nAs your shadow crosses mine"
	var realLRC strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&realLRC, "[%02d:%02d.00]Line %d\n", i/6, (i%6)*10, i)
	}
	// The share is a strict bar: exactly 25 % (2 of 8) is synced, one more
	// untimed line (2 of 9) is not — pinned so the constants cannot drift
	// without this reading it.
	twoTimed := "[00:10.00]A\n[00:20.00]B\n"
	var sixProse strings.Builder
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&sixProse, "Prose %d\n", i)
	}
	cases := []struct {
		name         string
		body         string
		taggedSynced bool
		wantSource   Source
	}{
		{"one cue marker in a transcript is plain", transcript, false, SourceTextPlain},
		{"a synced-typed tag over a sparse body is still plain", transcript, true, SourceTextPlain},
		{"a real 20-line LRC is synced", realLRC.String(), false, SourceTextLRC},
		{"a synced-typed tag over a real LRC ranks as vorbis-synced", realLRC.String(), true, SourceVorbisSynced},
		{"two of eight timed is at the bar and synced", twoTimed + sixProse.String(), false, SourceTextLRC},
		{"two of nine timed is below the bar and plain", twoTimed + sixProse.String() + "Prose 7\n", false, SourceTextPlain},
		{"a single timed line beside ANY untimed text is plain", "Intro\n[00:12.00]Hi", false, SourceTextPlain},
		// No untimed text → never sparse: a lone timed line, a timed line
		// beside a clear event, and a lone `♪` keep today's verdict.
		{"a lone timed line stays synced", "[00:12.00]Hi", false, SourceTextLRC},
		{"a timed line beside a clear event stays synced", "[00:12.00]Hi\n[00:15.00]", false, SourceTextLRC},
		{"a lone timed instrumental marker stays synced", "[00:00.00]♪", false, SourceTextLRC},
		// Known ID tags are not untimed text: a two-line LRC under a
		// metadata header is not "2 of 6".
		{"metadata lines do not dilute the share", "[ar:Artist]\n[ti:Title]\n[al:Album]\n[by:Me]\n[00:10.00]A\n[00:12.00]B", false, SourceTextLRC},
		// The app's OTHER arm: no timed TEXT line at all. A body whose only
		// tags are clear events is LRC-shaped to LooksLikeLRC and has zero
		// timed text lines — the app's parse returns the plain document
		// from `timed.isEmpty` before it ever asks the sparse question,
		// whose verbatim guard would answer "not sparse" (gemini on #904).
		{"a clear event beside prose is not a timeline", "Prose\n[00:12.00]", false, SourceTextPlain},
		{"clear events beside prose are not a timeline", "[00:12.00]\n[00:15.00]\nWords\nMore words", false, SourceTextPlain},
		{"a synced-typed tag over clear events and prose is plain", "Prose\n[00:12.00]", true, SourceTextPlain},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := TextCandidate(c.body, "en", c.taggedSynced, 0)
			if !ok {
				t.Fatal("the body is a candidate")
			}
			wantSynced := c.wantSource != SourceTextPlain
			wantFormat := FormatText
			if wantSynced {
				wantFormat = FormatLRC
			}
			if got.Source != c.wantSource || got.Doc.Synced != wantSynced || got.Doc.Format != wantFormat {
				t.Fatalf("got source=%q synced=%v format=%q, want %q/%v/%q", got.Source, got.Doc.Synced, got.Doc.Format, c.wantSource, wantSynced, wantFormat)
			}
			if norm, _ := Normalize(got.Doc.Body); got.Doc.Language != "en" || got.Language != "en" || norm != got.Doc.Body {
				t.Fatalf("language carries through and the body is a Normalize fixed point: %+v", got)
			}
		})
	}
}

// The plain verdict changes the wire document (format, synced) and therefore
// the tag, which is what puts the track into the client delta once the file
// re-extracts. Pinned so the two spellings of one body cannot share a tag.
func TestSparseVerdictReKeysTheTag(t *testing.T) {
	body := "Intro\n[00:12.00]Hi"
	plain, _ := TextCandidate(body, "", false, 0)
	forcedLRC := Doc{Format: FormatLRC, Synced: true, Body: plain.Doc.Body}
	if Tag(plain.Doc) == Tag(forcedLRC) {
		t.Fatal("the plain and synced readings of one body must not share a tag")
	}
}
