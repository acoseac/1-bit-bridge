package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, newWaveforms, false); rc != 0 {
		t.Fatalf("runAnalyzeGC rc=%d\nstderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("analyze --gc unlinked the relocated waveform: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("control: the true orphan survived (%v)", err)
	}
}
