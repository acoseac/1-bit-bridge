package smartplaylist

import (
	"strconv"
	"strings"
)

// Camelot wheel mapping + harmonic-compatibility scoring for Auto Mix.
//
// The Camelot wheel labels each key 1..12 with a letter: A = minor (inner
// ring), B = major (outer ring). Numbers advance by perfect fifths, so two
// keys mix harmonically when they are the same, relatives (8A↔8B), or
// adjacent on the wheel (8A↔7A / 8A↔9A). Two "energy" moves are also
// included per the Gemini consult (2026-06-14): the +2-same-letter step
// (8A→10A, builds energy) and the diagonal +1-number-with-mode-flip
// (8A→9B). Both carry a higher step-cost than a direct lock.

// Camelot is a position on the wheel: Num 1..12, Minor=true → "A".
type Camelot struct {
	Num   int
	Minor bool
}

// camelotMajor / camelotMinor map keyRoot (0..11, C=0) → Camelot number for
// the major / minor mode. Verified against the canonical wheel.
//
//	C  C# D  D# E  F  F# G  G# A  A# B
var camelotMajor = [12]int{8, 3, 10, 5, 12, 7, 2, 9, 4, 11, 6, 1}
var camelotMinor = [12]int{5, 12, 7, 2, 9, 4, 11, 6, 1, 8, 3, 10}

// ToCamelot resolves a (keyRoot, mode) into a wheel position. ok=false for an
// out-of-range root or an unrecognised mode (the track can't be sequenced
// harmonically and is dropped from the Auto Mix pool). Exported so the admin
// console's harmonic-coverage wheel maps the same (root, mode) → wheel code
// the sequencer uses — single source of truth.
func ToCamelot(keyRoot int, mode string) (Camelot, bool) {
	if keyRoot < 0 || keyRoot > 11 {
		return Camelot{}, false
	}
	switch mode {
	case "minor":
		return Camelot{Num: camelotMinor[keyRoot], Minor: true}, true
	case "major":
		return Camelot{Num: camelotMajor[keyRoot], Minor: false}, true
	default:
		return Camelot{}, false
	}
}

// FromCamelot is the inverse of ToCamelot: it parses a wheel code like
// "8A" / "8B" back into the (keyRoot, mode) pair stored in track_analysis.
// Letter A = minor, B = major; input is upper-cased + trimmed so "8a" works.
// ok=false for a malformed code, an out-of-range number, or an unknown
// letter. Used by the admin Library Inspector's "filter by harmonic key"
// deep-link from the coverage wheel — the same single-source mapping as
// ToCamelot, so a clicked wheel segment and the rows it filters always
// agree. camelotMinor/camelotMajor are permutations of 1..12, so the
// reverse lookup is an unambiguous bijection.
func FromCamelot(code string) (keyRoot int, mode string, ok bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) < 2 {
		return 0, "", false
	}
	letter := code[len(code)-1]
	num, err := strconv.Atoi(code[:len(code)-1])
	if err != nil || num < 1 || num > 12 {
		return 0, "", false
	}
	var table *[12]int
	switch letter {
	case 'A':
		table, mode = &camelotMinor, "minor"
	case 'B':
		table, mode = &camelotMajor, "major"
	default:
		return 0, "", false
	}
	for root, n := range table {
		if n == num {
			return root, mode, true
		}
	}
	return 0, "", false // unreachable: tables cover every number 1..12
}

// camelotCircDiff is the circular distance between two wheel numbers (1..12),
// so 12 and 1 are 1 apart.
func camelotCircDiff(a, b int) int {
	d := a - b
	if d < 0 {
		d = -d
	}
	if 12-d < d {
		return 12 - d
	}
	return d
}

// compatibilityCost ranks a transition prev→next: lower is smoother.
//
//	0 — identical key
//	1 — relative major/minor (same number, opposite letter) OR adjacent (same letter, ±1)
//	2 — energy move: +2 same letter, OR diagonal (opposite letter, ±1)
//	3 — incompatible (excluded from the primary sequencing pass)
func compatibilityCost(a, b Camelot) int {
	if a.Num == b.Num && a.Minor == b.Minor {
		return 0
	}
	sameLetter := a.Minor == b.Minor
	cd := camelotCircDiff(a.Num, b.Num)
	switch {
	case a.Num == b.Num && !sameLetter: // relative
		return 1
	case sameLetter && cd == 1: // adjacent
		return 1
	case sameLetter && cd == 2: // +2 energy
		return 2
	case !sameLetter && cd == 1: // diagonal energy
		return 2
	default:
		return 3
	}
}
