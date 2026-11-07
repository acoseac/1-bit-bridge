package updater

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Backlog B705: the installed version's first boot that comes more than the
// boot window after its install (`bridge update` with the service stopped and
// a start the next day, or a crash between the swap and the restart) is the
// install working, as any boot of that version is, and no clear at boot drops
// the record that a version other than the backup's opened the data dir while
// the backup may stand: an operator's rollback refuses on that record
// (RollbackRefusal), and with it gone the backup went back over a store that
// version had migrated.

// TestALateFirstBootOfTheInstalledVersionConfirmsTheInstall: DecideBootAction
// takes the install's version booting for the install on probation whatever
// the marker's age, as it already did whatever its swap (backlog B660: until
// B660, for its success, which stamped it installed at boot; the start that
// answers and stays up confirms it now); a marker that no boot of its version
// reads keeps its answers: abandoned past the window, whatever its swap, and
// inside it not swapped (the Windows SCM stop's window) or failed.
func TestALateFirstBootOfTheInstalledVersionConfirmsTheInstall(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	marker := func(target string, age time.Duration, swapped bool) State {
		return State{Status: "installing", TargetVersion: target, PreviousVersion: "0.2.1",
			AttemptedAt: now.Add(-age), SwapStarted: swapped}
	}
	late := recencyWindow + time.Second
	admitted := marker("0.2.3", recencyWindow+time.Hour, true)
	admitted.DataDirOpenedBy = "0.2.3"
	for _, tc := range []struct {
		name    string
		st      State
		current string
		want    BootAction
	}{
		{"swapped, just past the window", marker("0.2.3", late, true), "0.2.3", BootInstallProbation},
		{"never swapped, just past the window", marker("0.2.3", late, false), "0.2.3", BootInstallProbation},
		{"a start the next day", marker("0.2.3", 30*time.Hour, true), "0.2.3", BootInstallProbation},
		{"the install's version spelled with a v", marker("v0.2.3", late, true), "0.2.3", BootInstallProbation},
		{"the running version spelled with a v", marker("0.2.3", late, false), "v0.2.3", BootInstallProbation},
		{"its admission recorded", admitted, "0.2.3", BootInstallProbation},
		{"the backup's version, swapped, past the window", marker("0.2.3", late, true), "0.2.1", BootClearAbandoned},
		{"the backup's version, never swapped, past the window", marker("0.2.3", late, false), "0.2.1", BootClearAbandoned},
		{"another version, past the window", marker("0.2.3", late, true), "0.2.9", BootClearAbandoned},
		{"another version spelled with a v, past the window", marker("v0.2.3", late, true), "v0.2.4", BootClearAbandoned},
		{"the backup's version, never swapped, inside the window", marker("0.2.3", time.Minute, false), "0.2.1", BootClearNotSwapped},
		{"the backup's version, swapped, inside the window", marker("0.2.3", time.Minute, true), "0.2.1", BootInstallFailed},
		{"the install's version, inside the window", marker("0.2.3", time.Minute, true), "0.2.3", BootInstallProbation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideBootAction(tc.st, tc.current, now); got != tc.want {
				t.Errorf("DecideBootAction = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAClearAtBootKeepsTheRecordARollbackRefusesOn: ClearInstall keeps, with
// a hold and a veto, the record that a version other than the backup's opened
// the data dir (DataDirOpenedBy, beside the backup's version,
// PreviousVersion), since the clears that call it leave the backup where it
// is: an operator's rollback then refuses on the record, writing and renaming
// nothing. A marker without that record is cleared as before, and the
// rollback goes on.
func TestAClearAtBootKeepsTheRecordARollbackRefusesOn(t *testing.T) {
	hold := State{RejectedVersion: "0.2.4", RejectedReason: "0.2.4 refuses this bridge's settings: line 4: x",
		RejectedConfigDigest: "a digest of the settings", MigrationVeto: "0.2.4"}
	withHold := func(st State) State {
		st.RejectedVersion, st.RejectedReason, st.RejectedConfigDigest = hold.RejectedVersion, hold.RejectedReason, hold.RejectedConfigDigest
		st.MigrationVeto = hold.MigrationVeto
		return st
	}
	recorded := func(st State) State {
		st.DataDirOpenedBy = "0.2.3"
		return st
	}
	keptRecord := State{DataDirOpenedBy: "0.2.3", PreviousVersion: "0.2.1"}
	for _, tc := range []struct {
		name    string
		marker  func(State) State
		want    State
		refuses bool
	}{
		{"a version recorded, with a hold and a veto", func(st State) State { return withHold(recorded(st)) },
			withHold(keptRecord), true},
		{"a version recorded alone", recorded, keptRecord, true},
		{"no version recorded, a hold and a veto", withHold, hold, false},
		{"no version recorded", func(st State) State { return st }, State{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newRollbackLayout(t)
			st := tc.marker(l.marker)
			l.save(t, st)
			if err := ClearInstall(l.dataDir, st); err != nil {
				t.Fatalf("ClearInstall = %v", err)
			}
			if got, err := LoadState(l.dataDir); err != nil || got != tc.want {
				t.Errorf("the marker after the clear = %+v (%v), want %+v", got, err, tc.want)
			}
			if _, err := os.Lstat(StatePath(l.dataDir)); (tc.want == State{}) != os.IsNotExist(err) {
				t.Errorf("the marker's file after the clear: lstat %v, want it gone only where nothing is kept", err)
			}
			before := l.files()
			err := New(Options{}).Rollback(context.Background(), l.opts())
			if !tc.refuses {
				if err != nil {
					t.Errorf("the rollback after the clear = %v, want the backup put back", err)
				}
				return
			}
			if !errors.Is(err, ErrRollbackRefused) {
				t.Fatalf("the rollback after the clear = %v, want it refused: 0.2.3 opened the data dir, and the backup is 0.2.1", err)
			}
			l.requireUnchanged(t, before)
		})
	}
}
