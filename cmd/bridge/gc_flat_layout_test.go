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

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// flatLegacyTree seeds the pre-source-mirroring layout: every sidecar
// directly under outputDir, no subdirectories at all
// (`<outputDir>/<hash>-<variantID>.flac`). `reapable` rows record a flat
// sidecar that is NOT on disk — the operator deleted it, or a disk
// cleanup did — and `orphans` flat files are left over from a variant
// generation no row names any more. Exactly one `--gc` away from clean,
// and the shape the wedge needs: the forward sweep's removals leave the
// directory with zero dirents, because WalkDir never created any.
func flatLegacyTree(t *testing.T, dir string, reapable, orphans int) (*manifest.Store, []string) {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const variant = "upscaled-v2-176400-24"

	for i := 0; i < reapable; i++ {
		source := fmt.Sprintf("Artist/Album/%02d.flac", i)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		// Flat, hash-named, and deliberately absent from disk.
		flat := filepath.Join(dir, fmt.Sprintf("%040x-%s.flac", i, variant))
		if err := store.UpsertVariant(ctx, manifest.VariantRow{
			SourcePath: source, VariantID: variant, SidecarPath: flat, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	paths := make([]string, orphans)
	for i := 0; i < orphans; i++ {
		// A superseded generation: same flat layout, no row names it.
		paths[i] = filepath.Join(dir, fmt.Sprintf("%040x-upscaled-v1-88200-24.flac", 0xbeef+i))
		writeFixtureFile(t, paths[i], 1000)
	}
	return store, paths
}

// TestRunGCReapsRowsItsOwnForwardSweepEmptiedTheDirFor drives the real
// `--gc` TWICE over a flat-layout fixture with NO override flags.
//
// The wedge: the forward sweep unlinks every file, which leaves the
// directory GENUINELY empty, and the reverse guard then reads that as a
// cleanly-unmounted volume and refuses to reap a single row. Re-running
// cannot help — the directory is still empty, so it refuses again having
// removed nothing, and those rows can never be reaped by `--gc` at all.
// The source-mirrored layout hides it: WalkDir removes no directories,
// so an emptied subtree still leaves dirents behind.
//
// The two-run shape is the point. A single run that merely exits 0 does
// not prove the wedge is gone.
func TestRunGCReapsRowsItsOwnForwardSweepEmptiedTheDirFor(t *testing.T) {
	dir := t.TempDir()
	store, orphans := flatLegacyTree(t, dir, 5, 5)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("run 1 rc=%d, want 0\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}
	for _, p := range orphans {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("run 1 left orphan %s (%v)", p, err)
		}
	}
	if rows, _ := store.AllVariants(ctx); len(rows) != 0 {
		t.Fatalf("run 1 left %d row(s) whose sidecar is gone; the reverse sweep refused\nstderr: %s",
			len(rows), stderr.String())
	}

	// Run 2 is a clean no-op: nothing to unlink, no row to reap.
	stdout.Reset()
	stderr.Reset()
	if rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("run 2 rc=%d, want 0\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}
}

// TestRunGCStillRefusesAVariantsDirThatWasAlreadyEmpty is the positive
// control for the guard that still matters: the directory read empty and
// THIS RUN did nothing to make it so, which is the cleanly-unmounted
// mountpoint the guard was written for (PR #207 / the 2026-07-21 review's
// M15). Asserting on the refusal text naming the mount is what makes a
// fix that simply deletes the guard fail here.
func TestRunGCStillRefusesAVariantsDirThatWasAlreadyEmpty(t *testing.T) {
	dir := t.TempDir()
	store, _ := flatLegacyTree(t, dir, 5, 0) // rows, and not one file on disk
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc == 0 {
		t.Fatalf("--gc reaped rows against an empty variants dir it did not empty\nstdout: %s\nstderr: %s",
			stdout.String(), stderr.String())
	}
	if rows, _ := store.AllVariants(ctx); len(rows) != 5 {
		t.Errorf("%d row(s) survived a refused --gc, want 5", len(rows))
	}
	out := stderr.String()
	for _, want := range []string{"variants directory is empty", "refusing to delete rows"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not say %q:\n%s", want, out)
		}
	}
	// The refusal must NAME the mount — that is the half a fix which simply
	// deleted the guard would lose. Either rendering counts: the message
	// formats the path with %q, and on Windows that comes back with its
	// backslashes ESCAPED (`C:\\Users\\...`), so a bare substring check
	// against the raw path fails on that platform alone (caught by the
	// Windows CI leg). Accepting both keeps the assertion about the mount
	// being named rather than about the verb it is named with.
	if !strings.Contains(out, dir) && !strings.Contains(out, fmt.Sprintf("%q", dir)) {
		t.Errorf("refusal does not name the mount %s:\n%s", dir, out)
	}
}
