package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The CLI half of the partial-walk refusal (2026-09-28). Both `--gc`
// sweeps took their mass-orphan verdict over the part of the tree their
// walk could read, printed "N entries could not be read; they were
// neither counted nor removed", and went on. Measured on main with the
// shape below: `upscale --gc` and `analyze --gc` each unlinked the 15
// visible orphans and exited 0, with 1,000 stranded files behind the
// locked directory that make the whole tree a lost index.

// lockedStrandedFiles writes n files no row references under dir/Locked,
// laid out the way the sidecar path of each would be, and locks the
// directory from this user for the rest of the test. It returns the
// directory, so a test can unlock it to count what survived.
func lockedStrandedFiles(t *testing.T, dir string, n int, sidecarAt func(root, source string) string) string {
	t.Helper()
	locked := filepath.Join(dir, "Locked")
	for i := 0; i < n; i++ {
		writeFixtureFile(t, sidecarAt(locked, fmt.Sprintf("Artist/Hidden %d/%03d.flac", i%4, i)), 10)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	return locked
}

// regularFilesUnder counts the regular files under dir, unlocking locked
// first so the count covers what the sweep could not see.
func regularFilesUnder(t *testing.T, dir, locked string) int {
	t.Helper()
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
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

func variantSidecarAt(root, source string) string {
	return transcode.VariantSidecarPath(root, source, "upscaled-v2-176400-24")
}

// TestRunGCRefusesAPartialWalkUntilAllowed drives the real `--gc` over
// CodeRabbit's #1063 shape: 20 rows each with its file, 15 stranded files
// in view, 1,000 behind a directory this user cannot list. The walk's
// counts (15 orphans of 35 files against 20 rows) pass the mass-orphan
// check; the whole tree's (1,015 of 1,035) refuse. It refuses now, with a
// flag that waives this refusal and nothing else: with
// --allow-partial-walk the check runs over what the walk could list, and
// reclaims the 15.
func TestRunGCRefusesAPartialWalkUntilAllowed(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory, so the walk would not be partial")
	dir := t.TempDir()
	store, stranded := strandedTree(t, dir, 20, 15)
	locked := lockedStrandedFiles(t, dir, 1000, variantSidecarAt)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc == 0 {
		t.Fatalf("--gc took a verdict over part of the tree\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused --gc unlinked %s: %v", p, err)
		}
	}
	for _, want := range []string{
		"could not list 1 director(y/ies)",
		"15 orphan(s) of 35 file(s) against 20 row(s)",
		"--allow-partial-walk",
		"Nothing was unlinked",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal does not say %q:\n%s", want, stderr.String())
		}
	}
	if rows, _ := store.AllVariants(ctx); len(rows) != 20 {
		t.Errorf("%d rows after a refused --gc, want the 20 untouched", len(rows))
	}

	// The waiver lifts this refusal and nothing else: the check runs over
	// the part the walk could list, which is an ordinary crop.
	stdout.Reset()
	stderr.Reset()
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20, allowPartialWalk: true}); rc != 0 {
		t.Fatalf("--allow-partial-walk rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--allow-partial-walk left %s (%v)", p, err)
		}
	}
	if got := regularFilesUnder(t, dir, locked); got != 1020 {
		t.Errorf("%d files left, want the 20 live and the 1,000 the walk could not see", got)
	}

	// Control on the fixture: read whole, the same tree is a lost index,
	// which is the verdict the partial walk would have let through.
	stdout.Reset()
	stderr.Reset()
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc == 0 ||
		!strings.Contains(stderr.String(), "1000 of 1020 file(s)") {
		t.Errorf("read whole, the tree should refuse as a mass orphaning (rc=%d):\n%s", rc, stderr.String())
	}
}

// TestRunGCWaivesThePartialWalkRefusalWithTheMassOrphanOne — the partial
// walk is refused only because it cannot support the mass-orphan verdict.
// `--allow-mass-orphans` has set that verdict aside, so it needs no second
// flag; the reverse is not true, and `--allow-partial-walk` still runs the
// check over what the walk could list (TestRunGCRefusesAPartialWalkUntilAllowed's
// control).
func TestRunGCWaivesThePartialWalkRefusalWithTheMassOrphanOne(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory, so the walk would not be partial")
	dir := t.TempDir()
	store, stranded := strandedTree(t, dir, 20, 15)
	lockedStrandedFiles(t, dir, 30, variantSidecarAt)

	var stdout, stderr bytes.Buffer
	if rc := runGC(context.Background(), &stdout, &stderr, store, dir, t.TempDir(),
		gcOptions{maxDeletePercent: 20, allowMassOrphans: true}); rc != 0 {
		t.Fatalf("--allow-mass-orphans rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--allow-mass-orphans left %s (%v)", p, err)
		}
	}
}

// TestRunGCProceedsPastTheFilesystemsLostFound — a variants directory that
// is an ext4 volume's mount point holds the volume's root-owned lost+found,
// which no `--gc` run as the bridge's user can list. It is the
// filesystem's, not part of the tree, so an ordinary crop there is
// reclaimed with no flag.
func TestRunGCProceedsPastTheFilesystemsLostFound(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists lost+found, so there would be nothing to exempt")
	dir := t.TempDir()
	store, stranded := strandedTree(t, dir, 20, 15)
	lostFound := filepath.Join(dir, "lost+found")
	writeFixtureFile(t, filepath.Join(lostFound, "#12345"), 10)
	if err := os.Chmod(lostFound, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lostFound, 0o755) })

	var stdout, stderr bytes.Buffer
	if rc := runGC(context.Background(), &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("--gc refused over the filesystem's lost+found: rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %s survived (%v)", p, err)
		}
	}
	if strings.Contains(stderr.String(), "could not be read") {
		t.Errorf("the filesystem's lost+found was reported as unreadable:\n%s", stderr.String())
	}
}

// waveformTree seeds `fresh` analysis rows each with its waveform at the
// canonical path, and `stranded` waveform files no row references, under
// dir — the analyze twin of strandedTree.
func waveformTree(t *testing.T, dir string, fresh, stranded int) (*manifest.Store, []string) {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for i := 0; i < fresh; i++ {
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
	paths := make([]string, stranded)
	for i := range paths {
		paths[i] = analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: fmt.Sprintf("Artist/Album %d/%02d.flac", i%4, i)}.SidecarPath()
		writeFixtureFile(t, paths[i], 20)
	}
	return store, paths
}

// TestRunAnalyzeGCRefusesAPartialWalkUntilAllowed — the waveform twin, in
// the same shape and with the same flag: the enumeration lesson again, a
// fix that lists the sites it covers misses one.
func TestRunAnalyzeGCRefusesAPartialWalkUntilAllowed(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory, so the walk would not be partial")
	dir := analyze.WaveformDirFor(t.TempDir())
	store, stranded := waveformTree(t, dir, 20, 15)
	locked := lockedStrandedFiles(t, dir, 1000, func(root, source string) string {
		return analyze.AnalyzeSpec{OutputDir: root, SourceLibraryRel: source}.SidecarPath()
	})
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, dir, analyzeGCOptions{}); rc == 0 {
		t.Fatalf("analyze --gc took a verdict over part of the tree\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused analyze --gc unlinked %s: %v", p, err)
		}
	}
	for _, want := range []string{"analyze --gc: refusing to run", "could not list 1 director(y/ies)", "--allow-partial-walk"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal does not say %q:\n%s", want, stderr.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, dir, analyzeGCOptions{allowPartialWalk: true}); rc != 0 {
		t.Fatalf("analyze --gc --allow-partial-walk rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--allow-partial-walk left %s (%v)", p, err)
		}
	}
	if got := regularFilesUnder(t, dir, locked); got != 1020 {
		t.Errorf("%d files left, want the 20 live and the 1,000 the walk could not see", got)
	}
}

// TestEveryForwardSweepingGCCommandOffersThePartialWalkOverride — the
// sweep, not the symptom, as for --allow-empty, --allow-mass-delete and
// --allow-mass-orphans: every command whose `--gc` unlinks sidecar FILES
// refuses a partial walk now, so each must offer the way past it.
// `artwork --gc` takes no inventory and makes no mass-orphan verdict, and
// is exempt by name.
func TestEveryForwardSweepingGCCommandOffersThePartialWalkOverride(t *testing.T) {
	requireForwardSweepGCOverride(t, "allow-partial-walk")
}
