package upload

import (
	"path"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzValidateRelPath fuzzes the newest untrusted-input surface in the
// tree: client-supplied relative paths, arriving over HTTP, that decide
// where bytes land on the operator's disk.
//
// It carries a PROPERTY assertion rather than a bare no-panic check, in
// the shape of FuzzResolveContainment: a path ValidateRelPath ACCEPTS
// must satisfy every invariant the commit path then relies on. The
// property is asymmetric — a rejection is always fine, so only a genuine
// escape fails it.
func FuzzValidateRelPath(f *testing.F) {
	seeds := []string{
		"", ".", "..", "/", "//", "a", "a.flac",
		"Artist/Album/01 Track.flac",
		"Artist/Album/cover.jpg",
		"../etc/passwd.flac",
		"a/../../b.flac",
		"./a.flac",
		"a/./b.flac",
		"/abs/path.flac",
		`C:\Windows\x.flac`,
		`a\b.flac`,
		"con.flac", "CON.flac", "aux.flac", "nul.flac", "lpt1.flac",
		".hidden/a.flac",
		"a/.hidden.flac",
		".bridge-upload/x.flac",
		".bridge-trash/1/x.flac",
		"trailing./a.flac",
		"trailing /a.flac",
		"a\x00b.flac",
		"a\nb.flac",
		"a<b.flac", "a>b.flac", "a:b.flac", `a"b.flac`, "a|b.flac", "a?b.flac", "a*b.flac",
		"Ünïcödé/Ålbüm/01 Träck.flac",
		"日本語/アルバム/01.flac",
		"a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z.flac",
		strings.Repeat("a", 300) + ".flac",
		strings.Repeat("a/", 200) + "x.flac",
		"a.exe", "a.sh", "a", "a.",
		"\xff\xfe.flac",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rel string) {
		got, err := ValidateRelPath(rel)
		if err != nil {
			// A rejection is always an acceptable answer. The property is
			// only about what gets THROUGH.
			if got != "" {
				t.Fatalf("ValidateRelPath(%q) returned both a path %q and an error %v", rel, got, err)
			}
			return
		}

		// --- properties every ACCEPTED path must satisfy ---

		if got == "" {
			t.Fatalf("accepted %q but returned an empty path", rel)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("accepted %q -> %q which is not valid UTF-8", rel, got)
		}
		if strings.ContainsRune(got, 0) {
			t.Fatalf("accepted %q -> %q which contains NUL", rel, got)
		}
		// A backslash must never survive: on POSIX it is a legal filename
		// character and on Windows filepath.Join would split on it, so the
		// same declared path would mean different things per host.
		if strings.ContainsRune(got, '\\') {
			t.Fatalf("accepted %q -> %q which contains a backslash", rel, got)
		}
		if strings.HasPrefix(got, "/") {
			t.Fatalf("accepted %q -> %q which is absolute", rel, got)
		}
		if got != path.Clean(got) {
			t.Fatalf("accepted %q -> %q which is not in clean form (Clean gives %q)",
				rel, got, path.Clean(got))
		}

		// THE containment property: no segment may traverse upward, and
		// none may be a dot-directory (which is how staging and trash
		// hide from the scanner — a client that could write there would
		// be writing into the bridge's own working state).
		for _, seg := range strings.Split(got, "/") {
			if seg == "" {
				t.Fatalf("accepted %q -> %q with an empty segment", rel, got)
			}
			if seg == "." || seg == ".." {
				t.Fatalf("accepted %q -> %q with a %q segment", rel, got, seg)
			}
			if strings.HasPrefix(seg, ".") {
				t.Fatalf("accepted %q -> %q with a dot-prefixed segment %q", rel, got, seg)
			}
		}

		// The belt-and-braces staging check must agree: nothing accepted
		// here may be reported as living under staging or trash.
		if IsUnderStaging(got) {
			t.Fatalf("accepted %q -> %q which IsUnderStaging reports as inside staging/trash", rel, got)
		}

		// Only file types the upload path claims to accept get through.
		if accepted, _ := AcceptedExt(got); !accepted {
			t.Fatalf("accepted %q -> %q whose extension %q is not an accepted type",
				rel, got, path.Ext(got))
		}
	})
}

// FuzzAcceptedExt is a pure no-panic sweep over the extension classifier.
// It has no property beyond termination — the classification itself is a
// lookup — but it is on the same untrusted string.
func FuzzAcceptedExt(f *testing.F) {
	for _, s := range []string{"", ".", "a.flac", "a.FLAC", "a.jpg", "a.exe", ".flac", "a.b.c.dsf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		accepted, isAudio := AcceptedExt(name)
		// An audio file is necessarily an accepted one; the converse does
		// not hold (companion files are accepted and not audio).
		if isAudio && !accepted {
			t.Fatalf("AcceptedExt(%q) = (accepted=false, isAudio=true); an audio file must be accepted", name)
		}
	})
}
