package manifest

import "strings"

// ExplicitSignals is one track's inputs to ExplicitVerdict. The iOS twin
// passes the same inputs to ExplicitContent.verdict; TestExplicitVerdict
// is the shared truth table.
//
// Rtng is set when an MP4 `rtng` integer was read. 1 and 4 are explicit.
// A present 0 or 2 is not, and it does not cancel another signal. Nil
// means the atom is absent.
//
// ItunesAdvisory is the first value of the ITUNESADVISORY field, and
// Explicit is the first value of the EXPLICIT field. Empty means the
// field is absent or empty. Within one field the first value wins; the
// caller has already chosen it.
//
// Title is the raw track-title tag (MP4 ©nam, Vorbis TITLE, ID3v2 TIT2),
// after TrimSpace and before any display cleaning that strips bracket
// markers. An album title is not Title, and neither is a title guessed
// from the path.
type ExplicitSignals struct {
	Rtng           *uint64
	ItunesAdvisory string
	Explicit       string
	Title          string
}

// ExplicitVerdict is the whole explicit-content decision. Any signal that
// says explicit wins: a clean `rtng` does not hide a freeform advisory, an
// EXPLICIT field or a title marker.
func ExplicitVerdict(s ExplicitSignals) bool {
	if s.Rtng != nil && (*s.Rtng == 1 || *s.Rtng == 4) {
		return true
	}
	if explicitAdvisoryText(s.ItunesAdvisory) || explicitAdvisoryText(s.Explicit) {
		return true
	}
	return titleMarksExplicit(s.Title)
}

// explicitAdvisoryText is the advisory value table, after the trim the
// tag readers already apply. Equality after case-folding, never a numeric
// parse: "01" is not "1".
func explicitAdvisoryText(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "4", "true", "yes", "explicit", "e":
		return true
	default:
		return false
	}
}

// titleMarksExplicit reports a marker on the raw track title. Bracket
// forms match anywhere; the parenthetical forms match only at the end.
// Clean forms, and the word with no brackets, do not.
func titleMarksExplicit(title string) bool {
	if title == "" {
		return false
	}
	folded := strings.ToLower(title)
	if strings.Contains(folded, "[explicit version]") ||
		strings.Contains(folded, "[explicit]") ||
		strings.Contains(folded, "[e]") {
		return true
	}
	trimmed := strings.TrimSpace(folded)
	return strings.HasSuffix(trimmed, "(explicit version)") ||
		strings.HasSuffix(trimmed, "(explicit)")
}
