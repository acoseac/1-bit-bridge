package analyze

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// The states of openerFailingAs's source.
const (
	sourceOpensAndReads int32 = iota
	sourceOpenFails
	sourceReadFails
)

// brokenRead is a source that opens and cannot be read: what a mount whose
// reads fail answers, its size and mtime as they were.
type brokenRead struct{}

func (brokenRead) Read([]byte) (int, error) { return 0, syscall.EIO }
func (brokenRead) Close() error             { return nil }

// openerFailingAs is the repair's own open of its source (RepairSpec.open)
// while state says the source opens and reads; then its open fails (a
// permission gone), or its read does, the source's size and mtime as they
// were. A test's, so that each fails on any host and as root.
func openerFailingAs(state *atomic.Int32) func(string) (io.ReadCloser, error) {
	return func(p string) (io.ReadCloser, error) {
		switch state.Load() {
		case sourceOpenFails:
			return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
		case sourceReadFails:
			return brokenRead{}, nil
		}
		return openSourceFile(p)
	}
}

// replaceDirFromAnyGoroutine is replaceDir for a hook that may run on the
// pool's worker: a failure is reported without ending that goroutine.
func replaceDirFromAnyGoroutine(t *testing.T, f *repairFixture) {
	if err := os.Rename(f.dir, f.dir+".volume"); err != nil {
		t.Error(err)
		return
	}
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Error(err)
	}
}

// verdictReplacingTheFolder is a decoder that replaces the waveform directory
// as it decodes, then refuses the source with verdict, the source still
// readable and unchanged: the verdict lands on a folder the repair was not
// admitted on.
func verdictReplacingTheFolder(t *testing.T, f *repairFixture, verdict error) curveSource {
	src := failingCurveSource(verdict)
	src.decode = func(context.Context, string, curveProbe, func([]float64)) (int64, error) {
		replaceDirFromAnyGoroutine(t, f)
		return 0, verdict
	}
	return src
}

// TestADecodersVerdictCountsOnlyOverTheFolderItWasAdmittedOn: a decoder's
// verdict on the source is the curve's failure only while the waveform
// directory at the path is still the one the repair was admitted on. A folder
// replaced during the decode, the source still readable and unchanged, makes
// the verdict the directory's refusal, ErrWaveformDirAway, and no regeneration
// failure is recorded. Red at round 2's code, which asked the source again
// before the verdict counted and not the folder (backlog B652, round 3).
func TestADecodersVerdictCountsOnlyOverTheFolderItWasAdmittedOn(t *testing.T) {
	verdict := markUnreadable(errors.New("sox FAIL formats: can't open input file"))
	t.Run("the runner", func(t *testing.T) {
		f := newRepairFixture(t)
		_, err := runWaveformRepair(context.Background(), f.spec, verdictReplacingTheFolder(t, f, verdict))
		if !errors.Is(err, ErrWaveformDirAway) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v (counts: %t), want ErrWaveformDirAway counting nothing", err, CountsAgainstTheCurve(err))
		}
	})
	t.Run("through the pool, nothing recorded", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := NewPool(f.store, 1, 4, WithFsync(noFsync),
			WithRepairRunner(func(ctx context.Context, spec RepairSpec) (RepairResult, error) {
				return runWaveformRepair(ctx, spec, verdictReplacingTheFolder(t, f.repairFixture, verdict))
			}))
		t.Cleanup(p.Stop)
		requireRefusedBeforeItsCommit(t, f, p, "a verdict over a folder replaced during the decode")
	})
}

// TestAFailedSyncCountsOnlyWhileTheSourceIsTheOneRead: a sync that fails is
// the curve's failure only while the source is still the version the repair
// read and can still be read: a source rewritten, removed, or the same by its
// size and mtime but no longer opened or read as the sync fails earns no
// strike. Red at round 2's code, whose writeFailure asked the folder and the
// cancellation and not the source (backlog B652, round 3).
func TestAFailedSyncCountsOnlyWhileTheSourceIsTheOneRead(t *testing.T) {
	cases := []struct {
		name string
		// during runs inside the sync that fails: the source as it is then.
		during func(t *testing.T, f *repairPoolFixture, state *atomic.Int32)
	}{
		{"the source rewritten", func(t *testing.T, f *repairPoolFixture, _ *atomic.Int32) {
			if err := os.WriteFile(f.spec.SourceAbsPath, []byte("another version, longer"), 0o644); err != nil {
				t.Error(err)
			}
		}},
		{"the source removed", func(t *testing.T, f *repairPoolFixture, _ *atomic.Int32) {
			if err := os.Remove(f.spec.SourceAbsPath); err != nil {
				t.Error(err)
			}
		}},
		{"the source no longer opened", func(_ *testing.T, _ *repairPoolFixture, state *atomic.Int32) {
			state.Store(sourceOpenFails)
		}},
		{"the source no longer read", func(_ *testing.T, _ *repairPoolFixture, state *atomic.Int32) {
			state.Store(sourceReadFails)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairPoolFixture(t)
			var state atomic.Int32
			f.spec.open = openerFailingAs(&state)
			p := repairPool(t, f, func(string) error {
				tc.during(t, f, &state)
				return syscall.EIO
			})
			requireRefusedBeforeItsCommit(t, f, p, "a sync that failed with "+tc.name)
		})
	}
}

// TestBothReadsOfTheSourceAreAskedOnEveryHost: the source's one-byte read is
// asked through the repair's own open (RepairSpec.open, a test's here, so
// that a read fails on any host and as root, where a mode of 0 stops none):
// before the probe, where an open that succeeds and a read that fails make
// the source unreachable and no decoder starts; and again before a decoder's
// verdict counts, where a read that fails then, the first having passed,
// makes the verdict the source's, counting nothing. Each red with its own read
// removed (backlog B652, round 3).
func TestBothReadsOfTheSourceAreAskedOnEveryHost(t *testing.T) {
	readFailing := func() func(string) (io.ReadCloser, error) {
		var state atomic.Int32
		state.Store(sourceReadFails)
		return openerFailingAs(&state)
	}
	t.Run("the read before the probe", func(t *testing.T) {
		f := newRepairFixture(t)
		f.spec.open = readFailing()
		started := false
		src := fakeCurveSource(48000, nil)
		probe := src.probe
		src.probe = func(ctx context.Context, p string) curveProbe {
			started = true
			return probe(ctx, p)
		}
		_, err := runWaveformRepair(context.Background(), f.spec, src)
		if !errors.Is(err, ErrSourceUnreachable) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v (counts: %t), want ErrSourceUnreachable counting nothing", err, CountsAgainstTheCurve(err))
		}
		if started {
			t.Error("a decoder was started over a source whose read fails")
		}
		if left := entriesUnder(t, f.dir); len(left) != 0 {
			t.Errorf("the repair wrote %v for a source it could not read", left)
		}
	})
	t.Run("the read before the probe, through the pool", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		f.spec.open = readFailing()
		requireRefusedBeforeItsCommit(t, f, repairPool(t, f, noFsync), "a source whose first read fails")
	})
	// verdictThenReadFails is a decoder that refuses the source, its reads
	// failing from the decode on: the read before the probe passed.
	verdictThenReadFails := func(state *atomic.Int32) curveSource {
		verdict := markUnreadable(errors.New("sox FAIL formats: can't read input file"))
		src := failingCurveSource(verdict)
		src.decode = func(context.Context, string, curveProbe, func([]float64)) (int64, error) {
			state.Store(sourceReadFails)
			return 0, verdict
		}
		return src
	}
	t.Run("the read before a verdict counts", func(t *testing.T) {
		f := newRepairFixture(t)
		var state atomic.Int32
		f.spec.open = openerFailingAs(&state)
		_, err := runWaveformRepair(context.Background(), f.spec, verdictThenReadFails(&state))
		if !errors.Is(err, ErrSourceUnreachable) || CountsAgainstTheCurve(err) {
			t.Errorf("repair error = %v (counts: %t), want ErrSourceUnreachable counting nothing", err, CountsAgainstTheCurve(err))
		}
	})
	t.Run("the read before a verdict counts, through the pool", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		var state atomic.Int32
		f.spec.open = openerFailingAs(&state)
		p := NewPool(f.store, 1, 4, WithFsync(noFsync),
			WithRepairRunner(func(ctx context.Context, spec RepairSpec) (RepairResult, error) {
				return runWaveformRepair(ctx, spec, verdictThenReadFails(&state))
			}))
		t.Cleanup(p.Stop)
		requireRefusedBeforeItsCommit(t, f, p, "a verdict over a source whose read fails after it")
	})
}
