package policy

import (
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
)

// TestEveryPrefillAProfileMayGiveTheSinkTakes (B728 B7; B5's record left it
// to B7): a tuple's prefill is its profile's dsdPrefillMs, and the DSD sink
// refuses a prefill past its bound at the open, so the bound is at least the
// most a profile's lint lets through, and every profile in the database is
// inside it: the Chord Hugo 2's 700 ms was past the 500 ms the sink took
// before.
func TestEveryPrefillAProfileMayGiveTheSinkTakes(t *testing.T) {
	if lint := time.Duration(dacprofile.MaxTimingMs) * time.Millisecond; lint > alsapcm.MaxDSDPrefill {
		t.Fatalf("a profile may give a %v prefill, and the DSD sink takes %v", lint, alsapcm.MaxDSDPrefill)
	}
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range db.Profiles {
		if p.Timing == nil {
			continue
		}
		if d := time.Duration(p.Timing.DSDPrefillMs) * time.Millisecond; d > alsapcm.MaxDSDPrefill {
			t.Errorf("profile %s's prefill %v is past the DSD sink's %v", p.ID, d, alsapcm.MaxDSDPrefill)
		}
	}
}
