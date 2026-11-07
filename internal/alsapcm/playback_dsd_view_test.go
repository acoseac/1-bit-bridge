package alsapcm_test

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The read side of the player's DSD (B728 B8) while DSD plays, through the
// engine's Run, the backend and the real sinks over the fake kernel: a change
// of what the read side says that leaves what plays as it was is published
// beside the set occurrences were admitted under, never as a set that
// replaces it.

// TestAFirstUseObservedWhileDSDPlaysKeepsTheProducerWriting (B728 B8 round
// 2): a transport's first use observed, which every first DSD playback of a
// transport records once its 5 s have played, reaches the session's monitor
// while an occurrence plays and its successor waits admitted under the set
// published: the read side says it, under the generation it had, and nothing
// an occurrence was admitted under moves. The successor joins in the same
// sink as a gapless successor joins under the set published now, a native
// one's frames straight after its predecessor's and a converted one's chain
// continued through its predecessor's join window (dsdconv.Continuous): both
// heard, nothing re-planned, faulted or reopened. The producer is held inside
// the first track's content until the change is published, so the join is
// decided after it.
func TestAFirstUseObservedWhileDSDPlaysKeepsTheProducerWriting(t *testing.T) {
	observed := func(st dsdstate.State) dsdstate.State {
		for i := range st.DACs {
			for j := range st.DACs[i].FirstUse {
				if st.DACs[i].FirstUse[j].Transport == dsdstate.TransportNative {
					st.DACs[i].FirstUse[j].ObservedAt = playbacktest.T0
				}
			}
		}
		return st
	}
	warned := func(st dsdstate.State) dsdstate.State {
		key := playbacktest.DACKey()
		st.Warnings = append(st.Warnings, dsdstate.Warning{Code: dsdstate.WarningMigrationRecordMissing, DAC: &key, At: playbacktest.T0})
		return st
	}
	for _, c := range []struct {
		name string
		// rig is the rig, and what plays: the test DAC's own native, or an
		// unknown DAC's DSD converted (B728 B11).
		rig func(t *testing.T) *dsdRig
		// change is a change of the state that moves no plan: native's first
		// use observed where native plays; where the set converts, a warning
		// the bound DAC owns.
		change func(dsdstate.State) dsdstate.State
		said   func(v *playback.DSDView) bool
	}{
		{"a native successor", func(t *testing.T) *dsdRig { return newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative)) }, observed,
			func(v *playback.DSDView) bool {
				for _, f := range v.DAC.FirstUse {
					if f.Transport == dsdstate.TransportNative {
						return f.State == playback.FirstUseObserved
					}
				}
				return false
			}},
		{"a converted successor in its predecessor's join window", func(t *testing.T) *dsdRig {
			d := newConvRig(t)
			d.k.HardwareFormats(formatCode[format.S243LE])
			return d
		}, warned, func(v *playback.DSDView) bool {
			return len(v.Warnings) == 1 && v.Warnings[0].Code == dsdstate.WarningMigrationRecordMissing
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := c.rig(t)
			a, b := track(playbacktest.DSD64, 0.5, noise(1)), track(playbacktest.DSD64, 0.5, noise(2))
			d.files.add("a", a)
			d.files.add("b", b)
			parked, unstall := d.files.stall("a", int64(len(a)/2))
			t.Cleanup(unstall)
			ids := d.insert("a", "b")
			d.play()
			awaitHeld(t, "a's read halfway through its content", parked)
			ob := d.preparedOcc(ids[1])
			before := d.be.Policy()
			d.store.Set(c.change(d.store.State()))
			d.monitorTick()
			after := d.be.Policy()
			if after.Generation != before.Generation || after.BootID != before.BootID {
				t.Errorf("a change that plays nothing new published generation %d, want %d", after.Generation, before.Generation)
			}
			if after.DSD == nil || after.DSD.DAC == nil || !c.said(after.DSD) {
				t.Fatalf("the read side published %+v: the change is not said", after.DSD)
			}
			unstall()
			d.k.Free()
			s := d.waitStopped(engine.ReasonEndOfQueue)
			evs := d.rec.events()
			if count(evs, "audible", ob) != 1 || count(evs, "audible", 0) != 2 || count(evs, "fault", 0) != 0 || count(evs, "replan", 0) != 0 || len(s.Faults()) != 0 {
				t.Errorf("the successor did not join as it was admitted: %s; faults %+v", kinds(evs), s.Faults())
			}
			if v := d.k.View(); v.Opens != 1 {
				t.Errorf("%d opens, want a and b in one sink", v.Opens)
			}
			if d.pcmOpens() == 1 {
				// The converted pair: the DAC given the two tracks' bits as one
				// chain, its window computed with b's first bits.
				same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), chained(t, convPlan, a, b))
			} else if got := d.openFormats(); len(got) != 1 || got[0] != format.DSDU32BE {
				t.Errorf("the DSD opens asked %v, want one DSD_U32_BE", got)
			}
		})
	}
}
