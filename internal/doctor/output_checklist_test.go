package doctor

import (
	"context"
	"testing"
)

// TestRunPrintsTheOutputChecklistLast: a player box's checklist (Player,
// DAC, Music, Phone) is part of the doctor's report, after the install's
// own checks, and a bridge that is no player adds nothing.
func TestRunPrintsTheOutputChecklistLast(t *testing.T) {
	lines := []Check{
		{Name: "player", Status: OK, Summary: "Volumio 4.119 · MPD 0.24.0 on port 6600"},
		{Name: "dac", Status: Warn, Summary: "iFi found, but MPD's output does not name it", Hint: "Point MPD's audio_output at the DAC."},
		{Name: "music", Status: OK, Summary: "Ready: can play music from another bridge or NAS"},
		{Name: "phone", Status: Warn, Summary: "No phone paired yet", Hint: "Scan the pairing QR."},
	}
	asked := 0
	d := Deps{OutputChecklist: func(context.Context) []Check { asked++; return lines }}
	rep := Run(context.Background(), d)
	if asked != 1 {
		t.Fatalf("the checklist was asked %d times", asked)
	}
	n := len(rep.Checks)
	if n < len(lines) {
		t.Fatalf("report has %d checks", n)
	}
	for i, want := range lines {
		got := rep.Checks[n-len(lines)+i]
		if got != want {
			t.Fatalf("check %d is %+v, want %+v", n-len(lines)+i, got, want)
		}
	}

	none := Run(context.Background(), Deps{OutputChecklist: func(context.Context) []Check { return nil }})
	base := Run(context.Background(), Deps{})
	if len(none.Checks) != len(base.Checks) {
		t.Fatalf("a bridge that is no player added %d checks", len(none.Checks)-len(base.Checks))
	}
}
