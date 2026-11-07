//go:build !windows

package transcode

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// awayGuard stands in for cmd/bridge's renditionOutputGuard: it refuses every
// variants directory while away is set, and counts what it was asked.
type awayGuard struct {
	away  atomic.Bool
	asked atomic.Int32
}

func (g *awayGuard) CheckOutput(context.Context, string) error {
	g.asked.Add(1)
	if g.away.Load() {
		return &VariantsDirAwayError{Found: "is not there"}
	}
	return nil
}

// TestThePoolAsksItsOutputGuardBeforeAJobWrites drives the real pool and the
// real runner with the guard the pool injects when a job starts (backlog
// B727): three jobs into a variants directory it refuses fail, write nothing
// there, strike no source and are reported once; the job after it lets the
// directory through renders and reports the output back with what the outage
// cost. A spec that carries a guard of its own keeps it.
func TestThePoolAsksItsOutputGuardBeforeAJobWrites(t *testing.T) {
	const rel = "Music/Album/01.flac"
	a := newToolFreePool(t, standInTools(t, map[string]string{"sox": standInSoxWrites}), rel)
	g := &awayGuard{}
	g.away.Store(true)
	a.pool.SetOutputGuard(g)
	spec := sourceFile(t, rel)

	for i, got := range a.run(t, spec, 3) {
		if !strings.HasPrefix(got, "failed: ") || !strings.Contains(got, "not mounted") {
			t.Errorf("job #%d announced %q, want the guard's refusal", i+1, got)
		}
	}
	if n := g.asked.Load(); n != 3 {
		t.Errorf("the guard was asked %d time(s), want once per job (3)", n)
	}
	if entries, err := os.ReadDir(spec.OutputDir); err != nil || len(entries) != 0 {
		t.Errorf("the refused jobs left %d entr(ies) in the variants directory (%v), want none", len(entries), err)
	}
	if suppressed, records := a.strikes(t, rel); suppressed != 0 || records != 0 {
		t.Errorf("%d suppressed, %d strike record(s) after three refused jobs, want none", suppressed, records)
	}
	warns := a.log.Failures()
	if len(warns) != 1 || !strings.Contains(warns[0], logOutputUnavailable) ||
		!strings.Contains(warns[0], "output="+outputVariants) || !strings.Contains(warns[0], "the variants directory is not there") {
		t.Errorf("three refused jobs logged these warnings:\n%s\nwant one, the variants output's, saying what the guard found",
			strings.Join(warns, "\n"))
	}

	g.away.Store(false)
	if got := a.run(t, spec, 1); got[0] != "done" {
		t.Fatalf("the job after the guard let the directory through announced %q, want done", got[0])
	}
	if _, err := os.Stat(spec.SidecarPath()); err != nil {
		t.Errorf("the rendition is not in place: %v", err)
	}
	back := a.lines("INFO", logOutputBack)
	if len(back) != 1 || !strings.Contains(back[0], "output="+outputVariants) || !strings.Contains(back[0], "failedJobs=3") {
		t.Errorf("recovery lines:\n%s\nwant one, the variants output back after 3 failed jobs", strings.Join(back, "\n"))
	}

	// A guard the spec carries is the one asked.
	own := &awayGuard{}
	spec.OutputGuard = own
	g.asked.Store(0)
	if got := a.run(t, spec, 1); got[0] != "done" {
		t.Fatalf("a job carrying its own guard announced %q, want done", got[0])
	}
	if own.asked.Load() != 1 || g.asked.Load() != 0 {
		t.Errorf("asked: the spec's guard %d, the pool's %d; want the spec's alone", own.asked.Load(), g.asked.Load())
	}
}
