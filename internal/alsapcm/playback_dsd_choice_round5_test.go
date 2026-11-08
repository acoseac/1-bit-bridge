package alsapcm_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A reconciliation that revokes a session's DSD while a successor's own sink
// activates (B728 B9 round 5, point 1), through the engine's Run, the backend
// and the real native sinks over the fake kernel: the revocation is
// Backend.Revoke's, whose fault stands.

// stopsLog is a DSD policy's log (playback.DSDPolicy.Log) that holds, at the
// gate armed, the goroutine that says a session's DSD stops: a
// reconciliation, between its judgement of the session's occurrences and its
// call to Backend.Revoke. It says nothing else.
type stopsLog struct {
	mu   sync.Mutex
	hold *gate
}

// arm holds the next line saying a session's DSD stops at g.
func (l *stopsLog) arm(g *gate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hold = g
}

func (l *stopsLog) Info(string, ...any) {}

func (l *stopsLog) Warn(msg string, _ ...any) {
	if !strings.Contains(msg, "its DSD stops") {
		return
	}
	l.mu.Lock()
	g := l.hold
	l.hold = nil
	l.mu.Unlock()
	if g != nil {
		g.pass()
	}
}

// closedOrGiven waits until o's sink is noted closed (true) or b's node, the
// second opened, has been given a byte (false), whichever comes first,
// reading the rig's notes, which nothing else in the test reads.
func (d *dsdRig) closedOrGiven(o engine.OccurrenceID, what string) bool {
	d.t.Helper()
	given := d.k.When(func(v alsapcm.KernelView) bool { return len(v.OpenAt) >= 2 && v.GivenBytes > v.OpenAt[1] })
	deadline := time.After(alsapcm.Watchdog())
	for {
		select {
		case n := <-d.notes:
			if n.Kind == playback.NoteClosed && n.Occurrence == o {
				return true
			}
		case <-given:
			return false
		case <-deadline:
			alsapcm.LogGoroutines(d.t)
			d.t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
		}
	}
}

// TestARevocationWhileASuccessorsSinkActivatesKeepsItsFault (B728 B9 round
// 5, point 1): where the engine hands nothing off again (no
// engine.ReplanEvents), a reconciliation that takes a successor's rate away
// revokes the session's DSD, and the revocation is Backend.Revoke's alone,
// which records its fault while the successor's sink is the session's and
// only then revokes the sinks. A reconciliation that revoked the successor's
// sink first let its activation answer the sink's own fence, and the
// successor be taken back and its sink closed, before Backend.Revoke ran,
// which then found no sink of the session's to stop and recorded no fault: b
// was skipped and the session went on. Here b's sink admits its activation,
// the reconciliation is held between its judgement and its call to Revoke
// (its line saying the session's DSD stops), and the admission answers
// meanwhile: the session stops native_revoked, and nothing more opens.
func TestARevocationWhileASuccessorsSinkActivatesKeepsItsFault(t *testing.T) {
	w := newWriteSeams()
	log := &stopsLog{}
	d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(p *playback.DSDPolicy) { p.Log = log },
		func(o *rigOptions) { o.seams, o.noReplan = w.seams(nil), true })
	_, ob, letAdmit := heldAdmissionOn(t, d, w)
	stops := newGate(t)
	log.arm(stops)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	// b's activation answers while the reconciliation is held: its sink, if
	// nothing revoked it, activates and gives its node the prefill; if the
	// reconciliation revoked it, the activation answers the sink's own fence,
	// and b is taken back, its sink closed and noted so (the note comes once
	// the take-back has closed it, the node's own close by the sink's writer
	// coming first).
	letAdmit()
	if d.closedOrGiven(ob, "b's activation answered") {
		t.Logf("b was taken back from its sink before the session's revocation ran")
	}
	stops.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
	s := d.tickUntilStopped(engine.ReasonFault)
	d.rig.stop()
	if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
		t.Errorf("faults %+v; want the revocation's alone", f)
	}
	if v := d.k.View(); v.Opens != 2 {
		t.Errorf("%d opens: want a's and b's alone, nothing opened once the session's DSD was revoked", v.Opens)
	}
}
