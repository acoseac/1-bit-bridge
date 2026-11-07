//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// consolePoolCounts is the pool part of GET /api/upscale/stats this test
// waits on.
type consolePoolCounts struct {
	Pool *struct {
		QueueLen int    `json:"queueLen"`
		Inflight int    `json:"inflight"`
		Enqueued uint64 `json:"enqueued"`
		Done     uint64 `json:"done"`
		Failed   uint64 `json:"failed"`
	} `json:"pool"`
}

// waitForThePoolToSettle waits, until the test binary's deadline, for serve's
// upscale pool to have taken at least one job and finished every job it took,
// and returns its counts then.
func waitForThePoolToSettle(t *testing.T, b *consoleBridge) consolePoolCounts {
	t.Helper()
	deadline := serveGiveUpTime(t)
	for {
		var got consolePoolCounts
		getJSON(t, b.console, b.adminBase+"/api/upscale/stats", "", &got)
		if p := got.Pool; p != nil && p.Enqueued > 0 && p.QueueLen == 0 && p.Inflight == 0 && p.Done+p.Failed >= p.Enqueued {
			return got
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			t.Fatalf("serve's upscale pool never finished a job before the test's deadline (%+v); stderr=%s",
				got.Pool, b.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServesOwnPoolWritesNothingIntoAVariantsVolumeThatIsAway: serve's own
// upscale pool, the one its pre-generation sweep, the console's renders and a
// download's render feed (the path a NAS's late volume really takes), asks the
// output guard serve sets on it (upscalePool.SetOutputGuard). The variants
// directory is on a volume mounted at mnt, which is away: mnt is a bare
// directory, and the catalog records a rendition under the directory. The
// sweep, asked from the console as its Sweep now does, offers the pool a
// hi-res track, which the stand-in sox would render; the pool's guard refuses
// it: nothing is made under mnt, nothing is published, the refusal is the
// pool's output Warn, and the row recorded under the volume is still there
// once serve stops. The CLI's renders and the pool test set guards of their
// own; this is the one test that reaches serve's.
func TestServesOwnPoolWritesNothingIntoAVariantsVolumeThatIsAway(t *testing.T) {
	useRenderingSox(t)
	base := t.TempDir()
	mnt := filepath.Join(base, "usb")
	variantsDir := filepath.Join(mnt, "variants")
	const rel = "Artist/Album/01.flac"
	const recorded = "upscaled-v2-176400-24"
	var dataDir string
	fill := func(lib string) {
		src := filepath.Join(lib, filepath.FromSlash(rel))
		writeHiResFLACFixture(t, src)
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		// The rendition the catalog records, made while the volume was there.
		dataDir = filepath.Join(filepath.Dir(lib), "data")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		rate, bits := 96000.0, 24
		if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: fi.Size(),
			ModTime: fi.ModTime(), Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
			t.Fatal(err)
		}
		sidecar := transcode.VariantSidecarPath(variantsDir, rel, recorded)
		writeFixtureFile(t, sidecar, 4096)
		if err := store.UpsertVariant(context.Background(), manifest.VariantRow{SourcePath: rel, VariantID: recorded,
			SidecarPath: sidecar, Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 4096,
			SourceMTimeNS: fi.ModTime().UnixNano(), SourceSize: fi.Size(), CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		// The volume goes: its tree away, the bare mount point left.
		if err := os.Rename(mnt, mnt+".volume"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(mnt, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Registered before startConsoleBridge registers its drain, so each runs
	// after serve has returned: the sweep loop reads the delay once, as it
	// starts, and serve's pool logs into the recorder.
	prev := autoOptimizeSettleDelay
	autoOptimizeSettleDelay = time.Millisecond
	t.Cleanup(func() { autoOptimizeSettleDelay = prev })
	rec := loggingtest.Record(t)
	// minFreeBytes 1: the sweep's free-space floor must not be what keeps it
	// from queueing on a small CI volume.
	b := startConsoleBridge(t, "upscale:\n  enabled: true\n  variantsDir: "+variantsDir+
		"\n  autoOptimize:\n    enabled: true\n    minFreeBytes: 1\n", fill, withUsableSox)

	before := b.autoOptimizeSweeps.Load()
	nudgeAutoOptimize(t, b.console, b.adminBase)
	if card := waitForAutoOptimizeSweep(t, b, before); card.Last == nil {
		t.Fatalf("fixture broken: the pre-generation card records no sweep; stderr=%s", b.stderr.String())
	}
	got := waitForThePoolToSettle(t, b)

	entries, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		_ = filepath.WalkDir(mnt, func(p string, _ os.DirEntry, _ error) error {
			if r, err := filepath.Rel(mnt, p); err == nil && r != "." {
				names = append(names, filepath.ToSlash(r))
			}
			return nil
		})
		t.Errorf("serve's pool made %v under the mount point of the variants volume while it was away", names)
	}
	if got.Pool.Done != 0 {
		t.Errorf("serve's pool published %d rendition(s) into a variants directory that reads as away (%+v)",
			got.Pool.Done, *got.Pool)
	}
	var refusals []string
	for _, l := range rec.Carrying("pool: rendition output unavailable") {
		if strings.HasPrefix(l, "WARN") && strings.Contains(l, "output=variants") {
			refusals = append(refusals, l)
		}
	}
	if len(refusals) != 1 {
		t.Errorf("serve's pool said %d time(s) that the variants output is away, want once:\n%s",
			len(refusals), strings.Join(rec.All(), "\n"))
	}

	stopServed(t, b.servedBridge)
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if row, err := store.GetVariant(context.Background(), rel, recorded); err != nil || row == nil {
		t.Errorf("the row of the rendition on the volume that is away is gone (%v, %v)", row, err)
	}
}
