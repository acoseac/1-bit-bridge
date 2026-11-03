package alsapcm_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The seek's paths through the engine, the backend and the sink (B674): back
// and relative, while paused, with a successor in the sink, one after
// another, a Stop during the reopen, and a From the content cannot take.

// heardFor lets the DAC play d of what the session gave it, once the writer
// has filled the hardware buffer of the open in use, and has the reporter
// measure it.
func (r *rig) heardFor(d time.Duration) {
	r.t.Helper()
	r.k.Await(r.t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.k.Play(r.t, d)
	r.tick()
}

// opened waits until the device has been opened n times and the writer of
// the last open waits on the DAC with its hardware buffer full.
func (r *rig) opened(n int) {
	r.t.Helper()
	r.k.Await(r.t, fmt.Sprintf("open %d's hardware buffer full", n), func(v alsapcm.KernelView) bool { return v.Opens == n && runningAndWaiting(v) })
}

// givenTo is the 16-bit samples the n-th open of the device (from 0) was
// given.
func givenTo(t *testing.T, k *alsapcm.FakeKernel, n int) []int32 {
	t.Helper()
	v, given := k.View(), k.Given()
	end := len(given)
	if n+1 < len(v.OpenAt) {
		end = v.OpenAt[n+1]
	}
	return unpack(t, given[v.OpenAt[n]:end], format.S16LE, 16)
}

// aHeard starts a rig on a, the frames of a 16-bit WAV at 44.1 kHz, and has
// the DAC play its first 100 ms.
func aHeard(t *testing.T, a []int32) *rig {
	t.Helper()
	r := newRig(t)
	r.files.add("a", wav(44100, 16, a))
	r.insert("a")
	r.play()
	r.heardFor(100 * time.Millisecond)
	r.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
	return r
}

// TestPlaybackSeeksBackAndRelativelyBitExactly pins a seek forward, then a
// relative one back, through the engine: each session at a target is given
// the track from its frame and nothing else, the relative target counted
// from the position the backend measured (the first target plus what the DAC
// played since), and the DAC hears each run of the track once, in order.
func TestPlaybackSeeksBackAndRelativelyBitExactly(t *testing.T) {
	const rate = 44100
	a := pattern(1, 4*rate, 16)
	r := aHeard(t, a)
	if err := r.eng.SeekAbsolute(bg, 3); err != nil {
		t.Fatalf("a seek to 3 s: %v", err)
	}
	r.opened(2)
	r.k.Play(t, 100*time.Millisecond)
	r.tick()
	played := int64(r.k.View().Consumed)
	r.waitSnap("the position past 3 s", func(s *engine.Snapshot) bool { return s.Position().Frames == 3*rate+played })
	if err := r.eng.SeekRelative(bg, -2); err != nil {
		t.Fatalf("a seek of -2 s: %v", err)
	}
	back := 3*rate + played - 2*rate
	start := r.waitCall("the third start", func(c call) bool { return c.kind == "start" && c.s == 3 })
	if start.at != back {
		t.Fatalf("-2 s at %d frames started from %d, want %d", 3*rate+played, start.at, back)
	}
	r.opened(3)
	r.k.Free()
	r.waitStopped(engine.ReasonEndOfQueue)
	if got := givenTo(t, r.k, 1); len(got) < int(2*played) || !slices.Equal(got[:2*played], a[2*3*rate:2*(3*rate+played)]) {
		t.Errorf("the open at 3 s was not given the track from 3 s")
	}
	if got := givenTo(t, r.k, 2); !slices.Equal(got, a[2*back:]) {
		t.Errorf("the open at %d frames was given %d samples, not exactly the %d of the track from there", back, len(got), len(a[2*back:]))
	}
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	first := len(got) - int(2*played) - len(a[2*back:])
	if first <= 0 || !slices.Equal(got, slices.Concat(a[:first], a[2*3*rate:2*(3*rate+played)], a[2*back:])) {
		t.Errorf("the DAC heard %d samples; want the start, then %d frames from 3 s, then the track from %d frames", len(got), played, back)
	}
}

// TestPlaybackSeeksWhilePausedBitExactly pins a seek while paused through the
// engine: the paused session is released and its device closed, nothing is
// opened until Play, and the open Play makes is given the track from the
// target and nothing else.
func TestPlaybackSeeksWhilePausedBitExactly(t *testing.T) {
	const rate = 44100
	a := pattern(1, 4*rate, 16)
	r := aHeard(t, a)
	if err := r.eng.Pause(bg); err != nil {
		t.Fatal(err)
	}
	if err := r.eng.SeekAbsolute(bg, 2); err != nil {
		t.Fatalf("a seek while paused: %v", err)
	}
	if v := r.k.View(); v.Open || v.Opens != 1 || r.eng.Snapshot().State() != engine.Paused {
		t.Fatalf("after a seek while paused the device is open %v after %d opens, the player %s; want closed, paused", v.Open, v.Opens, r.eng.Snapshot().State())
	}
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("play at the target: %v", err)
	}
	r.k.Free()
	r.waitStopped(engine.ReasonEndOfQueue)
	if got := givenTo(t, r.k, 1); !slices.Equal(got, a[2*2*rate:]) {
		t.Errorf("Play at the target opened the device with %d samples, not exactly the %d of the track from 2 s", len(got), len(a[2*2*rate:]))
	}
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	first := len(got) - len(a[2*2*rate:])
	if first <= 0 || !slices.Equal(got[:first], a[:first]) || !slices.Equal(got[first:], a[2*2*rate:]) {
		t.Errorf("the DAC heard %d samples; want the start, then the track from 2 s", len(got))
	}
}

// TestPlaybackDiscardsBufferedSuccessorsAtASeek pins the successor a seek
// finds in the sink: a, 1.5 s, and b after it, one transport, b's first
// frames in the sink's store behind a's. A seek within a drops the session,
// b's frames with it, and the session at the target is given a from the
// target and then b from its start, gaplessly, b named again after a (the
// gapless hand-off resumes from the new position) and heard once.
func TestPlaybackDiscardsBufferedSuccessorsAtASeek(t *testing.T) {
	const rate = 44100
	r := newRig(t)
	a, b := pattern(1, rate*3/2, 16), pattern(2, rate, 16)
	r.files.add("a", wav(rate, 16, a))
	r.files.add("b", wav(rate, 16, b))
	ids := r.insert("a", "b")
	r.play()
	ob := r.submittedOcc(ids[1])
	r.heardFor(100 * time.Millisecond)
	r.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
	if err := r.eng.SeekAbsolute(bg, 1); err != nil {
		t.Fatalf("a seek to 1 s: %v", err)
	}
	named := r.waitCall("b named after a again", func(c call) bool {
		return c.kind == "prepare" && c.s == 2 && c.next != nil && c.next.Entry == ids[1]
	})
	if named.next.ID == ob {
		t.Errorf("the session at the target named b as the occurrence the released one held")
	}
	r.k.Free()
	r.waitStopped(engine.ReasonEndOfQueue)
	tail := slices.Concat(a[2*rate:], b)
	if got := givenTo(t, r.k, 1); !slices.Equal(got, tail) {
		t.Errorf("the open at the target was given %d samples, not a from 1 s then b, %d", len(got), len(tail))
	}
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	first := len(got) - len(tail)
	if first <= 0 || !slices.Equal(got[:first], a[:first]) || !slices.Equal(got[first:], tail) {
		t.Errorf("the DAC heard %d samples; want a's start, a from 1 s, then b, each once", len(got))
	}
	if n := count(r.rec.events(), "audible", named.next.ID); n != 1 {
		t.Errorf("b was heard %d times in the session at the target, want once", n)
	}
}

// TestPlaybackPlaysOnlyTheLastOfRepeatedSeeks pins two seeks one after the
// other, the DAC holding: the first one's session is released before the DAC
// plays a frame of it, and the DAC hears the start and then the track from
// the second target alone.
func TestPlaybackPlaysOnlyTheLastOfRepeatedSeeks(t *testing.T) {
	const rate = 44100
	a := pattern(1, 4*rate, 16)
	r := aHeard(t, a)
	for _, s := range []int64{1, 3} {
		if err := r.eng.SeekAbsolute(bg, s); err != nil {
			t.Fatalf("a seek to %d s: %v", s, err)
		}
	}
	r.k.Free()
	r.waitStopped(engine.ReasonEndOfQueue)
	v := r.k.View()
	if got := givenTo(t, r.k, len(v.OpenAt)-1); !slices.Equal(got, a[2*3*rate:]) {
		t.Errorf("the last open was given %d samples, not exactly the track from 3 s", len(got))
	}
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	first := len(got) - len(a[2*3*rate:])
	if first <= 0 || !slices.Equal(got[:first], a[:first]) || !slices.Equal(got[first:], a[2*3*rate:]) {
		t.Errorf("the DAC heard %d samples; want the start, then the track from 3 s alone", len(got))
	}
}

// TestPlaybackStopsDuringASeeksReopen pins a Stop while the session started
// for a seek opens the device: held in its check after open, the open is
// closed unwritten once the Stop releases the session, the seek answers
// superseded, and nothing of the target is given or heard.
func TestPlaybackStopsDuringASeeksReopen(t *testing.T) {
	r := aHeard(t, pattern(1, 4*44100, 16))
	held, release := r.unit.holdNextAdmit()
	t.Cleanup(release)
	seek := make(chan error, 1)
	go func() { seek <- r.eng.SeekAbsolute(bg, 2) }()
	receiveIn(t, held, "the seek's open held in its check after open")
	stop := make(chan error, 1)
	go func() { stop <- r.eng.Stop(bg) }()
	r.waitSnap("the stop applied", func(s *engine.Snapshot) bool {
		return s.State() == engine.Releasing && s.Pending() == engine.IntentStop
	})
	release()
	if err := receiveIn(t, stop, "the stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := receiveIn(t, seek, "the seek"); engine.ReasonOf(err) != engine.ReasonSuperseded {
		t.Errorf("the seek a stop replaced: %v, want superseded", err)
	}
	r.waitStopped(engine.ReasonStopped)
	v := r.k.View()
	if v.Open || v.Opens != 2 || v.Closes != 2 || len(v.OpenAt) != 2 || v.OpenAt[1] != v.GivenBytes {
		t.Errorf("the seek's open: open %v after %d opens and %d closes, given %d bytes from %v; want closed unwritten", v.Open, v.Opens, v.Closes, v.GivenBytes, v.OpenAt)
	}
}

// TestPlaybackRefusesAFromItCannotTakeAsAFailure pins a start the content
// cannot take at its From: DSD (its demuxer's byte position is no time
// seek), a frame past a PCM track's end, and its exact end (nothing follows
// it) each fail the Start with an error, never an *engine.Unplayable, so the
// engine never skips the entry for it, and nothing opens the device.
func TestPlaybackRefusesAFromItCannotTakeAsAFailure(t *testing.T) {
	dsd, err := os.ReadFile(filepath.Join("..", "player", "decode", "testdata", "dsd-goldens", "lsb_dsd64_whole_blocks.dsf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		data []byte
		from int64
	}{
		{"DSD from a frame", dsd, 1},
		{"PCM past its end", wav(44100, 16, pattern(1, 1000, 16)), 2000},
		{"PCM at its exact end", wav(44100, 16, pattern(1, 1000, 16)), 1000},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDirect(t, nil)
			d.files.add("x", c.data)
			err := d.startFrom(1, "x", c.from)
			var u *engine.Unplayable
			if err == nil || errors.As(err, &u) {
				t.Fatalf("a start from %d: %v, want a failure that is no refusal", c.from, err)
			}
			if v := d.k.View(); v.Opens != 0 {
				t.Errorf("a start from a frame the content cannot take opened the device %d times", v.Opens)
			}
			d.release(1)
			d.files.allClosed(t)
		})
	}
}
