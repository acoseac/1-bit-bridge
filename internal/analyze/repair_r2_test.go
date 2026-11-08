package analyze

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// regularFilesUnder lists the regular files below dir, relative to it.
func regularFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

func writeSentinel(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireBody(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("%s holds %q (%v), want %q as it was put there", path, got, err, want)
	}
}

// TestARepairWorksOnlyThroughTheDirectoryItWasAdmittedOn: once the decode is
// done, the waveform directory the repair was admitted on is opened as a root
// and checked against the identity the job carries, and every directory,
// scratch, removal and rename of the repair goes through that root. A folder
// put at the path after that check, holding a scratch and a curve at the
// repair's own names, is left exactly as it was put there: nothing made in
// it, nothing removed, both of them intact; an empty one gets nothing; and
// one put there between the last check and the rename keeps its scratch and
// its curve, the rename landing in the directory the repair was admitted on.
// Red at round 1's code for the first, whose stale-scratch removal by path
// deleted the scratch there (backlog B652, round 2).
func TestARepairWorksOnlyThroughTheDirectoryItWasAdmittedOn(t *testing.T) {
	// sentinels puts a scratch and a curve at the repair's own names in the
	// folder now at the path.
	sentinels := func(t *testing.T, f *repairFixture) {
		t.Helper()
		writeSentinel(t, f.spec.SidecarPath()+AnalysisTmpSuffix, "a scratch at the repair's own name")
		writeSentinel(t, f.spec.SidecarPath(), "a curve at the repair's own name")
	}
	requireSentinels := func(t *testing.T, f *repairFixture) {
		t.Helper()
		want := []string{"Artist", "Artist/Album", "Artist/Album/01.wav.waveform.bin", "Artist/Album/01.wav.waveform.bin.tmp"}
		if got := entriesUnder(t, f.dir); !reflect.DeepEqual(got, want) {
			t.Errorf("the folder now at the path holds %v, want only what was put there: %v", got, want)
		}
		requireBody(t, f.spec.SidecarPath()+AnalysisTmpSuffix, "a scratch at the repair's own name")
		requireBody(t, f.spec.SidecarPath(), "a curve at the repair's own name")
	}
	t.Run("a folder holding a scratch and a curve at its names", func(t *testing.T) {
		f := newRepairFixture(t)
		hooks := &repairHooks{beforeMkdir: func() {
			f.replaceDir(t)
			sentinels(t, f)
		}}
		_, err := runWaveformRepairWith(context.Background(), f.spec, fakeCurveSource(48000, nil), hooks)
		if !errors.Is(err, ErrWaveformDirAway) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v, want ErrWaveformDirAway counting nothing", err)
		}
		requireSentinels(t, f)
		if left := regularFilesUnder(t, f.dir+".volume"); len(left) != 0 {
			t.Errorf("the directory the repair was admitted on holds %v after a refused repair, want no curve and no scratch", left)
		}
	})
	t.Run("an empty folder", func(t *testing.T) {
		f := newRepairFixture(t)
		hooks := &repairHooks{beforeMkdir: func() { f.replaceDir(t) }}
		_, err := runWaveformRepairWith(context.Background(), f.spec, fakeCurveSource(48000, nil), hooks)
		if !errors.Is(err, ErrWaveformDirAway) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v, want ErrWaveformDirAway counting nothing", err)
		}
		if left := entriesUnder(t, f.dir); len(left) != 0 {
			t.Errorf("the repair made %v in the folder now at the path", left)
		}
	})
	t.Run("a folder put there between the last check and the rename", func(t *testing.T) {
		// The rename itself goes through the root: the curve lands in the
		// directory the repair was admitted on, now moved aside, and the
		// scratch and curve at its names in the folder now at the path stay
		// as they are. The pool's check before the commit refuses the commit.
		f := newRepairFixture(t)
		hooks := &repairHooks{atRename: func() {
			f.replaceDir(t)
			sentinels(t, f)
		}}
		res, err := runWaveformRepairWith(context.Background(), f.spec, fakeCurveSource(48000, nil), hooks)
		if err != nil {
			t.Fatalf("repair: %v", err)
		}
		requireSentinels(t, f)
		moved := filepath.Join(f.dir+".volume", "Artist", "Album", "01.wav.waveform.bin")
		if got, err := os.ReadFile(moved); err != nil || int64(len(got)) != res.WaveformSize {
			t.Errorf("the curve in the directory the repair was admitted on: %d bytes (%v), want the %d it wrote", len(got), err, res.WaveformSize)
		}
	})
}

// repairPool is a pool over f's store whose repairs render through the fake
// source and whose durability barrier is sync, the WithFsync seam.
func repairPool(t *testing.T, f *repairPoolFixture, sync func(string) error) *Pool {
	t.Helper()
	p := NewPool(f.store, 1, 4, WithFsync(sync),
		WithRepairRunner(func(ctx context.Context, spec RepairSpec) (RepairResult, error) {
			return runWaveformRepair(ctx, spec, fakeCurveSource(48000, nil))
		}))
	t.Cleanup(p.Stop)
	return p
}

// requireRefusedBeforeItsCommit runs f's repair through p and requires it
// refused before its commit: failed, the row as it was, no strike.
func requireRefusedBeforeItsCommit(t *testing.T, f *repairPoolFixture, p *Pool, what string) {
	t.Helper()
	if err := p.EnqueueRepair(f.spec); err != nil {
		t.Fatal(err)
	}
	if st := waitForPoolSettled(t, p, what); st.Enqueued != 1 || st.Done != 0 || st.Failed != 1 {
		t.Errorf("%s: pool = %+v, want the one repair refused before its commit", what, st)
	}
	requireNoStrike(t, f)
}

// TestARepairCommitsOnlyWhileItsDirectoryAndItsSourceAreTheOnesItRead: right
// before the commit, the directory at the path is asked whether it is still
// the root the curve was written into, and the source whether it is still the
// version the decode read. A root keeps the folder it opened, so a folder
// replaced after the curve's sync took the curve and the sync with it, and a
// commit would record a curve that is not at its path; a source rewritten
// during the sync is another version than the curve's. Neither commits, and
// neither strikes. Red at round 1's code, which committed both.
func TestARepairCommitsOnlyWhileItsDirectoryAndItsSourceAreTheOnesItRead(t *testing.T) {
	t.Run("the folder replaced after a successful sync", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := repairPool(t, f, func(string) error {
			f.replaceDir(t)
			return nil
		})
		requireRefusedBeforeItsCommit(t, f, p, "replaced after the sync")
	})
	t.Run("the source's mtime moved during the sync", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		later := time.Unix(0, f.spec.SourceMTimeNS).Add(time.Hour)
		p := repairPool(t, f, func(string) error {
			return os.Chtimes(f.spec.SourceAbsPath, later, later)
		})
		requireRefusedBeforeItsCommit(t, f, p, "the source moved")
	})
}

// TestASyncFailureIsClassifiedAsEveryWriteFailureIs: the durability barrier's
// failure is a write's failure (writeFailure): with the folder at the path no
// longer the one written into, it is the directory's refusal and counts
// nothing; with the folder as it was, it is a curve not written, counted.
// Red at round 1's code for the first, which counted every sync failure.
func TestASyncFailureIsClassifiedAsEveryWriteFailureIs(t *testing.T) {
	t.Run("after the folder was replaced", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := repairPool(t, f, func(string) error {
			f.replaceDir(t)
			return syscall.EIO
		})
		requireRefusedBeforeItsCommit(t, f, p, "a sync that failed once the folder was replaced")
	})
	t.Run("the folder as it was", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := repairPool(t, f, func(string) error { return syscall.EIO })
		if err := p.EnqueueRepair(f.spec); err != nil {
			t.Fatal(err)
		}
		if st := waitForPoolSettled(t, p, "a sync that failed"); st.Failed != 1 {
			t.Fatalf("pool = %+v, want the repair failed", st)
		}
		rec, err := f.store.GetWaveformRegenFailure(context.Background(), f.spec.SourceLibraryRel)
		if err != nil || rec == nil || rec.Failures != 1 {
			t.Errorf("record after a sync that failed on the curve's own directory = %+v (%v), want one counted failure", rec, err)
		}
		requireRowAsSeeded(t, f)
	})
}

// TestASourceItCannotReadIsUnreachable: a source the repair cannot open or
// read is unreachable, never the curve's failure, asked before the probe and
// again before a decoder's verdict is taken, since a source that went
// unreadable during the decode answers as a verdict too. Red at round 1's
// code, which asked only whether the source could be stat'ed and counted the
// decoder's refusal of a file it could not open.
func TestASourceItCannotReadIsUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode of 0 stops no read on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	verdict := markUnreadable(errors.New("sox FAIL formats: can't open input file"))
	unreadable := func(t *testing.T, path string) {
		t.Helper()
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	}
	t.Run("unreadable before the repair began", func(t *testing.T) {
		f := newRepairFixture(t)
		unreadable(t, f.spec.SourceAbsPath)
		decoded := false
		src := failingCurveSource(verdict)
		failing := src.decode
		src.decode = func(ctx context.Context, p string, pr curveProbe, on func([]float64)) (int64, error) {
			decoded = true
			return failing(ctx, p, pr, on)
		}
		_, err := runWaveformRepair(context.Background(), f.spec, src)
		if !errors.Is(err, ErrSourceUnreachable) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v (counts: %t), want ErrSourceUnreachable counting nothing", err, CountsAgainstTheCurve(err))
		}
		if decoded {
			t.Error("a decoder was started over a source the repair could not read")
		}
	})
	t.Run("made unreadable during the decode", func(t *testing.T) {
		f := newRepairFixture(t)
		src := failingCurveSource(verdict)
		src.decode = func(context.Context, string, curveProbe, func([]float64)) (int64, error) {
			unreadable(t, f.spec.SourceAbsPath)
			return 0, verdict
		}
		_, err := runWaveformRepair(context.Background(), f.spec, src)
		if !errors.Is(err, ErrSourceUnreachable) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v (counts: %t), want ErrSourceUnreachable counting nothing", err, CountsAgainstTheCurve(err))
		}
	})
	t.Run("through the pool, no strike", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		unreadable(t, f.spec.SourceAbsPath)
		p := NewPool(f.store, 1, 4, WithFsync(noFsync),
			WithRepairRunner(func(ctx context.Context, spec RepairSpec) (RepairResult, error) {
				return runWaveformRepair(ctx, spec, failingCurveSource(verdict))
			}))
		t.Cleanup(p.Stop)
		requireRefusedBeforeItsCommit(t, f, p, "an unreadable source")
	})
}
