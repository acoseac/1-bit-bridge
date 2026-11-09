package analyze

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// fakeCurveSource stands in for sox: two channels of a deterministic signal
// of frames frames, so a test can render a curve on any host. onDecode, when
// set, runs inside the decode, where a test acts while the repair is between
// its checks.
func fakeCurveSource(frames int, onDecode func()) curveSource {
	return curveSource{
		probe: func(context.Context, string) curveProbe {
			return curveProbe{channels: 2, channelsOK: true, tool: decoderSox,
				expectedSec: float64(frames) / AnalysisSampleRate}
		},
		decode: func(_ context.Context, _ string, pr curveProbe, onFrame func([]float64)) (int64, error) {
			if onDecode != nil {
				onDecode()
			}
			frame := make([]float64, pr.channels)
			for i := 0; i < frames; i++ {
				v := 0.5 * math.Sin(float64(i)/37)
				frame[0], frame[1] = v, -v/2
				onFrame(frame)
			}
			return int64(frames), nil
		},
	}
}

// failingCurveSource's decode fails with err.
func failingCurveSource(err error) curveSource {
	s := fakeCurveSource(1, nil)
	s.decode = func(context.Context, string, curveProbe, func([]float64)) (int64, error) { return 0, err }
	return s
}

// repairFixture is a source file and a waveform directory, and the spec of a
// repair of the source's curve admitted on that directory.
type repairFixture struct {
	root, dir string
	spec      RepairSpec
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "waveforms")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "lib", "Artist", "Album", "01.wav")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("RIFF-not-really-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	id, err := fsutil.DirIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &repairFixture{root: root, dir: dir, spec: RepairSpec{
		AnalyzeSpec: AnalyzeSpec{
			SourceAbsPath: src, SourceLibraryRel: "Artist/Album/01.wav",
			SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size(), OutputDir: dir,
		},
		OutputDirIdentity: id,
		Build:             "test-build",
	}}
}

// replaceDir stands in for a volume that went: the directory moved aside and
// another, empty, made at its path (a mountpoint the local directory again).
func (f *repairFixture) replaceDir(t *testing.T) {
	t.Helper()
	if err := os.Rename(f.dir, f.dir+".volume"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// entriesUnder lists what is below dir, relative to it.
func entriesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, rerr := filepath.Rel(dir, p); rerr == nil && rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

// TestTheRepairRendersTheCurveTheAnalysisRenders: a curve rendered again is
// the curve an analysis of the same source renders, byte for byte, so its tag
// is the tag the row records and nothing a client keys on moves. Both go
// through decodeCurve, the one body.
func TestTheRepairRendersTheCurveTheAnalysisRenders(t *testing.T) {
	f := newRepairFixture(t)
	src := fakeCurveSource(48000*3+17, nil)
	analysed, err := runAnalysis(context.Background(), f.spec.AnalyzeSpec, src)
	if err != nil {
		t.Fatalf("analysis: %v", err)
	}
	want, err := os.ReadFile(analysed.WaveformPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(analysed.WaveformPath); err != nil {
		t.Fatal(err)
	}
	repaired, err := runWaveformRepair(context.Background(), f.spec, src)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	got, err := os.ReadFile(repaired.WaveformPath)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.WaveformPath != analysed.WaveformPath || !bytes.Equal(got, want) ||
		repaired.WaveformTag != analysed.WaveformTag || repaired.WaveformSize != analysed.WaveformSize {
		t.Fatalf("the repair wrote %s (tag %s, %d bytes), the analysis %s (tag %s, %d bytes): a curve rendered "+
			"again must be the analysis's own, or its tag moves for nothing",
			repaired.WaveformPath, repaired.WaveformTag, len(got), analysed.WaveformPath, analysed.WaveformTag, len(want))
	}
}

// TestTheRepairReplacesAPartialCopyAtomically: a file of another size at the
// canonical path (a copy in flight, cut short) is replaced by the curve
// written beside it and renamed over it: until the rename the partial copy is
// there as it was, the new curve in its scratch beside it, and after it the
// path names another file holding the whole curve. Never deleted first. Red
// on 3d0a0d05, which renders nothing again.
func TestTheRepairReplacesAPartialCopyAtomically(t *testing.T) {
	f := newRepairFixture(t)
	final := f.spec.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		t.Fatal(err)
	}
	partial := []byte("1BWF-cut-short")
	if err := os.WriteFile(final, partial, 0o600); err != nil {
		t.Fatal(err)
	}
	before := openedIdentity(t, final)

	var atRename struct {
		seen          bool
		final, beside []byte
	}
	hooks := &repairHooks{beforeRename: func(tmp, dst string) {
		atRename.seen = true
		atRename.final, _ = os.ReadFile(dst)
		atRename.beside, _ = os.ReadFile(tmp)
	}}
	res, err := runWaveformRepairWith(context.Background(), f.spec, fakeCurveSource(48000, nil), hooks)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !atRename.seen {
		t.Fatal("the repair renamed nothing")
	}
	if !bytes.Equal(atRename.final, partial) {
		t.Errorf("just before the rename the canonical path held %q, want the partial copy as it was: "+
			"a repair writes beside it and never deletes it first", atRename.final)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, atRename.beside) || len(got) == len(partial) || int64(len(got)) != res.WaveformSize {
		t.Errorf("the canonical path holds %d bytes after the repair, want the %d the repair wrote beside it",
			len(got), len(atRename.beside))
	}
	if os.SameFile(before, openedIdentity(t, final)) {
		t.Error("the canonical path names the same file as the partial copy: it was written over in place, " +
			"not replaced by a rename")
	}
	if left := entriesUnder(t, filepath.Dir(final)); len(left) != 1 {
		t.Errorf("the curve's directory holds %v after the repair, want the curve alone", left)
	}
}

// openedIdentity is path's identity read from an open handle, which Windows
// reads at the call (fsutil.DirIdentity says why never os.Stat).
func openedIdentity(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestARepairWritesNothingIntoAWaveformDirectoryThatWent: a repair admitted
// on a waveform directory that has gone since its judgment (moved aside and
// another directory at its path, a volume's mountpoint the local directory
// again; or nothing at the path at all) makes no directory and no file at
// the path, and what it answers counts nothing against the curve; one that
// goes once the repair's root is open leaves no file in the directory the
// root holds either, its scratch removed there (the curve's directories it
// made there stay). Asked first, before the source is opened; again after
// the decode, as the root opens on the job's identity; and again before the
// rename. A guard: it asserts only what must not happen, so it is green on
// 3d0a0d05, which has no repair, and its negative controls (the checks
// removed) show the directory and the curve made.
func TestARepairWritesNothingIntoAWaveformDirectoryThatWent(t *testing.T) {
	t.Run("before the repair began", func(t *testing.T) {
		f := newRepairFixture(t)
		f.replaceDir(t)
		decoded := false
		_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, func() { decoded = true }))
		requireNoStrikeFor(t, err)
		if decoded {
			t.Error("the repair decoded the source over a waveform directory that went: a decode is what " +
				"the check spares")
		}
		if left := entriesUnder(t, f.dir); len(left) != 0 {
			t.Errorf("the repair made %v in the directory now at the waveform directory's path", left)
		}
	})
	t.Run("during the decode", func(t *testing.T) {
		f := newRepairFixture(t)
		_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, func() { f.replaceDir(t) }))
		requireNoStrikeFor(t, err)
		if left := entriesUnder(t, f.dir); len(left) != 0 {
			t.Errorf("the repair made %v in the directory now at the waveform directory's path", left)
		}
	})
	t.Run("gone altogether", func(t *testing.T) {
		f := newRepairFixture(t)
		if err := os.Remove(f.dir); err != nil {
			t.Fatal(err)
		}
		_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil))
		requireNoStrikeFor(t, err)
		if _, serr := os.Lstat(f.dir); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("the waveform directory is at its path again after the repair (%v): a repair never "+
				"makes the waveform directory", serr)
		}
	})
	t.Run("before the rename", func(t *testing.T) {
		f := newRepairFixture(t)
		final := f.spec.SidecarPath()
		_, err := runWaveformRepairWith(context.Background(), f.spec, fakeCurveSource(48000, nil),
			&repairHooks{beforeRename: func(string, string) { f.replaceDir(t) }})
		requireNoStrikeFor(t, err)
		if _, serr := os.Lstat(final); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("a curve was published at %s in a waveform directory that went before its rename (%v)", final, serr)
		}
		// Nor in the directory it was admitted on, now moved aside, which its
		// writes go through: the check before the rename refused the rename
		// there (round 2), and its scratch was cleaned up there too.
		if left := regularFilesUnder(t, f.dir+".volume"); len(left) != 0 {
			t.Errorf("the directory the repair was admitted on holds %v, want the rename never made and its scratch gone", left)
		}
	})
	t.Run("no identity carried", func(t *testing.T) {
		f := newRepairFixture(t)
		f.spec.OutputDirIdentity = nil
		_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil))
		requireNoStrikeFor(t, err)
		if left := entriesUnder(t, f.dir); len(left) != 0 {
			t.Errorf("a repair carrying no waveform directory made %v", left)
		}
	})
}

func requireNoStrikeFor(t *testing.T, err error) {
	t.Helper()
	if CountsAgainstTheCurve(err) {
		t.Errorf("a waveform directory that went counts against the curve (%v): it earns no strike", err)
	}
}

// TestARepairOverAWaveformDirectoryThatWentSaysSo: the refusal is
// ErrWaveformDirAway, naming no path.
func TestARepairOverAWaveformDirectoryThatWentSaysSo(t *testing.T) {
	f := newRepairFixture(t)
	f.replaceDir(t)
	_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil))
	if !errors.Is(err, ErrWaveformDirAway) || CountsAgainstTheCurve(err) {
		t.Fatalf("repair error = %v, want ErrWaveformDirAway counting nothing", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte(f.root)) {
		t.Errorf("the refusal %q names the data directory's path", err)
	}
}

// TestARepairRefusesASourceThatIsNotItsRowsVersion: the source must still be
// the version the row's measurements describe, before the decode and after
// it; a changed one goes through ordinary analysis, an unreachable one is a
// fault of the host, and neither counts against the curve or writes one.
func TestARepairRefusesASourceThatIsNotItsRowsVersion(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, f *repairFixture)
		want   error
	}{
		{"changed before", func(t *testing.T, f *repairFixture) {
			if err := os.WriteFile(f.spec.SourceAbsPath, []byte("another version, longer"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, ErrSourceChanged},
		{"gone before", func(t *testing.T, f *repairFixture) {
			if err := os.Remove(f.spec.SourceAbsPath); err != nil {
				t.Fatal(err)
			}
		}, ErrSourceUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.change(t, f)
			_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil))
			if !errors.Is(err, tc.want) || CountsAgainstTheCurve(err) {
				t.Fatalf("repair error = %v (counts: %t), want %v counting nothing", err, CountsAgainstTheCurve(err), tc.want)
			}
			if left := entriesUnder(t, f.dir); len(left) != 0 {
				t.Errorf("the repair wrote %v for a source that is not its row's version", left)
			}
		})
	}
	t.Run("changed during the decode", func(t *testing.T) {
		f := newRepairFixture(t)
		_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, func() {
			if err := os.WriteFile(f.spec.SourceAbsPath, []byte("rewritten while it decoded"), 0o644); err != nil {
				t.Fatal(err)
			}
		}))
		if !errors.Is(err, ErrSourceChanged) {
			t.Fatalf("repair error = %v, want ErrSourceChanged", err)
		}
		if left := entriesUnder(t, f.dir); len(left) != 0 {
			t.Errorf("the repair wrote %v, a curve of bytes that changed under its decode", left)
		}
	})
	t.Run("a decoder's refusal of a source that went is no verdict", func(t *testing.T) {
		f := newRepairFixture(t)
		src := failingCurveSource(markUnreadable(errors.New("sox: can't open input file")))
		src.probe = func(context.Context, string) curveProbe {
			if err := os.Remove(f.spec.SourceAbsPath); err != nil {
				t.Fatal(err)
			}
			return curveProbe{channels: 2, channelsOK: true}
		}
		_, err := runWaveformRepair(context.Background(), f.spec, src)
		if !errors.Is(err, ErrSourceUnreachable) || CountsAgainstTheCurve(err) {
			t.Fatalf("repair error = %v (counts: %t), want ErrSourceUnreachable counting nothing: a mount that "+
				"dropped during the decode answers as a verdict", err, CountsAgainstTheCurve(err))
		}
	})
}

// TestWhatARepairCountsAgainstTheCurve: a decoder's verdict on a source still
// there and unchanged, and a curve rendered and not written, count; nothing
// else does.
func TestWhatARepairCountsAgainstTheCurve(t *testing.T) {
	t.Run("a decoder's verdict", func(t *testing.T) {
		f := newRepairFixture(t)
		_, err := runWaveformRepair(context.Background(), f.spec,
			failingCurveSource(markUnreadable(errors.New("sox: decoded 1.0s of 9.0s probed — source appears truncated"))))
		if !SourceUnreadable(err) || !CountsAgainstTheCurve(err) {
			t.Fatalf("repair error = %v, want the decoder's verdict, counted", err)
		}
	})
	t.Run("a decoder that could not run", func(t *testing.T) {
		f := newRepairFixture(t)
		_, err := runWaveformRepair(context.Background(), f.spec,
			failingCurveSource(errors.New(`start sox: exec: "sox": executable file not found in $PATH`)))
		if err == nil || CountsAgainstTheCurve(err) {
			t.Fatalf("repair error = %v, want a failure that counts nothing", err)
		}
	})
	t.Run("a curve that could not be written", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("a directory's write bit stops nothing on Windows or for root")
		}
		f := newRepairFixture(t)
		if err := os.Chmod(f.dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) })
		_, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil))
		if !errors.Is(err, ErrCurveNotWritten) || !CountsAgainstTheCurve(err) {
			t.Fatalf("repair error = %v, want ErrCurveNotWritten, counted", err)
		}
		if bytes.Contains([]byte(err.Error()), []byte(f.root)) {
			t.Errorf("the failure %q names the data directory's path; it is the reason the doctor prints", err)
		}
	})
}

// waitForPoolSettled waits, until the test binary's deadline, for the pool
// to have finished every job it took: none at all, on a tree that queues no
// repair.
func waitForPoolSettled(t *testing.T, p *Pool, what string) PoolStats {
	t.Helper()
	return waitForPool(t, p, what, func(st PoolStats) bool {
		return st.QueueLen == 0 && st.Inflight == 0 && st.Done+st.Failed+st.Dropped >= st.Enqueued
	})
}

// waitForPool waits, until the test binary's deadline, for cond on the pool's
// counters.
func waitForPool(t *testing.T, p *Pool, what string, cond func(PoolStats) bool) PoolStats {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Minute)
	}
	for {
		st := p.Stats()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the pool's counters never got there: %+v", what, st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// repairPoolFixture is a store with one analysed track whose row carries
// measurements the repair must keep, and a repair of its curve.
type repairPoolFixture struct {
	*repairFixture
	store *manifest.Store
	base  manifest.AnalysisRow
}

func newRepairPoolFixture(t *testing.T) *repairPoolFixture {
	t.Helper()
	f := newRepairFixture(t)
	s := newStore(t)
	ctx := context.Background()
	rel := f.spec.SourceLibraryRel
	if err := s.UpsertTrack(ctx, &manifest.Track{Path: rel, Size: f.spec.SourceSize,
		ModTime: time.Unix(0, f.spec.SourceMTimeNS)}); err != nil {
		t.Fatal(err)
	}
	rg, peak := -3.25, 0.75
	key, bpm, dr, bw := 9, 171, 3, 15001
	if err := s.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath: rel, WaveformPath: f.spec.SidecarPath(), WaveformTag: "0badc0de", WaveformSize: 99,
		SourceMTimeNS: f.spec.SourceMTimeNS, SourceSize: f.spec.SourceSize, SchemaVersion: WaveformSchemaVersion,
		CreatedAt: 1234, ReplayGainTrackDB: &rg, KeyRoot: &key, KeyMode: "minor", BPM: &bpm,
		TruePeakDB: &peak, DRScore: &dr, AudioMD5State: "mismatch", BandwidthHz: &bw,
		Spectrum: []byte("1BSP-seeded"),
	}); err != nil {
		t.Fatal(err)
	}
	base, err := s.GetAnalysis(ctx, rel)
	if err != nil || base == nil {
		t.Fatalf("seeded row: %v %v", base, err)
	}
	f.spec.Base = *base
	return &repairPoolFixture{repairFixture: f, store: s, base: *base}
}

// TestTransientRepairFailuresEarnNoStrike: through the pool, a repair that
// failed for a reason that is not the curve's records no regeneration
// failure, and leaves the row as it was: the waveform directory gone, the
// source changed or unreachable, a decoder that could not run, the per-job
// timeout and a shutdown. A guard: it asserts only what must not happen, so
// it is green on 3d0a0d05, which has no repair; its negative control (every
// failure counted) shows the strike.
func TestTransientRepairFailuresEarnNoStrike(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"the waveform directory went", dirAway{errors.New("the directory at the path is another one")}},
		{"the source changed", ErrSourceChanged},
		{"the source is unreachable", ErrSourceUnreachable},
		{"a decoder that could not run", errors.New(`start sox: exec: "sox": executable file not found`)},
	}
	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairPoolFixture(t)
			p := NewPool(f.store, 1, 4, WithFsync(noFsync),
				WithRepairRunner(func(context.Context, RepairSpec) (RepairResult, error) { return RepairResult{}, tc.err }))
			defer p.Stop()
			if err := p.EnqueueRepair(f.spec); err != nil {
				t.Fatal(err)
			}
			waitForPoolSettled(t, p, tc.name)
			requireNoStrike(t, f)
		})
	}
	t.Run("the per-job timeout", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := NewPool(f.store, 1, 4, WithFsync(noFsync), WithJobTimeout(20*time.Millisecond),
			WithRepairRunner(func(ctx context.Context, _ RepairSpec) (RepairResult, error) {
				<-ctx.Done()
				return RepairResult{}, markUnreadable(errors.New("sox: killed"))
			}))
		defer p.Stop()
		if err := p.EnqueueRepair(f.spec); err != nil {
			t.Fatal(err)
		}
		waitForPoolSettled(t, p, "timeout")
		requireNoStrike(t, f)
	})
	t.Run("a shutdown", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		var started atomic.Bool
		p := NewPool(f.store, 1, 4, WithFsync(noFsync),
			WithRepairRunner(func(ctx context.Context, _ RepairSpec) (RepairResult, error) {
				started.Store(true)
				<-ctx.Done()
				return RepairResult{}, markUnreadable(errors.New("sox: killed"))
			}))
		if err := p.EnqueueRepair(f.spec); err != nil {
			t.Fatal(err)
		}
		// The repair running, or a pool with nothing to run.
		waitForPool(t, p, "the repair running", func(st PoolStats) bool {
			return started.Load() || st.Inflight == 0 && st.Done+st.Failed+st.Dropped >= st.Enqueued
		})
		p.Stop()
		requireNoStrike(t, f)
	})
}

// TestARepairThatFailsOnTheCurvesAccountIsCounted: a decoder's verdict and a
// curve not written are counted, once each, against the repair's build and
// the source's version, and the row is left as it was.
func TestARepairThatFailsOnTheCurvesAccountIsCounted(t *testing.T) {
	for _, counted := range []struct {
		name string
		err  error
	}{
		{"a decoder's verdict", markUnreadable(errors.New("sox: decoded 1.0s of 9.0s probed — source appears truncated"))},
		{"a curve not written", curveNotWritten{errors.New("no space left on device")}},
	} {
		t.Run(counted.name, func(t *testing.T) {
			f := newRepairPoolFixture(t)
			p := NewPool(f.store, 1, 4, WithFsync(noFsync),
				WithRepairRunner(func(context.Context, RepairSpec) (RepairResult, error) { return RepairResult{}, counted.err }))
			defer p.Stop()
			if err := p.EnqueueRepair(f.spec); err != nil {
				t.Fatal(err)
			}
			if st := waitForPoolSettled(t, p, counted.name); st.Enqueued != 1 || st.Failed != 1 {
				t.Fatalf("pool = %+v, want the one repair enqueued and failed", st)
			}
			rec, err := f.store.GetWaveformRegenFailure(context.Background(), f.spec.SourceLibraryRel)
			if err != nil || rec == nil || rec.Failures != 1 || rec.Build != f.spec.Build ||
				rec.SourceMTimeNS != f.spec.SourceMTimeNS || rec.SourceSize != f.spec.SourceSize {
				t.Fatalf("record after %s = %+v (%v), want one failure against the build and the source's version", counted.name, rec, err)
			}
			requireRowAsSeeded(t, f)
		})
	}
}

func requireNoStrike(t *testing.T, f *repairPoolFixture) {
	t.Helper()
	if rec, err := f.store.GetWaveformRegenFailure(context.Background(), f.spec.SourceLibraryRel); err != nil || rec != nil {
		t.Errorf("a transient failure was counted against the curve: %+v (%v)", rec, err)
	}
	requireRowAsSeeded(t, f)
}

func requireRowAsSeeded(t *testing.T, f *repairPoolFixture) {
	t.Helper()
	row, err := f.store.GetAnalysis(context.Background(), f.spec.SourceLibraryRel)
	if err != nil || row == nil {
		t.Fatalf("row: %v %v", row, err)
	}
	if row.WaveformPath != f.base.WaveformPath || row.WaveformTag != f.base.WaveformTag ||
		row.WaveformSize != f.base.WaveformSize || !sameMeasurements(*row, f.base) {
		t.Errorf("a failed repair changed the row: %+v, want %+v", *row, f.base)
	}
}

// sameMeasurements compares everything on a row but its waveform columns.
func sameMeasurements(a, b manifest.AnalysisRow) bool {
	eqF := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqI := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.SourceMTimeNS == b.SourceMTimeNS && a.SourceSize == b.SourceSize &&
		a.SchemaVersion == b.SchemaVersion && a.CreatedAt == b.CreatedAt &&
		eqF(a.ReplayGainTrackDB, b.ReplayGainTrackDB) && eqI(a.KeyRoot, b.KeyRoot) && a.KeyMode == b.KeyMode &&
		eqI(a.BPM, b.BPM) && eqF(a.TruePeakDB, b.TruePeakDB) && eqI(a.DRScore, b.DRScore) &&
		a.AudioMD5State == b.AudioMD5State && a.AudioMD5Attempts == b.AudioMD5Attempts &&
		eqI(a.BandwidthHz, b.BandwidthHz) && bytes.Equal(a.Spectrum, b.Spectrum)
}

// TestARepairRecordsTheCurveAndNothingElse: through the pool, a repair's
// commit writes the waveform's three columns and keeps every measurement the
// row carries, values no decode of this source would produce; the record of
// earlier failures is cleared. Red on 3d0a0d05, which renders nothing again.
func TestARepairRecordsTheCurveAndNothingElse(t *testing.T) {
	f := newRepairPoolFixture(t)
	if _, err := f.store.RecordWaveformRegenFailure(context.Background(), f.spec.SourceLibraryRel, f.spec.Build,
		f.spec.SourceMTimeNS, f.spec.SourceSize, "an earlier failure"); err != nil {
		t.Fatal(err)
	}
	p := NewPool(f.store, 1, 4, WithFsync(noFsync),
		WithRepairRunner(func(ctx context.Context, spec RepairSpec) (RepairResult, error) {
			return runWaveformRepair(ctx, spec, fakeCurveSource(48000, nil))
		}))
	defer p.Stop()
	if err := p.EnqueueRepair(f.spec); err != nil {
		t.Fatal(err)
	}
	st := waitForPoolSettled(t, p, "repair")
	if st.Done != 1 || st.Enqueued != 1 {
		t.Fatalf("pool = %+v, want the one repair enqueued and done", st)
	}
	row, err := f.store.GetAnalysis(context.Background(), f.spec.SourceLibraryRel)
	if err != nil || row == nil {
		t.Fatalf("row: %v %v", row, err)
	}
	curve, err := os.ReadFile(f.spec.SidecarPath())
	if err != nil {
		t.Fatal(err)
	}
	if row.WaveformPath != f.spec.SidecarPath() || row.WaveformSize != int64(len(curve)) ||
		row.WaveformTag != waveformTag(curve) {
		t.Errorf("row's waveform = %s, %d bytes, tag %s; want the curve written, %d bytes, tag %s",
			row.WaveformPath, row.WaveformSize, row.WaveformTag, len(curve), waveformTag(curve))
	}
	if !sameMeasurements(*row, f.base) {
		t.Errorf("the repair changed the row's measurements: %+v, want them as seeded %+v", *row, f.base)
	}
	if rec, err := f.store.GetWaveformRegenFailure(context.Background(), f.spec.SourceLibraryRel); err != nil || rec != nil {
		t.Errorf("the record of earlier failures stands after a repair succeeded: %+v (%v)", rec, err)
	}
}
