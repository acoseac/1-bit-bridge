package doctor

import (
	"context"
	"testing"
)

// TestRunPrintsThePlayersDACLineAfterItsStorage pins where the report
// carries the line on the bridge's own player's DAC node (Deps.PlayerDAC,
// B676): asked once, right after the player's storage line and before the
// output checklist, which stays last; a func that returns nothing, and none
// at all, add no line.
func TestRunPrintsThePlayersDACLineAfterItsStorage(t *testing.T) {
	storage := Check{Name: "player-storage", Status: OK, Summary: "/srv/bridge/data is on ext4"}
	dac := Check{Name: "player-dac", Status: Fail, Summary: "the player cannot open its DAC: /dev/snd/pcmC5D0p, which the audio group owns, is refused"}
	checklist := []Check{{Name: "player", Status: OK, Summary: "Volumio 4.231 · MPD 0.24.0 on port 6600"}}
	asked := 0
	rep := Run(context.Background(), Deps{
		PlayerStorage:   func(context.Context) []Check { return []Check{storage} },
		PlayerDAC:       func(context.Context) []Check { asked++; return []Check{dac} },
		OutputChecklist: func(context.Context) []Check { return checklist },
	})
	n := len(rep.Checks)
	if asked != 1 || n < 3 {
		t.Fatalf("the DAC line was asked %d times, and the report has %d checks", asked, n)
	}
	if rep.Checks[n-3] != storage || rep.Checks[n-2] != dac || rep.Checks[n-1] != checklist[0] {
		t.Errorf("the report ends %+v, want the storage line, the DAC line, then the checklist", rep.Checks[n-3:])
	}
	base := Run(context.Background(), Deps{})
	for _, d := range []Deps{{}, {PlayerDAC: func(context.Context) []Check { return nil }}} {
		if got := Run(context.Background(), d); len(got.Checks) != len(base.Checks) {
			t.Errorf("a config that asks for no player added %d checks", len(got.Checks)-len(base.Checks))
		}
	}
}
