// Log export: filter the bridge's on-disk log by minimum level and age,
// optionally redacting host-identifying detail, and stream the result as a
// download.
//
// # Why this parses text rather than reading structured records
//
// logging.Init installs slog's TEXT handler on stderr, and the service units
// redirect stdout AND stderr to one file. So the log is a mix: structured
// records (`time=… level=INFO msg=… component=…`) interleaved with the
// unstructured startup banner the CLI prints to stdout (version line, library
// name, TLS fingerprint block). There is no second, structured copy to read —
// the file IS the log — so filtering means parsing it back.
//
// TextHandler always emits `time=` then `level=` as the first two fields, in
// that order, which makes a strict prefix parse both cheap and self-limiting:
// a line that does not start that way is unstructured by definition, and is
// treated as such rather than guessed at.
package admin

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// maxLogScanBytes bounds how much of the log one export reads.
//
// Nothing rotates the bridge's log today, so the file is unbounded and a
// long-lived bridge can carry hundreds of MiB. The scan therefore starts from
// the END minus this budget rather than at byte zero: the operator asking for
// "the last hour" wants the tail, and reading forward from the start of a
// 500 MiB file to find it would be pure waste.
//
// The cost is that "Everything" on a very large log returns the last 64 MiB,
// not all of it — which is why exportStats carries Truncated and every writer
// emits a footer saying so. A silent partial export would be the worse bug.
const maxLogScanBytes = 64 << 20

// maxLogLineBytes caps a single line. A panic record carries a full
// debug.Stack() (TextHandler quotes it, so it stays one physical line), which
// comfortably exceeds bufio.Scanner's 64 KiB default — that default would have
// aborted the scan at the first panicking-file record, i.e. exactly the line an
// operator is exporting the log to find.
const maxLogLineBytes = 4 << 20

// logLevel orders the slog levels for "this level and above" filtering.
//
// Ordered, not a set, deliberately. Exact-level matching lets an operator
// export "warnings" and silently omit every ERROR — the worst events missing
// from the file they are about to attach to a bug report.
type logLevel int

const (
	levelDebug logLevel = iota
	levelInfo
	levelWarn
	levelError
	// levelNone marks a line with no parseable level: the CLI banner that
	// shares this file. Not a severity — see keepUnstructured.
	levelNone
)

// parseLogLevelName maps the wire vocabulary (the `level=` filter value) onto
// the ordering. Unknown values are rejected by the caller rather than silently
// defaulting, so a typo'd query can't quietly widen an export.
func parseLogLevelName(s string) (logLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "all", "": // "" = the UI's default "everything"
		return levelDebug, true
	case "info":
		return levelInfo, true
	case "warn", "warning":
		return levelWarn, true
	case "error":
		return levelError, true
	}
	return levelDebug, false
}

// levelFromRecord maps a slog TextHandler level token onto the ordering.
//
// Handles the offset form TextHandler emits for non-standard levels
// ("WARN+1", "ERROR+4") by matching on the base name, so a custom level sorts
// with its neighbours instead of falling through to levelNone and being
// mistaken for banner text.
func levelFromRecord(tok string) logLevel {
	switch {
	case strings.HasPrefix(tok, "ERROR"):
		return levelError
	case strings.HasPrefix(tok, "WARN"):
		return levelWarn
	case strings.HasPrefix(tok, "INFO"):
		return levelInfo
	case strings.HasPrefix(tok, "DEBUG"):
		return levelDebug
	}
	return levelNone
}

// parseLogLine extracts the level and timestamp from one line.
//
// Returns levelNone when the line is not a structured record. Both fields are
// read as a strict prefix — `time=<value> level=<value> ` — because that is
// what TextHandler guarantees; scanning the whole line for "level=" instead
// would match the text inside a quoted msg.
func parseLogLine(line string) (logLevel, time.Time) {
	rest, ok := strings.CutPrefix(line, "time=")
	if !ok {
		return levelNone, time.Time{}
	}
	tsTok, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return levelNone, time.Time{}
	}
	lvlTok, ok := strings.CutPrefix(rest, "level=")
	if !ok {
		return levelNone, time.Time{}
	}
	if i := strings.IndexByte(lvlTok, ' '); i >= 0 {
		lvlTok = lvlTok[:i]
	}
	lvl := levelFromRecord(lvlTok)
	if lvl == levelNone {
		return levelNone, time.Time{}
	}
	// RFC3339Nano covers TextHandler's millisecond form. A record whose
	// timestamp will not parse still counts as structured — its level is
	// usable, and only the age filter degrades (see logFilter.keep).
	ts, err := time.Parse(time.RFC3339Nano, tsTok)
	if err != nil {
		return lvl, time.Time{}
	}
	return lvl, ts
}

// logFilter decides which lines survive an export.
type logFilter struct {
	// minLevel keeps this level and every more severe one.
	minLevel logLevel
	// since drops records older than this instant. Zero = no age bound.
	since time.Time
	// redact rewrites host-identifying detail. See redactLogLine.
	redact bool

	// lastTS carries the most recent parsed timestamp forward so an
	// unstructured line can be age-filtered by its neighbours. Banner lines
	// have no clock of their own, and dropping them from a time-bounded
	// export purely for that reason would lose the version and library
	// header that makes the rest interpretable.
	lastTS time.Time
}

// keepUnstructured reports whether banner lines belong in this export.
//
// Only when no level filter is active. Under "warnings and above" a banner
// line is not an event of that severity, so carrying it would misrepresent the
// filter; under "Everything" it is exactly the context the operator wants. The
// bundle sidesteps the question by recording version/platform in its own
// header rather than relying on the banner surviving.
func (f *logFilter) keepUnstructured() bool { return f.minLevel == levelDebug }

// keep applies the filter to one line, returning the text to emit.
func (f *logFilter) keep(line string) (string, bool) {
	lvl, ts := parseLogLine(line)
	if lvl == levelNone {
		if !f.keepUnstructured() {
			return "", false
		}
		// Age-gate by the previous record's clock. Unknown (nothing parsed
		// yet — banner at the very top of the scan) keeps the line: at worst
		// it adds a few lines of header to a windowed export.
		if !f.since.IsZero() && !f.lastTS.IsZero() && f.lastTS.Before(f.since) {
			return "", false
		}
		return f.render(line), true
	}
	if !ts.IsZero() {
		f.lastTS = ts
	}
	if lvl < f.minLevel {
		return "", false
	}
	// An unparseable timestamp fails OPEN against the age bound: the record's
	// severity already qualified it, and dropping it for a malformed clock
	// would hide exactly the corrupted-line case worth seeing.
	if !f.since.IsZero() && !ts.IsZero() && ts.Before(f.since) {
		return "", false
	}
	return f.render(line), true
}

func (f *logFilter) render(line string) string {
	if f.redact {
		return redactLogLine(line)
	}
	return line
}

var (
	// quotedPathRe matches an absolute path inside a quoted value, to the
	// CLOSING QUOTE rather than to the first space.
	//
	// This is the common case and it has to come first. TextHandler quotes any
	// value containing a space, and music paths contain spaces constantly
	// ("Album (Deluxe)", "The Dark Side of the Moon") — so the unquoted rule
	// below, which stops at whitespace, would redact `/Music/Album` and leave
	// ` (Deluxe)/track.flac` sitting in the output. Matching to the quote
	// removes the whole value.
	quotedPathRe = regexp.MustCompile(`"(?:/|[A-Za-z]:\\)[^"]*"`)
	// absPathRe matches a POSIX absolute path in a position where a path can
	// legitimately start: line start, whitespace, `=`, a quote, or an opening
	// bracket / paren / comma.
	//
	// `:` is deliberately NOT in the prefix class — that is what keeps this
	// off the "//" of a URL, whose preceding character is always the scheme
	// colon. The bracket and comma had to be ADDED: the startup banner prints
	// its roots as `(roots: [/srv/Music /srv/More])`, so a class of only
	// whitespace/quote/equals matched none of them and the banner leaked
	// every library path (caught by the handler tests, not by review).
	//
	// The path body stays greedy over `]` and `)`. A real album directory
	// contains parens — `/Music/Album (Deluxe)/track.flac` — and stopping at
	// one would leave the tail of the path visible, which is a worse failure
	// than swallowing a closing bracket.
	absPathRe = regexp.MustCompile(`(^|[\s="\[(,])(/[^\s"=]*/[^\s"=]*)`)
	// winPathRe matches a drive-letter path (`C:\Users\…`).
	winPathRe = regexp.MustCompile(`(^|[\s="\[(,])([A-Za-z]:\\[^\s"=]*)`)
	// ipv4Re matches a dotted quad. Four octets, so version strings like
	// "0.1.8" cannot match.
	ipv4Re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	// ipv6Re matches a bracketed IPv6 literal, the form that appears in the
	// bridge's endpoint and RemoteAddr logging.
	ipv6Re = regexp.MustCompile(`\[[0-9a-fA-F:]{2,}(?:%[0-9a-zA-Z._-]+)?\]`)
)

// redactLogLine removes the two classes of host-identifying detail the
// bridge's own privacy commitment names: absolute filesystem paths and IP
// addresses.
//
// Deliberately CONSERVATIVE about what it is. This makes a log safe to paste
// into a public issue in the ordinary case; it is not an anonymiser, and the
// UI says so rather than implying a guarantee. Library-RELATIVE paths
// (`path=Artist/Album/track.flac`) are kept — they carry no host or account
// detail and are usually the whole reason the line is being read.
//
// Loopback addresses survive on purpose: 127.0.0.1 and ::1 identify nobody,
// and blanking them would make the admin/listener lines unreadable for no
// privacy gain.
// Known limitation, stated rather than hidden: an UNQUOTED absolute path
// containing a space leaves its tail behind, because the unquoted rule stops
// at whitespace and there is no closing delimiter to run to. In practice the
// only such text is the startup banner's `(roots: [/srv/My Music])` — every
// structured record quotes a spaced value and takes the quoted rule. Widening
// the unquoted rule past whitespace would swallow the rest of the LINE, which
// is the worse failure: it would eat the log message itself.
func redactLogLine(line string) string {
	// Quoted first: it is the more specific rule, and once it has replaced a
	// value there is no '/' left inside for the unquoted rules to re-match.
	line = quotedPathRe.ReplaceAllString(line, `"<path>"`)
	line = absPathRe.ReplaceAllString(line, "${1}<path>")
	line = winPathRe.ReplaceAllString(line, "${1}<path>")
	line = ipv4Re.ReplaceAllStringFunc(line, func(ip string) string {
		if strings.HasPrefix(ip, "127.") || ip == "0.0.0.0" {
			return ip
		}
		return "<ip>"
	})
	line = ipv6Re.ReplaceAllStringFunc(line, func(ip string) string {
		if ip == "[::1]" || ip == "[::]" {
			return ip
		}
		return "<ip>"
	})
	return line
}

// exportStats is what the footer reports.
type exportStats struct {
	// Scanned / Emitted are line counts, so a filter that produced nothing
	// reads as "0 of 12,431" rather than an empty file with no explanation.
	Scanned int
	Emitted int
	// Truncated is set when the scan started mid-file because the log
	// exceeded maxLogScanBytes.
	Truncated bool
	// LineTooLong is set when a record exceeded maxLogLineBytes and the scan
	// stopped there. Surfaced rather than swallowed: the export is short, and
	// the operator needs to know why.
	LineTooLong bool
}

// streamFilteredLog writes the filtered log to w and returns what it did.
//
// The caller has already set response headers, so a mid-stream failure cannot
// change the status code — which is why the footer, not the HTTP status,
// carries the truncation signal.
func streamFilteredLog(w io.Writer, path string, f *logFilter) (exportStats, error) {
	var st exportStats
	file, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return st, err
	}
	if info.Size() > maxLogScanBytes {
		if _, err := file.Seek(info.Size()-maxLogScanBytes, io.SeekStart); err != nil {
			return st, err
		}
		st.Truncated = true
	}

	br := bufio.NewReaderSize(file, 256<<10)
	if st.Truncated {
		// The seek landed mid-record. Discard to the next newline so the
		// first emitted line is a whole one — a half line would fail the
		// prefix parse and be misread as banner text.
		if _, err := br.ReadString('\n'); err != nil && err != io.EOF {
			return st, err
		}
	}

	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLineBytes)
	bw := bufio.NewWriterSize(w, 64<<10)
	for sc.Scan() {
		st.Scanned++
		if out, ok := f.keep(sc.Text()); ok {
			st.Emitted++
			if _, err := bw.WriteString(out); err != nil {
				return st, err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return st, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		if err == bufio.ErrTooLong {
			st.LineTooLong = true
		} else {
			_ = bw.Flush()
			return st, err
		}
	}
	return st, bw.Flush()
}

// writeExportFooter appends the provenance every exported log carries.
//
// Always emitted, including on a clean full export: an operator reading the
// file later — or a maintainer reading it in an issue — needs to know whether
// they are holding the whole log, and whether it was redacted, without having
// to ask.
func writeExportFooter(w io.Writer, st exportStats, f *logFilter, sinceLabel string) {
	fmt.Fprintf(w, "\n# --- 1-bit-bridge log export ---\n")
	fmt.Fprintf(w, "# generated:  %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "# level:      %s and above\n", levelLabel(f.minLevel))
	fmt.Fprintf(w, "# period:     %s\n", sinceLabel)
	fmt.Fprintf(w, "# redacted:   %t", f.redact)
	if f.redact {
		fmt.Fprintf(w, " (absolute paths and non-loopback IPs replaced; not an anonymiser)")
	}
	fmt.Fprintf(w, "\n# lines:      %d emitted of %d scanned\n", st.Emitted, st.Scanned)
	if st.Truncated {
		fmt.Fprintf(w, "# TRUNCATED:  log exceeds %d MiB; only the most recent %d MiB were scanned\n",
			maxLogScanBytes>>20, maxLogScanBytes>>20)
	}
	if st.LineTooLong {
		fmt.Fprintf(w, "# TRUNCATED:  a record exceeded %d MiB and the scan stopped there\n",
			maxLogLineBytes>>20)
	}
}

func levelLabel(l logLevel) string {
	switch l {
	case levelError:
		return "error"
	case levelWarn:
		return "warn"
	case levelInfo:
		return "info"
	default:
		return "debug"
	}
}

// parseSincePreset maps the UI's relative-period vocabulary onto a lower
// bound, plus a human label for the footer.
//
// Presets rather than absolute datetimes: the question an operator actually
// asks is "what happened just now", and a relative window answers it without a
// date picker or a timezone to get wrong. An unknown value is rejected by the
// caller — quietly falling back to "everything" would hand back a far larger
// file than was asked for.
func parseSincePreset(s string, now time.Time) (time.Time, string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all":
		return time.Time{}, "everything on disk", true
	case "15m":
		return now.Add(-15 * time.Minute), "last 15 minutes", true
	case "1h":
		return now.Add(-time.Hour), "last hour", true
	case "24h":
		return now.Add(-24 * time.Hour), "last 24 hours", true
	case "7d":
		return now.Add(-7 * 24 * time.Hour), "last 7 days", true
	}
	return time.Time{}, "", false
}
