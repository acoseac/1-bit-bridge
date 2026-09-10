package admin

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestPlayerMIMEDivergesFromDLNA is the guard player_audio.go's docblock has
// named all along without it existing.
//
// The two tables answer different questions and must not be unified.
// `dlna.defaultMIMEForExtension` is renderer-interop truth, chosen from field
// reports about hardware players, and it maps `.flac` to `audio/x-flac` — which
// is right for a DLNA renderer and WRONG for a browser: measured in Chromium,
// canPlayType("audio/flac") is "probably" while canPlayType("audio/x-flac") is
// "". Reusing it would have made FLAC — 88% of the reference library — look
// unplayable in the web player.
//
// The property, not a second copy of the delta: every type this table returns
// is one a browser will accept, and the `x-` vendor spellings that make the
// DLNA table right are exactly what disqualifies it here. A table comparison
// would need `defaultMIMEForExtension` exported, which is widening one
// package's API to let another's test read it.
func TestPlayerMIMEDivergesFromDLNA(t *testing.T) {
	// The extension that carries the whole point, pinned by value.
	if got := playerContentType(".flac"); got != "audio/flac" {
		t.Errorf("playerContentType(\".flac\") = %q, want audio/flac — audio/x-flac is the "+
			"DLNA answer and Chromium reports \"\" for it, i.e. unplayable", got)
	}

	// Every extension the table names, plus the DSD ones it deliberately
	// refuses to name. A vendor-prefixed type reaching a browser is the
	// failure mode; octet-stream is the honest "no browser decodes this".
	for _, ext := range []string{
		".flac", ".mp3", ".m4a", ".mp4", ".m4b", ".wav", ".wave",
		".aif", ".aiff", ".ogg", ".oga", ".opus", ".dsf", ".dff", ".iso",
	} {
		got := playerContentType(ext)
		if strings.HasPrefix(got, "audio/x-") {
			t.Errorf("playerContentType(%q) = %q — an `x-` vendor spelling is the "+
				"renderer-interop answer, and browsers refuse it", ext, got)
		}
		if got == "" {
			t.Errorf("playerContentType(%q) = \"\" — every extension must get an answer, "+
				"even if it is %q", ext, octetStream)
		}
	}

	// DSD stays opaque bytes rather than gaining a type that invites an
	// engine to try decoding a 1-bit stream.
	for _, ext := range []string{".dsf", ".dff"} {
		if got := playerContentType(ext); got != octetStream {
			t.Errorf("playerContentType(%q) = %q, want %q", ext, got, octetStream)
		}
	}

	// And the tables stay SEPARATE: reaching into internal/dlna for a MIME
	// answer is the unification this exists to prevent, and it would compile.
	//
	// Parsed, not grepped. The docblock five lines above the function says
	// "Deliberately NOT dlna.defaultMIMEForExtension" — a text scan finds the
	// commentary explaining the rule and reports the rule as broken, which is
	// the shape the CSS and Enrich guards in this tree already record. The AST
	// carries no comments, so the question asked is about the CODE.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "player_audio.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Checked at the IMPORT, not the selector. A selector check matches only
	// the default package name, so `import dlnaMIME "…/internal/dlna"` would
	// call `dlnaMIME.defaultMIMEForExtension` straight past it. The import is
	// also the stricter question: this file has no business reaching into the
	// renderer-interop package at all, whatever it calls. (CodeRabbit on #897.)
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if strings.HasSuffix(path, "/internal/dlna") {
			name := "(default)"
			if imp.Name != nil {
				name = imp.Name.Name
			}
			t.Errorf("player_audio.go imports %s as %s — two contracts, two tables, and "+
				"the DLNA table maps .flac to audio/x-flac, which browsers refuse",
				path, name)
		}
	}
}
