package alsapcm_test

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// faulted waits for playback to stop on a fault, and returns the fault.
func (r *rig) faulted() (engine.Fault, *engine.Snapshot) {
	r.t.Helper()
	s := r.waitStopped(engine.ReasonFault)
	faults := s.Faults()
	if len(faults) != 1 {
		r.t.Fatalf("faults %+v, want one", faults)
	}
	return faults[0], s
}

// heardAt has the reporter measure, and returns the position it reported
// once the engine shows it: the engine applies the backend's events on its
// own goroutine, after the reporter has sent them.
func (r *rig) heardAt() engine.Position {
	r.t.Helper()
	r.tick()
	var last engine.Position
	for _, e := range r.rec.events() {
		if e.kind == "position" {
			last = e.pos
		}
	}
	if last.Occurrence == 0 {
		r.t.Fatalf("no position reported: %s", kinds(r.rec.events()))
	}
	r.waitSnap("the engine to show the reported position", func(s *engine.Snapshot) bool { return s.Position() == last })
	return last
}

// neverRecovered fails the test if the sink was ever prepared again: every
// PREPARE is an open's, and Recover would add one.
func (r *rig) neverRecovered() {
	r.t.Helper()
	if v := r.k.View(); v.Prepares != v.Opens {
		r.t.Errorf("%d PREPAREs in %d opens: the backend recovered the stream", v.Prepares, v.Opens)
	}
}

// TestPlaybackFaultsOnAnXrunAndNeverRecovers pins an underrun: the DAC ran
// dry, the sink stopped, and the backend reports a fault (xrun) with the
// occurrence heard and its frames played, stops, and never recovers: the
// engine stops playback and records it, nothing played after.
func TestPlaybackFaultsOnAnXrunAndNeverRecovers(t *testing.T) {
	r := newRig(t)
	r.k.Free()
	r.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	ids := r.insert("a")
	r.k.XrunAt(30000)
	r.play()
	f, _ := r.faulted()
	// The kernel stops a stream once its pointer reaches the last frame
	// written, so the frames played are every one written before the stop.
	if v := r.k.View(); f.Reason != engine.ReasonXrun || f.Entry != ids[0] || f.Frames < 30000 || f.Frames > int64(v.Total) {
		t.Errorf("fault %+v; want an xrun in entry %d with the %d frames written before it played", f, ids[0], v.Total)
	}
	r.neverRecovered()
}

// TestPlaybackFaultsOnADisconnect pins the device going away while it
// plays (ENODEV): a fault (disconnected), the position where it went, and
// no recovery.
func TestPlaybackFaultsOnADisconnect(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(48000, 24, pattern(1, 60000, 24)))
	r.insert("a")
	r.play()
	r.files.fed(t, "a")
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.k.Play(t, 300*time.Millisecond)
	before := r.heardAt()
	r.k.Unplug()
	f, s := r.faulted()
	if f.Reason != engine.ReasonDisconnected || f.Frames < before.Frames {
		t.Errorf("fault %+v; want disconnected, at or past the %d frames heard before", f, before.Frames)
	}
	if s.Position().Frames != f.Frames {
		t.Errorf("the position %+v is not where the fault froze it, %d", s.Position(), f.Frames)
	}
	r.neverRecovered()
}

// TestPlaybackFaultsOnAnUnplugWhilePaused pins the device going away during
// a pause that holds it: a fault (disconnected), the position frozen where
// the pause left it.
func TestPlaybackFaultsOnAnUnplugWhilePaused(t *testing.T) {
	r, _ := pausedRig(t)
	paused := r.heardAt()
	r.k.Unplug()
	f, s := r.faulted()
	if f.Reason != engine.ReasonDisconnected {
		t.Errorf("fault %+v; want disconnected", f)
	}
	if s.Position().Frames != paused.Frames {
		t.Errorf("the position moved from %d to %d across the unplug of a paused device", paused.Frames, s.Position().Frames)
	}
	r.neverRecovered()
}

// TestPlaybackFaultsOnADeviceError pins any other failure of the device (a
// STATUS the kernel refuses with an error no state explains): a fault
// (device_error), and no recovery.
func TestPlaybackFaultsOnADeviceError(t *testing.T) {
	r := newRig(t)
	r.k.Free()
	r.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	r.insert("a")
	r.k.FailStatusAt(30000)
	r.play()
	f, _ := r.faulted()
	if f.Reason != engine.ReasonDeviceError || f.Frames <= 0 {
		t.Errorf("fault %+v; want a device error after content played", f)
	}
	r.neverRecovered()
}

// TestPlaybackShutsDownWithinItsBoundWithTheDeviceHeldInAWrite pins the
// shutdown with the device stuck in a kernel call that does not return
// (the writer's write parked inside the fake kernel): the engine's Run
// returns within its bound, the release unconfirmed; the backend holds only
// the goroutines that wait on that sink, abandoned: the teardown in its
// Close, and, held during the session's last drain, the call that handed
// the sink the drain; every decoder and content is closed meanwhile; and
// once the kernel lets go the device closes and every goroutine the backend
// started is joined.
func TestPlaybackShutsDownWithinItsBoundWithTheDeviceHeldInAWrite(t *testing.T) {
	for _, c := range []struct {
		name   string
		frames int
		// held is the goroutines left waiting on the sink: the teardown alone
		// mid-stream; at the last drain also the call that handed the sink the
		// drain, unless the shutdown came first (0: one or two).
		held int
	}{
		{"mid-stream, the producer waiting for room", 200000, 1},
		{"during the last drain", 1000, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			parked, release := r.k.ParkNextWrite()
			t.Cleanup(release)
			r.files.add("a", wav(48000, 24, pattern(1, c.frames, 24)))
			r.insert("a")
			r.play()
			receiveIn(t, parked, "the writer's first write to stick in the kernel")
			if c.held == 0 {
				r.waitCall("the engine to name the end", func(x call) bool { return x.kind == "prepare" && x.next == nil })
			}
			began := time.Now()
			r.cancel()
			receiveIn(t, r.ran, "the engine's Run to return")
			if took, bound := time.Since(began), engine.ReleaseTimeout+2*time.Second; took > bound {
				t.Errorf("Run took %v to shut down, past its bound of %v", took, bound)
			}
			if st := r.be.Stats(); (c.held != 0 && st.Goroutines != c.held) || (c.held == 0 && (st.Goroutines < 1 || st.Goroutines > 2)) {
				t.Errorf("with the kernel holding the write the backend holds %d goroutines, want %d, each waiting on that sink", st.Goroutines, c.held)
			}
			if v := r.k.View(); !v.Open {
				t.Fatalf("the device closed while its write was held in the kernel")
			}
			r.files.allClosed(t)
			release()
			ctx, cancel := context.WithTimeout(bg, alsapcm.Watchdog())
			defer cancel()
			if err := r.be.Wait(ctx); err != nil {
				t.Fatalf("once the kernel let go: %v", err)
			}
			if v := r.k.View(); v.Open {
				t.Errorf("the device is still open once the kernel let go")
			}
		})
	}
}
