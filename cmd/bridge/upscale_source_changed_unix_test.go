//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/flactest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// fakeRenderingSoxCLI stands in for sox(1): it writes a rendition to its
// output argument, the argv entry after `-t flac`, whatever its input is: a
// whole FLAC at the 48 kHz / 16-bit target the test's job asks for, which
// transcode.Run reads before it publishes.
var fakeRenderingSoxCLI = `#!/bin/sh
out=""; prev=""; prev2=""
for a in "$@"; do
  if [ "$prev2" = "-t" ] && [ "$prev" = "flac" ]; then out="$a"; break; fi
  prev2="$prev"; prev="$a"
done
[ -n "$out" ] || { echo "fake sox: no output argument" >&2; exit 1; }
printf '` + flactest.ShPrintf(flactest.Stream(48000, 2, 16, flactest.Block, 0)) + `' > "$out"
exit 0
`

// TestTheCLIRendersNothingFromAFileThatChangedDuringItsRun: `bridge
// optimize` / `render` / `upscale` classify the whole library first and
// render afterwards, so a file retagged while the run works through the
// candidates before it reaches the worker with the version classification
// saw. The worker renders through transcode.Run, which checks the file
// again: the file is reported as a failure naming the change, and no
// rendition is recorded. On main the worker rendered the new bytes under the
// row's older stamp and recorded a rendition the serve path refuses. The
// positive control renders an unchanged file.
func TestTheCLIRendersNothingFromAFileThatChangedDuringItsRun(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sox"), []byte(fakeRenderingSoxCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, changed := range []bool{true, false} {
		t.Run(map[bool]string{true: "changed", false: "unchanged"}[changed], func(t *testing.T) {
			dir := t.TempDir()
			store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			const rel = "Album/01.flac"
			src := filepath.Join(dir, "lib", "Album", "01.flac")
			if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			rate, bits := 96000.0, 24
			if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: fi.Size(),
				ModTime: fi.ModTime(), Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
				t.Fatal(err)
			}
			spec := transcode.JobSpec{SourceAbsPath: src, SourceLibraryRel: rel,
				SourceMTimeNS: fi.ModTime().UnixNano(), SourceSize: fi.Size(), SourceSampleRate: 96000, SourceBits: 24,
				TargetSampleRate: 48000, TargetBits: 16, Kind: transcode.JobKindOptimize,
				Quality: transcode.QualityVeryHigh, OutputDir: filepath.Join(dir, "variants")}
			if changed {
				f, err := os.OpenFile(src, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.Write([]byte("retagged"))
				_ = f.Close()
				later := time.Now().Add(time.Minute)
				if err := os.Chtimes(src, later, later); err != nil {
					t.Fatal(err)
				}
			}

			jobs := make(chan upscaleCandidate, 1)
			jobs <- upscaleCandidate{spec: spec, needsRun: true}
			close(jobs)
			var stderr bytes.Buffer
			var done, failed uint64
			runUpscaleWorker(context.Background(), &stderr, store, jobs, &done, &failed)
			row, err := store.GetVariant(context.Background(), rel, spec.VariantID())
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				if done != 1 || row == nil {
					t.Fatalf("the positive control: done=%d row=%v stderr=%q, want it rendered and recorded", done, row, stderr.String())
				}
				return
			}
			if failed != 1 || done != 0 || !strings.Contains(stderr.String(), "FAIL "+rel) ||
				!strings.Contains(stderr.String(), "changed on disk") {
				t.Errorf("done=%d failed=%d stderr=%q, want one FAIL naming the file and the change", done, failed, stderr.String())
			}
			if row != nil {
				t.Errorf("a rendition was recorded (stamped %d/%d) from bytes that are not that version", row.SourceMTimeNS, row.SourceSize)
			}
		})
	}
}
