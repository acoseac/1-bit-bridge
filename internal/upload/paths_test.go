package upload

import (
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		// Shapes a real library produces.
		{"simple", "Pink Floyd/Dark Side/01 Speak to Me.flac", true},
		{"root level file", "track.flac", true},
		{"unicode", "Zdob și Zdub/Ethnomecanica/01 Ливень.flac", true},
		{"spaces and punctuation", "AC-DC/Back in Black (Remaster)/01 Hells Bells.flac", true},
		{"cover art", "Artist/Album/cover.jpg", true},
		{"booklet", "Artist/Album/booklet.pdf", true},
		{"lyrics companion", "Artist/Album/01 Song.lrc", true},
		{"cue sheet", "Artist/Album/album.cue", true},
		{"uppercase extension", "Artist/Album/01 Song.FLAC", true},

		// Traversal and absolutes.
		{"parent traversal", "../etc/passwd.flac", false},
		{"embedded traversal", "Artist/../../etc/passwd.flac", false},
		{"dot segment", "Artist/./Album/x.flac", false},
		{"absolute", "/etc/passwd.flac", false},
		{"drive qualified", "C:/Windows/x.flac", false},
		{"unc-ish", "//server/share/x.flac", false},
		{"empty", "", false},
		{"double slash", "Artist//Album/x.flac", false},
		{"trailing slash", "Artist/Album/", false},

		// Separator confusion. A backslash is REFUSED rather than
		// interpreted: as a separator it is wrong on POSIX, as a literal it
		// is dangerous on Windows, so the same declared path would mean two
		// different things depending on the host.
		{"backslash traversal", `..\..\Windows\System32\x.flac`, false},
		{"backslash in name", `AC\DC/x.flac`, false},

		// Bytes that must never reach a filesystem call.
		{"NUL", "Artist/Al\x00bum/x.flac", false},
		{"newline", "Artist/Al\nbum/x.flac", false},
		{"control char", "Artist/Al\x07bum/x.flac", false},

		// Windows-hostile names, refused on every host: a library restored
		// onto Windows must not carry a file that cannot be opened there.
		{"reserved CON", "Artist/Album/CON.flac", false},
		{"reserved NUL basename", "Artist/NUL/x.flac", false},
		{"reserved COM1", "Artist/Album/COM1.flac", false},
		{"reserved lowercase", "Artist/Album/con.flac", false},
		{"reserved as directory", "CON/Album/x.flac", false},
		{"not reserved — CONCERT", "Artist/CONCERT/x.flac", true},
		{"not reserved — COM10", "Artist/Album/COM10.flac", true},
		{"trailing dot", "Artist/Album./x.flac", false},
		{"trailing space", "Artist/Album /x.flac", false},
		{"colon", "Artist/Al:bum/x.flac", false},
		{"question mark", "Artist/Album/x?.flac", false},
		{"pipe", "Artist/Album/x|y.flac", false},

		// Dot-prefixed segments would land inside a directory the scanner
		// skips — which is how staging and trash hide — so a client must
		// never be able to write there.
		{"dot dir", ".bridge-upload/x.flac", false},
		{"dot dir nested", "Artist/.hidden/x.flac", false},
		{"dot file", "Artist/Album/.hidden.flac", false},

		// Extension gate.
		{"executable", "Artist/Album/evil.sh", false},
		{"archive", "Artist/Album/album.zip", false},
		{"no extension", "Artist/Album/README", false},
		{"png cover is refused, not silently ignored", "Artist/Album/cover.png", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateRelPath(c.in)
			if c.ok && err != nil {
				t.Fatalf("ValidateRelPath(%q) = error %v, want accepted", c.in, err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("ValidateRelPath(%q) = %q, want rejected", c.in, got)
				}
				if !errors.Is(err, ErrInvalidPath) {
					t.Errorf("rejection does not wrap ErrInvalidPath: %v", err)
				}
			}
		})
	}
}

func TestValidateRelPathLengthCaps(t *testing.T) {
	long := strings.Repeat("a", maxSegmentBytes+1) + ".flac"
	if _, err := ValidateRelPath("Artist/" + long); err == nil {
		t.Error("an over-long segment was accepted")
	}
	deep := strings.Repeat("d/", maxDepth+1) + "x.flac"
	if _, err := ValidateRelPath(deep); err == nil {
		t.Error("an over-deep path was accepted")
	}
	ok := strings.Repeat("a", maxSegmentBytes-len(".flac")) + ".flac"
	if _, err := ValidateRelPath(ok); err != nil {
		t.Errorf("a segment exactly at the cap was rejected: %v", err)
	}
}

// TestAcceptedExtUsesTheScannersOwnAudioSet is the anti-drift pin. The audio
// set is manifest.Ext itself rather than a copy, so this asserts the wiring
// rather than a duplicated list: every extension the scanner will index must be
// uploadable, or a user can upload a file the library then ignores.
func TestAcceptedExtUsesTheScannersOwnAudioSet(t *testing.T) {
	for ext := range manifest.Ext {
		accepted, isAudio := AcceptedExt("x" + ext)
		if !accepted {
			t.Errorf("%s is indexed by the scanner but refused by upload", ext)
		}
		if !isAudio {
			t.Errorf("%s is in manifest.Ext but not reported as audio", ext)
		}
	}
	if _, isAudio := AcceptedExt("cover.jpg"); isAudio {
		t.Error("a companion file reported itself as audio")
	}
}

func TestIsUnderStaging(t *testing.T) {
	for _, p := range []string{
		StagingDirName + "/sid/f.part",
		"Artist/" + StagingDirName + "/x",
		".bridge-trash/2026/x.flac",
	} {
		if !IsUnderStaging(p) {
			t.Errorf("IsUnderStaging(%q) = false, want true", p)
		}
	}
	if IsUnderStaging("Artist/Album/01.flac") {
		t.Error("a normal path was reported as staging")
	}
}

// TestPartSuffixIsNotIndexable is the second layer under the dot-directory:
// even a staged file that somehow got walked cannot be enqueued, because
// ".part" is not an extension the scanner extracts.
func TestPartSuffixIsNotIndexable(t *testing.T) {
	if manifest.Ext[PartSuffix] {
		t.Fatalf("%q is in manifest.Ext — a partially uploaded file could be indexed as a track", PartSuffix)
	}
	if accepted, _ := AcceptedExt("abc" + PartSuffix); accepted {
		t.Errorf("%q is an uploadable extension; a client could name a file to survive commit as a part", PartSuffix)
	}
}
