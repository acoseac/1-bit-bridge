package manifest

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The shared playlist parse-case table (backlog B431, the app's B430):
// testdata/playlist-parse-cases.tsv is the iOS app's file, copied byte for
// byte. Its header states the columns, the escapes and the rules. These tests
// read it the way the app's PlaylistParseCaseTableTests does: split on LF and
// TAB, unescape to bytes, and compare the parser's output with the expected
// columns byte for byte.

// playlistCaseTableRows is the number of rows the iOS copy holds at a9a16365.
const playlistCaseTableRows = 29

type playlistCaseRow struct {
	name       string
	playlist   string
	input      []byte
	resolved   [][]byte
	unresolved [][]byte
	line       int
}

// unescapePlaylistCase decodes one escaped column: \n, \r, \t, \\ and \xNN.
// Any other byte stands for itself, and any other escape is an error.
func unescapePlaylistCase(field string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(field); i++ {
		if field[i] != '\\' {
			out = append(out, field[i])
			continue
		}
		if i+1 >= len(field) {
			return nil, fmt.Errorf("a trailing backslash in %q", field)
		}
		switch field[i+1] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case '\\':
			out = append(out, '\\')
		case 'x':
			if i+3 >= len(field) {
				return nil, fmt.Errorf("a bad \\x escape in %q", field)
			}
			b, err := strconv.ParseUint(field[i+2:i+4], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("a bad \\x escape in %q", field)
			}
			out = append(out, byte(b))
			i += 2
		default:
			return nil, fmt.Errorf("an unknown escape in %q", field)
		}
		i++
	}
	return out, nil
}

// playlistCaseList decodes a list column: split on "|" before unescaping (a
// "|" in a path is \x7C). An empty column is an empty list.
func playlistCaseList(field string) ([][]byte, error) {
	if field == "" {
		return nil, nil
	}
	var out [][]byte
	for _, part := range strings.Split(field, "|") {
		b, err := unescapePlaylistCase(part)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func readPlaylistCaseRows(t *testing.T) []playlistCaseRow {
	t.Helper()
	raw, err := os.ReadFile("testdata/playlist-parse-cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.IndexByte(raw, '\r') >= 0 {
		t.Fatal("the table holds a CR byte: it was checked out with CRLF line ends. " +
			".gitattributes pins it to LF; the table escapes every CR it means as \\r")
	}
	var rows []playlistCaseRow
	sawHeader := false
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "case\tplaylist\tinput\tresolved\tunresolved" {
			sawHeader = true
			continue
		}
		cols := strings.Split(line, "\t")
		if !sawHeader || len(cols) != 5 {
			t.Fatalf("line %d: %d columns, or no header above it", i+1, len(cols))
		}
		playlist, err := unescapePlaylistCase(cols[1])
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		input, err := unescapePlaylistCase(cols[2])
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		resolved, err := playlistCaseList(cols[3])
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		unresolved, err := playlistCaseList(cols[4])
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		rows = append(rows, playlistCaseRow{
			name: cols[0], playlist: string(playlist), input: input,
			resolved: resolved, unresolved: unresolved, line: i + 1,
		})
	}
	return rows
}

// showPlaylistCase renders byte lists for a failure message.
func showPlaylistCase(list [][]byte) string {
	parts := make([]string, len(list))
	for i, b := range list {
		parts[i] = strconv.QuoteToASCII(string(b))
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

// runPlaylistCase is what the indexer does with one file: the parse, then
// the path rule on each line, against the playlist's own folder.
func runPlaylistCase(playlist string, input []byte) (resolved, unresolved [][]byte) {
	lines, _ := parsePlaylistLines(input)
	dir := playlistFileDir(playlist)
	for _, line := range lines {
		if p, ok := playlistEntryPath(dir, line); ok {
			resolved = append(resolved, []byte(p))
		} else {
			unresolved = append(unresolved, []byte(line))
		}
	}
	return resolved, unresolved
}

// TestPlaylistParseCaseTableUnescape checks the decoder the rows rely on: a
// broken one would make every row compare the wrong bytes.
func TestPlaylistParseCaseTableUnescape(t *testing.T) {
	got, err := unescapePlaylistCase(`a\n\r\t\\\x00\xC3\xA9\x7C`)
	if err != nil || !bytes.Equal(got, []byte{0x61, 0x0A, 0x0D, 0x09, 0x5C, 0x00, 0xC3, 0xA9, 0x7C}) {
		t.Fatalf("unescape = %v, %v", got, err)
	}
	for _, bad := range []string{`\q`, `\xZ1`, `a\`, `\x1`} {
		if _, err := unescapePlaylistCase(bad); err == nil {
			t.Errorf("unescape(%q) took a bad escape", bad)
		}
	}
	list, err := playlistCaseList(`a\x7Cb|c`)
	if err != nil || len(list) != 2 || string(list[0]) != "a|b" || string(list[1]) != "c" {
		t.Fatalf("list = %q, %v", list, err)
	}
	if list, err := playlistCaseList(""); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %q, %v", list, err)
	}
}

// TestPlaylistParseCaseTableCoversEveryRequiredForm checks the copy is the
// table the app passes: every row parsed, unique names, the forms the app's
// test requires, in the bytes as well as the names.
func TestPlaylistParseCaseTableCoversEveryRequiredForm(t *testing.T) {
	rows := readPlaylistCaseRows(t)
	if len(rows) != playlistCaseTableRows {
		t.Fatalf("premise: %d rows parsed, want %d", len(rows), playlistCaseTableRows)
	}
	names := map[string]bool{}
	for _, r := range rows {
		if names[r.name] {
			t.Errorf("case %q appears twice", r.name)
		}
		names[r.name] = true
	}
	for _, want := range []string{
		"utf8-lf-extinf-two-deep", "utf8-no-bom-non-ascii", "utf8-bom-then-directive",
		"utf8-bom-then-path", "cp1252-crlf-accents", "crlf", "cr-only",
		"hash-lines-are-not-entries", "blank-and-whitespace-lines", "dot-slash", "dot-dot",
		"climb-above-the-share-root", "climb-above-then-back-down", "nfc-name-kept", "nfd-name-kept",
		"nfc-and-nfd-stay-two-paths", "duplicates-kept-in-order", "backslash-separators",
		"utf8-bom-then-invalid-utf8", "cp1251-ascii-and-cyrillic",
	} {
		if !names[want] {
			t.Errorf("a required form has no row: %s", want)
		}
	}
	bom := []byte{0xEF, 0xBB, 0xBF}
	has := func(what string, pred func(playlistCaseRow) bool) {
		t.Helper()
		for _, r := range rows {
			if pred(r) {
				return
			}
		}
		t.Errorf("no row holds %s", what)
	}
	has("a UTF-8 BOM", func(r playlistCaseRow) bool { return bytes.HasPrefix(r.input, bom) })
	has("CR line ends and no LF", func(r playlistCaseRow) bool {
		return bytes.IndexByte(r.input, '\r') >= 0 && bytes.IndexByte(r.input, '\n') < 0
	})
	has("bytes that are not UTF-8", func(r playlistCaseRow) bool { return !utf8.Valid(r.input) })
	has("a byte Windows-1252 leaves undefined (B437)", func(r playlistCaseRow) bool {
		return !utf8.Valid(r.input) && bytes.ContainsAny(r.input, "\x81\x8d\x8f\x90\x9d")
	})
	has("a BOM before bytes that are not UTF-8 (B437)", func(r playlistCaseRow) bool {
		return bytes.HasPrefix(r.input, bom) && !utf8.Valid(r.input[len(bom):])
	})
	has("a line the path rule drops", func(r playlistCaseRow) bool { return len(r.unresolved) > 0 })
}

// TestPlaylistParseCaseTable runs every row through the indexer's parser and
// path rule and compares the output with the row byte for byte.
func TestPlaylistParseCaseTable(t *testing.T) {
	checked := 0
	for _, r := range readPlaylistCaseRows(t) {
		resolved, unresolved := runPlaylistCase(r.playlist, r.input)
		if showPlaylistCase(resolved) != showPlaylistCase(r.resolved) {
			t.Errorf("line %d %s: resolved %s, want %s", r.line, r.name,
				showPlaylistCase(resolved), showPlaylistCase(r.resolved))
		}
		if showPlaylistCase(unresolved) != showPlaylistCase(r.unresolved) {
			t.Errorf("line %d %s: unresolved %s, want %s", r.line, r.name,
				showPlaylistCase(unresolved), showPlaylistCase(r.unresolved))
		}
		checked++
	}
	if checked != playlistCaseTableRows {
		t.Fatalf("premise: %d rows ran, want %d", checked, playlistCaseTableRows)
	}
}

// The tests below pin what the indexer does where the table has no row. Each
// expectation is what the app's parser (PlaylistImport.swift at iOS a9a16365)
// answered for the same bytes, measured with that file compiled on macOS 27
// (Swift 6.4, Foundation): the table leaves these out, and the two parsers
// agreeing on them is still the point.

// lineEntries is the parse and the path rule, as strings, for a playlist at
// Playlists/mix.m3u.
func lineEntries(input string) (resolved, unresolved []string) {
	r, u := runPlaylistCase("Playlists/mix.m3u", []byte(input))
	for _, b := range r {
		resolved = append(resolved, string(b))
	}
	for _, b := range u {
		unresolved = append(unresolved, string(b))
	}
	return resolved, unresolved
}

func TestPlaylistParserEndsLinesWhereTheAppDoes(t *testing.T) {
	// NEL, LINE SEPARATOR and PARAGRAPH SEPARATOR end a line, as LF, CR and
	// CRLF do. The table's header names the first two; the app's splitter
	// (NSString line enumeration) takes U+2029 too.
	for _, sep := range []string{"\n", "\r", "\r\n", "\u0085", "\u2028", "\u2029"} {
		got, _ := lineEntries("a.flac" + sep + "b.flac")
		if strings.Join(got, "|") != "Playlists/a.flac|Playlists/b.flac" {
			t.Errorf("%q: %q, want two lines", sep, got)
		}
	}
	// Nothing else does: these stay inside the line.
	for _, sep := range []string{"\v", "\f", "\x1c", "\x1d", "\x1e", "\x1f", "\x00"} {
		got, _ := lineEntries("a.flac" + sep + "b.flac")
		if len(got) != 1 || got[0] != "Playlists/a.flac"+sep+"b.flac" {
			t.Errorf("%q: %q, want one line", sep, got)
		}
	}
}

func TestPlaylistParserTrimsWhatTheAppTrims(t *testing.T) {
	// Unicode White_Space, which is the app's whitespacesAndNewlines set.
	for _, r := range []rune{0x09, 0x0B, 0x0C, 0x20, 0x85, 0xA0, 0x1680, 0x2000, 0x2005, 0x200A,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000} {
		got, _ := lineEntries(string(r) + "a.flac" + string(r))
		if len(got) != 1 || got[0] != "Playlists/a.flac" {
			t.Errorf("U+%04X: %q, want it trimmed", r, got)
		}
	}
	// Format and control characters are not trimmed.
	for _, r := range []rune{0x00, 0x1C, 0x1F, 0x7F, 0x180E, 0x200B, 0x200C, 0x200D} {
		got, _ := lineEntries(string(r) + "a.flac" + string(r))
		if len(got) != 1 || got[0] != "Playlists/"+string(r)+"a.flac"+string(r) {
			t.Errorf("U+%04X: %q, want it kept", r, got)
		}
	}
	// A blank line of other spaces is no entry.
	if got, _ := lineEntries("   \u3000  \n\u00a0\n"); len(got) != 0 {
		t.Errorf("blank lines: %q", got)
	}
}

func TestPlaylistParserDropsOneMoreBOMAfterTheBytes(t *testing.T) {
	bom := "\xEF\xBB\xBF"
	// The app strips one BOM as bytes, and its UTF-8 decoder drops one more
	// at the start of the text; a third is kept.
	if got, _ := lineEntries(bom + bom + "a.flac\n"); strings.Join(got, "|") != "Playlists/a.flac" {
		t.Errorf("two BOMs: %q", got)
	}
	if got, _ := lineEntries(bom + bom + bom + "a.flac\n"); strings.Join(got, "|") != "Playlists/\uFEFFa.flac" {
		t.Errorf("three BOMs: %q", got)
	}
	// A BOM inside the file is a character of its line.
	if got, _ := lineEntries("a.flac\n" + bom + "b.flac\n"); strings.Join(got, "|") != "Playlists/a.flac|Playlists/\uFEFFb.flac" {
		t.Errorf("a BOM mid-file: %q", got)
	}
	// Windows-1252 has no BOM: past the stripped one, EF BB BF are three letters.
	if got, _ := lineEntries(bom + bom + "a.flac\n\xE9\n"); strings.Join(got, "|") != "Playlists/\u00EF\u00BB\u00BFa.flac|Playlists/\u00E9" {
		t.Errorf("two BOMs in a Windows-1252 file: %q", got)
	}
	if got, _ := lineEntries(bom); len(got) != 0 {
		t.Errorf("a BOM alone: %q", got)
	}
}

func TestPlaylistPathRuleBeyondTheTable(t *testing.T) {
	for _, c := range []struct {
		line, want string
		placed     bool
	}{
		{"/abs/x.flac", "abs/x.flac", true},                  // from the share root
		{"//abs//x.flac", "abs/x.flac", true},                // empty segments collapse
		{"/./x.flac", "x.flac", true},                        //
		{"/../x.flac", "", false},                            // above the root from the root
		{"/", "", true},                                      // the root itself
		{".", "Playlists", true},                             // the playlist's folder
		{"..", "", true},                                     // the root, from one below
		{"../..", "", false},                                 //
		{"a/", "Playlists/a", true},                          //
		{".../x.flac", "Playlists/.../x.flac", true},         // three dots are a name
		{"C:/x.flac", "", false},                             // a drive letter
		{`C:\x.flac`, "", false},                             // folded first
		{"C:/", "", false},                                   //
		{"C:", "Playlists/C:", true},                         // too short to be a drive
		{"c:x.flac", "Playlists/c:x.flac", true},             // no slash after the colon
		{"1:/x.flac", "Playlists/1:/x.flac", true},           // not a letter
		{"\u00e9:/x.flac", "Playlists/\u00e9:/x.flac", true}, // not an ASCII letter
		{"http://x/y.flac", "", false},                       // a URL
		{"file:///x.flac", "", false},                        //
		{"dir/a://b.flac", "", false},                        // "://" anywhere in a relative line
		{"/x://y.flac", "x:/y.flac", true},                   // but not in an absolute one
		{`\\nas\music\a.flac`, "nas/music/a.flac", true},     // a UNC path reads as absolute
		{"a%20b.flac", "Playlists/a%20b.flac", true},         // nothing is percent-decoded
		{"a#b.flac", "Playlists/a#b.flac", true},             // "#" inside a line is a character
	} {
		got, dropped := lineEntries(c.line + "\n")
		switch {
		case c.placed && (len(got) != 1 || got[0] != c.want || len(dropped) != 0):
			t.Errorf("%q: placed %q dropped %q, want %q", c.line, got, dropped, c.want)
		case !c.placed && (len(got) != 0 || len(dropped) != 1):
			t.Errorf("%q: placed %q dropped %q, want it dropped", c.line, got, dropped)
		}
	}
	// A playlist at the root has nothing above it.
	r, u := runPlaylistCase("mix.m3u", []byte("..\n.\n"))
	if showPlaylistCase(r) != showPlaylistCase([][]byte{[]byte("")}) || showPlaylistCase(u) != showPlaylistCase([][]byte{[]byte("..")}) {
		t.Errorf("root playlist: placed %s dropped %s", showPlaylistCase(r), showPlaylistCase(u))
	}
}

func TestPlaylistFileDisplayNameIsTheAppsName(t *testing.T) {
	// The app names an imported playlist (entry.name as NSString)
	// .deletingPathExtension: the file name without its last extension.
	for path, want := range map[string]string{
		"Playlists/Road Trip.m3u8":  "Road Trip",
		"Mix.m3u":                   "Mix",
		"a/My.Mix.m3u8":             "My.Mix",
		"a/Mix.M3U":                 "Mix",
		"a/Mix .m3u":                "Mix ",
		"a/Mix..m3u":                "Mix.",
		"a/ Mix.m3u":                " Mix",
		"a/Mix.m3u8.m3u":            "Mix.m3u8",
		"a/Caf\u00e9.m3u":           "Caf\u00e9",
		"a/Cafe\u0301.m3u":          "Cafe\u0301",
		"Music/Playlists/Late.m3u8": "Late",
	} {
		if got := playlistFileDisplayName(path); got != want {
			t.Errorf("%q: %q, want %q", path, got, want)
		}
	}
}

func TestIsPlaylistFileName(t *testing.T) {
	for name, want := range map[string]bool{
		"Mix.m3u": true, "Mix.m3u8": true, "Mix.M3U": true, "Mix.M3u8": true,
		".hidden.m3u": false, "Mix.m3u ": false, "Mix.pls": false, "Mix.m3u.bak": false,
		"m3u": false, "Mix.flac": false, "Mix.xspf": false,
	} {
		if got := isPlaylistFileName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

func TestPlaylistParserStopsPastTheEntryCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= playlistFileMaxEntries; i++ {
		b.WriteString("a\n")
	}
	lines, tooMany := parsePlaylistLines([]byte(b.String()))
	if !tooMany || len(lines) > playlistFileMaxEntries {
		t.Fatalf("%d lines, tooMany %v; want the cap's refusal", len(lines), tooMany)
	}
	lines, tooMany = parsePlaylistLines([]byte(strings.Repeat("a\n", playlistFileMaxEntries)))
	if tooMany || len(lines) != playlistFileMaxEntries {
		t.Fatalf("%d lines, tooMany %v; want exactly the cap kept", len(lines), tooMany)
	}
}
