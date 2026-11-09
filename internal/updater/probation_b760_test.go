package updater

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// TestCountingAStartAgainUndoesItsTakeBackAlone: CountFailedStartAgain puts
// back the count a start's take-back took where the marker still holds what
// that take-back left, and writes nothing over a take-back that never landed,
// a confirmation, an install of another version, or a start that tried no
// count (backlog B760).
func TestCountingAStartAgainUndoesItsTakeBackAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      State
		counted int
		wrote   bool
		want    int
	}{
		{"after its take-back", probationMarker(1), 2, true, 2},
		{"after its take-back, no count before it", probationMarker(0), 1, true, 1},
		{"after its take-back over the crash rollback's record", CrashRollbackRecord(probationMarker(2), "0.2.4"), 3, true, 3},
		{"a take-back that never landed", probationMarker(2), 2, false, 2},
		{"a start that tried no count", probationMarker(0), 0, false, 0},
		{"after the confirmation", State{Status: "installed", TargetVersion: "0.2.4", PreviousVersion: "0.2.3",
			InstalledAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}, 1, false, 0},
		{"over an install of another version", State{Status: "installing", TargetVersion: "0.2.5", PreviousVersion: "0.2.4",
			AttemptedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), SwapStarted: true}, 1, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustSave(t, dir, tc.st)
			wrote, err := CountFailedStartAgain(t.Context(), dir, "0.2.4", tc.counted)
			if err != nil || wrote != tc.wrote {
				t.Fatalf("CountFailedStartAgain = %v, %v; want %v", wrote, err, tc.wrote)
			}
			got := mustLoad(t, dir)
			if got.FailedStarts != tc.want {
				t.Errorf("the marker = %+v, want %d starts counted", got, tc.want)
			}
			if !tc.wrote && got != tc.st {
				t.Errorf("the marker = %+v, want it as it was, %+v", got, tc.st)
			}
		})
	}
}

// TestTheAdmissionKeepsItsStartsHoldWhereItsCountsWriteFailed: where the count
// is the admission's only write and that write fails, the start keeps its hold
// on its own lock (backlog B760).
func TestTheAdmissionKeepsItsStartsHoldWhereItsCountsWriteFailed(t *testing.T) {
	t.Run("published, its barrier failed", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		armed := false
		t.Cleanup(SetBeforeDisarmRecordForTest(func(string) { armed = true }))
		t.Cleanup(SetMarkerBarrierForTest(func(step string) error {
			if step == "sync" && armed {
				armed = false
				return syscall.EIO
			}
			return nil
		}))
		res, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
		if err != nil || res.Counted || res.CountMissed == nil || res.CountTried != 2 {
			t.Fatalf("the admission = %+v, %v; want a missed count said, the count it tried", res, err)
		}
		t.Cleanup(res.Start.Release)
		st := mustLoad(t, dir)
		if res.Start.Token() == "" || st.RunningStart != res.Start.Token() || !RunningServeCounted(dir, st, "0.2.4") {
			t.Errorf("the marker = %+v and the start's token %q, want the token published and read as a start running", st, res.Start.Token())
		}
	})
	t.Run("published nothing", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		before := mustLoad(t, dir)
		res, err := AdmitBoot(t.Context(), dir, "0.2.4", func(string, State) error { return errors.New("the disk is full") })
		if err != nil || res.Counted || res.CountMissed == nil {
			t.Fatalf("the admission = %+v, %v; want a missed count said", res, err)
		}
		t.Cleanup(res.Start.Release)
		if res.Start == nil || res.Start.Token() == "" {
			t.Errorf("the admission's start hold = %+v, want the start's own, kept", res.Start)
		}
		if got := mustLoad(t, dir); got != before || RunningServeCounted(dir, got, "0.2.4") {
			t.Errorf("the marker = %+v, want it as it was, %+v, with no start read as running", got, before)
		}
	})
}

// TestATakeBackSaysItReachedItsWriteWhateverTheWriteAnswered: a take-back
// that finds the marker holding the count its start wrote reports that it
// reached its write, whether the write landed, published and then failed its
// barrier (SaveState renames first, so the next ask finds no count of its own
// and answers no write and no error), or failed before it published; one that
// finds another count reaches no write, its save never asked (backlog B760's
// second round: serve counts a start again only after a take-back that reached
// its write, CountFailedStartAgain).
func TestATakeBackSaysItReachedItsWriteWhateverTheWriteAnswered(t *testing.T) {
	t.Run("published, then its barrier failed", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		armed := true
		t.Cleanup(SetMarkerBarrierForTest(func(step string) error {
			if step == "sync" && armed {
				armed = false
				return syscall.EIO
			}
			return nil
		}))
		if reached, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 2, nil); err == nil || !reached {
			t.Fatalf("a take-back whose barrier failed = %v, %v; want its write reached, and the failure", reached, err)
		}
		if armed {
			t.Fatal("the take-back's barrier was never asked: the test shows nothing")
		}
		if st := mustLoad(t, dir); st.FailedStarts != 1 {
			t.Fatalf("the marker after a take-back whose barrier failed = %+v, want its count taken back, published", st)
		}
		if reached, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 2, nil); err != nil || reached {
			t.Errorf("the take-back asked again = %v, %v; want no write and no error, its count gone back already", reached, err)
		}
	})
	t.Run("failed before it published", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		before := mustLoad(t, dir)
		reached, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 2, func(string, State) error { return errors.New("the disk is full") })
		if err == nil || !reached {
			t.Fatalf("a take-back whose write failed = %v, %v; want its write reached, and the failure", reached, err)
		}
		if got := mustLoad(t, dir); got != before {
			t.Errorf("the marker = %+v, want it as it was, %+v", got, before)
		}
	})
	t.Run("a count that never landed", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		saved := false
		reached, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 2, func(dir string, st State) error {
			saved = true
			return SaveState(dir, st)
		})
		if err != nil || reached || saved {
			t.Errorf("a take-back over a count its start never wrote = %v, %v (saved %v); want no write reached", reached, err, saved)
		}
	})
}
