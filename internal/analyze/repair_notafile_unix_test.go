//go:build unix

package analyze

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestASourceThatIsNoRegularFileIsNeverOpened: a source path that names no
// regular file now is not the version the row describes, whatever its size
// and mtime say, and is told so from its stat, before anything opens it or a
// decoder starts: a named pipe at the path, given the row's size and mtime so
// that only its kind tells it apart, is ErrSourceChanged, the stat's refusal,
// counting nothing, where an open's would be unreachable (backlog B652, round
// 2).
func TestASourceThatIsNoRegularFileIsNeverOpened(t *testing.T) {
	f := newRepairFixture(t)
	if err := os.Remove(f.spec.SourceAbsPath); err != nil {
		t.Fatal(err)
	}
	fsutiltest.MakeFIFO(t, f.spec.SourceAbsPath)
	info, err := os.Stat(f.spec.SourceAbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.spec.SourceMTimeNS, f.spec.SourceSize = info.ModTime().UnixNano(), info.Size()
	started := false
	src := fakeCurveSource(48000, nil)
	probe := src.probe
	src.probe = func(ctx context.Context, p string) curveProbe {
		started = true
		return probe(ctx, p)
	}
	var repairErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, repairErr = runWaveformRepair(context.Background(), f.spec, src)
	}()
	if !fsutiltest.AwaitPastFIFOs(t, "the repair", fsutiltest.ServeBound, done, f.spec.SourceAbsPath) {
		return
	}
	if !errors.Is(repairErr, ErrSourceChanged) || CountsAgainstTheCurve(repairErr) {
		t.Errorf("repair error = %v (counts: %t), want ErrSourceChanged counting nothing: a source that is no "+
			"regular file is refused from its stat", repairErr, CountsAgainstTheCurve(repairErr))
	}
	if started {
		t.Error("a decoder was started over a source that is no regular file")
	}
	if left := entriesUnder(t, f.dir); len(left) != 0 {
		t.Errorf("the repair wrote %v for a source that is no regular file", left)
	}
}
