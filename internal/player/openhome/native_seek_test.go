package openhome

import (
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"testing"
)

func TestNativeSecondsSeek803(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "playing"
		if paused {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			p := newTestPlayer(t)
			p.available()
			p.add("dsd", nil)
			must(t, "play", p.eng.Play(bg))
			s := p.be.session(t, 1)
			d := engine.Details{Frames: 2822400 * 60, RateHz: 2822400, Bits: 1, Channels: 2, Codec: "DSD", Lossless: true}
			s.hear(s.first.ID, &d)
			p.heardNow(s.first.ID)
			if paused {
				must(t, "pause", p.eng.Pause(bg))
			}
			before := p.eng.Snapshot()
			h := p.handler("")
			for _, action := range []string{"SeekSecondAbsolute", "SeekSecondRelative"} {
				r := invoke(t, h, playlist, action, "Value", "5")
				if r.fault != 803 {
					t.Fatalf("%s fault=%d want803", action, r.fault)
				}
				after := p.eng.Snapshot()
				if before.Selected() != after.Selected() || before.State() != after.State() || before.Position() != after.Position() {
					t.Fatal("803 changed transport")
				}
			}
		})
	}
}
