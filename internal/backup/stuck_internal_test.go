package backup

import "testing"

// TestTwoHoldsGivenUpUnderOneKeyEachStandsUntilItsOwnEnd: two holds given up
// whose marks share a key, as a verification's worker (which holds this
// process's slot) and the write of INCOMPLETE it starts for a stop that came
// before its lock (abandonPending, which holds the lock) do, each hold the
// folder until each is let go, in whichever order they end: the record of
// holds given up keeps every mark a key stands under, and a mark's end takes
// its own alone (backlog B653's fourth round). Red where a key held one mark:
// the later overwrote the earlier and its end erased the key while the
// earlier hold still stood, which the step test's abandonPending case, run
// twenty times under the race detector, failed on three times in twelve.
func TestTwoHoldsGivenUpUnderOneKeyEachStandsUntilItsOwnEnd(t *testing.T) {
	root := t.TempDir()
	key := spellingKey(root)
	for _, order := range []string{"the later ends first", "the earlier ends first"} {
		t.Run(order, func(t *testing.T) {
			earlier := markStuck([]string{key}, "", StuckCheck{Snapshot: "earlier"})
			later := markStuck([]string{key}, "", StuckCheck{Snapshot: "later"})
			first, second, standing := later, earlier, "earlier"
			if order == "the earlier ends first" {
				first, second, standing = earlier, later, "later"
			}
			first.drop()
			if c, held := stuckBy(root); !held || c.Snapshot != standing {
				t.Errorf("once one hold ended, the folder reads %+v, %v; want the %s hold's mark standing", c, held, standing)
			}
			second.drop()
			if c, held := stuckBy(root); held {
				t.Errorf("once both holds ended, the folder still reads %+v", c)
			}
		})
	}
}
