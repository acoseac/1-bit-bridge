package doctor

import (
	"context"
	"testing"
)

// TestRunPrintsThePlayersStorageBeforeTheChecklist pins where the report
// carries the line on the bridge's own player's storage (Deps.PlayerStorage,
// B603): asked once, after Run's own checks and right before the output
// checklist, which stays last; a func that returns nothing, and none at all,
// add no line.
func TestRunPrintsThePlayersStorageBeforeTheChecklist(t *testing.T) {
	storage := Check{Name: "player-storage", Status: OK, Summary: "/data/1-bit-bridge/config/data is on Volumio's data partition (mmcblk0p3, ext4)"}
	checklist := []Check{{Name: "player", Status: OK, Summary: "Volumio 4.231 · MPD 0.24.0 on port 6600"}, {Name: "phone", Status: OK, Summary: "1 device paired"}}
	asked := 0
	rep := Run(context.Background(), Deps{
		PlayerStorage:   func(context.Context) []Check { asked++; return []Check{storage} },
		OutputChecklist: func(context.Context) []Check { return checklist },
	})
	n := len(rep.Checks)
	if asked != 1 || n < 3 {
		t.Fatalf("the storage line was asked %d times, and the report has %d checks", asked, n)
	}
	if got := rep.Checks[n-3]; got != storage {
		t.Errorf("the line before the checklist is %+v, want %+v", got, storage)
	}
	if rep.Checks[n-2] != checklist[0] || rep.Checks[n-1] != checklist[1] {
		t.Errorf("the checklist is not last: %+v", rep.Checks[n-2:])
	}
	base := Run(context.Background(), Deps{})
	for _, d := range []Deps{{}, {PlayerStorage: func(context.Context) []Check { return nil }}} {
		if got := Run(context.Background(), d); len(got.Checks) != len(base.Checks) {
			t.Errorf("a config that asks for no player added %d checks", len(got.Checks)-len(base.Checks))
		}
	}
}
