package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The doctor half of the 2026-09-20 follow-up. These drive
// buildDoctorDeps and doctor.Run — a check nothing dispatches to, or a
// probe nothing wires, is one of the three shapes this repo records for
// "shipped a dead feature with a green suite", and internal/doctor's own
// tests cannot see either.

// variantsIndexInstall writes a bridge.yaml whose variants directory is
// `dir`, seeds `rows` variant rows with their files at the canonical
// paths, and strands `orphans` more files that no row references.
func variantsIndexInstall(t *testing.T, dir string, rows, orphans int) (cfgPath, variantsDir string) {
	t.Helper()
	cfgPath = writeInstallAt(t, dir, "Artist/Album/01.flac")
	variantsDir = filepath.Join(dir, "variants")
	appendYAML(t, cfgPath, "upscale:\n    enabled: true\n    variantsDir: "+variantsDir+"\n")

	store, err := manifest.OpenStore(manifest.DefaultDBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	const variant = "upscaled-v2-176400-24"
	for i := 0; i < rows; i++ {
		source := fmt.Sprintf("Artist/Kept/%02d.flac", i)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		p := transcode.VariantSidecarPath(variantsDir, source, variant)
		writeFixtureFile(t, p, 50)
		if err := store.UpsertVariant(ctx, manifest.VariantRow{
			SourcePath: source, VariantID: variant, SidecarPath: p, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < orphans; i++ {
		writeFixtureFile(t, transcode.VariantSidecarPath(variantsDir,
			fmt.Sprintf("Artist/Stranded %d/%02d.flac", i%3, i), variant), 1000)
	}
	// The directory must exist even with nothing in it — an absent one is
	// a different (legitimate) state.
	if err := os.MkdirAll(variantsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfgPath, variantsDir
}

func appendYAML(t *testing.T, cfgPath, body string) {
	t.Helper()
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorReportsAVariantCatalogThatLostItsIndex drives the whole
// pipeline — buildDoctorDeps, the wired probe, doctor.Run — over the
// field report's shape, and asserts the report an operator actually sees.
func TestDoctorReportsAVariantCatalogThatLostItsIndex(t *testing.T) {
	dir := t.TempDir()
	cfgPath, variantsDir := variantsIndexInstall(t, dir, 2, 40)

	d := buildDoctorDeps(cfgPath)
	if d.VariantsIndex == nil {
		t.Fatal("the variants-index probe is not wired — the check would answer 'run after the first scan' forever")
	}
	rep := doctor.Run(context.Background(), d)
	c := findCheck(t, rep, "variants-index")
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "40 of 42 sidecar file(s)") || !strings.Contains(c.Summary, "2 row(s)") {
		t.Errorf("summary does not carry both counts: %q", c.Summary)
	}
	if !strings.Contains(c.Summary, variantsDir) {
		t.Errorf("summary does not name the directory: %q", c.Summary)
	}
	if !strings.Contains(c.Hint, "LOST INDEX") {
		t.Errorf("hint does not recognise the shape: %q", c.Hint)
	}
	// The sample is relative to the directory the summary already names —
	// a doctor report gets pasted into issues.
	if strings.Contains(c.Hint, variantsDir) {
		t.Errorf("hint repeats absolute paths: %q", c.Hint)
	}
	if !strings.Contains(c.Hint, "Artist"+string(filepath.Separator)+"Stranded") {
		t.Errorf("hint names no example: %q", c.Hint)
	}
	// The sample is capped and the rest are accounted for — 40 orphans,
	// doctorVariantsIndexSamples named, the remainder counted. A hint that
	// listed all forty would scroll the summary off the screen.
	if want := fmt.Sprintf("+%d more", 40-doctorVariantsIndexSamples); !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not cap its sample at %d and account for the rest (%q): %q",
			doctorVariantsIndexSamples, want, c.Hint)
	}
	// The verdict has to come from the SWEEP's own threshold, not a
	// second copy of the rule: what the doctor warns about is exactly
	// what `bridge upscale --gc` refuses.
	if !strings.Contains(c.Hint, "REFUSES") {
		t.Errorf("hint does not say the sweep would refuse: %q", c.Hint)
	}
}

// TestDoctorVariantsIndexIsQuietOnAHealthyBridge — the check must not be
// a permanent warning. A catalog whose rows and files agree is `ok`, and
// the counts are still in the summary.
func TestDoctorVariantsIndexIsQuietOnAHealthyBridge(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := variantsIndexInstall(t, dir, 6, 0)
	rep := doctor.Run(context.Background(), buildDoctorDeps(cfgPath))
	c := findCheck(t, rep, "variants-index")
	if c.Status != doctor.OK {
		t.Fatalf("a healthy bridge warns: %s / %s", c.Summary, c.Hint)
	}
	if !strings.Contains(c.Summary, "6 variant row(s), 6 sidecar file(s), all referenced") {
		t.Errorf("summary: %q", c.Summary)
	}
}

// TestDoctorVariantsIndexAcceptsARelocatedCatalog — the probe builds its
// known set with integrity.KnownSidecarSet, so a catalog whose rows all
// name the OLD host reads as fully referenced rather than as a tree of
// orphans. Without the canonical spelling this check would fire on every
// bridge that had just been moved, which is the one moment its warning
// must not be noise.
func TestDoctorVariantsIndexAcceptsARelocatedCatalog(t *testing.T) {
	dir := t.TempDir()
	cfgPath, variantsDir := variantsIndexInstall(t, dir, 0, 0)
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")

	store, err := manifest.OpenStore(manifest.DefaultDBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const variant = "upscaled-v2-176400-24"
	for i := 0; i < 12; i++ {
		source := fmt.Sprintf("Artist/Moved/%02d.flac", i)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, transcode.VariantSidecarPath(variantsDir, source, variant), 50)
		if err := store.UpsertVariant(ctx, manifest.VariantRow{
			SourcePath: source, VariantID: variant,
			SidecarPath: transcode.VariantSidecarPath(oldDir, source, variant), Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()

	rep := doctor.Run(ctx, buildDoctorDeps(cfgPath))
	c := findCheck(t, rep, "variants-index")
	if c.Status != doctor.OK {
		t.Fatalf("a relocated catalog read as a lost index: %s / %s", c.Summary, c.Hint)
	}
}

// TestDoctorVariantsIndexProbeReportsAnUnreadableManifest — the same
// reading the sidecar-paths probe took after CodeRabbit's #937 round: a
// database that is there but cannot be read must reach the check as an
// error, never as ok/"no manifest".
func TestDoctorVariantsIndexProbeReportsAnUnreadableManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes do not deny stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	cfgPath, _ := variantsIndexInstall(t, dir, 2, 0)
	dataDir := filepath.Join(dir, "data")

	if err := os.Chmod(dataDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })
	d := buildDoctorDeps(cfgPath)
	if d.VariantsIndex == nil {
		t.Fatal("probe left unwired for an unreadable manifest")
	}
	if _, err := d.VariantsIndex(context.Background()); err == nil {
		t.Fatal("probe over an unreadable manifest returned no error")
	}

	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	if d = buildDoctorDeps(cfgPath); d.VariantsIndex != nil {
		t.Fatal("probe wired for a manifest that does not exist")
	}
}

// TestDoctorVariantsIndexWalkIsBounded — `/api/doctor` is fetched on a
// settings-page render (the prereq chips), not only from the "Run checks"
// button, so this walk must not be unbounded on a 200k-sidecar tree.
//
// Two halves, because they can fail independently. The BEHAVIOUR (a walk
// stops at its budget and scopes its answer) is driven over a tiny tree
// with a tiny budget. The WIRING (the closure buildDoctorDeps builds
// passes the real constant rather than 0) is read off the result, because
// a probe that walked unbounded would return every count looking perfectly
// right — the one mistake no number here reveals.
func TestDoctorVariantsIndexWalkIsBounded(t *testing.T) {
	dir := t.TempDir()
	cfgPath, variantsDir := variantsIndexInstall(t, dir, 0, 0)
	for i := 0; i < 12; i++ {
		writeFixtureFile(t, filepath.Join(variantsDir, "d", fmt.Sprintf("%06d.flac", i)), 1)
	}
	dbPath := manifest.DefaultDBPath(filepath.Join(dir, "data"))

	// The budget counts TRAVERSED entries, so five of them here are the
	// root, `d`, and three files — Files lands below the cap by however
	// many directories the walk crossed, which is exactly what makes the
	// cap a wall-clock bound rather than a bound on candidates.
	idx, err := variantsIndexCounts(context.Background(), dbPath, variantsDir, 20, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Truncated || idx.Files != 3 {
		t.Fatalf("truncated=%v files=%d, want a walk that stopped at the budget of 5 entries (root + d + 3 files)",
			idx.Truncated, idx.Files)
	}
	// The wiring: whatever buildDoctorDeps builds must carry a real cap.
	d := buildDoctorDeps(cfgPath)
	if d.VariantsIndex == nil {
		t.Fatal("probe not wired")
	}
	wired, err := d.VariantsIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if wired.Budget != doctorVariantsIndexBudget {
		t.Fatalf("the wired probe walks under budget %d, want %d — an unbounded walk here is a settings-page render "+
			"walking the whole variants tree, and every count it returns still looks right", wired.Budget, doctorVariantsIndexBudget)
	}
	if doctorVariantsIndexBudget <= 0 {
		t.Fatal("the budget constant does not bound anything")
	}
	// Under it, the tiny tree is answered whole — the cap must not make
	// every bridge's report a scoped one.
	if wired.Truncated {
		t.Errorf("a 12-file tree was truncated at a budget of %d", doctorVariantsIndexBudget)
	}
}

func findCheck(t *testing.T, rep doctor.Report, name string) doctor.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in the report — it is declared but never dispatched to", name)
	return doctor.Check{}
}
