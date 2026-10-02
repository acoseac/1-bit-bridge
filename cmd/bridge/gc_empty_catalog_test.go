package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The forward sweep is the one that deletes FILES, and it runs first — so by
// the time gcCheckOutputDirBeforeReverseSweep fires on the row side, the
// sidecars are already gone. These pin the direction that was missing.

// TestGCRefusalNamesTheCatalogItIsTalkingAbout — the refusal is shared by
// `upscale --gc` (variant rows, the variants directory) and `analyze --gc`
// (analysis rows, the waveform directory). A refusal that says "no variant row
// references any sidecar" during a waveform GC sends the operator to the wrong
// table, at the moment they are deciding whether to pass --allow-empty.
// (Gemini on #895.) Driven through both real sweeps since 2026-09-29.
func TestGCRefusalNamesTheCatalogItIsTalkingAbout(t *testing.T) {
	for _, sw := range emptyCatalogSweeps {
		t.Run(sw.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixtureFile(t, filepath.Join(dir, "Artist", "Album", sw.candidate), 1)
			_, out := sw.run(t, dir, false, false)
			want, other := []string{"GC forward sweep:", "variant row", "variants directory"}, "waveform"
			if sw.name == "analyze --gc" {
				want, other = []string{"analyze --gc:", "analysis row", "waveform directory"}, "variant"
			}
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("the refusal does not say %q:\n%s", w, out)
				}
			}
			if strings.Contains(out, other) {
				t.Errorf("the refusal talks about the other catalog (%q):\n%s", other, out)
			}
		})
	}
}

// TestEveryGCCommandOffersTheEmptyCatalogOverride is the sweep, not the
// symptom. Three commands reach runGC and a fourth reaches runAnalyzeGC; the
// refusal is only usable if each of them offers the flag that lifts it, and a
// fifth `--gc` added later must not quietly ship a refusal with no way out.
//
// Anchored on the flag NAME in an fs.Bool call, and gated on a floor: a scan
// that stops matching reports no problems, which is the outcome that hides the
// drift. (stripGoComments would blank the string literals this looks for, so
// the scan is deliberately raw — a `--gc` mentioned in prose cannot satisfy
// `fs.Bool("gc"`.)
func TestEveryGCCommandOffersTheEmptyCatalogOverride(t *testing.T) {
	gcRe := regexp.MustCompile(`fs\.Bool\("gc"`)
	allowRe := regexp.MustCompile(`fs\.Bool\("allow-empty"`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// CRLF-normalised: nothing pins eol, so a Windows checkout would
		// otherwise make every literal scan in this tree find nothing.
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if !gcRe.MatchString(src) {
			continue
		}
		checked++
		if !allowRe.MatchString(src) {
			t.Errorf("%s declares --gc but not --allow-empty: an operator whose library "+
				"really is empty gets a refusal with no way past it", name)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d --gc commands found — the scan is broken, so this test "+
			"is not checking anything", checked)
	}
}

// The empty-catalog refusal decides from the sweep's inventory (backlog
// B65): it refuses when the walk found a file this sweep would remove, and
// never over anything else. Until 2026-09-29 it asked whether the directory
// held ANY entry (VariantsDirSweepBlockReason's one-entry read), so a
// variants or waveform directory left holding empty folders, a .DS_Store or
// the filesystem's lost+found needed --allow-empty for a `--gc` with
// nothing to remove, while #1084 had already made the background sweep ask
// its inventory. Measured on main with the real binary: `upscale --gc` and
// `analyze --gc` each exited 1 over every one of those shapes.

// emptyCatalogStore opens a manifest store with no rows at all.
func emptyCatalogStore(t *testing.T) *manifest.Store {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// emptyCatalogSweep is one `--gc` sweep, run over dir with an empty
// catalog: its name, the name of a file it removes as an orphan, and how to
// run it with or without --allow-empty and --allow-mass-orphans. run
// returns the exit code and everything the run printed.
type emptyCatalogSweep struct {
	name      string
	candidate string
	run       func(t *testing.T, dir string, allowEmpty, allowMassOrphans bool) (rc int, out string)
}

// emptyCatalogSweeps are the two `--gc` sweeps with an empty-catalog
// refusal: `upscale --gc` (runGC, which optimize and render reach too) and
// `analyze --gc`.
var emptyCatalogSweeps = []emptyCatalogSweep{
	{"upscale --gc", "01.flac.upscaled-v2-176400-24.flac", func(t *testing.T, dir string, allowEmpty, allowMassOrphans bool) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		rc := runGC(context.Background(), &stdout, &stderr, emptyCatalogStore(t), dir, t.TempDir(),
			gcOptions{maxDeletePercent: 20, allowEmpty: allowEmpty, allowMassOrphans: allowMassOrphans})
		return rc, stdout.String() + stderr.String()
	}},
	{"analyze --gc", "01.flac" + analyze.WaveformExt, func(t *testing.T, dir string, allowEmpty, allowMassOrphans bool) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		rc := runAnalyzeGC(context.Background(), &stdout, &stderr, emptyCatalogStore(t), dir,
			analyzeGCOptions{allowEmpty: allowEmpty, allowMassOrphans: allowMassOrphans})
		return rc, stdout.String() + stderr.String()
	}},
}

// emptyCatalogRefusalSays is what every empty-catalog refusal says, in both
// sweeps: no row of the catalog references a sidecar.
const emptyCatalogRefusalSays = "references any sidecar"

// plantUnstattableLink puts at link a symlink to a directory behind a
// directory this user cannot search, so a stat of it fails with a
// permission error: an entry the walk counts as unreadable but not as a
// directory it could not list. Skips where the fixture cannot build that.
func plantUnstattableLink(t *testing.T, link string) {
	t.Helper()
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(blocked, "sub"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	lockDir(t, blocked)
	if _, err := os.Stat(link); err == nil {
		t.Skip("this user can stat through a 0000 directory — the fixture cannot reproduce the state")
	}
}

// TestGCEmptyCatalogProceedsOverNothingItWouldRemove — over a tree that
// holds nothing the sweep would remove, an empty catalog is a bridge that
// never rendered or analysed anything, or whose files are all gone: the
// sweep proceeds and leaves what it does not manage where it was. On main
// every shape but the empty directory refused, exit 1, "holds files".
func TestGCEmptyCatalogProceedsOverNothingItWouldRemove(t *testing.T) {
	type shape struct {
		name string
		// plant builds the shape in dir and returns the entries that must
		// survive the run.
		plant func(t *testing.T, dir string) []string
		locks bool
		// only names the one sweep the shape applies to; "" is both.
		only string
	}
	for _, sh := range []shape{
		{name: "an empty directory", plant: func(*testing.T, string) []string { return nil }},
		{name: "the renditions' empty folders", plant: func(t *testing.T, dir string) []string {
			return []string{mkdirUnder(t, dir, filepath.Join("Artist", "Album")), mkdirUnder(t, dir, "Other")}
		}},
		{name: "the filesystem's lost+found", locks: true, plant: func(t *testing.T, dir string) []string {
			return []string{lockDir(t, filepath.Join(dir, "lost+found"))}
		}},
		// analyze --gc removes waveforms only; upscale --gc removes every
		// file (a nil Consider), so a .DS_Store refuses there
		// (TestGCEmptyCatalogRefusesOverAFileItWouldRemove).
		{name: "a .DS_Store", only: "analyze --gc", plant: func(t *testing.T, dir string) []string {
			p := filepath.Join(dir, ".DS_Store")
			writeFixtureFile(t, p, 1)
			return []string{p}
		}},
	} {
		for _, sw := range emptyCatalogSweeps {
			if sh.only != "" && sh.only != sw.name {
				continue
			}
			t.Run(sw.name+" over "+sh.name, func(t *testing.T) {
				if sh.locks {
					skipUnlessModeBitsDeny(t, "root lists lost+found, so there would be nothing to exempt")
				}
				dir := t.TempDir()
				kept := sh.plant(t, dir)
				rc, out := sw.run(t, dir, false, false)
				if rc != 0 || strings.Contains(out, emptyCatalogRefusalSays) {
					t.Fatalf("an empty catalog over %s: rc=%d, want 0 and no refusal:\n%s", sh.name, rc, out)
				}
				for _, p := range kept {
					if _, err := os.Lstat(p); err != nil {
						t.Errorf("%s is gone after the run: %v", p, err)
					}
				}
			})
		}
	}
}

// TestGCEmptyCatalogRemovesAWaveformScratchFileWithoutARefusal — a
// `.waveform.bin.tmp` is analyze's own half-written litter, which its sweep
// removes whatever the catalog says: an empty catalog puts no scratch file
// at risk, so it is no reason to refuse, and the run removes it.
func TestGCEmptyCatalogRemovesAWaveformScratchFileWithoutARefusal(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "Artist", "Album", "01.flac"+analyze.WaveformExt+analyze.AnalysisTmpSuffix)
	writeAgedFile(t, scratch, time.Hour)
	rc, out := emptyCatalogSweeps[1].run(t, dir, false, false)
	if rc != 0 || strings.Contains(out, emptyCatalogRefusalSays) {
		t.Fatalf("an empty catalog over one scratch file: rc=%d, want 0 and no refusal:\n%s", rc, out)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("the scratch file survived the run (%v)", err)
	}
}

// TestGCEmptyCatalogRefusesOverAFileItWouldRemove — wherever the walk finds
// a file the sweep would remove, an empty catalog makes it an orphan, and
// the run refuses without --allow-empty, below the mass-orphan floor too:
// a sidecar of the sweep's own family in both sweeps, any file at all in
// `upscale --gc` (its nil Consider removes a .DS_Store as readily as a
// rendition), and an entry the walk could not stat, weighed as one such
// file. The refusal counts what it saw and names an example, so the
// operator deciding on --allow-empty can see what would go.
func TestGCEmptyCatalogRefusesOverAFileItWouldRemove(t *testing.T) {
	type shape struct {
		name string
		// plant builds the shape in dir for a sweep that removes files named
		// candidate, and returns the entry that must survive the refusal.
		plant func(t *testing.T, dir, candidate string) string
		locks bool
		only  string
		// says is what the refusal must say besides the flag and the cause.
		says []string
	}
	for _, sh := range []shape{
		{name: "one file of its family", plant: func(t *testing.T, dir, candidate string) string {
			p := filepath.Join(dir, "Artist", "Album", candidate)
			writeFixtureFile(t, p, 1)
			return p
		}, says: []string{"holds 1 file(s) this sweep would remove", filepath.Join("Artist", "Album")}},
		{name: "a .DS_Store", only: "upscale --gc", plant: func(t *testing.T, dir, _ string) string {
			p := filepath.Join(dir, ".DS_Store")
			writeFixtureFile(t, p, 1)
			return p
		}, says: []string{"holds 1 file(s) this sweep would remove", ".DS_Store"}},
		{name: "a link the walk could not stat", locks: true, plant: func(t *testing.T, dir, candidate string) string {
			link := filepath.Join(dir, "Parked", candidate)
			plantUnstattableLink(t, link)
			return link
		}, says: []string{"holds 1 file(s) this sweep would remove", "could not stat"}},
	} {
		for _, sw := range emptyCatalogSweeps {
			if sh.only != "" && sh.only != sw.name {
				continue
			}
			t.Run(sw.name+" over "+sh.name, func(t *testing.T) {
				if sh.locks {
					skipUnlessModeBitsDeny(t, "root stats through any directory, so the link would resolve")
				}
				dir := t.TempDir()
				kept := sh.plant(t, dir, sw.candidate)
				rc, out := sw.run(t, dir, false, false)
				if rc == 0 || !strings.Contains(out, emptyCatalogRefusalSays) {
					t.Fatalf("an empty catalog over %s: rc=%d, want the empty-catalog refusal:\n%s", sh.name, rc, out)
				}
				for _, want := range append([]string{"--allow-empty", "--config"}, sh.says...) {
					if !strings.Contains(out, want) {
						t.Errorf("the refusal does not say %q:\n%s", want, out)
					}
				}
				if _, err := os.Lstat(kept); err != nil {
					t.Errorf("a refused run removed %s: %v", kept, err)
				}
			})
		}
	}
}

// TestGCEmptyCatalogLeavesAnUnlistedDirectoryToThePartialWalk — a directory
// the walk could not list may hold renditions or nothing, and the walk
// reached no file to count, so the empty-catalog refusal has nothing to
// say about it: the partial walk's refusal does, as in the background
// sweep. On main the run refused with the empty-catalog message ("holds
// files") about a tree whose files it had not seen.
func TestGCEmptyCatalogLeavesAnUnlistedDirectoryToThePartialWalk(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory, so the walk would not be partial")
	for _, sw := range emptyCatalogSweeps {
		t.Run(sw.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixtureFile(t, filepath.Join(dir, "Locked", sw.candidate), 1)
			lockDir(t, filepath.Join(dir, "Locked"))
			rc, out := sw.run(t, dir, false, false)
			if rc == 0 || strings.Contains(out, emptyCatalogRefusalSays) || !strings.Contains(out, "could not list 1 director(y/ies)") {
				t.Fatalf("rc=%d, want the partial walk's refusal and not the empty catalog's:\n%s", rc, out)
			}
		})
	}
}

// TestGCEmptyCatalogOverrideNamesEveryFlagTheRunNeeds — past --allow-empty,
// an empty catalog over ten or more files meets the mass-orphan check (with
// no rows, every one of them is more than the catalog holds), so the
// refusal names both flags there and --allow-empty alone below the floor,
// and each flag does what it says.
func TestGCEmptyCatalogOverrideNamesEveryFlagTheRunNeeds(t *testing.T) {
	for _, sw := range emptyCatalogSweeps {
		t.Run(sw.name, func(t *testing.T) {
			few, many := t.TempDir(), t.TempDir()
			for i := 0; i < 3; i++ {
				writeAgedFile(t, filepath.Join(few, "Album", fmt.Sprintf("%02d", i), sw.candidate), time.Hour)
			}
			for i := 0; i < 12; i++ {
				writeAgedFile(t, filepath.Join(many, "Album", fmt.Sprintf("%02d", i), sw.candidate), time.Hour)
			}

			rc, out := sw.run(t, few, false, false)
			if rc == 0 || !strings.Contains(out, "--allow-empty") || strings.Contains(out, "--allow-mass-orphans") {
				t.Errorf("three files: rc=%d, want a refusal naming --allow-empty alone:\n%s", rc, out)
			}
			if rc, out := sw.run(t, few, true, false); rc != 0 || regularFilesUnderDir(t, few) != 0 {
				t.Errorf("three files with --allow-empty: rc=%d, %d file(s) left, want 0 and none:\n%s", rc, regularFilesUnderDir(t, few), out)
			}

			rc, out = sw.run(t, many, false, false)
			if rc == 0 || !strings.Contains(out, "--allow-empty") || !strings.Contains(out, "--allow-mass-orphans") {
				t.Errorf("twelve files: rc=%d, want a refusal naming both flags:\n%s", rc, out)
			}
			if rc, out := sw.run(t, many, true, false); rc == 0 || regularFilesUnderDir(t, many) != 12 {
				t.Errorf("twelve files with --allow-empty alone: rc=%d, %d file(s) left, want the mass-orphan refusal and all 12:\n%s",
					rc, regularFilesUnderDir(t, many), out)
			}
			if rc, out := sw.run(t, many, true, true); rc != 0 || regularFilesUnderDir(t, many) != 0 {
				t.Errorf("twelve files with both flags: rc=%d, %d file(s) left, want 0 and none:\n%s", rc, regularFilesUnderDir(t, many), out)
			}
		})
	}
}

// mkdirUnder makes rel under dir and returns it.
func mkdirUnder(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// regularFilesUnderDir counts the regular files under dir.
func regularFilesUnderDir(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
