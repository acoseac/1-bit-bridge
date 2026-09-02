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
	SourceSidecarLRC   Source = "sidecar-lrc"
	SourceSYLT         Source = "sylt"
	SourceVorbisSynced Source = "vorbis-synced"
	SourceTextLRC      Source = "text-lrc" // USLT / ©lyr / LYRICS whose text is LRC-shaped
	SourceSidecarTTML  Source = "sidecar-ttml"
	SourceTextPlain    Source = "text"
	SourceSidecarText  Source = "sidecar-txt"
)

// Rank orders sources; TTML sits below the synced LRC-shaped sources until
// clients parse it (the app's TTML parser is a later PR — a bridge picking
// a `.ttml` over a `.lrc` today would hand the phone nothing it can show).
func (s Source) Rank() int {
	switch s {
	case SourceSidecarLRC:
		return 0
	case SourceSYLT:
		return 1
	case SourceVorbisSynced:
		return 2
	case SourceTextLRC:
		return 3
	case SourceSidecarTTML:
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
// the longest body. Exact duplicate bodies collapse (dhowden's `Lyrics()`
// and the raw walk surface the same USLT twice).
func Pick(cands []Candidate) (Candidate, bool) {
	if len(cands) == 0 {
		return Candidate{}, false
	}
	seen := map[string]int{}
	out := cands[:0:0]
	for _, c := range cands {
		key := string(c.Source) + "\x00" + c.Doc.Body
		if i, dup := seen[key]; dup {
			// Same document twice (dhowden's Lyrics() accessor drops the
			// frame's language; the raw walk keeps it) — keep the richer.
			if out[i].Language == "" && c.Language != "" {
				out[i] = c
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Source.Rank() != b.Source.Rank() {
			return a.Source.Rank() < b.Source.Rank()
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return len(a.Doc.Body) > len(b.Doc.Body)
	})
	return out[0], true
}

// Normalize makes the body deterministic before hashing and storage: the
// UTF-8 BOM goes, CRLF / CR become LF, NFC, trailing spaces and tabs per
// line go, and the text ends in exactly one newline-free tail. Returns
// ok=false for an empty body or one past MaxBodyBytes.
func Normalize(body string) (string, bool) {
	s := strings.TrimPrefix(body, "\uFEFF")
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

// Tag is the first 8 lowercase hex of sha256(normalized body) — the same
// short content-tag shape as waveformTag.
func Tag(normalizedBody string) string {
	sum := sha256.Sum256([]byte(normalizedBody))
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

// junkDescriptors are USLT descriptors some taggers stamp on every frame;
// they carry no selection signal (the iOS parser's list).
var junkDescriptors = map[string]bool{
	"lyrics": true, "unsynced": true, "default": true, "description": true,
	"song lyrics": true, "text": true, "api": true, "amazon": true, "song id": true,
}

// DescriptorPriority ranks an ID3 USLT descriptor for Pick.
func DescriptorPriority(descriptor string) int {
	d := strings.ToLower(strings.TrimSpace(descriptor))
	if d == "" {
		return 0
	}
	for junk := range junkDescriptors {
		if strings.Contains(d, junk) {
			return 2
		}
	}
	return 1
}
