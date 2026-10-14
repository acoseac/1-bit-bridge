package transcode

import (
	"errors"
	"fmt"
	"testing"
)

// TestAnAlbumMateWaitStrikesNothing — a DSD render whose album gain could
// not be decided, because an album-mate's peak could not be measured for a
// reason that is not a fact about its file (ErrAlbumMateUnmeasured, backlog
// B215), fails and is announced like any other failure, but strikes nothing
// against its OWN file however often it happens: three strikes would
// suppress a good file for 30 days over its album-mate's full scratch
// volume. The positive control: the same harness strikes a plain failure.
func TestAnAlbumMateWaitStrikesNothing(t *testing.T) {
	const rel = "Music/Album/01.dsf"
	a := newAnnouncingPool(t, rel)
	s := &scripted{}
	a.pool.runner = s.run
	src := dsdSource(t, rel)
	mateWait := func() (RunResult, error) {
		return RunResult{}, fmt.Errorf("dsd render: album gain for %s: %w: album-mate Music/Album/02.dsf: %w",
			rel, ErrAlbumMateUnmeasured, errors.New("write render scratch: no space left on device"))
	}
	s.then(mateWait, mateWait, mateWait, mateWait)
	a.run(t, src, 4)

	if suppressed, records := a.strikes(t, rel); suppressed != 0 || records != 0 {
		t.Errorf("%d suppressed, %d strike record(s) after four album-mate waits, want none", suppressed, records)
	}
	if waits := a.lines("INFO", "pool: the render waits for an album-mate's peak"); len(waits) != 4 {
		t.Errorf("%d wait lines, want one per failed render", len(waits))
	}
	if failed := a.pool.Stats().Failed; failed != 4 {
		t.Errorf("Stats().Failed = %d, want the four waits counted as failures", failed)
	}

	s.then(func() (RunResult, error) { return RunResult{}, errors.New("sox: exit status 2") })
	a.run(t, src, 1)
	if _, records := a.strikes(t, rel); records != 1 {
		t.Errorf("a plain failure left %d strike record(s), want 1: the harness's strike count is not read", records)
	}
}
