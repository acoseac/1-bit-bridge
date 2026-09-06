package lyrics

import (
	"strings"
	"testing"
)

func TestNormalizeIsDeterministic(t *testing.T) {
	a, ok := Normalize("\uFEFF[00:01.00]Hello  \r\n[00:02.00]World\r\n\r\n")
	if !ok {
		t.Fatal("expected ok")
	}
	b, _ := Normalize("[00:01.00]Hello\n[00:02.00]World")
	if a != b {
		t.Fatalf("BOM / CRLF / trailing whitespace must not change the body:\n%q\n%q", a, b)
	}
	da, db := Doc{Format: FormatLRC, Synced: true, Body: a}, Doc{Format: FormatLRC, Synced: true, Body: b}
	if Tag(da) != Tag(db) || len(Tag(da)) != 8 {
		t.Fatalf("tag must be stable and 8 hex: %q %q", Tag(da), Tag(db))
	}
	// Every client-visible field re-keys the tag, not just the body.
	if Tag(Doc{Format: FormatText, Synced: false, Body: a}) == Tag(da) ||
		Tag(Doc{Format: FormatLRC, Synced: true, Body: a, Language: "en"}) == Tag(da) {
		t.Fatal("format / synced / language must participate in the tag")
	}
	// NFC: a decomposed é (e + COMBINING ACUTE ACCENT) folds to the
	// precomposed form — explicit escapes so an editor's own NFC pass can't
	// make this test vacuous.
	c, _ := Normalize("cafe\u0301")
	d, _ := Normalize("caf\u00e9")
	if c != d {
		t.Fatalf("NFC must fold: %q vs %q", c, d)
	}
}

func TestNormalizeRejectsEmptyAndOversized(t *testing.T) {
	if _, ok := Normalize("  \n\r\n "); ok {
		t.Fatal("whitespace-only must be rejected")
	}
	if _, ok := Normalize(strings.Repeat("a", MaxBodyBytes+1)); ok {
		t.Fatal("oversized must be rejected")
	}
}

func TestLooksLikeLRC(t *testing.T) {
	cases := map[string]bool{
		"[00:01.00]Hello":         true,
		"[00:01]Hello":            true,
		"[1:05.3]x":               true,
		"［00:01.00］full-width":    true,
		"[01:02:03.45]hours":      true,
		"[ar:Adele]\n[00:01.00]x": true,
		"Just words\nMore words":  false,
		"[ar:Adele]\n[ti:Hello]":  false,
		"":                        false,
	}
	for in, want := range cases {
		if got := LooksLikeLRC(in); got != want {
			t.Errorf("LooksLikeLRC(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTextCandidateClassifies(t *testing.T) {
	c, ok := TextCandidate("[00:01.00]A\n[00:02.00]B", "en", false, 0)
	if !ok || c.Source != SourceTextLRC || !c.Doc.Synced || c.Doc.Format != FormatLRC || c.Doc.Language != "en" {
		t.Fatalf("LRC-shaped text: %+v", c)
	}
	c, _ = TextCandidate("[00:01.00]A", "", true, 0)
	if c.Source != SourceVorbisSynced {
		t.Fatalf("a synced-typed tag ranks as vorbis-synced: %+v", c)
	}
	c, _ = TextCandidate("Only words", "", true, 0)
	if c.Source != SourceTextPlain || c.Doc.Synced || c.Doc.Format != FormatText {
		t.Fatalf("a synced-typed tag without tags is plain text: %+v", c)
	}
	if _, ok := TextCandidate("   ", "", false, 0); ok {
		t.Fatal("empty must not be a candidate")
	}
}

func TestPickPrecedenceAndTies(t *testing.T) {
	plain, _ := TextCandidate("Words", "", false, 0)
	lrc, _ := TextCandidate("[00:01.00]A", "", false, 0)
	sidecar := Candidate{Source: SourceSidecarLRC, Doc: Doc{Format: FormatLRC, Synced: true, Body: "[00:01.00]S"}}
	sylt := Candidate{Source: SourceSYLT, Doc: Doc{Format: FormatLRC, Synced: true, Body: "[00:01.00]Y"}}
	ttml := Candidate{Source: SourceSidecarTTML, Doc: Doc{Format: FormatTTML, Synced: true, Body: "<tt/>"}}
	txt := Candidate{Source: SourceSidecarText, Doc: Doc{Format: FormatText, Body: "T"}}
	got, _ := Pick([]Candidate{txt, plain, ttml, lrc, sylt, sidecar})
	if got.Source != SourceSidecarTTML {
		t.Fatalf("sidecar .ttml must win: %v", got.Source)
	}
	got, _ = Pick([]Candidate{txt, plain, lrc, sylt, sidecar})
	if got.Source != SourceSidecarLRC {
		t.Fatalf("sidecar .lrc beats every embedded source: %v", got.Source)
	}
	got, _ = Pick([]Candidate{plain, lrc, sylt})
	if got.Source != SourceSYLT {
		t.Fatalf("SYLT beats LRC-shaped text: %v", got.Source)
	}
	got, _ = Pick([]Candidate{txt, plain})
	if got.Source != SourceTextPlain {
		t.Fatalf("an embedded plain tag beats a .txt sidecar: %v", got.Source)
	}
	// Same source: an empty descriptor beats a junk one; then the longest.
	junk := Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "much longer junk text"}, Priority: 2}
	empty := Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "short"}, Priority: 0}
	got, _ = Pick([]Candidate{junk, empty})
	if got.Doc.Body != "short" {
		t.Fatalf("descriptor priority beats length: %q", got.Doc.Body)
	}
	// Equal source and priority: the longest body wins.
	short := Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "aa"}}
	long := Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "aaaa"}}
	if got, _ := Pick([]Candidate{short, long}); got.Doc.Body != "aaaa" {
		t.Fatalf("longest body wins: %q", got.Doc.Body)
	}
	// The same document twice (dhowden's Lyrics() accessor drops the frame
	// language; the raw walk keeps it): the one carrying a language survives.
	bare := Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "dup"}}
	rich := Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "dup", Language: "en"}, Language: "en"}
	if got, _ := Pick([]Candidate{bare, rich}); got.Language != "en" {
		t.Fatalf("duplicate collapse keeps the language-bearing candidate: %+v", got)
	}
	if _, ok := Pick(nil); ok {
		t.Fatal("no candidates → no pick")
	}
}

func TestDescriptorPriority(t *testing.T) {
	if DescriptorPriority("") != 0 || DescriptorPriority("Verse") != 1 || DescriptorPriority("Amazon Lyrics") != 2 {
		t.Fatal("descriptor ladder")
	}
	if DescriptorPriority("Rapid Verse") != 1 || DescriptorPriority("Context") != 1 {
		t.Fatal("short junk tokens match the whole descriptor only")
	}
	if DescriptorPriority("api") != 2 || DescriptorPriority("TEXT") != 2 {
		t.Fatal("exact junk tokens still demote")
	}
}

// TestPickDoesNotLaunderAFabricatedPriority pins the duplicate-merge rule.
//
// applyEmbeddedLyricsFromTag appends dhowden's m.Lyrics() FIRST with a
// hardcoded Priority 0 — "empty descriptor", the best rank there is — and then
// the raw frame walk appends the SAME frame with its real DescriptorPriority.
// (Verified in the module source: metadataID3v2.Lyrics() returns
// m.frames["USLT"].(*Comm).Text, literally the frame the walk re-reports.)
// Keeping the first sighting let a junk descriptor launder itself back to 0
// whenever the frame's language was absent or unmapped, so the junkExact /
// junkSubstring demotion silently did nothing and the junk frame outranked a
// clean sibling.
func TestPickDoesNotLaunderAFabricatedPriority(t *testing.T) {
	const junkBody = "Amazon-stamped body"
	const cleanBody = "Clean sibling body!" // same length, so only Priority can decide
	if len(junkBody) != len(cleanBody) {
		t.Fatalf("fixture broken: bodies must be the same length (%d vs %d)",
			len(junkBody), len(cleanBody))
	}
	cands := []Candidate{
		// m.Lyrics() — no descriptor to classify, so a fabricated 0.
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: junkBody}, Priority: 0},
		// The same frame through the raw walk, with its real descriptor and no
		// language to trigger the old "keep the richer" replacement.
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: junkBody},
			Priority: DescriptorPriority("Amazon")},
		// A clean sibling frame that should therefore win.
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: cleanBody}, Priority: 0},
	}
	got, ok := Pick(cands)
	if !ok {
		t.Fatal("Pick refused a non-empty set")
	}
	if got.Doc.Body != cleanBody {
		t.Errorf("the junk-descriptor frame won: %q.\nIts real DescriptorPriority is %d, but the "+
			"m.Lyrics() sighting re-entered it at 0 and the demotion did nothing.",
			got.Doc.Body, DescriptorPriority("Amazon"))
	}
}

// TestPickMergeKeepsTheRicherLanguage guards the half of the merge that was
// already right: m.Lyrics() drops the frame's language, the raw walk keeps it,
// and the surviving candidate must carry it.
func TestPickMergeKeepsTheRicherLanguage(t *testing.T) {
	body := "Same document twice"
	got, ok := Pick([]Candidate{
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: body}},
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: body, Language: "ja"},
			Language: "ja"},
	})
	if !ok {
		t.Fatal("Pick refused a non-empty set")
	}
	if got.Doc.Language != "ja" || got.Language != "ja" {
		t.Errorf("language lost on merge: Doc.Language=%q Language=%q", got.Doc.Language, got.Language)
	}
}

// TestPickIsDeterministicUnderShuffle is the unit-test twin of
// FuzzPickIsShuffleInvariant, on the one shape that actually exercises the
// tie-break tail: same source rank, same priority, same body LENGTH, different
// bodies. Two USLT frames with equal-length text is exactly that, and it is
// how a randomised m.Raw() iteration order reaches Pick.
//
// This test exists because the negative control caught its absence: deleting
// the whole tie-break tail left the suite green, because every fuzz seed
// happened to differ on rank or priority before ever reaching it.
func TestPickIsDeterministicUnderShuffle(t *testing.T) {
	cands := []Candidate{
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "zzz"}},
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "aaa"}},
		{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Body: "mmm"}},
	}
	want, ok := Pick(append([]Candidate(nil), cands...))
	if !ok {
		t.Fatal("Pick refused a non-empty set")
	}
	for i := range cands {
		for j := range cands {
			rotated := append([]Candidate(nil), cands...)
			rotated[i], rotated[j] = rotated[j], rotated[i]
			got, _ := Pick(rotated)
			if got.Doc.Body != want.Doc.Body {
				t.Fatalf("Pick is order-dependent: %q with the input as given, %q after "+
					"swapping %d and %d. Candidates arrive in Go map order, so this "+
					"winner flips between scans.", want.Doc.Body, got.Doc.Body, i, j)
			}
		}
	}
}
