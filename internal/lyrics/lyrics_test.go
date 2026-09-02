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
	if Tag(a) != Tag(b) || len(Tag(a)) != 8 {
		t.Fatalf("tag must be stable and 8 hex: %q %q", Tag(a), Tag(b))
	}
	// NFC: a decomposed é folds to the precomposed form.
	c, _ := Normalize("café")
	d, _ := Normalize("café")
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
	if got.Source != SourceSidecarLRC {
		t.Fatalf("sidecar .lrc must win: %v", got.Source)
	}
	got, _ = Pick([]Candidate{plain, lrc, sylt})
	if got.Source != SourceSYLT {
		t.Fatalf("SYLT beats LRC-shaped text: %v", got.Source)
	}
	got, _ = Pick([]Candidate{plain, ttml, lrc})
	if got.Source != SourceTextLRC {
		t.Fatalf("an LRC-shaped tag beats a .ttml until clients parse TTML: %v", got.Source)
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
	if _, ok := Pick(nil); ok {
		t.Fatal("no candidates → no pick")
	}
}

func TestDescriptorPriority(t *testing.T) {
	if DescriptorPriority("") != 0 || DescriptorPriority("Verse") != 1 || DescriptorPriority("Amazon Lyrics") != 2 {
		t.Fatal("descriptor ladder")
	}
}
