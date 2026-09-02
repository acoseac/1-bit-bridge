package lyrics

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// SYLTEntry is one synchronized text entry.
type SYLTEntry struct {
	Millis int64
	Text   string
}

// SYLT is a parsed ID3v2 synchronized-lyrics frame BODY (the bytes
// dhowden/tag leaves under "SYLT" / "SLT" in m.Raw(), header already
// consumed).
type SYLT struct {
	Language    string
	ContentType byte
	Descriptor  string
	Entries     []SYLTEntry
}

const (
	syltTimeFormatMPEGFrames = 1
	syltTimeFormatMillis     = 2

	// WholeLineMedianGapMillis mirrors the iOS parser's
	// `entriesLookLikeWholeLines`: marker-less entries this far apart
	// (median) are whole lines, not syllables.
	WholeLineMedianGapMillis = 1500
)

// ParseSYLT decodes a frame body. ok=false for the MPEG-frame time format
// (converting it needs the MP3 bitstream — dropped, like the iOS parser),
// content types other than lyrics (1) / transcription (2), or a malformed
// body. Encodings 0–3; a per-entry BOM is stripped (some writers stamp one
// on every entry in encoding 1).
func ParseSYLT(body []byte) (SYLT, bool) {
	if len(body) < 6 {
		return SYLT{}, false
	}
	enc := body[0]
	if enc > 3 {
		return SYLT{}, false
	}
	lang := strings.TrimRight(string(body[1:4]), "\x00")
	timeFormat := body[4]
	contentType := body[5]
	if timeFormat != syltTimeFormatMillis || (contentType != 1 && contentType != 2) {
		return SYLT{}, false
	}
	rest := body[6:]
	descriptor, rest, ok := readTerminated(rest, enc)
	if !ok {
		// No terminator anywhere in the remainder — no entry could be read
		// from it either (every entry needs one), so this is malformed.
		return SYLT{}, false
	}
	var entries []SYLTEntry
	for len(rest) > 0 {
		text, after, ok := readTerminated(rest, enc)
		if !ok {
			break
		}
		if len(after) < 4 {
			break
		}
		ms := int64(binary.BigEndian.Uint32(after[:4]))
		rest = after[4:]
		entries = append(entries, SYLTEntry{Millis: ms, Text: text})
	}
	if len(entries) == 0 {
		return SYLT{}, false
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Millis < entries[j].Millis })
	return SYLT{Language: lang, ContentType: contentType, Descriptor: descriptor, Entries: entries}, true
}

// readTerminated pulls one NUL-terminated string in `enc`; 2-byte,
// pair-aligned terminators for the UTF-16 encodings.
func readTerminated(b []byte, enc byte) (string, []byte, bool) {
	switch enc {
	case 1, 2:
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return decodeUTF16(b[:i], enc), b[i+2:], true
			}
		}
		return "", nil, false
	default:
		for i := 0; i < len(b); i++ {
			if b[i] == 0 {
				return decodeSingleByte(b[:i], enc), b[i+1:], true
			}
		}
		return "", nil, false
	}
}

func decodeSingleByte(b []byte, enc byte) string {
	if enc == 3 {
		s := strings.TrimPrefix(string(b), "\uFEFF")
		if !utf8.ValidString(s) {
			return strings.ToValidUTF8(s, "�")
		}
		return s
	}
	// ISO-8859-1: every byte is the code point.
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

func decodeUTF16(b []byte, enc byte) string {
	bigEndian := enc == 2
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			bigEndian, b = false, b[2:]
		case b[0] == 0xFE && b[1] == 0xFF:
			bigEndian, b = true, b[2:]
		}
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return strings.TrimPrefix(string(utf16.Decode(units)), "\uFEFF")
}

func hasNewlineMarker(s string) bool {
	return strings.HasPrefix(s, "\n") || strings.HasPrefix(s, "\r\n") ||
		strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\r\n")
}

// EntriesLookLikeWholeLines mirrors the iOS rule VERBATIM: no entry carries
// a newline marker, at least two textual entries, and either the median
// inter-entry gap is ≥ WholeLineMedianGapMillis or at least half the
// entries carry an internal space.
func EntriesLookLikeWholeLines(entries []SYLTEntry) bool {
	for _, e := range entries {
		if hasNewlineMarker(e.Text) {
			return false
		}
	}
	var textual []SYLTEntry
	for _, e := range entries {
		if strings.TrimSpace(e.Text) != "" {
			textual = append(textual, e)
		}
	}
	if len(textual) < 2 {
		return false
	}
	gaps := make([]int64, 0, len(textual)-1)
	for i := 1; i < len(textual); i++ {
		gaps = append(gaps, textual[i].Millis-textual[i-1].Millis)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	if gaps[len(gaps)/2] >= WholeLineMedianGapMillis {
		return true
	}
	spaced := 0
	for _, e := range textual {
		if strings.ContainsFunc(strings.TrimSpace(e.Text), unicode.IsSpace) {
			spaced++
		}
	}
	return spaced*2 >= len(textual)
}

func lrcTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d.%03d", ms/60000, (ms/1000)%60, ms%1000)
}

// splitMarkers strips EVERY leading / trailing newline marker from an entry
// (a writer that doubles them must not leak a raw line break into a timed
// LRC line, which the phone's parser would drop as a tagless continuation)
// and reports which sides carried one. Interior newlines become spaces for
// the same reason.
func splitMarkers(text string) (body string, leading, trailing bool) {
	leading = strings.HasPrefix(text, "\n") || strings.HasPrefix(text, "\r\n")
	trailing = strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r\n")
	body = strings.Trim(text, "\r\n")
	body = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(body)
	return body, leading, trailing
}

// renderLine serializes one assembled line: a single entry is a plain timed
// line, a run of syllables carries enhanced word tags.
func renderLine(entries []SYLTEntry) (line string, wordTimed bool) {
	if len(entries) == 1 {
		return "[" + lrcTime(entries[0].Millis) + "]" + entries[0].Text, false
	}
	var b strings.Builder
	b.WriteString("[" + lrcTime(entries[0].Millis) + "]")
	for _, e := range entries {
		b.WriteString("<" + lrcTime(e.Millis) + ">" + e.Text)
	}
	return b.String(), true
}

// ToLRC renders a frame as LRC with the SAME line-assembly rules the iOS
// SYLT parser applies: a leading newline on an entry starts a line, a
// trailing one ends it, marker-less line-spaced entries are one line each,
// and a run of syllables becomes ONE line carrying enhanced `<mm:ss.xxx>`
// word tags (so the phone recovers word timing through its LRC parser). A
// whitespace-only entry between lines is a clear event — an empty timed
// line. Returns wordTimed=true when any line carries word tags.
func ToLRC(s SYLT) (string, bool) {
	wholeLine := EntriesLookLikeWholeLines(s.Entries)
	var lines []string
	var current []SYLTEntry
	wordTimed := false
	flush := func() {
		if len(current) == 0 {
			return
		}
		line, word := renderLine(current)
		lines = append(lines, line)
		wordTimed = wordTimed || word
		current = nil
	}
	nextStartsLine := true
	for _, e := range s.Entries {
		text, leading, trailing := splitMarkers(e.Text)
		startsLine := nextStartsLine || wholeLine || leading
		nextStartsLine = trailing
		if startsLine {
			flush()
		}
		if strings.TrimSpace(text) != "" {
			current = append(current, SYLTEntry{Millis: e.Millis, Text: text})
			continue
		}
		// A truly EMPTY entry is a clear event — it ends whatever line is
		// open and renders as an empty timed line (the LRC shape the phone
		// reads as "nothing is sung now"). The dummy "" at 0 ms many
		// writers prepend is not: nothing precedes it. A bare newline entry
		// is only a line break.
		if !leading && !trailing {
			flush()
			if e.Millis > 0 || len(lines) > 0 {
				lines = append(lines, "["+lrcTime(e.Millis)+"]")
			}
		}
		nextStartsLine = true
	}
	flush()
	return strings.Join(lines, "\n"), wordTimed
}
