package alsapcm_test

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

type stopsLog struct {
	mu   sync.Mutex
	hold *gate
	why  []string
}

func (l *stopsLog) arm(g *gate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hold = g
}

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

func TestARevocationWhileASuccessorsSinkActivatesKeepsItsFault(t *testing.T) {
	w := newLifecycleSeams()
	log := &stopsLog{}
	d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), func(p *playback.DSDPolicy) { p.Log = log },
		func(o *rigOptions) { o.seams, o.noReplan = w.seams(nil), true })
	_, ob, letAdmit := heldAdmissionOn(t, d, w)
	stops := newGate(t)
	log.arm(stops)
	res := d.refreshAfterHotplug(false)
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	letAdmit()
	d.closedOrGiven(ob, "b's activation answered")
	stops.letGo()
	refreshed(t, res)
	s := d.tickUntilStopped(engine.ReasonFault)
	d.rig.stop()
	wantRevoked(t, d, s, log)
	wantUnwritten(t, d, 2, "b's")
}

// A failed reading must interrupt a retiring native tail immediately, while
// the ordinary release caller is still held before session cancellation.
func TestAnUnreadPolicyStopsARetiringNativeSink(t *testing.T) {
	var broken atomic.Bool
	w := newLifecycleSeams()
	d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), func(p *playback.DSDPolicy) {
		bound := p.Bound
		p.Bound = func() (playback.BoundDAC, bool) {
			if broken.Load() {
				panic("binding read")
			}
			return bound()
		}
	}, func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", dsd(playbacktest.DSD64, 1))
	d.insert("a")
	d.play()
	rel := w.holdReleasing(t)
	done := make(chan error, 1)
	go func() {
		done <- d.be.ReleaseNative(bg, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: d.sessionOf()}, Mode: engine.ReleaseTrackChange})
	}()
	awaitOrdered(t, rel.in, "retiring release before session cancellation")
	broken.Store(true)
	d.be.RefreshDSD(true)
	awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return !v.Open && v.Closes == v.Opens }), "the first failed reading to stop the retiring node")
	rel.letGo()
	select {
	case <-done:
	case <-time.After(alsapcm.Ordered()):
		t.Fatal("retiring release did not finish")
	}
	d.rig.stop()
}
