package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B760's fourth residual: the confirmation begins once ServeTLS runs,
// so a start that had not begun serving was never said not to be ready, and
// `bridge doctor` read "ok, on probation" throughout. A start is watched from
// its admission now, reporting only.

// heldBeforeServing is a hold of serve's start just before it begins serving
// (probationSeams.beforeServing).
type heldBeforeServing struct{ held, release *onceCloser }

// holdBeforeServing holds the start seams belong to just before it begins
// serving, until the test lets it go.
func holdBeforeServing(seams *probationSeams) *heldBeforeServing {
	h := &heldBeforeServing{held: newOnceCloser(), release: newOnceCloser()}
	seams.beforeServing = func() {
		h.held.close()
		<-h.release.ch
	}
	return h
}

// waitHeld waits for serve to reach the hold.
func (h *heldBeforeServing) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-h.held.ch:
	case <-time.After(withinTheTest):
		t.Fatal("serve never reached its hold before it begins serving")
	}
}

// waitForWithin returns the first event of kind, waiting for it for at most
// within, and reports whether it came.
func (w *probationWatch) waitForWithin(kind probationEventKind, within time.Duration) (probationEvent, bool) {
	deadline := time.After(within)
	for {
		for _, e := range w.events() {
			if e.kind == kind {
				return e, true
			}
		}
		select {
		case <-w.signal:
		case <-deadline:
			return probationEvent{}, false
		}
	}
}

// TestAStartThatHasNotBegunServingIsSaidNotReady: a start held just before it
// begins serving, past its deadline from its admission, is said not to have
// begun serving, once, and recorded not ready (NotReady), its count its own,
// and `bridge doctor` reads a fail beside it, not "ok, on probation"; after
// its clean stop NotReady is kept, so the doctor still fails (what the stop
// does to its count is backlog B760's); and a later start that serves clears NotReady at its admission,
// the doctor reading ok beside it. Until B760 nothing was said of such a start.
func TestAStartThatHasNotBegunServingIsSaidNotReady(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(probationTiming{window: time.Hour, ask: 20 * time.Millisecond, retry: 20 * time.Millisecond,
		round: 5 * time.Second, serve: 200 * time.Millisecond})
	hold := holdBeforeServing(seams)
	sb := r.bootWith(t, serveOpts{probation: seams})
	t.Cleanup(hold.release.close)
	hold.waitHeld(t)
	e, ok := events.waitForWithin(probationNotReady, withinTheTest)
	if !ok {
		t.Fatalf("a start held before it serves, past its deadline, was never said not ready; it did %+v; stderr=%s",
			events.events(), sb.stderr.String())
	}
	if !strings.Contains(e.why, "not begun serving") {
		t.Errorf("the not-ready event = %+v, want it to say the start has not begun serving", e)
	}
	if st := markerOf(t, r.dataDir); !st.NotReady || st.FailedStarts != 1 {
		t.Errorf("the marker while the start is held = %+v, want NotReady recorded and this start's count", st)
	}
	if n := strings.Count(sb.stderr.String(), "has not begun serving"); n != 1 {
		t.Errorf("serve said it has not begun serving %d times, want once; stderr=%s", n, sb.stderr.String())
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "fail" || !strings.Contains(line.Summary, "did not answer as ready") {
		t.Errorf("bridge doctor beside a start that has not begun serving = %+v, want a fail saying a start did not answer", line)
	}
	sb.stop()
	hold.release.close()
	waitForExit(t, sb, 0)
	if st := markerOf(t, r.dataDir); !st.NotReady || st.FailedStarts != 0 {
		t.Errorf("the marker after the clean stop = %+v, want its count taken back and NotReady kept", st)
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "fail" || !strings.Contains(line.Summary, "did not answer as ready") {
		t.Errorf("bridge doctor after the stop = %+v, want the fail kept", line)
	}

	later, laterSeams := newProbationWatch(longProbation)
	sb = r.bootWith(t, serveOpts{probation: laterSeams})
	later.waitFor(t, probationWindow)
	if st := markerOf(t, r.dataDir); st.NotReady || st.FailedStarts != 1 {
		t.Errorf("the marker beside a later start that serves = %+v, want NotReady cleared at its admission, its count its own", st)
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "ok" || !strings.Contains(line.Summary, "on probation") {
		t.Errorf("bridge doctor beside the later start = %+v, want ok, on probation", line)
	}
	stopServed(t, sb)
}

// TestAStartStoppedBeforeItsDeadlineIsNotSaidNotReady: a start held before it
// begins serving and stopped before its deadline is a short cancelled start:
// nothing says it is not ready, its count goes back, and the doctor reads ok.
func TestAStartStoppedBeforeItsDeadlineIsNotSaidNotReady(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(longProbation)
	hold := holdBeforeServing(seams)
	sb := r.bootWith(t, serveOpts{probation: seams})
	t.Cleanup(hold.release.close)
	hold.waitHeld(t)
	sb.stop()
	hold.release.close()
	waitForExit(t, sb, 0)
	if st := markerOf(t, r.dataDir); st.NotReady || st.FailedStarts != 0 {
		t.Errorf("the marker after a stop before the deadline = %+v, want nothing recorded and the count taken back", st)
	}
	if n := events.count(probationNotReady); n != 0 || strings.Contains(sb.stderr.String(), "has not begun serving") {
		t.Errorf("a start stopped before its deadline was said not ready (%d events); stderr=%s", n, sb.stderr.String())
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "ok" {
		t.Errorf("bridge doctor after the stop = %+v, want ok", line)
	}
}

// TestTheStartWatchFiresOnlyWhileTheStartHasNotBegunServing: the watch says
// once that the start has not begun serving, and records NotReady, at its
// deadline where nothing ended it before; and records nothing, its deadline
// long past, where the start began serving first, or serve's stop came first.
func TestTheStartWatchFiresOnlyWhileTheStartHasNotBegunServing(t *testing.T) {
	const deadline = 50 * time.Millisecond
	for _, tc := range []struct {
		name  string
		end   func(w *startWatch, stop context.CancelFunc)
		fires bool
	}{
		{"nothing ends it", func(*startWatch, context.CancelFunc) {}, true},
		{"the start begins serving", func(w *startWatch, _ context.CancelFunc) { w.served() }, false},
		{"serve's stop", func(_ *startWatch, stop context.CancelFunc) { stop() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.DataDirOpenedBy, r.marker.FailedStarts = version.ServerVersion, 1
			r.saveMarker(t, r.dataDir)
			events, seams := newProbationWatch(probationTiming{serve: deadline})
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			stderr := &safeBuffer{}
			w := watchTheStart(ctx, r.dataDir, version.ServerVersion, seams, stderr)
			tc.end(w, stop)
			within := 8 * deadline
			if tc.fires {
				within = withinTheTest
			}
			_, fired := events.waitForWithin(probationNotReady, within)
			w.end()
			if fired != tc.fires {
				t.Fatalf("the watch said the start had not begun serving: %v, want %v; stderr=%s", fired, tc.fires, stderr.String())
			}
			if st := markerOf(t, r.dataDir); st.NotReady != tc.fires || st.FailedStarts != 1 {
				t.Errorf("the marker = %+v, want NotReady %v and the count as it was", st, tc.fires)
			}
			if n := strings.Count(stderr.String(), "has not begun serving"); n != boolInt(tc.fires) {
				t.Errorf("the watch said it %d times, want %d; stderr=%s", n, boolInt(tc.fires), stderr.String())
			}
		})
	}
}

// TestAStartThatBeginsServingEndsItsWatch: a start that begins serving ends
// the watch on it there, before its confirmation's first round, so a start
// that serves is never said not to have begun.
func TestAStartThatBeginsServingEndsItsWatch(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationWindow)
	if seen := events.events(); seen[0].kind != probationServing || events.count(probationServing) != 1 {
		t.Errorf("the events of a start that serves = %+v, want the watch's end there first, once", seen)
	}
	stopServed(t, sb)
}
