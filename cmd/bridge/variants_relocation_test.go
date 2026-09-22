package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The 2026-09-20 field report against the CLI and the serve wiring —
// the integrity package's own tests cover the watcher; these drive the
// real store through `bridge upscale --gc`, `bridge analyze --gc` and the
// /v1/download lookup adapter, the other three consumers of a recorded
// sidecar path that used to treat "not at the recorded path" as "gone".

// relocatedStore seeds n tracks whose variant rows record a sidecar under
// oldDir (never created on this host) while the file sits byte-identical
// at its canonical place under newDir. Returns the store and the canonical
// paths, in track order.
func relocatedStore(t *testing.T, oldDir, newDir string, n int) (*manifest.Store, []string) {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	canonical := make([]string, n)
	for i := 0; i < n; i++ {
		source := fmt.Sprintf("Artist/Album %d/%02d - Track.flac", i%3, i)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		const variant = "upscaled-v2-176400-24"
		canonical[i] = transcode.VariantSidecarPath(newDir, source, variant)
		if err := os.MkdirAll(filepath.Dir(canonical[i]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canonical[i], make([]byte, 1000+i), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertVariant(ctx, manifest.VariantRow{
			SourcePath: source, VariantID: variant,
			SidecarPath: transcode.VariantSidecarPath(oldDir, source, variant), Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: int64(1000 + i),
			SourceMTimeNS: 1, SourceSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return store, canonical
}

// TestRunGCAdoptsARelocatedCatalogAndKeepsItsFiles — `bridge upscale --gc`
// after the move. The forward sweep must not unlink the files (its known
// set carries the canonical spelling), and the reverse sweep must adopt
// the rows rather than reap them. A true orphan file and a row missing
// at both locations, seeded beside them, are the positive controls: the
// sweep still does what it is for.
func TestRunGCAdoptsARelocatedCatalogAndKeepsItsFiles(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := filepath.Join(t.TempDir(), "srv", "bridge-variants")
	store, canonical := relocatedStore(t, oldDir, newDir, 12)
	ctx := context.Background()

	orphan := filepath.Join(newDir, "Artist", "Album 0", "nobody.flac.upscaled-v2-176400-24.flac")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: "Artist/Gone/01.flac", Size: 1, ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(ctx, manifest.VariantRow{
		SourcePath: "Artist/Gone/01.flac", VariantID: "upscaled-v2-176400-24",
		SidecarPath: transcode.VariantSidecarPath(oldDir, "Artist/Gone/01.flac", "upscaled-v2-176400-24"),
		Format:      "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 5, SourceMTimeNS: 1, SourceSize: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(ctx, &stdout, &stderr, store, newDir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc != 0 {
		t.Fatalf("runGC rc=%d\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}
	for _, p := range canonical {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("forward sweep unlinked a relocated sidecar: %s: %v", p, err)
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("control: the true orphan survived the forward sweep (%v)", err)
	}
	rows, err := store.AllVariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 12 {
		t.Fatalf("%d rows after --gc, want the 12 relocated ones (the row missing at both locations reaped)", len(rows))
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.SidecarPath, newDir+string(filepath.Separator)) {
			t.Errorf("row %s still records %s — not adopted", r.SourcePath, r.SidecarPath)
		}
	}
	if !strings.Contains(stdout.String(), "adopted 12 relocated row(s)") || !strings.Contains(stdout.String(), "removed 1 orphan row(s)") {
		t.Errorf("summary does not say what happened:\n%s", stdout.String())
	}
}

// TestRunGCRefusesAMassDeleteUntilAllowed — the rows point at the old
// host and the tree holds the sidecars, but under a layout the probe
// does not know (here: the whole old tree copied into a subdirectory).
// Adoption cannot rescue them. The gc refuses as a whole — BEFORE the
// forward sweep, which would otherwise unlink every one of those files
// as an orphan and then hand the reverse sweep a tree that "holds no
// sidecars" — names the override, and touches nothing; with
// --allow-mass-delete it runs both sweeps as it always has.
//
// The first draft of this test put the guard inside the reverse sweep
// and asserted only on rows; it went green while the forward sweep had
// already removed the files. The files are the assertion that matters.
func TestRunGCRefusesAMassDeleteUntilAllowed(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	store, canonical := relocatedStore(t, oldDir, newDir, 12)
	ctx := context.Background()
	// The operator copied the old tree to <newDir>/old/… — real sidecars,
	// none at a canonical path.
	moved := make([]string, len(canonical))
	for i, p := range canonical {
		rel, _ := filepath.Rel(newDir, p)
		moved[i] = filepath.Join(newDir, "old", rel)
		if err := os.MkdirAll(filepath.Dir(moved[i]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p, moved[i]); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(ctx, &stdout, &stderr, store, newDir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc == 0 {
		t.Fatalf("runGC proceeded over a relocation in progress\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--allow-mass-delete") || !strings.Contains(stderr.String(), "12 of 12 rows (100%)") {
		t.Errorf("refusal must name the override and the numbers:\n%s", stderr.String())
	}
	rows, _ := store.AllVariants(ctx)
	if len(rows) != 12 {
		t.Fatalf("%d rows after a refused --gc, want all 12 kept", len(rows))
	}
	for _, p := range moved {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused --gc unlinked %s: %v — the guard ran after the forward sweep", p, err)
		}
	}

	stdout.Reset()
	stderr.Reset()
	rc = runGC(ctx, &stdout, &stderr, store, newDir, t.TempDir(), gcOptions{maxDeletePercent: 20, allowMassDelete: true})
	if rc != 0 {
		t.Fatalf("runGC --allow-mass-delete rc=%d\nstderr: %s", rc, stderr.String())
	}
	if rows, _ = store.AllVariants(ctx); len(rows) != 0 {
		t.Errorf("%d rows after --allow-mass-delete, want 0", len(rows))
	}
	for _, p := range moved {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--allow-mass-delete left the orphan %s (%v)", p, err)
		}
	}
}

// TestEveryTranscodeGCCommandOffersTheMassDeleteOverride is the sweep the
// --allow-empty guard already has: every command that reaches runGC (and
// therefore the relocation guard) must offer the way past it. A refusal
// with no override is the shape `artwork --gc` shipped in for months.
func TestEveryTranscodeGCCommandOffersTheMassDeleteOverride(t *testing.T) {
	callRe := regexp.MustCompile(`\brunGC\(`)
	flagRe := regexp.MustCompile(`fs\.Bool\("allow-mass-delete"`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "upscale.go" {
			// upscale.go both defines runGC and calls it; it is checked
			// below by name so the definition does not count as a call.
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if !callRe.MatchString(src) {
			continue
		}
		checked++
		if !flagRe.MatchString(src) {
			t.Errorf("%s calls runGC but does not declare --allow-mass-delete: a relocation refusal there has no way past it", name)
		}
	}
	raw, err := os.ReadFile("upscale.go")
	if err != nil {
		t.Fatal(err)
	}
	if !flagRe.MatchString(strings.ReplaceAll(string(raw), "\r\n", "\n")) {
		t.Errorf("upscale.go does not declare --allow-mass-delete")
	}
	checked++
	if checked < 3 {
		t.Fatalf("only %d runGC callers found — the scan is broken, so this test is not checking anything", checked)
	}
}

// TestVariantStoreAdapterAdoptsARelocatedSidecarOnLookup — the serve-side
// consumer. A /v1/download lookup for a relocated row must come back with
// the canonical path (so serveVariant streams the file instead of reaping
// the row) and must have adopted it in the store; a row whose canonical
// file has the wrong size answers api.ErrVariantSidecarUnavailable, which
// serveVariant maps to a 410 with no reap; a row present at its recorded
// path is untouched.
func TestVariantStoreAdapterAdoptsARelocatedSidecarOnLookup(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	store, canonical := relocatedStore(t, oldDir, newDir, 3)
	ctx := context.Background()
	adapter := &variantStoreAdapter{
		provider:    manifest.NewProvider(store, nil),
		store:       store,
		variantsDir: func() string { return newDir },
	}

	// Row 0: relocated → adopted, canonical path returned.
	rec, err := adapter.LookupVariant(ctx, "Artist/Album 0/00 - Track.flac", "upscaled-v2-176400-24")
	if err != nil || rec == nil {
		t.Fatalf("lookup: rec=%v err=%v", rec, err)
	}
	if rec.SidecarPath != canonical[0] {
		t.Errorf("record path = %s, want the canonical %s", rec.SidecarPath, canonical[0])
	}
	row, err := store.GetVariant(ctx, "Artist/Album 0/00 - Track.flac", "upscaled-v2-176400-24")
	if err != nil || row == nil || row.SidecarPath != canonical[0] {
		t.Errorf("store row after lookup = %+v (err %v), want sidecar_path adopted to %s", row, err, canonical[0])
	}

	// Row 1: the copy is not whole → unavailable, row kept as recorded.
	if err := os.Truncate(canonical[1], 10); err != nil {
		t.Fatal(err)
	}
	rec, err = adapter.LookupVariant(ctx, "Artist/Album 1/01 - Track.flac", "upscaled-v2-176400-24")
	if !errors.Is(err, api.ErrVariantSidecarUnavailable) {
		t.Fatalf("lookup over a partial copy: rec=%v err=%v, want ErrVariantSidecarUnavailable", rec, err)
	}
	row, _ = store.GetVariant(ctx, "Artist/Album 1/01 - Track.flac", "upscaled-v2-176400-24")
	if row == nil || !strings.HasPrefix(row.SidecarPath, oldDir) {
		t.Errorf("a partial copy must leave the row as recorded, got %+v", row)
	}

	// Row 2: missing at both locations → the record is returned as
	// recorded, so serveVariant's ENOENT branch reaps it (its job).
	if err := os.Remove(canonical[2]); err != nil {
		t.Fatal(err)
	}
	rec, err = adapter.LookupVariant(ctx, "Artist/Album 2/02 - Track.flac", "upscaled-v2-176400-24")
	if err != nil || rec == nil || !strings.HasPrefix(rec.SidecarPath, oldDir) {
		t.Errorf("missing at both: rec=%+v err=%v, want the recorded path back", rec, err)
	}

	// A fixture with no store / no dir (the pre-relocation projection)
	// hands the recorded path over untouched.
	bare := &variantStoreAdapter{provider: manifest.NewProvider(store, nil)}
	rec, err = adapter.LookupVariant(ctx, "Artist/Album 0/00 - Track.flac", "upscaled-v2-176400-24")
	if err != nil || rec == nil || rec.SidecarPath != canonical[0] {
		t.Errorf("second lookup of an adopted row: rec=%+v err=%v", rec, err)
	}
	if rec, err := bare.LookupVariant(ctx, "Artist/Album 2/02 - Track.flac", "upscaled-v2-176400-24"); err != nil || rec == nil {
		t.Errorf("bare adapter: rec=%+v err=%v", rec, err)
	}
}

// TestRunAnalyzeGCKeepsARelocatedWaveform — `bridge analyze --gc` after a
// dataDir move: the row records the old dataDir's waveform path, the
// file sits at its canonical place under the current waveform dir. The
// forward sweep must keep it (its known set carries the canonical
// spelling) and still reap the true orphan beside it.
func TestRunAnalyzeGCKeepsARelocatedWaveform(t *testing.T) {
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	oldWaveforms := filepath.Join(t.TempDir(), "old-data", "waveforms")
	newWaveforms := analyze.WaveformDirFor(filepath.Join(t.TempDir(), "new-data"))

	const source = "Artist/Album/01.flac"
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 1, ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	canonical := analyze.AnalyzeSpec{OutputDir: newWaveforms, SourceLibraryRel: source}.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("waveform"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(filepath.Dir(canonical), "nobody.waveform.bin")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath:   source,
		WaveformPath: analyze.AnalyzeSpec{OutputDir: oldWaveforms, SourceLibraryRel: source}.SidecarPath(),
		WaveformTag:  "abcd1234", WaveformSize: 8, SourceMTimeNS: 1, SourceSize: 1,
		SchemaVersion: analyze.WaveformSchemaVersion, CreatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, newWaveforms, false, false); rc != 0 {
		t.Fatalf("runAnalyzeGC rc=%d\nstderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("analyze --gc unlinked the relocated waveform: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("control: the true orphan survived (%v)", err)
	}
}

// TestRunGCKeepsAMismatchedSidecarWithoutFailing — a copy in flight
// (the canonical file is there, shorter than the row records) is a KEEP,
// not a fault: the row stays as recorded, the summary names it, and the
// exit code is 0 — a `--gc` run from cron while a copy lands must not
// report a failed job for a healthy state. The watcher keeps Mismatched
// apart from Failed for the same reason (CodeRabbit on #937).
func TestRunGCKeepsAMismatchedSidecarWithoutFailing(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	store, canonical := relocatedStore(t, oldDir, newDir, 3)
	ctx := context.Background()
	if err := os.Truncate(canonical[1], 10); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(ctx, &stdout, &stderr, store, newDir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc != 0 {
		t.Fatalf("runGC rc=%d for a copy in flight, want 0\nstderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 row(s) with a mismatched sidecar") || !strings.Contains(stdout.String(), "0 failure(s)") {
		t.Errorf("summary should count the mismatch on its own, not as a failure:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not adopted, not deleted") {
		t.Errorf("the keep should be said on stderr:\n%s", stderr.String())
	}
	rows, err := store.AllVariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kept, adopted int
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.SidecarPath, oldDir):
			kept++
		case strings.HasPrefix(r.SidecarPath, newDir):
			adopted++
		}
	}
	if kept != 1 || adopted != 2 {
		t.Errorf("rows: %d kept as recorded, %d adopted; want 1 and 2", kept, adopted)
	}
}

// TestDoctorSidecarProbeReportsAnUnreadableManifest — the probe is left
// unwired only when bridge.db is genuinely absent (a fresh install). A
// database that is there but cannot be read must reach the check as an
// error, never as ok/"no manifest" (CodeRabbit on #937).
func TestDoctorSidecarProbeReportsAnUnreadableManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes do not deny stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
	dataDir := filepath.Join(dir, "data")

	// Present and readable: wired, and it answers.
	d := buildDoctorDeps(cfgPath)
	if d.RelocatedSidecars == nil {
		t.Fatal("probe not wired for a present manifest")
	}
	if _, err := d.RelocatedSidecars(context.Background()); err != nil {
		t.Fatalf("probe over a readable manifest: %v", err)
	}

	// Present but unreadable: still wired, and the error reaches the check.
	if err := os.Chmod(dataDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })
	d = buildDoctorDeps(cfgPath)
	if d.RelocatedSidecars == nil {
		t.Fatal("probe left unwired for an unreadable manifest — the check would answer ok about a database it cannot read")
	}
	if _, err := d.RelocatedSidecars(context.Background()); err == nil {
		t.Fatal("probe over an unreadable manifest returned no error")
	}

	// Absent: not wired, so the check says "run after the first scan".
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	if d = buildDoctorDeps(cfgPath); d.RelocatedSidecars != nil {
		t.Fatal("probe wired for a manifest that does not exist")
	}
}

// TestDLNAVariantLocatorAdoptsARelocatedSidecar — the renderer-facing
// consumer, and the one that could not ask at lookup time.
//
// The DLNA index bakes `sidecar_path` into VariantInfo once per 30 s
// cache rebuild, so after a move every rendition's `<res>` names a file
// that is not there and the file handler answers 410 — which a renderer
// does not fall back from, it just stops. The locator is consulted only
// on the open failure, which is why it can afford to hit the database.
func TestDLNAVariantLocatorAdoptsARelocatedSidecar(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	store, canonical := relocatedStore(t, oldDir, newDir, 3)
	ctx := context.Background()
	loc := newDLNAVariantLocator(store, func() string { return newDir }, slog.Default())
	const variant = "upscaled-v2-176400-24"
	recorded := transcode.VariantSidecarPath(oldDir, "Artist/Album 0/00 - Track.flac", variant)

	got := loc.LocateVariantSidecar(ctx, "Artist/Album 0/00 - Track.flac", variant, recorded)
	if got != canonical[0] {
		t.Errorf("locate = %q, want the canonical %q", got, canonical[0])
	}
	row, err := store.GetVariant(ctx, "Artist/Album 0/00 - Track.flac", variant)
	if err != nil || row == nil || row.SidecarPath != canonical[0] {
		t.Errorf("store row after locate = %+v (err %v), want adopted to %s", row, err, canonical[0])
	}

	// A partial copy answers "nowhere": streaming half a rendition to a
	// renderer is worse than the 410 it already handles.
	if err := os.Truncate(canonical[1], 10); err != nil {
		t.Fatal(err)
	}
	partial := transcode.VariantSidecarPath(oldDir, "Artist/Album 1/01 - Track.flac", variant)
	if got := loc.LocateVariantSidecar(ctx, "Artist/Album 1/01 - Track.flac", variant, partial); got != "" {
		t.Errorf("locate over a partial copy = %q, want \"\"", got)
	}

	// Missing at both → nowhere. Unlike the API adapter this must NOT
	// hand back the recorded path: there is no reaper on this side, and
	// the caller has already failed to open it.
	if err := os.Remove(canonical[2]); err != nil {
		t.Fatal(err)
	}
	gone := transcode.VariantSidecarPath(oldDir, "Artist/Album 2/02 - Track.flac", variant)
	if got := loc.LocateVariantSidecar(ctx, "Artist/Album 2/02 - Track.flac", variant, gone); got != "" {
		t.Errorf("locate over a row missing at both locations = %q, want \"\"", got)
	}

	// The path the caller already failed on is never handed back, even
	// when the probe says the file is present — that failure was
	// permissions or I/O, and re-offering it is a retry loop.
	if got := loc.LocateVariantSidecar(ctx, "Artist/Album 0/00 - Track.flac", variant, canonical[0]); got != "" {
		t.Errorf("locate over the path the caller tried = %q, want \"\"", got)
	}

	// Unwired dependencies yield a nil locator, so the handler's gate
	// stays off rather than nil-dereferencing on the one request it
	// exists for.
	if newDLNAVariantLocator(nil, func() string { return newDir }, slog.Default()) != nil {
		t.Error("a locator with no store must be nil")
	}
	if newDLNAVariantLocator(store, nil, slog.Default()) != nil {
		t.Error("a locator with no variants dir must be nil")
	}
	// A missing LOGGER is defaulted, not refused: it is not a dependency
	// the lookup needs, and the only deref is in the branch where the
	// adoption UPDATE has already failed — the worst place to find a
	// second fault.
	//
	// The CONSTRUCTOR is what gets pinned, not that branch. Reaching it
	// needs UpdateVariantSidecarPath to fail AFTER LookupVariant found
	// the row, i.e. a concurrent delete or a DB fault, and neither is
	// expressible through this API: a first draft deleted the row and
	// re-inserted it, which left the update succeeding and the warn
	// never firing, so the control passed with the default removed. A
	// decision that cannot be driven is pinned where it CAN be — here,
	// that the field is never left nil.
	nolog := newDLNAVariantLocator(store, func() string { return newDir }, nil)
	if nolog == nil {
		t.Fatal("a locator with no logger must still be built — the feature does not depend on it")
	}
	if l, ok := nolog.(*dlnaVariantLocator); !ok || l.log == nil {
		t.Errorf("a locator built with no logger kept a nil one (%T): the adoption-failure "+
			"branch would deref it, on the path that has already gone wrong", nolog)
	}
}

// TestAnalysisStoreAdapterAdoptsARelocatedWaveform — the waveform half
// of the relocation story (#938), and the one with no second chance.
//
// Nothing reaps a `track_analysis` row and nothing regenerates one: the
// analysis skip gate keys on the SOURCE's mtime and size, which a host
// move leaves untouched, so a stranded curve is a 410 that stays a 410
// for the lifetime of the install.
func TestAnalysisStoreAdapterAdoptsARelocatedWaveform(t *testing.T) {
	oldData := filepath.Join(t.TempDir(), "old-data")
	newData := filepath.Join(t.TempDir(), "new-data")
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	const source = "Artist/Album/01 - Track.flac"
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	newDir := analyze.WaveformDirFor(newData)
	canonical := analyze.AnalyzeSpec{OutputDir: newDir, SourceLibraryRel: source}.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2048)
	if err := os.WriteFile(canonical, body, 0o644); err != nil {
		t.Fatal(err)
	}
	// The row records the OLD dataDir, which is what a copied database
	// looks like on the new host.
	recorded := analyze.AnalyzeSpec{
		OutputDir: analyze.WaveformDirFor(oldData), SourceLibraryRel: source,
	}.SidecarPath()
	if err := store.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath: source, WaveformPath: recorded, WaveformTag: "deadbeef",
		WaveformSize: int64(len(body)), SourceMTimeNS: 1, SourceSize: 100,
		SchemaVersion: analyze.WaveformSchemaVersion,
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &analysisStoreAdapter{
		provider:    manifest.NewProvider(store, nil),
		store:       store,
		waveformDir: func() string { return newDir },
	}
	rec, err := adapter.LookupAnalysis(ctx, source)
	if err != nil || rec == nil {
		t.Fatalf("lookup: rec=%v err=%v", rec, err)
	}
	if rec.WaveformPath != canonical {
		t.Errorf("record path = %s, want the canonical %s", rec.WaveformPath, canonical)
	}
	row, err := store.GetAnalysis(ctx, source)
	if err != nil || row == nil || row.WaveformPath != canonical {
		t.Errorf("store row after lookup = %+v (err %v), want waveform_path adopted to %s", row, err, canonical)
	}

	// A partial copy is NOT adopted — the recorded path comes back, the
	// open fails, and the handler answers as it always has.
	if err := os.Truncate(canonical, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAnalysisWaveformPath(ctx, source, recorded); err != nil {
		t.Fatal(err)
	}
	rec, err = adapter.LookupAnalysis(ctx, source)
	if err != nil || rec == nil || rec.WaveformPath != recorded {
		t.Errorf("lookup over a partial copy: rec=%+v err=%v, want the recorded path back", rec, err)
	}

	// The projection-only fixture (no store, no dir) is the
	// pre-relocation behaviour, unchanged.
	bare := &analysisStoreAdapter{provider: manifest.NewProvider(store, nil)}
	if rec, err := bare.LookupAnalysis(ctx, source); err != nil || rec == nil || rec.WaveformPath != recorded {
		t.Errorf("bare adapter: rec=%+v err=%v", rec, err)
	}
}

// TestVariantDeleterAdapterLocatesARowThatRecordedNoPath.
//
// LocateVariantSidecar short-circuited on an empty `sidecar_path` and
// answered "at the recorded path", with no path — which the delete
// handler reads as already-gone and reconciles by deleting the row.
// That makes the row's silence about its own file the end of the
// enquiry, when the canonical path under the current variants directory
// is exactly where a file with no recorded path would be found:
// locateRecordedFile stats "" (ENOENT everywhere) and then the
// canonical one, which is the whole shape #959 added the lookup for.
//
// The cost of the short-circuit was the failure #959 fixed, one branch
// over: deletedCount up, freedBytes flat, the file still on disk, and a
// tree `bridge upscale --gc` then refuses as orphans it cannot explain.
func TestVariantDeleterAdapterLocatesARowThatRecordedNoPath(t *testing.T) {
	dir := t.TempDir()
	const (
		src     = "Artist/Album/01 - Track.flac"
		variant = "upscaled-v2-176400-24"
	)
	canonical := transcode.VariantSidecarPath(dir, src, variant)
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &variantDeleterAdapter{variantsDir: func() string { return dir }}

	got := a.LocateVariantSidecar(api.VariantSummary{
		SourcePath: src, VariantID: variant, SidecarPath: "", SizeBytes: 10,
	})
	if got.Placement != api.VariantSidecarRelocated || got.Path != canonical {
		t.Errorf("locate over an empty recorded path = %+v, want Relocated at %s — "+
			"the file is exactly where a row with no path would put it", got, canonical)
	}

	// A size that disagrees is a copy in flight, the same reading every
	// other row gets: unlink nothing, delete nothing.
	if err := os.WriteFile(canonical, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := a.LocateVariantSidecar(api.VariantSummary{
		SourcePath: src, VariantID: variant, SizeBytes: 10,
	}); got.Placement != api.VariantSidecarCopyInFlight {
		t.Errorf("locate over a size mismatch = %+v, want CopyInFlight", got)
	}

	// Nothing at the canonical path either: the row really is orphaned,
	// and the handler's already-gone path reconciles it. This is the
	// case the short-circuit answered correctly by accident.
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if got := a.LocateVariantSidecar(api.VariantSummary{
		SourcePath: src, VariantID: variant, SizeBytes: 10,
	}); got.Placement != api.VariantSidecarAbsent || got.Path != "" {
		t.Errorf("locate over a row missing everywhere = %+v, want Absent with no path", got)
	}

	// And a fixture with no live directory keeps the pre-relocation
	// behaviour: no directory to judge, nothing to unlink.
	nilDir := &variantDeleterAdapter{}
	if got := nilDir.LocateVariantSidecar(api.VariantSummary{SourcePath: src, VariantID: variant}); got.Path != "" ||
		got.Placement != api.VariantSidecarRecorded {
		t.Errorf("locate with no variants directory = %+v, want Recorded with no path", got)
	}
}

// TestPathUnderRefusesASiblingWithAPrefixName.
//
// `pathUnder` decides whether an unlink can explain the probed variants
// directory being empty, and a string-prefix compare would call
// `/srv/variants-old/x` a child of `/srv/variants` — which is exactly
// the relocation shape, so the one wrong answer it could give is the
// one that matters.
func TestPathUnderRefusesASiblingWithAPrefixName(t *testing.T) {
	dir := filepath.Join("/srv", "variants")
	for _, tc := range []struct {
		name, p string
		want    bool
	}{
		{"a child", filepath.Join(dir, "Artist", "Album", "t.flac.upscaled-v2-176400-24.flac"), true},
		{"the directory itself", dir, true},
		{"a trailing-slash spelling", dir + string(filepath.Separator), true},
		// The prefix trap: a sibling whose name starts with dir's.
		{"a prefix-named sibling", filepath.Join("/srv", "variants-old", "x.flac"), false},
		{"a parent", "/srv", false},
		{"an unrelated tree", filepath.Join("/mnt", "other", "x.flac"), false},
		{"no directory", "", false},
		{"no path", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dir
			if tc.name == "no directory" {
				d = ""
			}
			p := tc.p
			if tc.name == "no path" {
				p = ""
			}
			if got := pathUnder(d, p); got != tc.want {
				t.Errorf("pathUnder(%q, %q) = %v, want %v", d, p, got, tc.want)
			}
		})
	}
}

// stringLiteralsContaining returns every string literal in a Go source
// file whose value contains marker.
//
// go/parser rather than a text scan, because the subject here IS a
// literal and this repo's comment strippers blank those — and the
// comments beside the lines this serves discuss directories by name, so
// a text scan would find its own commentary.
func stringLiteralsContaining(t *testing.T, file, marker string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(v, marker) {
			out = append(out, v)
		}
		return true
	})
	return out
}

// TestUnreadableIsReportedAsEntriesNotDirectories.
//
// SidecarInventory.Unreadable counts entries the walk could not
// resolve — a directory it could not descend into, AND a non-regular
// entry it could not stat, which since #969 includes a Windows
// junction as well as a symlink. Both CLI sweeps print that one count,
// and both called it directories and spoke of "their contents": already
// imprecise for an unstattable link, and plainly wrong for a junction,
// which sends the operator looking for the wrong thing in their own
// tree (CodeRabbit on #969).
//
// Both files, because one count printed from two places is the
// enumeration failure this repo keeps paying for: CodeRabbit flagged
// upscale.go and analyze.go carries the same string.
func TestUnreadableIsReportedAsEntriesNotDirectories(t *testing.T) {
	checked := 0
	for _, file := range []string{"upscale.go", "analyze.go"} {
		lines := stringLiteralsContaining(t, file, "could not be read")
		if len(lines) == 0 {
			t.Errorf("%s has no Unreadable report line — if it moved, move this guard with it", file)
		}
		for _, line := range lines {
			checked++
			if strings.Contains(line, "director") {
				t.Errorf("%s reports Unreadable as directories (%q) — it also counts a link or "+
					"junction the walk could not stat, and naming those directories sends the "+
					"operator after the wrong thing", file, line)
			}
			if !strings.Contains(line, "entr") {
				t.Errorf("%s no longer says what Unreadable counts (%q)", file, line)
			}
		}
	}
	if checked != 2 {
		t.Fatalf("scanned %d report lines, want 2 — the scan is not seeing both sweeps and "+
			"would pass no matter what they say", checked)
	}
}
