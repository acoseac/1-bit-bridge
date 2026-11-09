//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// fakeAnalysingSoxCLI stands in for sox in the analysis decode, with
// nothing but the shell's own builtins (the test's PATH holds this alone):
// the channel and duration probes answer two channels and a tenth of a
// second, and the decode writes that tenth of a second of two-channel
// silence as raw little-endian float32, 4,800 frames of eight zero bytes.
const fakeAnalysingSoxCLI = `#!/bin/sh
if [ "$1" = "--i" ]; then
  case "$2" in
    -c) echo 2 ;;
    -D) echo 0.100000 ;;
  esac
  exit 0
fi
i=0
while [ "$i" -lt 4800 ]; do
  printf '\000\000\000\000\000\000\000\000'
  i=$((i+1))
done
exit 0
`

// useAnalysingSox puts fakeAnalysingSoxCLI on PATH, alone.
func useAnalysingSox(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sox"), []byte(fakeAnalysingSoxCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

// analysisPoolCounts is the pool part of GET /api/analysis/stats.
type analysisPoolCounts struct {
	Pool *struct {
		QueueLen int    `json:"queueLen"`
		Inflight int    `json:"inflight"`
		Enqueued uint64 `json:"enqueued"`
		Done     uint64 `json:"done"`
		Failed   uint64 `json:"failed"`
	} `json:"pool"`
}

// waitForAnAnalysisJob waits, until the test binary's deadline, for serve's
// analysis pool to have taken a job and finished every job it took.
func waitForAnAnalysisJob(t *testing.T, b *consoleBridge) analysisPoolCounts {
	t.Helper()
	deadline := serveGiveUpTime(t)
	for {
		var got analysisPoolCounts
		getJSON(t, b.console, b.adminBase+"/api/analysis/stats", "", &got)
		if p := got.Pool; p != nil && p.Enqueued > 0 && p.QueueLen == 0 && p.Inflight == 0 && p.Done+p.Failed >= p.Enqueued {
			return got
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			t.Fatalf("serve's analysis pool never finished a job before the test's deadline (%+v); stderr=%s",
				got.Pool, b.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestARepairWritesNothingIntoAWaveformVolumeThatWentAfterItsLookup: the
// lookup's judgment admits the repair on the waveform directory as it was,
// the job waits in the queue, and the volume goes before it runs (moved
// aside, another directory at its path; or nothing at the path at all). The
// real waveform-only repair, decoding through a stand-in sox, makes no
// directory and no file there and records no strike, and the row is as it
// was. A guard, green on 3d0a0d05 (which queues nothing); its negative
// control (the repair's checks of the directory removed) shows the curve made
// in the directory that is not the volume's.
func TestARepairWritesNothingIntoAWaveformVolumeThatWentAfterItsLookup(t *testing.T) {
	for _, gone := range []string{"replaced", "removed"} {
		t.Run(gone, func(t *testing.T) {
			useAnalysingSox(t)
			f := newRegenFixture(t, 2)
			rel := f.tracks[0]
			moved := make(chan struct{})
			f.setRunner(func(ctx context.Context, spec analyze.RepairSpec) (analyze.RepairResult, error) {
				select {
				case <-moved:
				case <-ctx.Done():
					return analyze.RepairResult{}, ctx.Err()
				}
				return analyze.RunWaveformRepair(ctx, spec)
			})
			f.deleteCurve(t, rel)
			before := f.row(t, rel)
			f.lookup(t, rel)
			f.regen.guard.Wait()
			if err := os.Rename(f.dir, f.dir+".volume"); err != nil {
				t.Fatal(err)
			}
			if gone == "replaced" {
				if err := os.Mkdir(f.dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			close(moved)
			f.settle(t)
			if gone == "replaced" {
				if left, err := os.ReadDir(f.dir); err != nil || len(left) != 0 {
					t.Errorf("the repair made %v (%v) in the directory now at the waveform directory's path", left, err)
				}
			} else if _, err := os.Lstat(f.dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the repair made the waveform directory again (%v)", err)
			}
			if rec, err := f.store.GetWaveformRegenFailure(f.ctx, rel); err != nil || rec != nil {
				t.Errorf("the volume going was counted against the curve: %+v (%v)", rec, err)
			}
			if after := f.row(t, rel); after.WaveformPath != before.WaveformPath || after.WaveformTag != before.WaveformTag {
				t.Errorf("the row moved to %s (%s), want it as it was", after.WaveformPath, after.WaveformTag)
			}
		})
	}
}

// TestServeRendersADeletedCurveAgainOnItsLookup boots the real serve with
// analysis on, over two analysed tracks whose rows carry measurements no
// decode of them produces, deletes one curve and asks for it as a phone does:
// the first GET /v1/waveform answers 410 as it always has, and asks for the
// curve to be rendered again (the lookup serve wires, its regenerator, the
// catalog's guard, serve's own analysis pool and the real waveform-only
// repair, decoding through a stand-in sox); the next GET serves the curve,
// and the row's measurements are as they were. The adapter's tests stand in
// for the repair; this is the one that reaches serve's wiring. On 3d0a0d05
// the curve stays missing.
func TestServeRendersADeletedCurveAgainOnItsLookup(t *testing.T) {
	useAnalysingSox(t)
	tracks := []string{"Artist/Album/01 One.flac", "Artist/Album/02 Two.flac"}
	rec := loggingtest.Record(t)
	b := startConsoleBridge(t, "analysis:\n  enabled: true\n", func(lib string) {
		for _, rel := range tracks {
			writeHiResFLACFixture(t, filepath.Join(lib, filepath.FromSlash(rel)))
		}
	}, withUsableSox)
	waitForTracksIndexed(t, b.console, b.adminBase, len(tracks), b.stderr)

	dir := analyze.WaveformDirFor(b.dataDir)
	canonical := func(rel string) string {
		return analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: rel}.SidecarPath()
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(b.dataDir))
	if err != nil {
		t.Fatalf("open serve's store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, rel := range tracks {
		src, err := os.Stat(filepath.Join(b.lib, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		curve := []byte("1BWF-the-first-" + rel)
		if err := os.MkdirAll(filepath.Dir(canonical(rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canonical(rel), curve, 0o600); err != nil {
			t.Fatal(err)
		}
		row := manifest.AnalysisRow{
			SourcePath: rel, WaveformPath: canonical(rel), WaveformTag: tagOf(curve), WaveformSize: int64(len(curve)),
			SourceMTimeNS: src.ModTime().UnixNano(), SourceSize: src.Size(),
			SchemaVersion: analyze.WaveformSchemaVersion, CreatedAt: 1111,
		}
		seededMeasurements(&row)
		if err := store.UpsertAnalysis(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(canonical(tracks[0])); err != nil {
		t.Fatal(err)
	}

	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"waveform regeneration"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	if token == "" {
		t.Fatalf("the console's pairing link carries no token: %s", mint.PairURL)
	}
	path := "/v1/waveform?path=" + urlquery.Escape(tracks[0])
	if status, _ := getBytesWithToken(t, b, path, token); status != http.StatusGone {
		t.Fatalf("GET %s over a deleted curve = %d, want 410 as before", path, status)
	}
	got := waitForAnAnalysisJob(t, b)
	if got.Pool.Enqueued != 1 || got.Pool.Done != 1 {
		t.Fatalf("serve's analysis pool = %+v, want the one repair enqueued and done; stderr=%s", *got.Pool, b.stderr.String())
	}
	status, body := getBytesWithToken(t, b, path, token)
	if status != http.StatusOK || !bytes.HasPrefix(body, []byte("1BWF")) {
		t.Fatalf("GET %s after the repair = %d, %d bytes; want 200 and a curve; stderr=%s", path, status, len(body), b.stderr.String())
	}
	row, err := store.GetAnalysis(t.Context(), tracks[0])
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.WaveformTag != tagOf(body) || row.WaveformSize != int64(len(body)) {
		t.Errorf("row's waveform tag %s size %d, want the served curve's %s %d", row.WaveformTag, row.WaveformSize, tagOf(body), len(body))
	}
	if row.KeyMode != "minor" || row.AudioMD5State != "mismatch" || string(row.Spectrum) != "1BSP-seeded-curve" ||
		row.BPM == nil || *row.BPM != 171 || row.CreatedAt != 1111 {
		t.Errorf("the repair through serve rewrote a measurement: %+v", *row)
	}
	if lines := rec.Carrying("waveform lookup: rendering a missing curve again"); len(lines) != 1 {
		t.Errorf("serve said %d time(s) that it renders the curve again, want once:\n%s", len(lines), strings.Join(rec.All(), "\n"))
	}
}
