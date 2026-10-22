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

// TestPlaybackShutsDownWithinItsBoundWithTheDeviceHeldInTheKernel pins the
// shutdown with the device stuck in a kernel call that does not return (a
// write or a drain parked inside the fake kernel): the engine's Run returns
// within its bound, the release unconfirmed; the backend holds only the
// goroutines that wait on that sink, abandoned: the teardown in its Close,
// and, held in a drain, the call that handed the sink the drain, never the
// producer; every decoder and content is closed meanwhile, the next entry's
// that a drain for its transport waits on among them; and once the kernel
// lets go the device closes and every goroutine the backend started is
// joined.
func TestPlaybackShutsDownWithinItsBoundWithTheDeviceHeldInTheKernel(t *testing.T) {
	for _, c := range []struct {
		name   string
		frames int
		// next is a second entry, of another transport.
		next bool
		// drain parks the kernel's DRAIN, the write otherwise.
		drain bool
		// held is the goroutines left waiting on the sink: the teardown alone
		// mid-stream; held in a drain also the call that handed the sink the
		// drain; at the last drain with the write held, that call unless the
		// shutdown came before it (0: one or two).
		held int
	}{
		{"mid-stream, the producer waiting for room", 200000, false, false, 1},
		{"during the last drain", 1000, false, false, 0},
		{"in the drain for the next entry's transport, its content open", 1000, true, true, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			park, what := r.k.ParkNextWrite, "the writer's first write to stick in the kernel"
			if c.drain {
				park, what = r.k.ParkNextDrain, "the drain to stick in the kernel"
			}
			parked, release := park()
			t.Cleanup(release)
			r.files.add("a", wav(48000, 24, pattern(1, c.frames, 24)))
			refs := []string{"a"}
			if c.next {
				r.files.add("b", wav(44100, 16, pattern(2, 3000, 16)))
				refs = append(refs, "b")
			}
			r.insert(refs...)
			r.play()
			receiveIn(t, parked, what)
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

// TestPlaybackReleaseWaitsForTheProducerToLeaveItsDecoder pins the release's
// join of the producer: held in a read of the content that does not return
// (a file on a mount that hangs, which no context ends), the producer still
// holds its decoder and the content, so the release closes neither under it
// and is not acknowledged: past its deadline the engine holds it
// unconfirmed. Once the read returns, the producer leaves, the content is
// closed, and the release is acknowledged, late.
func TestPlaybackReleaseWaitsForTheProducerToLeaveItsDecoder(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(48000, 24, pattern(1, 200000, 24)))
	// The open decodes the first 4096 frames, bytes 44 to 24620 of the file;
	// the producer's first read of its own is held.
	parked, unpark := r.files.stall("a", 30000)
	t.Cleanup(unpark)
	r.insert("a")
	r.play()
	receiveIn(t, parked, "the producer's read of the content to hang")
	if err := r.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
	s := r.waitSnap("the release past its deadline, or the content closed", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return unconfirmed || r.files.isClosed("a")
	})
	if r.files.closedUnderARead("a") {
		t.Fatal("the release closed the content while the producer was in a read of it")
	}
	if _, unconfirmed := s.ReleaseUnconfirmed(); !unconfirmed {
		t.Fatal("the release was acknowledged while the producer was in a read of the content")
	}
	if n := count(r.rec.events(), "released", 0); n != 0 {
		t.Fatalf("Released reported while the producer held the content")
	}
	unpark()
	r.waitSnap("the late acknowledgement", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return !unconfirmed
	})
	if !r.files.isClosed("a") || r.files.closedUnderARead("a") {
		t.Errorf("the content was not closed once the producer left its read")
	}
	// The acknowledgement is late either way: Events.Released, or Release's
	// own answer when the teardown ends as Release gives up its wait.
	if n := count(r.rec.events(), "released", 0); n > 1 {
		t.Errorf("Released reported %d times, want once at most: %s", n, kinds(r.rec.events()))
	}
}
