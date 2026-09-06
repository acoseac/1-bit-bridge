// Package lyrics normalizes every lyrics source the scanner finds — ID3
// SYLT / USLT, Vorbis comments, MP4 ©lyr and sidecar files — into the ONE
// document shape `GET /v1/lyrics` serves. The iOS client re-parses the
// body with the same rules (LRC line + enhanced word tags, plain text), so
// the two sides stay in lockstep by construction; the parser fixtures are
// shared verbatim with the app's own parser tests.
package lyrics

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const (
	FormatLRC  = "lrc"
	FormatText = "text"
	FormatTTML = "ttml"

	// MaxBodyBytes bounds a body after normalization. Legitimate lyrics —
	// an opera libretto in enhanced LRC — stay well under 100 KiB; a
	// forged tag must not bloat the SQLite page cache or the manifest.
	MaxBodyBytes = 512 * 1024
)

// Doc is the wire document: `{format, synced, body, language}`.
type Doc struct {
	Format   string `json:"format"`
	Synced   bool   `json:"synced"`
	Body     string `json:"body"`
	Language string `json:"language,omitempty"`
}

// Source names where a document came from. Its Rank is the precedence
// (lower wins) — verified on the 2026-09-02 consult: a sidecar is the
// user's explicit override of a possibly read-only audio file; a dedicated
// synchronized tag beats an LRC-shaped unsynchronized one.
type Source string

const (
	SourceSidecarTTML  Source = "sidecar-ttml"
	SourceSidecarLRC   Source = "sidecar-lrc"
	SourceSYLT         Source = "sylt"
	SourceVorbisSynced Source = "vorbis-synced"
	SourceTextLRC      Source = "text-lrc" // USLT / ©lyr / LYRICS whose text is LRC-shaped
	SourceTextPlain    Source = "text"
	SourceSidecarText  Source = "sidecar-txt"
)

// Rank orders sources. A `.ttml` sidecar leads (word timing, agents,
// background vocals, translations — richer than any LRC), then `.lrc`,
// then the embedded synchronized tags, then LRC-shaped text, plain text,
// `.txt`. Mirror B2 of the app's PR-7: the phone's sidecar pick prefers
// `.ttml` in the same release, so the two sides never disagree about which
// file a track's lyrics come from.
func (s Source) Rank() int {
	switch s {
	case SourceSidecarTTML:
		return 0
	case SourceSidecarLRC:
		return 1
	case SourceSYLT:
		return 2
	case SourceVorbisSynced:
		return 3
	case SourceTextLRC:
		return 4
	case SourceTextPlain:
		return 5
	case SourceSidecarText:
		return 6
	}
	return 99
}

// Candidate is one source's document before precedence resolution.
type Candidate struct {
	Source   Source
	Doc      Doc
	Language string
	// Priority breaks ties inside one source: 0 = an empty ID3 descriptor,
	// 1 = a real descriptor, 2 = a junk descriptor ("Amazon", "Song ID"…).
	Priority int
	// SidecarName is the sidecar's file name (sidecar sources only).
	SidecarName string
}

// Pick returns the best candidate: lowest rank, then lowest priority, then
// the longest body, then a deterministic tail. Exact duplicate bodies collapse
// (dhowden's `Lyrics()` and the raw walk surface the same USLT twice).
func Pick(cands []Candidate) (Candidate, bool) {
	if len(cands) == 0 {
		return Candidate{}, false
	}
	seen := map[string]int{}
	out := cands[:0:0]
	for _, c := range cands {
		key := string(c.Source) + "\x00" + c.Doc.Body
		if i, dup := seen[key]; dup {
			out[i] = mergeDuplicate(out[i], c)
			continue
		}
		seen[key] = len(out)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return lessCandidate(out[i], out[j]) })
	return out[0], true
}

// mergeDuplicate folds a second sighting of the SAME document — same Source
// and Body, which is the whole dedup key — into the first. Every rule here is
// an ORDER-INDEPENDENT aggregation, and has to be: Pick folds sightings in
// whatever order they arrived, and they arrive partly from ranging over a Go
// map.
//
//   - The surviving Doc is chosen by fields this function never mutates:
//     synced first (a synchronized document is strictly more informative),
//     then format, purely as a stable tie-break. Choosing it with
//     lessCandidate looked right and was NOT — that comparator reads Priority,
//     which this function raises, so the accumulator's own mutation fed back
//     into the next comparison and three sightings folded to different answers
//     under different orders. FuzzPickIsShuffleInvariant found that; nothing
//     extractor-driven could have.
//   - Priority takes the MAXIMUM. dhowden's m.Lyrics() has no descriptor to
//     classify and is appended with a fabricated 0 — "empty descriptor", the
//     best rank there is — while returning the SAME *tag.Comm the raw walk
//     then re-reports with its real DescriptorPriority. Keeping the first
//     sighting let a junk descriptor ("Amazon", "Song ID") launder itself back
//     to 0 and defeat the junkExact / junkSubstring demotion entirely. Two
//     REAL frames with an identical body only tie-break against OTHER bodies,
//     where the pessimistic read is the safe one.
//   - Language takes the smallest non-empty, because m.Lyrics() drops the
//     frame's language while the raw walk keeps it. "First non-empty wins" is
//     not order-independent once two sightings disagree.
func mergeDuplicate(a, b Candidate) Candidate {
	kept, dup := a, b
	if (b.Doc.Synced && !a.Doc.Synced) ||
		(b.Doc.Synced == a.Doc.Synced && b.Doc.Format < a.Doc.Format) {
		kept, dup = b, a
	}
	if dup.Priority > kept.Priority {
		kept.Priority = dup.Priority
	}
	if lang := smallestNonEmpty(a.Language, b.Language); lang != "" {
		kept.Language = lang
		kept.Doc.Language = lang
	}
	return kept
}

// smallestNonEmpty is min() over the non-empty operands — commutative and
// associative, which is what makes the language merge fold-order-independent.
func smallestNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case b < a:
		return b
	}
	return a
}

// lessCandidate is a STRICT TOTAL order — deliberately total, not merely
// good enough to sort. The candidate slice is built partly by ranging over
// dhowden's `m.Raw()`, a Go map whose iteration order is randomised per run,
// so any pair the comparator calls equal is decided by chance. That is not a
// cosmetic wobble: an undecided pair flips the winner between scans, which
// re-keys lyricsTag, which bumps indexed_at, which pushes the track into every
// paired device's delta on every scan — the flapping-winner treadmill the
// duplicate elector is a strict total order to avoid.
func lessCandidate(a, b Candidate) bool {
	if ar, br := a.Source.Rank(), b.Source.Rank(); ar != br {
		return ar < br
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if la, lb := len(a.Doc.Body), len(b.Doc.Body); la != lb {
		return la > lb
	}
	// Nothing below expresses a preference — only that identical inputs always
	// produce the identical winner.
	if a.Doc.Body != b.Doc.Body {
		return a.Doc.Body < b.Doc.Body
	}
	if a.Doc.Language != b.Doc.Language {
		return a.Doc.Language < b.Doc.Language
	}
	if a.Doc.Format != b.Doc.Format {
		return a.Doc.Format < b.Doc.Format
	}
	if a.Doc.Synced != b.Doc.Synced {
		return a.Doc.Synced
	}
	return string(a.Source) < string(b.Source)
}

// Normalize makes the body deterministic before hashing and storage: every
// U+FEFF goes, CRLF / CR become LF, NFC, trailing spaces and tabs per line
// go, and the text ends in exactly one newline-free tail. Returns ok=false
// for an empty body or one past MaxBodyBytes.
//
// EVERY U+FEFF, not just a leading one, and that is a correctness requirement
// rather than tidiness: Normalize must be IDEMPOTENT, because resolveLyrics
// normalises a candidate body that TextCandidate or sidecarCandidate already
// normalised. Trimming only the prefix was not — `"\n\uFEFF"` normalised to
// `"\uFEFF"` (the BOM is not at index 0 on the first pass, and Go's
// unicode.IsSpace does NOT count U+FEFF, so TrimSpace keeps it), and the
// second pass then stripped it and REJECTED the now-empty body. A document
// accepted as a candidate would be silently dropped at resolve time. Found by
// FuzzNormalize within a minute of the target existing; a zero-width
// no-break space carries nothing in a lyrics document in any case.
func Normalize(body string) (string, bool) {
	// To a FIXED POINT, not once: deleting a U+FEFF splices its neighbours
	// together and can form a NEW one out of them. `"\xef\xbb" + BOM +
	// "\xbf"` is the minimal case — strip the middle BOM and the surrounding
	// invalid bytes become `EF BB BF`, which is a BOM. Each pass strictly
	// shortens the string, so this terminates. FuzzNormalize found both this
	// and the leading-only form it replaced.
	s := body
	for {
		stripped := strings.ReplaceAll(s, "\uFEFF", "")
		if stripped == s {
			break
		}
		s = stripped
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = norm.NFC.String(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.Join(lines, "\n")
	s = strings.TrimRight(s, "\n")
	s = strings.TrimLeft(s, "\n")
	if strings.TrimSpace(s) == "" || len(s) > MaxBodyBytes {
		return "", false
	}
	return s, true
}

// Tag is the first 8 lowercase hex of the SHA-256 over the CANONICAL
// document — format, synced flag, language and the normalized body, NUL-
// joined — the same short content-tag shape as waveformTag. Every client-
// visible field participates: a language-only or format-only change
// (same body, a `.lrc` replacing an identical USLT) must re-key the ETag
// and enter the manifest delta, not hide behind a body-only hash.
func Tag(doc Doc) string {
	synced := "0"
	if doc.Synced {
		synced = "1"
	}
	sum := sha256.Sum256([]byte(doc.Format + "\x00" + synced + "\x00" + doc.Language + "\x00" + doc.Body))
	return hex.EncodeToString(sum[:4])
}

// The iOS `LRCParser` line-tag shapes: `[mm:ss]`, `[mm:ss.xx]`, `[mm:ss,xx]`,
// `[mm:ss:xx]`, `[hh:mm:ss.xx]`, with full-width brackets accepted.
var (
	lineTag  = regexp.MustCompile(`^\s*[\[［【]\s*-?\d{1,3}:\d{1,2}(?:[.,:]\d{1,3})?\s*[\]］】]`)
	hoursTag = regexp.MustCompile(`^\s*[\[［【]\s*-?\d{1,2}:\d{1,2}:\d{1,2}[.,]\d{1,3}\s*[\]］】]`)
)

// LooksLikeLRC reports whether any line carries an LRC time tag — the
// promotion rule an unsynchronized text tag gets on both sides.
func LooksLikeLRC(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if lineTag.MatchString(line) || hoursTag.MatchString(line) {
			return true
		}
	}
	return false
}

// TextCandidate classifies a text blob: LRC-shaped text is a synced LRC
// document (`text-lrc`, or `vorbis-synced` when the tag itself claimed
// sync); anything else is plain text. Returns ok=false for an empty body.
func TextCandidate(text, language string, taggedSynced bool, priority int) (Candidate, bool) {
	body, ok := Normalize(text)
	if !ok {
		return Candidate{}, false
	}
	if LooksLikeLRC(body) {
		src := SourceTextLRC
		if taggedSynced {
			src = SourceVorbisSynced
		}
		return Candidate{Source: src, Doc: Doc{Format: FormatLRC, Synced: true, Body: body, Language: language},
			Language: language, Priority: priority}, true
	}
	return Candidate{Source: SourceTextPlain, Doc: Doc{Format: FormatText, Synced: false, Body: body, Language: language},
		Language: language, Priority: priority}, true
}

// Junk USLT descriptors some taggers stamp on every frame; they carry no
// selection signal (the iOS parser's list). Single words match the WHOLE
// descriptor only — "api" / "text" as substrings would demote "Rapid Verse"
// or "Context" (CodeRabbit on bridge #840); the multi-word tokens match
// anywhere.
var junkExact = map[string]bool{
	"lyrics": true, "unsynced": true, "default": true, "description": true,
	"text": true, "api": true,
}

var junkSubstring = []string{"song lyrics", "amazon", "song id"}

// DescriptorPriority ranks an ID3 USLT descriptor for Pick: 0 empty,
// 1 a real descriptor, 2 junk.
func DescriptorPriority(descriptor string) int {
	d := strings.ToLower(strings.TrimSpace(descriptor))
	if d == "" {
		return 0
	}
	if junkExact[d] {
		return 2
	}
	for _, junk := range junkSubstring {
		if strings.Contains(d, junk) {
			return 2
		}
	}
	return 1
}
