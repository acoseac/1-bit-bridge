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
