package alsapcm_test

import (
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// handedOff waits until the engine has taken o's Submitted, which it shows
// by naming what follows o, and returns the occurrence it named.
func (r *rig) handedOff(o engine.OccurrenceID) call {
	r.t.Helper()
	return r.waitCall("the engine to name what follows occurrence", func(c call) bool { return c.kind == "prepare" && c.after == o })
}

// submittedOcc is the occurrence of entry the backend reported submitted.
func (r *rig) submittedOcc(entry uint32) engine.OccurrenceID {
	r.t.Helper()
	var o engine.OccurrenceID
	r.waitCall("the engine to hand off the entry", func(c call) bool {
		if c.kind == "prepare" && c.next != nil && c.next.Entry == entry {
			o = c.next.ID
			return true
		}
		return false
	})
	r.waitEvents("the entry's first frames in the sink", func(evs []event) bool { return count(evs, "submitted", o) == 1 })
	return o
}

// TestPlaybackCutsARemovedEntryInTime pins an edit after hand-off that comes
// in time: b, deleted while its frames are in the sink's store and none of
// them has reached the kernel, is cut there; the engine names c to follow a
// instead, which the backend joins gaplessly where b began; and the DAC
// hears a, then c, and not one frame of b, in one open of the device.
func TestPlaybackCutsARemovedEntryInTime(t *testing.T) {
	r := newRig(t)
	a, b, c := pattern(1, 30000, 16), pattern(2, 5000, 16), pattern(3, 200000, 16)
	r.files.add("a", wav(44100, 16, a))
	r.files.add("b", wav(44100, 16, b))
	r.files.add("c", wav(44100, 16, c))
	ids := r.insert("a", "b", "c")
	r.play()
	ob := r.submittedOcc(ids[1])
	oc := r.submittedOcc(ids[2])
	r.handedOff(oc)
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	if err := r.eng.Delete(bg, ids[1]); err != nil {
		t.Fatal(err)
	}
	cut := r.waitCall("the engine to cut b", func(c call) bool { return c.kind == "cut" && c.from == ob })
	if !cut.ok || cut.err != nil {
		t.Fatalf("Cut of b, its frames in the store alone: %v, %v; want true", cut.ok, cut.err)
	}
	r.waitCall("the engine to name c after a", func(c call) bool {
		return c.kind == "prepare" && c.next != nil && c.next.Entry == ids[2] && c.next.ID != oc
	})
	r.k.Free()
	r.waitStopped(engine.ReasonEndOfQueue)
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	if want := append(slices.Clone(a), c...); len(got) != len(want) || sumOf(got) != sumOf(want) {
		t.Fatalf("the DAC heard %d samples; want a then c, %d, with nothing of b", len(got), len(want))
	}
	if v := r.k.View(); v.Opens != 1 {
		t.Errorf("%d opens: the cut needs no reopen", v.Opens)
	}
}

// TestPlaybackStopsAtTheBoundaryOfAnEntryCutTooLate pins an edit after
// hand-off that comes too late: b's first frames are in the kernel's buffer,
// so the cut answers false and drops nothing; when b becomes audible the
// engine stops playback, and nothing of c is heard.
func TestPlaybackStopsAtTheBoundaryOfAnEntryCutTooLate(t *testing.T) {
	r := newRig(t)
	a, b := pattern(1, 5000, 16), pattern(2, 30000, 16)
	r.files.add("a", wav(44100, 16, a))
	r.files.add("b", wav(44100, 16, b))
	r.files.add("c", wav(44100, 16, pattern(3, 30000, 16)))
	ids := r.insert("a", "b", "c")
	r.play()
	ob := r.submittedOcc(ids[1])
	r.handedOff(ob)
	// The hardware buffer, 8820 frames, holds all of a and b's first 3820.
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	if err := r.eng.Delete(bg, ids[1]); err != nil {
		t.Fatal(err)
	}
	cut := r.waitCall("the engine to cut b", func(c call) bool { return c.kind == "cut" && c.from == ob })
	if cut.ok {
		t.Fatalf("Cut of b, its first frames in the kernel's buffer, answered true")
	}
	// 150 ms is 6615 frames: past a's 5000, within what the buffer holds.
	r.k.Play(t, 150*time.Millisecond)
	r.tick()
	s := r.waitStopped(engine.ReasonRemovedAudible)
	if s.Selected() == ids[1] {
		t.Errorf("the selection moved to the deleted entry")
	}
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	if len(got) < len(a) || sumOf(got[:len(a)]) != sumOf(a) {
		t.Fatalf("the DAC did not hear a first")
	}
	// What followed a is b's start, which the cut could not take back, and
	// nothing of c.
	if rest := got[len(a):]; len(rest) > len(b) || sumOf(rest) != sumOf(b[:len(rest)]) {
		t.Errorf("after a the DAC heard %d samples that are not b's start", len(rest))
	}
	if v := r.k.View(); v.Open {
		t.Errorf("the device is still open after the stop")
	}
}
