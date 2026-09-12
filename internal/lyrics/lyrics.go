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

	// The two NETWORK sources. Everything above is derived from a file the
	// operator has; these come from Atlas, which relays LRCLIB.
	SourceAtlasLRC Source = "atlas-lrc" // synced LRC from Atlas
	SourceAtlas    Source = "atlas"     // plain text from Atlas
)

// IsNetwork reports whether a document came from off this machine rather than
// from a file the operator has.
//
// Three behaviours key on this and none of them should be spelled as a string
// prefix at the call site: the scanner must not REAP such a row when a local
// extraction finds nothing (there is no local file whose absence proves
// anything), /v1/lyrics must not run the mtime drift check against an audio
// file the document never came from, and the row carries no sidecar to stat.
func (s Source) IsNetwork() bool {
	return s == SourceAtlasLRC || s == SourceAtlas
}

// Rank orders sources. A `.ttml` sidecar leads (word timing, agents,
// background vocals, translations — richer than any LRC), then `.lrc`,
// then the embedded synchronized tags, then LRC-shaped text, plain text,
// `.txt`. Mirror B2 of the app's PR-7: the phone's sidecar pick prefers
// `.ttml` in the same release, so the two sides never disagree about which
// file a track's lyrics come from.
//
// The two NETWORK tiers straddle the local ones rather than sitting below
// them, and that is the app's DD3 rule — TIMING OUTRANKS SOURCE — expressed
// in this ladder rather than restated beside it. Ranks 0-4 are the timed
// local documents and 6-7 the untimed ones, so `atlas-lrc` goes at 5: above
// every untimed document including the operator's own, because a synced
// lyric is a different product from an unsynced one, and below every timed
// local document, because between two timed documents the operator's file
// is the better authority. Plain `atlas` is last outright — it beats
// nothing, since any local document is at least as trustworthy and this one
// is a guess made from a tag.
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
	case SourceAtlasLRC:
		return 5
	case SourceTextPlain:
		return 6
	case SourceSidecarText:
		return 7
	case SourceAtlas:
		return 8
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
	// Across BOTH language fields on BOTH candidates: production sets
	// Candidate.Language and Doc.Language together, but Pick is exported and
	// nothing enforces that, so folding only one pair would reintroduce the
	// arrival-order dependence this function exists to remove (CodeRabbit).
	if lang := smallestNonEmpty(
		smallestNonEmpty(a.Language, a.Doc.Language),
		smallestNonEmpty(b.Language, b.Doc.Language),
	); lang != "" {
		kept.Language = lang
		kept.Doc.Language = lang
	}
	if name := smallestNonEmpty(a.SidecarName, b.SidecarName); name != "" {
		kept.SidecarName = name
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

// stripBOMs removes every U+FEFF in ONE pass, including the ones that only
// come into existence as it works.
//
// Two properties are load-bearing and neither is obvious:
//
//   - Deleting a U+FEFF splices its neighbours together and can form a NEW one
//     out of them. `"\xef\xbb" + BOM + "\xbf"` is the minimal case: strip the
//     middle BOM and the surrounding invalid bytes become `EF BB BF`.
//     FuzzNormalize found it within a minute of the target existing.
//   - The obvious answer — `strings.ReplaceAll` to a fixed point — is
//     QUADRATIC, because each pass rescans and copies the whole string while
//     exposing only one nesting level. Both PR bots flagged it independently
//     and both were right: `"\xef\xbb"×n + BOM + "\xbf"×n` needs n passes, and
//     Normalize's MaxBodyBytes check happens AFTER, so the input is unbounded
//     at that point. The USLT / ©lyr / Vorbis path in particular reaches here
//     with no size gate of any kind — dhowden hands over whatever the frame
//     declared.
//
// Appending byte by byte and truncating whenever the output ENDS in a BOM is
// linear and is a true fixed point: any complete `EF BB BF` in the output was
// a suffix at the moment its final byte landed, so it was removed then; and a
// truncation only shortens a suffix, so it can never splice a new one.
func stripBOMs(body string) string {
	const bom = "\uFEFF"
	if !strings.Contains(body, bom) {
		return body // the overwhelmingly common case, no allocation
	}
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i++ {
		out = append(out, body[i])
		if n := len(out); n >= 3 && out[n-3] == 0xEF && out[n-2] == 0xBB && out[n-1] == 0xBF {
			out = out[:n-3]
		}
	}
	return string(out)
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
	s := stripBOMs(body)
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
// `[mm:ss:xx]`, `[hh:mm:ss.xx]`, with full-width brackets accepted. wordTag
// and metaTag are the app's other two patterns, used by TimedCoverage to
// count lines the way its parse loop does.
var (
	lineTag  = regexp.MustCompile(`^\s*[\[［【]\s*-?\d{1,3}:\d{1,2}(?:[.,:]\d{1,3})?\s*[\]］】]`)
	hoursTag = regexp.MustCompile(`^\s*[\[［【]\s*-?\d{1,2}:\d{1,2}:\d{1,2}[.,]\d{1,3}\s*[\]］】]`)
	// `<mm:ss.xx>` / `(mm:ss.xx)` anywhere in a line (enhanced LRC / A2).
	wordTag = regexp.MustCompile(`[<(]\s*-?\d{1,3}:\d{1,2}(?:[.,:]\d{1,3})?\s*[>)]`)
	// `[key:value]`, key alphabetic — an ID tag only when the key is one the
	// app's `LRCParser.metadataKeys` names; any other is a section header.
	metaTag = regexp.MustCompile(`^\s*\[([A-Za-z][A-Za-z0-9_-]*):(.*)\]\s*$`)
)

// lrcMetadataKeys mirrors `LRCParser.metadataKeys`: the ID tags a reader must
// never show. A `[Chorus: Rihanna]` line matches metaTag's shape and is NOT
// here, so it counts as text — on both sides.
var lrcMetadataKeys = map[string]bool{
	"ar": true, "ti": true, "al": true, "au": true, "by": true, "length": true,
	"re": true, "ve": true, "tool": true, "id": true,
	"offset": true, "la": true, "lang": true, "language": true,
}

// The app's sparse-coverage rule (`LRCParser.timedCoverageIsTooSparse`, iOS
// #1759), constants included. A synced document needs at least
// MinimumTimedLines timed TEXT lines when the body also carries untimed
// ones, and the timed lines must be at least MinimumTimedShare of the
// non-blank text lines. Below either bar the timestamps are cue markers in a
// transcript — a Genius-style lyric sheet with one `[4:20]` — and the body
// is a PLAIN document. A body with no untimed text is never sparse: one
// timed `♪` is still an instrumental verdict.
//
// The two sides must classify one blob alike, so these are the app's numbers
// and the truth-table test is lifted from its test suite verbatim.
const (
	MinimumTimedLines = 2
	MinimumTimedShare = 0.25
)

// IsSyncedLRCBody is the app's whole synced-or-plain decision for an
// LRC-shaped body, in one place: `LooksLikeLRC`, then `parse`'s
// `!timed.isEmpty && !timedCoverageIsTooSparse(...)`. TextCandidate keys on
// it; the fuzz property asserts the verdict against it.
func IsSyncedLRCBody(body string) bool {
	if !LooksLikeLRC(body) {
		return false
	}
	timed, untimed := TimedCoverage(body)
	return timed > 0 && !TimedCoverageIsTooSparse(timed, untimed)
}

// TimedCoverageIsTooSparse is `LRCParser.timedCoverageIsTooSparse`, line for
// line: the guard, the count floor, then the share. Zero timed lines is NOT
// sparse here — the app decides `timed.isEmpty` before it ever asks; see
// IsSyncedLRCBody for the arm that carries that case.
func TimedCoverageIsTooSparse(timedLines, untimedLines int) bool {
	if untimedLines <= 0 || timedLines <= 0 {
		return false
	}
	if timedLines < MinimumTimedLines {
		return true
	}
	return float64(timedLines) < MinimumTimedShare*float64(timedLines+untimedLines)
}

// TimedCoverage counts a body's lines the way the app's parse loop does — the
// inputs to TimedCoverageIsTooSparse. A source line is TIMED when it carries
// at least one line tag and text survives after the line tags and any word
// tags are removed (an empty timed line is a clear event, not text); UNTIMED
// when it is non-blank, carries no line tag, and is not a known LRC ID tag.
// Blank lines count for nothing. Several tags on one line are one source
// line, as in the app, where each stamp yields a LyricLine but the coverage
// counter increments once.
//
// The body is expected normalized (LF line ends, no BOM, trailing whitespace
// trimmed) — every caller reaches here through Normalize.
func TimedCoverage(body string) (timed, untimed int) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rest, stamps := consumeLineTags(line)
		if stamps == 0 {
			if m := metaTag.FindStringSubmatch(line); m != nil && lrcMetadataKeys[strings.ToLower(m[1])] {
				continue
			}
			untimed++
			continue
		}
		if strings.TrimSpace(wordTag.ReplaceAllString(rest, "")) != "" {
			timed++
		}
	}
	return timed, untimed
}

// consumeLineTags strips every leading line tag — hours form first, as the
// app's consumeLineTag tries them — and returns the remainder with the count.
func consumeLineTags(line string) (rest string, n int) {
	rest = line
	for {
		loc := hoursTag.FindStringIndex(rest)
		if loc == nil {
			loc = lineTag.FindStringIndex(rest)
		}
		if loc == nil {
			return rest, n
		}
		rest = rest[loc[1]:]
		n++
	}
}

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

// TextCandidate classifies a text blob: LRC-shaped text that carries at
// least one timed TEXT line, and whose timed lines are not a sparse minority
// of its text, is a synced LRC document (`text-lrc`, or `vorbis-synced` when
// the tag itself claimed sync); anything else is plain text. Returns
// ok=false for an empty body.
//
// The rule sits HERE, in the classification, and LooksLikeLRC stays any-line
// — the same split as the app, whose `looksLikeLRC` is any-line and whose
// `parse` returns the plain document when `timed.isEmpty ||
// timedCoverageIsTooSparse(...)` (iOS #1759). BOTH arms are mirrored:
// TimedCoverageIsTooSparse is the app's function line for line, guard
// included, so it answers "not sparse" for zero timed lines — the app never
// asks it that, because `timed.isEmpty` is decided first. A body whose only
// tags are clear events (`Prose\n[00:12.00]`) therefore has to be caught by
// the `timed > 0` arm here, exactly as it is caught there; folding it into
// the guard instead would break the verbatim truth table (gemini on #904
// saw the outcome and proposed that patch).
//
// Before this, a transcript with one `[4:20]` cue became a `text-lrc` row
// with `synced: true`: rank 4, above the complete plain document (rank 6)
// in the same file's other frame, and a stored verdict the bridge itself
// could not stand behind. The phone was already protected — it re-parses
// the body and treats `synced` as advisory — so the fix is to the bridge's
// own election and to what it stores. A tag that CLAIMED sync (Vorbis
// SYNCEDLYRICS) over such a body is plain too: the app parses the body, not
// the tag name.
func TextCandidate(text, language string, taggedSynced bool, priority int) (Candidate, bool) {
	body, ok := Normalize(text)
	if !ok {
		return Candidate{}, false
	}
	if IsSyncedLRCBody(body) {
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
