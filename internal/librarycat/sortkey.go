package librarycat

// Alphabetical sort keys and index buckets — the iOS AlphabetBucket
// mirror (com.acoseac.dsdplayer/AlphabetScrubber.swift).
//
// Two clients showing the same library must agree on where a name
// files, or the browser's A–Z index and the phone's scrubber disagree
// about the same artist. Same do-not-unify rule as genre.go.

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// sortKey mirrors AlphabetBucket.sortKey: dupes.SortName (the leading
// "the " strip) → NFD decomposition → drop combining marks → uppercase
// → keep only letters and digits.
//
// NFD rather than NFC is load-bearing: decomposition is what separates
// a base letter from its accent so "Éric" can lose the acute and file
// under E. The known gap, shared with the phone: ø ł đ æ are atomic
// code points with no decomposition, so "Bjørnstad" does NOT fold to
// "Bjornstad". Accepted on both sides — don't close it here alone.
func sortKey(s string) string {
	base := dupes.SortName(s)
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range norm.NFD.String(base) {
		if unicode.Is(unicode.Mn, r) {
			continue // combining mark from the decomposition
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// bucket mirrors AlphabetBucket.letter(for:): the first character of
// the sort key, A–Z, or "#" for anything else (digits, symbols, CJK,
// and the empty string).
func bucket(sortedKey string) string {
	for _, r := range sortedKey {
		if r >= 'A' && r <= 'Z' {
			return string(r)
		}
		return "#"
	}
	return "#"
}

// naturalCompare mirrors Swift's .numeric string comparison: digit
// runs compare as NUMBERS, not lexicographically, so "M2" sorts before
// "M83" and "Disc 2" before "Disc 10". Returns <0, 0 or >0.
//
// Operates on already-computed sort keys, which are uppercase
// alphanumerics only — so there is no case folding or separator
// handling to do here, and a digit run can't be split by punctuation.
func naturalCompare(a, b string) int {
	ar, br := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ar) && j < len(br) {
		ad, bd := isDigit(ar[i]), isDigit(br[j])
		if ad && bd {
			// Compare whole digit runs numerically. Leading zeros are
			// skipped so "007" == "7"; equal values then fall through
			// to the next segment rather than tie-breaking on width.
			si, sj := i, j
			for i < len(ar) && isDigit(ar[i]) {
				i++
			}
			for j < len(br) && isDigit(br[j]) {
				j++
			}
			if c := compareDigitRun(ar[si:i], br[sj:j]); c != 0 {
				return c
			}
			continue
		}
		if ar[i] != br[j] {
			if ar[i] < br[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case i < len(ar):
		return 1
	case j < len(br):
		return -1
	default:
		return 0
	}
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// compareDigitRun compares two digit runs as unsigned integers of
// unbounded length — no Atoi, so a 40-digit catalogue number in a
// title can't overflow into a wrong ordering.
func compareDigitRun(a, b []rune) int {
	a = trimLeadingZeros(a)
	b = trimLeadingZeros(b)
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	for k := range a {
		if a[k] != b[k] {
			if a[k] < b[k] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func trimLeadingZeros(r []rune) []rune {
	i := 0
	for i < len(r)-1 && r[i] == '0' {
		i++
	}
	return r[i:]
}
