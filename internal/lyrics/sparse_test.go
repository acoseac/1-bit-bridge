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

// The four app fixtures, run through the bridge's classifier. LooksLikeLRC
// stays any-line on both sides; the sparse rule sits in the CLASSIFICATION,
// as the app's sits in its parse.
func TestTextCandidateRanksASparseTimedBodyAsPlain(t *testing.T) {
	// One `[4:20]` cue in a Genius-style transcript used to yield a `text-lrc`
	// row with `synced: true` — a one-line synced stub, on the bridge's word,
	// outranking a complete plain document in the same file.
	transcript := "We found love in a hopeless place\nShine a light through an open door\n[04:20.00]Yellow diamonds in the light\nAnd we're standing side by side\nAs your shadow crosses mine"
	c, ok := TextCandidate(transcript, "en", false, 0)
	if !ok {
		t.Fatal("the transcript is a candidate")
	}
	if c.Source != SourceTextPlain || c.Doc.Synced || c.Doc.Format != FormatText {
		t.Fatalf("one cue marker in a transcript is plain text, got %+v", c)
	}
	if c.Doc.Language != "en" || c.Language != "en" || !strings.Contains(c.Doc.Body, "[04:20.00]") {
		t.Fatalf("language and body carry through unchanged: %+v", c)
	}

	// The tag itself claiming sync (Vorbis SYNCEDLYRICS) does not rescue a
	// body without a timeline: the app parses the body, not the tag name.
	if c, _ := TextCandidate(transcript, "", true, 0); c.Source != SourceTextPlain || c.Doc.Synced {
		t.Fatalf("a synced-typed tag over a sparse body is still plain text, got %+v", c)
	}

	// A real 20-line LRC is synced, as before.
	var lrc strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&lrc, "[%02d:%02d.00]Line %d\n", i/6, (i%6)*10, i)
	}
	if c, _ := TextCandidate(lrc.String(), "", false, 0); c.Source != SourceTextLRC || !c.Doc.Synced || c.Doc.Format != FormatLRC {
		t.Fatalf("a real LRC is synced, got %+v", c)
	}
	if c, _ := TextCandidate(lrc.String(), "", true, 0); c.Source != SourceVorbisSynced {
		t.Fatalf("a synced-typed tag over a real LRC ranks as vorbis-synced, got %+v", c)
	}

	// The share is a strict bar: exactly 25 % (2 of 8) is synced, one more
	// untimed line (2 of 9) is not — pinned so the constants cannot drift
	// without this reading it.
	timed := "[00:10.00]A\n[00:20.00]B\n"
	var six strings.Builder
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&six, "Prose %d\n", i)
	}
	if c, _ := TextCandidate(timed+six.String(), "", false, 0); c.Source != SourceTextLRC || !c.Doc.Synced {
		t.Fatalf("two of eight timed is at the bar and synced, got %+v", c)
	}
	if c, _ := TextCandidate(timed+six.String()+"Prose 7\n", "", false, 0); c.Source != SourceTextPlain || c.Doc.Synced {
		t.Fatalf("two of nine timed is below the bar and plain, got %+v", c)
	}

	// A single timed line beside ANY untimed text is sparse whatever the
	// share — one stamp is a cue, not a timeline.
	if c, _ := TextCandidate("Intro\n[00:12.00]Hi", "", false, 0); c.Source != SourceTextPlain || c.Doc.Synced {
		t.Fatalf("one timed line beside one untimed is plain, got %+v", c)
	}

	// No untimed text → never sparse: a lone timed line, and a timed line
	// beside a clear event, keep today's verdict.
	for _, body := range []string{"[00:12.00]Hi", "[00:12.00]Hi\n[00:15.00]", "[00:00.00]♪"} {
		if c, _ := TextCandidate(body, "", false, 0); c.Source != SourceTextLRC || !c.Doc.Synced {
			t.Fatalf("%q has no untimed text and stays synced, got %+v", body, c)
		}
	}

	// Known ID tags are not untimed text: a two-line LRC under a metadata
	// header is not "2 of 6".
	if c, _ := TextCandidate("[ar:Artist]\n[ti:Title]\n[al:Album]\n[by:Me]\n[00:10.00]A\n[00:12.00]B", "", false, 0); c.Source != SourceTextLRC {
		t.Fatalf("metadata lines do not dilute the share, got %+v", c)
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
