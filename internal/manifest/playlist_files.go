package manifest

import (
	"bytes"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Playlist files (backlog B431). The scanner reads each `.m3u` / `.m3u8`
// file in the library once per index and keeps the entry lines it parsed
// (the playlist_files table, migration v56). GET /v1/playlist-files places
// those lines with the path rule and resolves them against the track set
// when it is asked (PlaylistFiles), so a track indexed after its playlist
// was read is a member without the file being read again.
//
// The parse and the path rule are the iOS app's (PlaylistImport.swift),
// held to the table both repos share, testdata/playlist-parse-cases.tsv,
// whose header states every rule. A playlist file is never a track and is
// never written: nothing here opens one for writing.

const (
	// PlaylistFileMaxBytes is the size from which a playlist file is not
	// read. The app's importer reads a file only while its size is under
	// 2,000,000 bytes (LibraryScanner.readPlaylistFile) and imports a larger
	// one with no entries; the index keeps such a file with no lines, and
	// the feed lists it with tooLarge set.
	PlaylistFileMaxBytes = 2_000_000

	// playlistFileMaxEntries is the most entry lines a file keeps. A file
	// with more is kept as too large, like one past PlaylistFileMaxBytes:
	// under that size it takes lines averaging under 20 bytes, so it binds
	// only a file of very short lines, whose entries a long folder path
	// would otherwise multiply in the feed.
	playlistFileMaxEntries = 100_000

	// playlistFileParserVersion is stored with every read. A row an older
	// parser read is read again, as ExtractorVersion re-reads a track: bump
	// it when parsePlaylistLines changes what it keeps.
	playlistFileParserVersion = 1
)

// isPlaylistFileName reports whether the scanner reads a walked file as a
// playlist file: an .m3u or .m3u8 extension in any case, the app's
// MetadataNormalizer.isPlaylist, and not a dot-file, which no walk indexes.
func isPlaylistFileName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".m3u", ".m3u8":
		return true
	}
	return false
}

// playlistBOM is the UTF-8 byte order mark.
var playlistBOM = []byte{0xEF, 0xBB, 0xBF}

// decodePlaylistText is the app's PlaylistImport.decodeText. A UTF-8 BOM at
// the start is dropped as BYTES, once, before any decoding. The encoding is
// then decided for the whole file: UTF-8 when every byte is valid UTF-8,
// else Windows-1252 when no byte is one it leaves undefined, else
// ISO-8859-1, which maps each byte to the code point of its value and
// cannot fail.
//
// The app's UTF-8 decoder (String(data:encoding: .utf8)) also drops a BOM
// that starts the text it decodes, so a file that begins with two BOMs
// loses both and a third is kept (measured on macOS 27; the app's comment
// on decodeText reports the same of iOS 26.5 and 27.0). The Windows-1252
// and ISO-8859-1 decoders drop nothing.
func decodePlaylistText(data []byte) string {
	body := bytes.TrimPrefix(data, playlistBOM)
	if utf8.Valid(body) {
		return strings.TrimPrefix(string(body), "\uFEFF")
	}
	if text, ok := windows1252Strict(body); ok {
		return text
	}
	var b strings.Builder
	b.Grow(len(body) * 2)
	for _, c := range body {
		b.WriteRune(rune(c))
	}
	return b.String()
}

// windows1252Strict decodes b as Windows-1252, refusing the five bytes the
// encoding leaves undefined (81, 8D, 8F, 90, 9D), as the app's decoder
// does. windows1252 (container_text.go) is the 0x80–0x9F row.
func windows1252Strict(b []byte) (string, bool) {
	var out strings.Builder
	out.Grow(len(b) * 2)
	for _, c := range b {
		switch {
		case c < 0x80 || c >= 0xA0:
			out.WriteRune(rune(c))
		case c == 0x81 || c == 0x8D || c == 0x8F || c == 0x90 || c == 0x9D:
			return "", false
		default:
			out.WriteRune(windows1252[c-0x80])
		}
	}
	return out.String(), true
}

// isPlaylistLineEnd reports a character that ends a line in the app's
// splitter (NSString line enumeration): LF, CR, NEL, LINE SEPARATOR and
// PARAGRAPH SEPARATOR. CRLF is a CR and an LF with an empty line between,
// and an empty line is no entry.
func isPlaylistLineEnd(r rune) bool {
	switch r {
	case '\n', '\r', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// isPlaylistTrimmed reports a character the app trims from both ends of a
// line (trimmingCharacters(in: .whitespacesAndNewlines)): Unicode
// White_Space, and U+200B ZERO WIDTH SPACE, which Foundation's whitespace
// set still holds (it runs U+2000 to U+200B) though Unicode moved it out of
// the space separators. Measured against the app's parser on macOS 27;
// U+180E, U+200C, U+200D and U+FEFF are not trimmed there either.
func isPlaylistTrimmed(r rune) bool {
	return unicode.IsSpace(r) || r == '​'
}

// parsePlaylistLines is the app's parse up to the path rule
// (PlaylistImport.parseReportingDropped): the text decoded, split into
// lines, each trimmed at both ends (isPlaylistTrimmed), a blank line or one
// that starts with "#" skipped (#EXTM3U, #EXTINF and every other
// directive), and "\" folded to "/". The lines come back in file order,
// duplicates kept.
// tooMany reports a file with more than playlistFileMaxEntries entry lines;
// the lines read until then come back with it and are not kept.
//
// One known difference, outside the table: the app tests "#", "/", "\", ":"
// and "." as Swift Characters, so one followed by a combining mark, a
// zero-width joiner or a variation selector is another character there,
// and so is the "/" the app puts between the playlist's folder and a line
// that starts with one. Here they are plain characters. Measured with the
// app's PlaylistImport.swift (iOS a9a16365) on macOS 27: 30,000 generated
// inputs without such marks gave the same lines and paths byte for byte,
// and of 30,000 with them the 628 that differed were all one of those two
// shapes. No real file name starts a path segment with a combining mark.
func parsePlaylistLines(data []byte) (lines []string, tooMany bool) {
	text := decodePlaylistText(data)
	for text != "" {
		line := text
		if i := strings.IndexFunc(text, isPlaylistLineEnd); i >= 0 {
			_, size := utf8.DecodeRuneInString(text[i:])
			line, text = text[:i], text[i+size:]
		} else {
			text = ""
		}
		line = strings.TrimFunc(line, isPlaylistTrimmed)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(lines) == playlistFileMaxEntries {
			return lines, true
		}
		lines = append(lines, strings.ReplaceAll(line, `\`, "/"))
	}
	return lines, false
}

// playlistFileDir is the manifest path of the folder that holds the
// playlist file at manifestPath: "" for the manifest root.
func playlistFileDir(manifestPath string) string {
	if dir := path.Dir(manifestPath); dir != "." {
		return dir
	}
	return ""
}

// playlistEntryPath is the app's path rule
// (PlaylistImport.absolutePath(forRawPath:playlistAbsolutePath:)) in
// manifest-path form: line is an entry line as parsePlaylistLines keeps it,
// dir the manifest path of the playlist's folder. ok is false for a line
// the rule cannot place inside the manifest root.
//
// A drive-letter path ("C:/…") is not placed; a line starting with "/" is
// taken from the manifest root; any other line holding "://" (a URL,
// file:// included) is not placed; anything else is relative to the
// playlist's folder. "." segments and empty ones drop, ".." pops one, and
// a ".." with nothing left to pop puts the line outside the root even when
// later segments come back down: path.Clean would keep "../root/x", so
// this does not Clean. The manifest root is the app's share root, above
// the roots' folder names on a bridge with several roots, so a line may
// climb out of its own root into another. Bytes are kept: no Unicode
// normalization and no case folding (the lookup folds, PlaylistFiles).
func playlistEntryPath(dir, line string) (string, bool) {
	if isDriveLetterAbsolute(line) {
		return "", false
	}
	joined := line
	if !strings.HasPrefix(line, "/") {
		if strings.Contains(line, "://") {
			return "", false
		}
		joined = dir + "/" + line
	}
	parts := make([]string, 0, strings.Count(joined, "/")+1)
	for _, seg := range strings.Split(joined, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return "", false
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/"), true
}

// isDriveLetterAbsolute reports "X:/…" where X is an ASCII letter, after
// the backslash fold: a path on the Windows machine that wrote the file.
func isDriveLetterAbsolute(s string) bool {
	if len(s) < 3 || s[1] != ':' || s[2] != '/' {
		return false
	}
	c := s[0]
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// playlistFileDisplayName is the name the app gives a playlist it imports
// from a share: the file name without its last extension
// ((entry.name as NSString).deletingPathExtension). A leading dot starts no
// extension.
func playlistFileDisplayName(manifestPath string) string {
	base := path.Base(manifestPath)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		return base[:i]
	}
	return base
}
