package manifest

import (
	"strings"

	"github.com/dhowden/tag"
)

// genreFromTag is the genre a tag contributes to a row.
//
// An ID3 genre is read through dhowden/tag's Genre, which expands one
// numeric reference per pass and copies the string each time. A crafted
// TCON of a few thousand in-range references spends seconds there
// (measured: 4,000 copies of "(0)" took 6.5s). The bridge asks it only
// while the frame holds at most maxID3v2GenreRefs references, which is
// past any genre a tagger writes, and then stores exactly what Genre
// returns. More references than that are not a genre: the field is left
// empty and Genre is not called. Every other format's Genre is not that
// walk, and is stored as before.
func genreFromTag(m tag.Metadata) string {
	if m == nil {
		return ""
	}
	if raw, ok := id3GenreFrame(m); ok && id3GenreRefCount(raw) > maxID3v2GenreRefs {
		return ""
	}
	return strings.TrimSpace(m.Genre())
}

// maxID3v2GenreRefs is the most "(n)" references an ID3 genre may carry
// and still be expanded. A real tag names a handful; 32 is the bound
// past which the expansion stops being a genre and starts being the
// quadratic walk.
const maxID3v2GenreRefs = 32

// id3GenreFrame is the raw genre frame Genre reads: TCO in v2.2, TCON
// in v2.3 and v2.4. ok is false for every other format, and for an ID3
// tag that carries no such frame.
func id3GenreFrame(m tag.Metadata) (string, bool) {
	var key string
	switch m.Format() {
	case tag.ID3v2_2:
		key = "TCO"
	case tag.ID3v2_3, tag.ID3v2_4:
		key = "TCON"
	default:
		return "", false
	}
	s, ok := m.Raw()[key].(string)
	return s, ok
}

// id3GenreRefCount counts "(digits)" groups in a genre frame. dhowden
// expands one in-range group per pass, so the count is the bound on
// that walk. The scan is one pass over the frame.
func id3GenreRefCount(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '(' {
			continue
		}
		j := i + 1
		if j >= len(s) || s[j] < '0' || s[j] > '9' {
			continue
		}
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j < len(s) && s[j] == ')' {
			n++
			i = j
		}
	}
	return n
}
