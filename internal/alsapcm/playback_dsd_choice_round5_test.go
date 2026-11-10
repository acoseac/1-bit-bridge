package alsapcm_test

import (
	"slices"
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
// call to Backend.Revoke. It keeps the reason each such line gives, the one
// Revoke is called with (B728 B9 round 6), and says nothing else.
type stopsLog struct {
	mu   sync.Mutex
	hold *gate
	why  []string
}

// arm holds the next line saying a session's DSD stops at g.
func (l *stopsLog) arm(g *gate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hold = g
}

// reasons are the reasons the lines saying a session's DSD stops gave, in
// order.
func (l *stopsLog) reasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.why)
}

func (l *stopsLog) Info(string, ...any) {}

func (l *stopsLog) Warn(msg string, args ...any) {
	if !strings.Contains(msg, "its DSD stops") {
		return
	}
	l.mu.Lock()
	for i := 0; i+1 < len(args); i += 2 {
		if why, ok := args[i+1].(string); ok && args[i] == "why" {
			l.why = append(l.why, why)
		}
	}
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
// 5, point 1; round 6): where the engine hands nothing off again (no
// engine.ReplanEvents), a reconciliation that takes a successor's rate away
// revokes the session's DSD. Its fault is recorded in the reconciliation's own
// step, before the successor is marked stale, and the sink its activation is
// under way for is fenced there (round 6): until round 5 the reconciliation
// fenced it and left the fault to Backend.Revoke, so b was taken back and its
// sink closed before Revoke ran, which found no sink to stop and recorded no
// fault; round 5 left the sink to Revoke, so b's activation, answered in the
// window, gave b's node its prefill at the rate the write had taken away.
// Here b's sink admits its activation, the reconciliation is held between its
// judgement and its call to Revoke (its line saying the session's DSD stops),
// and the admission answers meanwhile: the session stops native_revoked, once,
// for the reason Revoke is given, nothing more opens, and once the rig has
// stopped b's node was given nothing and every node is closed.
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
	// b's activation answers while the reconciliation is held: the sink's own
	// fence, so b is taken back, its sink closed and noted so, or the engine,
	// told of the fault, releases the session first and its teardown closes the
	// sink. closedOrGiven only orders the steps (a sink nothing fenced activates
	// and gives its node its prefill, and the wait ends there): what the node
	// was given is read once the rig has stopped.
	letAdmit()
	d.closedOrGiven(ob, "b's activation answered")
	stops.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
	s := d.tickUntilStopped(engine.ReasonFault)
	d.rig.stop()
	wantRevoked(t, d, s, log)
	wantUnwritten(t, d, 2, "b's")
}
