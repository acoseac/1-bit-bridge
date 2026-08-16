package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realLogSample is captured from an actual `bridge serve` run, not
// hand-authored.
//
// That matters more than it looks. The log is a MIX — the service units send
// stdout and stderr to one file — so it interleaves the CLI's unstructured
// startup banner with slog TextHandler records. A hand-written fixture
// reproduces whatever shape its author believed; this one reproduces what the
// bridge actually writes, including the banner's blank line and the quoted
// msg= values containing spaces and '='.
const realLogSample = `1-bit-bridge 0.0.1 (protocol v1) — listening on https://127.0.0.1:17788
Library: "Upgrade Test" (roots: [/Users/arsenie/medialibtest])
Admin console: http://127.0.0.1:17789/ — add library folders, pair devices, view stats
time=2026-08-16T21:05:52.080+02:00 level=INFO msg="console listening" component=admin url=http://127.0.0.1:17789/
time=2026-08-16T21:05:55.766+02:00 level=INFO msg=http component=http request_id=1da7582ddf2846b5 method=GET path=/v1/health status=200
time=2026-08-16T21:05:59.587+02:00 level=ERROR msg=artwork component=enricher mbid=daf82b80 err="dial tcp 207.241.224.2:443: i/o timeout"
time=2026-08-16T21:07:30.982+02:00 level=WARN msg=http component=http request_id=887bcd68 method=PUT status=409
time=2026-08-16T21:07:33.180+02:00 level=DEBUG msg="cache probe" component=enricher`

func linesOf(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestParseLogLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		want    logLevel
		wantTS  bool
		comment string
	}{
		{"info record", `time=2026-08-16T21:05:52.080+02:00 level=INFO msg=hi`, levelInfo, true, ""},
		{"error record", `time=2026-08-16T21:05:59.587+02:00 level=ERROR msg=boom`, levelError, true, ""},
		{"warn record", `time=2026-08-16T21:07:30.982+02:00 level=WARN msg=x`, levelWarn, true, ""},
		{"debug record", `time=2026-08-16T21:07:33.180+02:00 level=DEBUG msg=x`, levelDebug, true, ""},
		{"banner line", `1-bit-bridge 0.0.1 (protocol v1) — listening`, levelNone, false, ""},
		{"blank", ``, levelNone, false, ""},
		{
			"msg containing level= must not be parsed as the level",
			`time=2026-08-16T21:05:52.080+02:00 level=INFO msg="rejected level=ERROR from peer"`,
			levelInfo, true,
			"a whole-line search for level= would find the one inside the quoted msg",
		},
		{
			"offset level sorts with its base",
			`time=2026-08-16T21:05:52.080+02:00 level=WARN+1 msg=x`,
			levelWarn, true,
			"TextHandler prints non-standard levels as BASE+N",
		},
		{
			"structured but unparseable timestamp keeps its level",
			`time=not-a-time level=ERROR msg=x`,
			levelError, false,
			"severity is still usable; only the age filter degrades",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lvl, ts := parseLogLine(tc.line)
			if lvl != tc.want {
				t.Errorf("level = %v, want %v (%s)", lvl, tc.want, tc.comment)
			}
			if got := !ts.IsZero(); got != tc.wantTS {
				t.Errorf("timestamp present = %v, want %v", got, tc.wantTS)
			}
		})
	}
}

// TestLogFilterMinLevelIncludesMoreSevere is the invariant the whole
// minimum-level design exists for: an export of "warnings" must never omit an
// ERROR. Exact-level matching passes every other test in this file and fails
// this one.
func TestLogFilterMinLevelIncludesMoreSevere(t *testing.T) {
	f := &logFilter{minLevel: levelWarn}
	var kept []string
	for _, line := range linesOf(realLogSample) {
		if out, ok := f.keep(line); ok {
			kept = append(kept, out)
		}
	}
	joined := strings.Join(kept, "\n")
	if !strings.Contains(joined, "level=ERROR") {
		t.Error("minLevel=warn dropped the ERROR record — this is the failure the " +
			"ordering exists to prevent; an operator exporting 'warnings' would " +
			"hand over a file with the worst events missing")
	}
	if !strings.Contains(joined, "level=WARN") {
		t.Error("minLevel=warn dropped the WARN record")
	}
	for _, unwanted := range []string{"level=INFO", "level=DEBUG"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("minLevel=warn kept %s", unwanted)
		}
	}
}

// TestLogFilterDropsBannerUnderLevelFilter pins keepUnstructured: banner text
// is not an event of any severity, so a level-filtered export must not carry
// it — while "everything" must.
func TestLogFilterDropsBannerUnderLevelFilter(t *testing.T) {
	countBanner := func(min logLevel) int {
		f := &logFilter{minLevel: min}
		n := 0
		for _, line := range linesOf(realLogSample) {
			if out, ok := f.keep(line); ok && strings.HasPrefix(out, "1-bit-bridge") {
				n++
			}
		}
		return n
	}
	if got := countBanner(levelDebug); got != 1 {
		t.Errorf("everything-mode kept %d banner lines, want 1 — the version header "+
			"is exactly the context an unfiltered export should carry", got)
	}
	if got := countBanner(levelWarn); got != 0 {
		t.Errorf("warn-and-above kept %d banner lines, want 0", got)
	}
}

func TestLogFilterSinceDropsOlderRecords(t *testing.T) {
	cut, err := time.Parse(time.RFC3339Nano, "2026-08-16T21:07:00.000+02:00")
	if err != nil {
		t.Fatal(err)
	}
	f := &logFilter{minLevel: levelDebug, since: cut}
	var kept []string
	for _, line := range linesOf(realLogSample) {
		if out, ok := f.keep(line); ok && strings.HasPrefix(out, "time=") {
			kept = append(kept, out)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d records, want 2 (the 21:07:30 WARN and 21:07:33 DEBUG)\n%s",
			len(kept), strings.Join(kept, "\n"))
	}
	for _, line := range kept {
		if !strings.Contains(line, "21:07:") {
			t.Errorf("kept a record from before the cut: %s", line)
		}
	}
}

// TestLogFilterKeepsRecordWithUnparseableTimestamp pins the fail-OPEN
// direction: a corrupted clock must not hide a qualifying record, because
// that is exactly the line worth seeing.
func TestLogFilterKeepsRecordWithUnparseableTimestamp(t *testing.T) {
	f := &logFilter{minLevel: levelDebug, since: time.Now()}
	if _, ok := f.keep(`time=garbled level=ERROR msg=x`); !ok {
		t.Error("dropped an ERROR whose timestamp would not parse; the age bound " +
			"must fail open so a corrupted line is visible rather than silently gone")
	}
}

func TestRedactLogLine(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         string
		wantGone   []string
		wantKept   []string
		wantSubstr string
	}{
		{
			name:     "posix absolute path",
			in:       `time=T level=INFO msg=x path=/Users/arsenie/Music/a.flac`,
			wantGone: []string{"/Users/arsenie"},
			wantKept: []string{"level=INFO"},
		},
		{
			name:     "windows drive path",
			in:       `time=T level=INFO msg=x path=C:\Users\me\Music\a.flac`,
			wantGone: []string{`C:\Users`},
		},
		{
			name:     "public ip",
			in:       `time=T level=ERROR msg=x err="dial tcp 207.241.224.2:443: timeout"`,
			wantGone: []string{"207.241.224.2"},
		},
		{
			name:     "lan ip",
			in:       `time=T level=INFO msg=x addr=192.168.0.42`,
			wantGone: []string{"192.168.0.42"},
		},
		{
			// Loopback identifies nobody, and blanking it makes the
			// listener lines unreadable for no privacy gain.
			name:     "loopback survives",
			in:       `time=T level=INFO msg=x url=http://127.0.0.1:7789/`,
			wantKept: []string{"127.0.0.1"},
		},
		{
			// Four octets are required, so a version can never match.
			name:     "version string is not an ip",
			in:       `time=T level=INFO msg=x version=0.1.8`,
			wantKept: []string{"0.1.8"},
		},
		{
			// A library-relative path carries no host or account detail and
			// is usually the whole reason the line is being read.
			name:     "relative library path survives",
			in:       `time=T level=INFO msg=x path=Artist/Album/track.flac`,
			wantKept: []string{"Artist/Album/track.flac"},
		},
		{
			name:     "url host is not mistaken for a path",
			in:       `time=T level=INFO msg=x url=https://archive.org/download/mbid-x`,
			wantKept: []string{"https://archive.org"},
		},
		{
			// The startup banner's shape. A prefix class of only
			// whitespace/quote/equals matched none of these, so every
			// library root leaked through a "redacted" export.
			name:     "bracketed roots list in the startup banner",
			in:       `Library: "Test" (roots: [/srv/Music /srv/More])`,
			wantGone: []string{"/srv/Music", "/srv/More"},
		},
		{
			// How a spaced path ACTUALLY appears: TextHandler quotes any
			// value containing a space, and music paths contain them
			// constantly. The unquoted rule stops at the space and would
			// leave " (Deluxe)/t.flac" behind, so the quoted rule must run
			// to the closing quote instead.
			name:     "quoted path with spaces is fully removed",
			in:       `time=T level=INFO msg=x path="/Users/me/Music/Album (Deluxe)/t.flac"`,
			wantGone: []string{"/Users/me", "Deluxe", "t.flac"},
			wantKept: []string{"level=INFO"},
		},
		{
			// Parens without spaces take the unquoted rule, which is greedy
			// over ')' precisely so the tail cannot survive.
			name:     "unquoted path with parens but no spaces",
			in:       `time=T level=INFO msg=x path=/Users/me/Music/Album(Deluxe)/t.flac`,
			wantGone: []string{"/Users/me", "Deluxe", "t.flac"},
		},
		{
			// The documented limitation, pinned so it cannot silently get
			// WORSE: an unquoted spaced path loses its head but keeps its
			// tail. Only the startup banner produces this shape.
			name:     "unquoted spaced path loses its head (known limitation)",
			in:       `Library: "Test" (roots: [/srv/My Music])`,
			wantGone: []string{"/srv/My"},
		},
		{
			name:     "ipv6 literal",
			in:       `time=T level=INFO msg=x addr=[fd22:b3fa:2e94::2]:17788`,
			wantGone: []string{"fd22:b3fa"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactLogLine(tc.in)
			for _, s := range tc.wantGone {
				if strings.Contains(got, s) {
					t.Errorf("redaction left %q in:\n%s", s, got)
				}
			}
			for _, s := range tc.wantKept {
				if !strings.Contains(got, s) {
					t.Errorf("redaction removed %q, which should survive:\n%s", s, got)
				}
			}
		})
	}
}

func writeTempLog(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bridge.log")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStreamFilteredLog(t *testing.T) {
	path := writeTempLog(t, realLogSample+"\n")
	var buf bytes.Buffer
	f := &logFilter{minLevel: levelWarn}
	st, err := streamFilteredLog(&buf, path, f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Scanned != 8 {
		t.Errorf("scanned %d lines, want 8", st.Scanned)
	}
	if st.Emitted != 2 {
		t.Errorf("emitted %d lines, want 2 (the ERROR and the WARN)\n%s", st.Emitted, buf.String())
	}
	if st.Truncated {
		t.Error("small file reported as truncated")
	}
}

// TestStreamFilteredLogTruncatesFromTheEnd pins that an oversized log is read
// from its TAIL, not its head. Reading forward from byte zero would return the
// OLDEST 64 MiB — the opposite of what every caller wants.
func TestStreamFilteredLogTruncatesFromTheEnd(t *testing.T) {
	var sb strings.Builder
	// SEVERAL distinctive early lines, not one.
	//
	// With a single early marker this test passed even against a scan that
	// started at byte zero: Truncated was still set, so the partial-line skip
	// discarded exactly that first line and the "oldest is absent" assertion
	// was satisfied by the wrong mechanism. Five markers cannot all be eaten
	// by a one-line skip, so the assertion now fails when the scan starts at
	// the head — which is the property being pinned. (Found by negative
	// control, not review.)
	for i := 0; i < 5; i++ {
		sb.WriteString("time=2026-08-16T21:00:00.000+02:00 level=ERROR msg=OLDEST_MARKER\n")
	}
	filler := "time=2026-08-16T21:01:00.000+02:00 level=ERROR msg=" + strings.Repeat("f", 512) + "\n"
	for sb.Len() < maxLogScanBytes+(1<<20) {
		sb.WriteString(filler)
	}
	sb.WriteString("time=2026-08-16T21:59:00.000+02:00 level=ERROR msg=NEWEST_MARKER\n")

	path := writeTempLog(t, sb.String())
	var buf bytes.Buffer
	st, err := streamFilteredLog(&buf, path, &logFilter{minLevel: levelError})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Truncated {
		t.Fatal("oversized log not reported as truncated")
	}
	out := buf.String()
	if !strings.Contains(out, "NEWEST_MARKER") {
		t.Error("the most recent record is missing — the scan must start from the END")
	}
	if strings.Contains(out, "OLDEST_MARKER") {
		t.Error("the oldest record survived a truncating scan, so the scan started " +
			"at byte zero; a windowed export would return stale lines")
	}
}

// TestStreamFilteredLogSkipsPartialFirstLine: seeking into the middle of a
// record leaves a fragment that would fail the prefix parse and be misread as
// banner text — and, under "everything", emitted as a corrupt half line.
func TestStreamFilteredLogSkipsPartialFirstLine(t *testing.T) {
	var sb strings.Builder
	filler := "time=2026-08-16T21:01:00.000+02:00 level=INFO msg=" + strings.Repeat("f", 4096) + "\n"
	for sb.Len() < maxLogScanBytes+(1<<20) {
		sb.WriteString(filler)
	}
	path := writeTempLog(t, sb.String())

	var buf bytes.Buffer
	// Everything-mode, which is the only mode that would emit a fragment.
	st, err := streamFilteredLog(&buf, path, &logFilter{minLevel: levelDebug})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Truncated {
		t.Fatal("expected a truncating scan")
	}
	first, _, _ := strings.Cut(buf.String(), "\n")
	if !strings.HasPrefix(first, "time=") {
		t.Errorf("first emitted line is a fragment, not a whole record:\n%.120q", first)
	}
}

func TestParseSincePreset(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in       string
		wantZero bool
		wantAgo  time.Duration
		wantOK   bool
	}{
		{"", true, 0, true},
		{"all", true, 0, true},
		{"15m", false, 15 * time.Minute, true},
		{"1h", false, time.Hour, true},
		{"24h", false, 24 * time.Hour, true},
		{"7d", false, 7 * 24 * time.Hour, true},
		{"nonsense", true, 0, false},
		{"3h", true, 0, false},
	} {
		got, _, ok := parseSincePreset(tc.in, now)
		if ok != tc.wantOK {
			t.Errorf("%q: ok = %v, want %v — an unknown preset must be REJECTED, "+
				"not defaulted to everything", tc.in, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if tc.wantZero {
			if !got.IsZero() {
				t.Errorf("%q: want no lower bound, got %v", tc.in, got)
			}
			continue
		}
		if want := now.Add(-tc.wantAgo); !got.Equal(want) {
			t.Errorf("%q: got %v, want %v", tc.in, got, want)
		}
	}
}

func TestParseLogLevelNameRejectsUnknown(t *testing.T) {
	if _, ok := parseLogLevelName("nonsense"); ok {
		t.Error("accepted an unknown level; a typo must not silently widen an export")
	}
	for _, s := range []string{"", "all", "debug", "info", "warn", "warning", "error", "ERROR"} {
		if _, ok := parseLogLevelName(s); !ok {
			t.Errorf("rejected valid level %q", s)
		}
	}
}

// TestTailRingKeepsMostRecent pins the ring's purpose — the bundle wants the
// END of the window — and that it reassembles lines split across Write calls,
// which bufio does routinely since it flushes on buffer size, not newlines.
func TestTailRingKeepsMostRecent(t *testing.T) {
	r := &tailRing{max: 3}
	for _, s := range []string{"a\n", "b\n", "c\n", "d\n", "e\n"} {
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(r.lines(), ",")
	if got != "c,d,e" {
		t.Errorf("lines = %q, want \"c,d,e\" (the most recent 3)", got)
	}
}

func TestTailRingReassemblesSplitWrites(t *testing.T) {
	r := &tailRing{max: 5}
	// One logical line delivered in three chunks, none newline-aligned.
	for _, s := range []string{"time=T lev", "el=INFO msg=sp", "lit\nnext\n"} {
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	lines := r.lines()
	if len(lines) != 2 || lines[0] != "time=T level=INFO msg=split" {
		t.Errorf("lines = %q, want the reassembled record then \"next\"", lines)
	}
}

func TestTailRingFlushesTrailingPartialLine(t *testing.T) {
	r := &tailRing{max: 5}
	if _, err := r.Write([]byte("no trailing newline")); err != nil {
		t.Fatal(err)
	}
	if got := r.lines(); len(got) != 1 || got[0] != "no trailing newline" {
		t.Errorf("lines = %q, want the unterminated final line to survive", got)
	}
}
