//go:build !windows

package transcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// fakeRenderingSox stands in for sox(1): it marks that it ran, changes the
// file FAKE_SOX_CHANGE names when that is set (a tagger writing the source
// while the render reads it), and writes a rendition to its output argument,
// the argv entry after `-t flac`: a whole FLAC at the 48 kHz / 16-bit target
// the test's jobs ask for.
var fakeRenderingSox = `#!/bin/sh
out=""; prev=""; prev2=""
for a in "$@"; do
  if [ "$prev2" = "-t" ] && [ "$prev" = "flac" ]; then out="$a"; break; fi
  prev2="$prev"; prev="$a"
done
[ -n "$out" ] || { echo "fake sox: no output argument" >&2; exit 1; }
: > "$FAKE_SOX_RAN"
if [ -n "$FAKE_SOX_CHANGE" ]; then printf 'retag' >> "$FAKE_SOX_CHANGE"; fi
` + shWrites("$out", stereoFLAC(48000, 16)) + `
exit 0
`

// TestAJobWhoseSourceChangedIsNotRenderedAndStrikesNothing drives the real
// pool running the real Run, with a stand-in sox that renders anything.
//
// A file that changes while its job waits in the queue, or while it
// renders, was rendered from the new bytes under the row's older stamp:
// measured on main, the job succeeded (done=1 failed=0) and the serve path
// answered 410 variant_stale for the rendition from then on. A file deleted
// while its job waited went to the tool too, which with a real sox fails on
// the missing input and strikes the source. Now the job
// fails with ErrSourceChanged: nothing is published or recorded, no temp is
// left, the source takes no strike (three would suppress a good file for
// 30 days), and the pool asks for a rescan of the file's directory, naming
// it by its row's path. A file changed while it waited is never handed to
// the tool at all. The positive control renders an unchanged file.
func TestAJobWhoseSourceChangedIsNotRenderedAndStrikesNothing(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sox"), []byte(fakeRenderingSox), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, tc := range []struct {
		name        string
		change      string // "" (no change), "queued" or "rendering"
		wantToolRan bool
	}{
		{name: "changed while it waited", change: "queued"},
		{name: "changed while it rendered", change: "rendering", wantToolRan: true},
		// Gone is a change too: on the tool, a missing input is a strike.
		// A NAS mount that drops under a queued batch looks like this to
		// every job behind it.
		{name: "deleted while it waited", change: "deleted"},
		{name: "unchanged", wantToolRan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := filepath.Join(t.TempDir(), "ran")
			t.Setenv("FAKE_SOX_RAN", ran)
			t.Setenv("FAKE_SOX_CHANGE", "")
			store := openTempStoreForPool(t)
			const rel = "Album/01.flac"
			src := filepath.Join(t.TempDir(), "lib", "01.flac")
			writeSource(t, src, 4096)
			spec := stampedAsScanned(JobSpec{SourceAbsPath: src, SourceLibraryRel: rel, SourceSampleRate: 96000,
				SourceBits: 24, TargetSampleRate: 48000, TargetBits: 16, Kind: JobKindOptimize,
				Quality: QualityVeryHigh, OutputDir: t.TempDir()})
			rate, bits := 96000.0, 24
			if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: spec.SourceSize,
				ModTime: time.Unix(0, spec.SourceMTimeNS), Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
				t.Fatal(err)
			}
			switch tc.change {
			case "queued":
				retag(t, src)
			case "rendering":
				t.Setenv("FAKE_SOX_CHANGE", src)
			case "deleted":
				if err := os.Remove(src); err != nil {
					t.Fatal(err)
				}
			}

			p := NewPool(store, 1, 4)
			t.Cleanup(p.Stop)
			p.fsyncFn = noopFsync
			var mu sync.Mutex
			var rescans []string
			p.SetSourceRescan(func(r string) {
				mu.Lock()
				defer mu.Unlock()
				rescans = append(rescans, r)
			})
			outcome := make(chan string, 1)
			p.SetOnJobFailed(func(_, _, errMsg string, _ float64, _ uuid.UUID, _ time.Time) { outcome <- "failed: " + errMsg })
			p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) { outcome <- "done" })
			if err := p.Enqueue(spec); err != nil {
				t.Fatal(err)
			}
			var got string
			select {
			case got = <-outcome:
			case <-time.After(10 * time.Second):
				t.Fatal("the job never finished")
			}

			_, statErr := os.Stat(ran)
			if toolRan := statErr == nil; toolRan != tc.wantToolRan {
				t.Errorf("the tool ran = %v, want %v", toolRan, tc.wantToolRan)
			}
			row, err := store.GetVariant(context.Background(), rel, spec.VariantID())
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotRescans := append([]string(nil), rescans...)
			mu.Unlock()
			if tc.change == "" {
				if got != "done" || row == nil {
					t.Fatalf("the positive control: %s, row %v, want it rendered and recorded", got, row)
				}
				if len(gotRescans) != 0 {
					t.Errorf("rescans %v for an unchanged file, want none", gotRescans)
				}
				return
			}
			if !strings.HasPrefix(got, "failed: ") || !strings.Contains(got, "changed on disk") {
				t.Fatalf("the job over a changed source announced %q, want a failure saying the file changed", got)
			}
			if row != nil {
				t.Errorf("a rendition was recorded (stamped %d/%d) from a source that is no longer that version: "+
					"the serve path answers it 410", row.SourceMTimeNS, row.SourceSize)
			}
			if _, err := os.Stat(spec.SidecarPath()); !os.IsNotExist(err) {
				t.Errorf("stat rendition = %v, want nothing published", err)
			}
			if tmps, _ := filepath.Glob(spec.SidecarPath() + ".*"); len(tmps) != 0 {
				t.Errorf("temps left beside the rendition: %v", tmps)
			}
			if n, err := store.ClearVariantFailuresUnderPrefix(context.Background(), "Album"); err != nil || n != 0 {
				t.Errorf("clearing strikes = (%d, %v), want 0: a changed source is not a bad file", n, err)
			}
			if len(gotRescans) != 1 || gotRescans[0] != rel {
				t.Errorf("rescans asked for %v, want [%s]", gotRescans, rel)
			}
		})
	}
}
