package alsapcm_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

func TestFailedResumeClosesTheOutputItCouldNotHandOff(t *testing.T) {
	tm := &timers{}
	drained, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unhold := func() { once.Do(func() { close(gate) }) }
	d := newDirectWith(t, nil, func(cfg *playback.Config) {
		cfg.Seams = &playback.Seams{TransportDrained: func() { close(drained); <-gate }}
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool {
			tm.afterFunc(d, f)
			return func() bool { panic("pause timer stop") }
		}
	})
	t.Cleanup(func() { d.release(1) })
	t.Cleanup(unhold)
	d.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	d.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	next := occurrence(102, 2, "b")
	if err := d.be.Prepare(bg, 1, 101, &next); err != nil {
		t.Fatal(err)
	}
	d.k.Free()
	receiveIn(t, drained, "a drained before transitioning to b")
	if err := d.be.Pause(bg, 1); err != nil {
		t.Fatal(err)
	}
	unhold()
	tm.fire(t)
	waitOn(t, "the pause between transports", d.rec.ch, func() bool {
		return count(d.rec.events(), "device released", 0) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), alsapcm.Watchdog())
	defer cancel()
	how, err := d.be.Resume(ctx, 1)
	if how != 0 || err == nil {
		t.Fatalf("resume = %v, %v; want the timer-stop fault", how, err)
	}
	// No engine is running this fixture, so a fault cannot trigger a release
	// that conceals the output leaked by Resume before it answers.
	if v := d.k.View(); v.Open || v.Opens != 2 || v.Closes != 2 {
		t.Errorf("failed resume retained its unhanded output: %+v; want two opens, both closed before session release", v)
	}
}
