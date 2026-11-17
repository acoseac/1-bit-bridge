package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// rootsYAML is a bridge.yaml naming roots as its libraryRoots, each quoted so
// a Windows path reads as written.
func rootsYAML(roots ...string) string {
	if len(roots) == 0 {
		return "libraryRoots: []\n"
	}
	var b strings.Builder
	b.WriteString("libraryRoots:\n")
	for _, r := range roots {
		b.WriteString("  - '" + r + "'\n")
	}
	return b.String()
}

// settingsSnapshot makes a snapshot folder whose bridge.yaml holds settings,
// listed in its manifest with the baseline its capture records, and answers
// the folder and the manifest.
func settingsSnapshot(t *testing.T, settings string) (string, Manifest) {
	t.Helper()
	snap := filepath.Join(t.TempDir(), "2026-11-17T05-30-00Z")
	if err := os.Mkdir(snap, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(settings)
	if err := os.WriteFile(filepath.Join(snap, "bridge.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return snap, Manifest{
		SchemaVersion: SchemaVersion,
		Files:         []string{ManifestDBFileName, "bridge.yaml"},
		Baselines: []Baseline{
			{Name: ManifestDBFileName, Size: 4096, SHA256: strings.Repeat("0", 64)},
			{Name: "bridge.yaml", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])},
		},
	}
}

// TestTheRootFormIsDecidedFromVerifiedSettings: a database restored alone is
// admitted only where its snapshot's own settings, the bytes its capture
// recorded, show the library form this bridge's config gives now (backlog
// B653 PR 2b): one root or none against several refuses, and so do several
// under other folder names, by case alone included, and a folder name that is
// none; settings changed since their capture, grown, unlisted, with no
// baseline, not there, not a regular file or not decoding refuse as
// unproven; a single root moved to another path and name, the same folder
// names in another order, and relative roots resolved against this bridge's
// config pass; and BRIDGE_LIBRARY_ROOTS decides neither side. Each refusal
// names the roots by their folder names alone.
func TestTheRootFormIsDecidedFromVerifiedSettings(t *testing.T) {
	far := t.TempDir() // where the roots are named; none is made
	liveDir := filepath.Join(t.TempDir(), "Library")
	if err := os.Mkdir(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(liveDir, "bridge.yaml")
	at := func(parts ...string) string { return filepath.Join(append([]string{far}, parts...)...) }
	fsRoot := filepath.VolumeName(far) + string(filepath.Separator)
	sep := string(os.PathListSeparator)
	single := rootsYAML(at("a", "Music"))

	for _, c := range []struct {
		name   string
		ours   string // this bridge's bridge.yaml
		theirs string // the snapshot's
		env    string // BRIDGE_LIBRARY_ROOTS, "" for unset
		alter  func(t *testing.T, snap string, m *Manifest)
		want   error // nil, ErrRootFormUnproven or ErrRootFormChanged
		says   []string
	}{
		{name: "one root against two", ours: rootsYAML(at("a", "Music")), theirs: rootsYAML(at("b", "Music"), at("b", "NAS")),
			want: ErrRootFormChanged, says: []string{`one library root ("Music")`, `2 library roots ("Music", "NAS")`}},
		{name: "two roots against one", ours: rootsYAML(at("a", "Music"), at("a", "NAS")), theirs: rootsYAML(at("b", "Music")),
			want: ErrRootFormChanged},
		{name: "two roots against none", ours: rootsYAML(at("a", "Music"), at("a", "NAS")), theirs: rootsYAML(),
			want: ErrRootFormChanged, says: []string{"no library root"}},
		{name: "two roots under other folder names", ours: rootsYAML(at("a", "Music"), at("a", "NAS")), theirs: rootsYAML(at("b", "Music"), at("b", "Other")),
			want: ErrRootFormChanged, says: []string{`"NAS"`, `"Other"`}},
		{name: "two roots whose folder names differ in case alone", ours: rootsYAML(at("a", "Music"), at("a", "NAS")), theirs: rootsYAML(at("b", "music"), at("b", "NAS")),
			want: ErrRootFormChanged, says: []string{`"Music"`, `"music"`}},
		{name: "a root of the filesystem among several", ours: rootsYAML(fsRoot, at("a", "NAS")), theirs: rootsYAML(fsRoot, at("b", "NAS")),
			want: ErrRootFormUnproven},
		{name: "an empty root among several", ours: rootsYAML("", at("a", "NAS")), theirs: rootsYAML("", at("b", "NAS")),
			want: ErrRootFormUnproven, says: []string{`(".")`}},
		{name: "a single root moved to another path and name", ours: rootsYAML(at("a", "Music")), theirs: rootsYAML(at("b", "Library"))},
		{name: "no root against one", ours: rootsYAML(), theirs: rootsYAML(at("b", "Music"))},
		{name: "the same folder names in another order", ours: rootsYAML(at("a", "Music"), at("a", "NAS")), theirs: rootsYAML(at("b", "NAS"), at("b", "Music"))},
		// "." is this bridge's config folder, Library, never the snapshot's.
		{name: "relative roots resolved against this bridge's config", ours: rootsYAML(at("x", "Library"), at("x", "NAS")), theirs: rootsYAML(".", "NAS")},
		{name: "the variable decides neither side where the forms are one", ours: rootsYAML(at("a", "Music"), at("a", "NAS")), theirs: rootsYAML(at("b", "Music"), at("b", "NAS")),
			env: at("e", "One")},
		{name: "the variable decides neither side where the forms differ", ours: rootsYAML(at("a", "Music")), theirs: rootsYAML(at("b", "Music"), at("b", "NAS")),
			env: at("e", "Music") + sep + at("e", "NAS"), want: ErrRootFormChanged},
		{name: "settings changed since their capture", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"not the file it captured"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				path := filepath.Join(snap, "bridge.yaml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-3] ^= 0x20 // the root's name's last letter, its case changed
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "settings grown since their capture", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"not the file it captured"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				f, err := os.OpenFile(filepath.Join(snap, "bridge.yaml"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if _, err := f.WriteString("\n"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "settings the manifest does not list", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"lists no bridge.yaml"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				m.Files = slices.DeleteFunc(m.Files, func(n string) bool { return n == "bridge.yaml" })
			}},
		{name: "settings with no baseline", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"no baseline"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				m.Baselines = slices.DeleteFunc(m.Baselines, func(b Baseline) bool { return b.Name == "bridge.yaml" })
			}},
		{name: "settings that are not there", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"not there"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				if err := os.Remove(filepath.Join(snap, "bridge.yaml")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "settings that are a directory", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"not a regular file"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				path := filepath.Join(snap, "bridge.yaml")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "settings that are a link to the bytes captured", ours: single, theirs: single, want: ErrRootFormUnproven, says: []string{"not a regular file"},
			alter: func(t *testing.T, snap string, m *Manifest) {
				path := filepath.Join(snap, "bridge.yaml")
				aside := filepath.Join(snap, "aside.yaml")
				if err := os.Rename(path, aside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(aside, path); err != nil {
					t.Skipf("no symbolic link can be made here: %v", err)
				}
			}},
		{name: "settings that do not decode", ours: single, theirs: rootsYAML(at("b", "Music")) + "libraryRoot: typo\n", want: ErrRootFormUnproven, says: []string{"does not decode"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv("BRIDGE_LIBRARY_ROOTS", c.env)
			}
			snap, m := settingsSnapshot(t, c.theirs)
			if c.alter != nil {
				c.alter(t, snap, &m)
			}
			err := checkRootForm(snap, m, configPath, []byte(c.ours))
			switch {
			case c.want == nil && err != nil:
				t.Fatalf("checkRootForm = %v, want the forms taken for one", err)
			case c.want == nil:
				return
			case !errors.Is(err, c.want):
				t.Fatalf("checkRootForm = %v, want %v", err, c.want)
			}
			for _, other := range []error{ErrRootFormUnproven, ErrRootFormChanged} {
				if other != c.want && errors.Is(err, other) {
					t.Errorf("checkRootForm = %v, which reads as %v too", err, other)
				}
			}
			words := err.Error()
			for _, s := range c.says {
				if !strings.Contains(words, s) {
					t.Errorf("the refusal %q does not say %q", words, s)
				}
			}
			// The roots by their folder names alone: no path of either
			// config, no value but a root's name.
			for _, s := range []string{far, liveDir, snap, "typo"} {
				if strings.Contains(words, s) {
					t.Errorf("the refusal %q names %q", words, s)
				}
			}
			if !strings.Contains(words, "nothing was restored") || !strings.Contains(words, "bridge restore without --database-only") {
				t.Errorf("the refusal %q does not say that nothing was restored and that the full restore is the way", words)
			}
		})
	}
}
