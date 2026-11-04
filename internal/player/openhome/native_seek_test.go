package openhome

import (
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// TestNativeSecondsSeek803 pins DSD's refusal of a seek within a track
// through the SOAP layer (B674): native DSD known by its Details (DSD, one
// bit), and DoP known by its native plan alone (DoP is DSD in PCM words,
// whatever its carrier), playing or paused, answer both seeks 803, Seek
// failed, with no call to the backend (no release, no start, nothing of the
// authorization a start would ask again), and nothing a reader sees changed:
// the transport, the selection, the track heard, its position and the
// generation.
func TestNativeSecondsSeek803(t *testing.T) {
	native := engine.Details{Frames: 2822400 * 60, RateHz: 2822400, Bits: 1, Channels: 2, Codec: "DSD", Lossless: true}
	dop := engine.NativePlan{Tuple: engine.NativeTuple{Transport: "dop", Format: "S32_LE", BitRate: 2822400}, PolicyID: "dop-64"}
	for _, c := range []struct {
		name string
		d    *engine.Details
		plan *engine.NativePlan
	}{{"native", &native, nil}, {"dop", nil, &dop}} {
		for _, paused := range []bool{false, true} {
			name := c.name + "/playing"
			if paused {
				name = c.name + "/paused"
			}
			t.Run(name, func(t *testing.T) {
				p := newTestPlayer(t)
				p.available()
				p.add("dsd", nil)
				must(t, "play", p.eng.Play(bg))
				s := p.be.session(t, 1)
				if c.plan != nil {
					s.ev.(engine.NativePlanEvents).NativePlan(s.id, s.first.ID, *c.plan)
				}
				s.hear(s.first.ID, c.d)
				p.heardNow(s.first.ID)
				s.successor(t, 1) // the end named after it: the session's hand-off settled
				s.position(s.first.ID, 5*2822400, 2822400)
				p.waitSnap("the position", func(sn *engine.Snapshot) bool { return sn.Position().Frames == 5*2822400 })
				if paused {
					must(t, "pause", p.eng.Pause(bg))
				}
				before, calls := p.eng.Snapshot(), p.be.callCounts()
				h := p.handler("")
				for _, a := range [][2]string{{"SeekSecondAbsolute", "5"}, {"SeekSecondAbsolute", "0"}, {"SeekSecondRelative", "-1"}, {"SeekSecondRelative", "5"}} {
					if r := invoke(t, h, playlist, a[0], "Value", a[1]); r.fault != 803 || r.description != "Seek failed" {
						t.Fatalf("%s(%s) in DSD: fault %d %q, want 803", a[0], a[1], r.fault, r.description)
					}
				}
				after := p.eng.Snapshot()
				bt, _ := before.Track()
				at, _ := after.Track()
				if after.Generation() != before.Generation() || after.State() != before.State() || after.Selected() != before.Selected() ||
					after.Position() != before.Position() || at.Occurrence != bt.Occurrence || after.TrackCount() != before.TrackCount() {
					t.Errorf("a seek in DSD changed the engine: %s at %d, position %+v", after.State(), after.Selected(), after.Position())
				}
				if got := p.be.callCounts(); fmt.Sprint(got) != fmt.Sprint(calls) {
					t.Errorf("a seek in DSD called the backend: %v, was %v", got, calls)
				}
			})
		}
	}
}
