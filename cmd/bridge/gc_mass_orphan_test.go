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
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The 2026-09-20 aftermath, which neither existing forward guard can see.
//
// gcRefuseEmptyKnownSetOverPopulatedDir asks "is the catalog EMPTY?" and
// the answer was no: three minutes after the sweep dropped 10,248 rows,
// the auto-optimize sweeper (candidate query: "no fresh variant row
// exists") had written 200 fresh ones. gcRefuseRelocationInProgress asks
// "how many ROWS have lost their file?" and the answer was none — every
// one of those 200 rows had its file exactly where it said. So
// `bridge upscale --gc` would have matched 200 files, called the other
// 10,048 orphans and unlinked 254 GiB, cleanly, exit 0.
//
// Nothing here is recoverable the way the row side is: a row can be
// re-pointed at a file that still exists, and a file that has been
// unlinked has to be transcoded again from source.

// strandedTree seeds `fresh` tracks whose variant rows are present and
// correct at their canonical paths, plus `stranded` sidecar files of the
// same family that no row references — the shape a lost index leaves.
func strandedTree(t *testing.T, dir string, fresh, stranded int) (*manifest.Store, []string) {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const variant = "upscaled-v2-176400-24"

	for i := 0; i < fresh; i++ {
		source := fmt.Sprintf("Artist/Re-rendered/%02d.flac", i)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		p := transcode.VariantSidecarPath(dir, source, variant)
		writeFixtureFile(t, p, 50)
		if err := store.UpsertVariant(ctx, manifest.VariantRow{
			SourcePath: source, VariantID: variant, SidecarPath: p, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	paths := make([]string, stranded)
	for i := 0; i < stranded; i++ {
		source := fmt.Sprintf("Artist/Album %d/%02d.flac", i%4, i)
		paths[i] = transcode.VariantSidecarPath(dir, source, variant)
		writeFixtureFile(t, paths[i], 1000)
	}
	return store, paths
}

func writeFixtureFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunGCRefusesAMassOrphanSweepUntilAllowed drives the real `--gc`
// over the incident's shape. The FILES are the assertion that matters —
// the same lesson TestRunGCRefusesAMassDeleteUntilAllowed records, where
// a first draft asserted on rows and went green with the tree already
// destroyed.
func TestRunGCRefusesAMassOrphanSweepUntilAllowed(t *testing.T) {
	dir := t.TempDir()
	store, stranded := strandedTree(t, dir, 2, 40)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc == 0 {
		t.Fatalf("--gc swept a tree the catalog no longer describes\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused --gc unlinked %s: %v — the guard ran after the sweep, not before it", p, err)
		}
	}
	if rows, _ := store.AllVariants(ctx); len(rows) != 2 {
		t.Errorf("%d rows after a refused --gc, want the 2 fresh ones untouched", len(rows))
	}
	out := stderr.String()
	// The refusal has to name the numbers, the way out, and the cause —
	// an operator is standing at a terminal deciding what to do.
	for _, want := range []string{
		"40 of 42 file(s)",
		"more than the 2 row(s)",
		"--allow-mass-orphans",
		"INDEX was lost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not say %q:\n%s", want, out)
		}
	}
	// ...and show a few of them, so the operator can tell their own
	// renditions from junk without going and looking.
	if n := strings.Count(out, "e.g. "); n != gcOrphanExamples {
		t.Errorf("refusal printed %d example(s), want %d", n, gcOrphanExamples)
	}

	// With the override the sweep does exactly what it always did.
	stdout.Reset()
	stderr.Reset()
	rc = runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20, allowMassOrphans: true})
	if rc != 0 {
		t.Fatalf("--allow-mass-orphans rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--allow-mass-orphans left %s (%v)", p, err)
		}
	}
	if rows, _ := store.AllVariants(ctx); len(rows) != 2 {
		t.Errorf("%d rows after --allow-mass-orphans, want the 2 fresh ones kept", len(rows))
	}
}

// TestRunGCStillReclaimsAnOrdinaryOrphanCrop — the guard must not turn
// `--gc` into a command that refuses to do its job. The shapes that
// legitimately leave a lot of orphans all have one thing in common: the
// catalog is still bigger than the junk. A naming-scheme change (the
// documented reason --gc exists) leaves one old file per current row.
func TestRunGCStillReclaimsAnOrdinaryOrphanCrop(t *testing.T) {
	dir := t.TempDir()
	store, legacy := strandedTree(t, dir, 40, 40)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("an ordinary orphan crop was refused: rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range legacy {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %s survived an allowed sweep (%v)", p, err)
		}
	}
	if !strings.Contains(stdout.String(), "removed 40 orphan file(s), kept 40 known sidecar(s)") {
		t.Errorf("summary does not say what happened:\n%s", stdout.String())
	}
}

// TestRunGCMassOrphanGuardLeavesSmallSweepsAlone — below the floor the
// ratio says nothing and the cost of being wrong is a handful of
// sidecars, so a tiny bridge must never see this refusal.
func TestRunGCMassOrphanGuardLeavesSmallSweepsAlone(t *testing.T) {
	dir := t.TempDir()
	store, orphans := strandedTree(t, dir, 1, 9)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("nine orphans tripped the guard: rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range orphans {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %s survived (%v)", p, err)
		}
	}
}

// TestRunAnalyzeGCRefusesAMassOrphanSweepUntilAllowed — the waveform
// twin. Cheaper to rebuild than a PCM rendition, which is why it is the
// milder of the two; not a different rule. (The enumeration lesson: a fix
// that lists the sites it covers misses one.)
func TestRunAnalyzeGCRefusesAMassOrphanSweepUntilAllowed(t *testing.T) {
	dataDir := t.TempDir()
	dir := analyze.WaveformDirFor(dataDir)
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	// Two rows whose waveforms are where they say, forty files nothing
	// references, and one half-written scratch file.
	for i := 0; i < 2; i++ {
		source := fmt.Sprintf("Artist/Re-analyzed/%02d.flac", i)
		p := analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: source}.SidecarPath()
		writeFixtureFile(t, p, 20)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 10, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertAnalysis(ctx, manifest.AnalysisRow{
			SourcePath: source, WaveformPath: p, SourceMTimeNS: 1, SourceSize: 10, SchemaVersion: "wf4",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var stranded []string
	for i := 0; i < 40; i++ {
		p := analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: fmt.Sprintf("Artist/Album %d/%02d.flac", i%4, i)}.SidecarPath()
		writeFixtureFile(t, p, 20)
		stranded = append(stranded, p)
	}
	scratch := filepath.Join(dir, "half-written.waveform.bin.tmp")
	writeFixtureFile(t, scratch, 3)

	var stdout, stderr bytes.Buffer
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, dir, false, false); rc == 0 {
		t.Fatalf("analyze --gc swept a tree the catalog no longer describes\nstderr: %s", stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused analyze --gc unlinked %s: %v", p, err)
		}
	}
	if !strings.Contains(stderr.String(), "--allow-mass-orphans") {
		t.Errorf("refusal does not name the override:\n%s", stderr.String())
	}
	// The scratch file is NOT part of the ratio — it is this sweep's own
	// litter — so a refusal must not have counted it among the forty.
	if !strings.Contains(stderr.String(), "40 of 42 file(s)") {
		t.Errorf("the .tmp scratch was counted as an orphan:\n%s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, dir, false, true); rc != 0 {
		t.Fatalf("analyze --gc --allow-mass-orphans rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--allow-mass-orphans left %s (%v)", p, err)
		}
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("the half-written scratch survived (%v)", err)
	}
}

// TestEveryForwardSweepingGCCommandOffersTheMassOrphanOverride is the
// sweep, not the symptom — the precedent `--allow-empty` and
// `--allow-mass-delete` both set. Every command whose `--gc` unlinks
// sidecar FILES must offer the way past the refusal, or a sixth one added
// later ships an un-escapable guard (which is how `artwork --gc` carried
// one for months).
//
// `artwork --gc` is deliberately out of scope and asserted so below.
func TestEveryForwardSweepingGCCommandOffersTheMassOrphanOverride(t *testing.T) {
	// Anchored on the declaration, not on prose: stripGoComments would
	// blank these string literals, so the scan is raw and a `--gc`
	// mentioned in a docblock cannot satisfy `fs.Bool("gc"`.
	gcRe := regexp.MustCompile(`fs\.Bool\("gc"`)
	allowRe := regexp.MustCompile(`fs\.Bool\("allow-mass-orphans"`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked, covered := 0, 0
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
		if name == "artwork.go" {
			// Artwork is cached from the network and keyed by content /
			// MBID, not by an absolute path a relocation can strand, so
			// the lost-index shape this guard detects cannot arise there.
			// Named explicitly so a later reader sees a decision rather
			// than an omission.
			if allowRe.MatchString(src) {
				t.Errorf("artwork.go grew --allow-mass-orphans; if that is deliberate, move it out of this exemption")
			}
			continue
		}
		if !allowRe.MatchString(src) {
			t.Errorf("%s declares a sidecar --gc but not --allow-mass-orphans: an operator whose files "+
				"really are junk gets a refusal with no way past it", name)
			continue
		}
		covered++
	}
	// A scan that stops matching reports no problems, which is the outcome
	// that hides the drift.
	if checked < 5 {
		t.Fatalf("scanned only %d --gc command(s); the anchor has drifted", checked)
	}
	if covered < 4 {
		t.Fatalf("only %d command(s) carry the override; upscale / optimize / render / analyze all should", covered)
	}
}

// TestRunGCForwardSweepTreatsAVanishedOrphanAsRemoved pins a window this
// PR opened: the inventory and the unlink are separate steps now, so a
// file another process removes in between reaches os.Remove as ENOENT.
// That is the outcome the sweep asked for, and counting it as a failure
// exits 1 — a cron'd `--gc` reporting a failed job for doing its job.
// `analyze --gc` has always read ENOENT this way. (CodeRabbit on #940.)
func TestRunGCForwardSweepTreatsAVanishedOrphanAsRemoved(t *testing.T) {
	dir := t.TempDir()
	vanishes := filepath.Join(dir, "gone.flac.upscaled-v2-176400-24.flac")
	stays := filepath.Join(dir, "here.flac.upscaled-v2-176400-24.flac")
	writeFixtureFile(t, vanishes, 8)
	writeFixtureFile(t, stays, 8)

	inv, code := gcTakeInventory(context.Background(), &bytes.Buffer{}, dir, map[string]struct{}{})
	if code != 0 || inv.Orphans != 2 {
		t.Fatalf("inventory: code=%d orphans=%d", code, inv.Orphans)
	}
	// Another process gets to one of them first.
	if err := os.Remove(vanishes); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	removed, _, failed, exitCode := runGCForwardSweep(context.Background(), &bytes.Buffer{}, &stderr, inv)
	if failed != 0 || exitCode != 0 {
		t.Fatalf("a file that vanished before the unlink counted as a failure (failed=%d exit=%d): %s",
			failed, exitCode, stderr.String())
	}
	if removed != 2 {
		t.Errorf("removed=%d, want both counted gone", removed)
	}
	if _, err := os.Stat(stays); !os.IsNotExist(err) {
		t.Errorf("control: the orphan that was still there survived (%v)", err)
	}
	// A real failure must still be one. A directory in the orphan list
	// (which the walk never produces, but the loop cannot know that) fails
	// with ENOTEMPTY, not ENOENT.
	sub := filepath.Join(dir, "sub")
	writeFixtureFile(t, filepath.Join(sub, "child.flac"), 1)
	stderr.Reset()
	_, _, failed, _ = runGCForwardSweep(context.Background(), &bytes.Buffer{}, &stderr,
		integrity.SidecarInventory{OrphanPaths: []string{sub}})
	if failed != 1 {
		t.Errorf("a genuine remove failure was swallowed (failed=%d): %s", failed, stderr.String())
	}
}
