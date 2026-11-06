package updater

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Backlog B660: an install counts as healthy only once a start of it has
// answered and stayed up; three starts that did not end cleanly roll it back,
// never past a migration of the store.

// probationMarker is an install of 0.2.4 over 0.2.3, swapped, its admission
// recorded, with n starts counted.
func probationMarker(n int) State {
	return State{Status: "installing", TargetVersion: "0.2.4", PreviousVersion: "0.2.3",
		AttemptedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), SwapStarted: true,
		DataDirOpenedBy: "0.2.4", FailedStarts: n}
}

// TestCrashRollbackDecidesOnTheMarkerAlone: the crash rollback's first
// check, the one that starts no process, by every term, with no recency
// window, and each refusal in words that name no path.
func TestCrashRollbackDecidesOnTheMarkerAlone(t *testing.T) {
	limit := ProbationFailedStarts
	with := func(f func(*State)) State {
		st := probationMarker(limit)
		f(&st)
		return st
	}
	crashRecord := CrashRollbackRecord(probationMarker(limit), "0.2.4")
	operatorRecord := OperatorRollbackRecord(probationMarker(0), "0.2.4")
	operatorRecord.DataDirOpenedBy = ""
	for _, tc := range []struct {
		name    string
		st      State
		current string
		ok      bool
		says    string
	}{
		{"no marker", State{}, "0.2.4", false, ""},
		{"an install confirmed", with(func(s *State) { s.Status, s.InstalledAt = "installed", time.Now() }), "0.2.4", false, ""},
		{"another version's install", probationMarker(limit), "0.2.9", false, ""},
		{"the backup's version running", probationMarker(limit), "0.2.3", false, ""},
		{"no start counted", probationMarker(0), "0.2.4", false, ""},
		{"one fewer than the limit", probationMarker(limit - 1), "0.2.4", false, ""},
		{"at the limit", probationMarker(limit), "0.2.4", true, ""},
		{"past the limit", probationMarker(limit + 1), "0.2.4", true, ""},
		{"at the limit, spelled with a v", probationMarker(limit), "v0.2.4", true, ""},
		{"at the limit, a month after its install", with(func(s *State) { s.AttemptedAt = s.AttemptedAt.AddDate(0, -1, 0) }), "0.2.4", true, ""},
		{"at the limit, no admission recorded", with(func(s *State) { s.DataDirOpenedBy = "" }), "0.2.4", true, ""},
		{"the crash rollback's own record, its rename not landed", crashRecord, "0.2.4", true, ""},
		{"an operator's rollback stopped before its rename", operatorRecord, "0.2.4", false, ""},
		{"never swapped", with(func(s *State) { s.SwapStarted = false }), "0.2.4", false, "never reached its swap"},
		{"no previous version", with(func(s *State) { s.PreviousVersion = "" }), "0.2.4", false, "no other version"},
		{"a previous version that is this one", with(func(s *State) { s.PreviousVersion = "0.2.4" }), "0.2.4", false, "no other version"},
		{"a migration of the store begun", with(func(s *State) { s.StoreMigratedBy = "0.2.4" }), "0.2.4", false, "began a migration of the store"},
		{"a migration begun by another version", with(func(s *State) { s.StoreMigratedBy = "0.2.9" }), "0.2.4", false, "0.2.9 began a migration"},
		{"the data dir opened by another version", with(func(s *State) { s.DataDirOpenedBy = "0.2.9" }), "0.2.4", false, "0.2.9 has opened the data dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := CrashRollback(tc.st, tc.current)
			if ok != tc.ok {
				t.Fatalf("CrashRollback = %v (%q), want %v", ok, why, tc.ok)
			}
			if !strings.Contains(why, tc.says) || (tc.says == "") != (why == "") {
				t.Errorf("CrashRollback's reason = %q, want it to say %q", why, tc.says)
			}
			if strings.ContainsAny(why, `/\`) {
				t.Errorf("CrashRollback's reason %q names a path", why)
			}
		})
	}
}

// TestTheCrashRollbacksRecordKeepsWhatTellsItFromTheOthers: the record the
// crash rollback writes before its rename is the install still, armed and
// finishable by a start of its version, keeps the count and the record of the
// data dir (which RollbackRecord drops), holds the version back whatever the
// settings, vetoes its migrations, and is a rollback done for the backup's
// version and a rollback overtaken for the install's.
func TestTheCrashRollbacksRecordKeepsWhatTellsItFromTheOthers(t *testing.T) {
	st := probationMarker(ProbationFailedStarts)
	st.RollbackRefused, st.NotReady = "an earlier refusal", true
	got := CrashRollbackRecord(st, "0.2.4")
	want := st
	want.Status, want.RollbackStarted = "", true
	want.RejectedVersion, want.RejectedConfigDigest, want.MigrationVeto = "0.2.4", "", "0.2.4"
	want.RejectedReason = got.RejectedReason
	want.RollbackRefused = ""
	if got != want {
		t.Errorf("CrashRollbackRecord =\n %+v\nwant\n %+v", got, want)
	}
	if !strings.Contains(got.RejectedReason, "did not end cleanly") || !strings.Contains(got.RejectedReason, "0.2.3") {
		t.Errorf("the record's reason = %q, want it to name the starts that did not end cleanly and the version put back", got.RejectedReason)
	}
	if !InstallArmed(got) {
		t.Error("the record is no install still: a start of 0.2.4 would not finish it")
	}
	if ok, why := CrashRollback(got, "0.2.4"); !ok {
		t.Errorf("CrashRollback over its own record = %q, want it finishable", why)
	}
	if a := DecideBootAction(got, "0.2.3", time.Now()); a != BootRollbackFinished {
		t.Errorf("the backup's version's boot over the record = %v, want BootRollbackFinished", a)
	}
	if a := DecideBootAction(got, "0.2.4", time.Now()); a != BootRollbackOvertaken {
		t.Errorf("the install's version's boot over the record = %v, want BootRollbackOvertaken", a)
	}
	if !MigrationVetoed(got, "0.2.4") || MigrationVetoed(got, "0.2.3") {
		t.Error("the record does not veto 0.2.4's migrations alone")
	}
	early := RollbackRecord(st, "0.2.4", "a reason", "a digest")
	if early.FailedStarts != 0 || early.DataDirOpenedBy != "" {
		t.Errorf("RollbackRecord = %+v, which keeps the count or the record: the count must tell the crash rollback's record apart", early)
	}
}

// crashRecord023Fixture is CrashRollbackRecord's JSON for an install of
// 0.2.4 over 0.2.3 after three starts that did not end cleanly, as this
// build writes it: the bytes a restored 0.2.3 reads.
const crashRecord023Fixture = `{
  "targetVersion": "0.2.4",
  "previousVersion": "0.2.3",
  "attemptedAt": "2026-11-06T12:00:00Z",
  "installedAt": "0001-01-01T00:00:00Z",
  "swapStarted": true,
  "rejectedVersion": "0.2.4",
  "rejectedReason": "0.2.4 was rolled back to 0.2.3, since 3 of its starts since its install did not end cleanly",
  "dataDirOpenedBy": "0.2.4",
  "rollbackStarted": true,
  "migrationVeto": "0.2.4",
  "failedStarts": 3
}
`

// state023 is updater.State as 0.2.3 declares it (tag v0.2.3, 0ddb8c72),
// frozen: the fields a restored 0.2.3 reads of a marker this build writes.
type state023 struct {
	Status               string    `json:"status,omitempty"`
	TargetVersion        string    `json:"targetVersion,omitempty"`
	PreviousVersion      string    `json:"previousVersion,omitempty"`
	AttemptedAt          time.Time `json:"attemptedAt,omitempty"`
	InstalledAt          time.Time `json:"installedAt,omitempty"`
	SwapStarted          bool      `json:"swapStarted,omitempty"`
	RejectedVersion      string    `json:"rejectedVersion,omitempty"`
	RejectedReason       string    `json:"rejectedReason,omitempty"`
	RejectedConfigDigest string    `json:"rejectedConfigDigest,omitempty"`
	DataDirOpenedBy      string    `json:"dataDirOpenedBy,omitempty"`
	RollbackStarted      bool      `json:"rollbackStarted,omitempty"`
	MigrationVeto        string    `json:"migrationVeto,omitempty"`
}

// decide023 is 0.2.3's DecideBootAction for a marker with no status, frozen:
// a rollback's record is a rollback overtaken for its target, done for its
// previous version, and nothing for any other; no status and no rollback is
// nothing.
func decide023(st state023, current string) BootAction {
	if st.Status != "" || !st.RollbackStarted {
		return BootNoop
	}
	switch {
	case normalizeTag(current) == normalizeTag(st.TargetVersion):
		return BootRollbackOvertaken
	case normalizeTag(current) == normalizeTag(st.PreviousVersion):
		return BootRollbackFinished
	}
	return BootNoop
}

// TestTheCrashRecordIsWhatAnOlderUpdaterReadsAsARollbackDone: the record is
// pinned from a literal fixture, which this build writes byte for byte, and
// which 0.2.3's State, frozen, decodes into a rollback done for 0.2.3 (its
// BootRollbackFinished, whose ClearInstall keeps the hold and the veto), the
// fields it does not know dropped; and a marker as 0.2.3's updater writes it,
// with none of the new fields, is an install on probation here, which its
// start counts.
func TestTheCrashRecordIsWhatAnOlderUpdaterReadsAsARollbackDone(t *testing.T) {
	st := probationMarker(ProbationFailedStarts)
	written, err := json.MarshalIndent(CrashRollbackRecord(st, "0.2.4"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(written) + "\n"; got != crashRecord023Fixture {
		t.Fatalf("CrashRollbackRecord's JSON =\n%s\nwant the fixture\n%s", got, crashRecord023Fixture)
	}
	var old state023
	if err := json.Unmarshal([]byte(crashRecord023Fixture), &old); err != nil {
		t.Fatal(err)
	}
	if a := decide023(old, "0.2.3"); a != BootRollbackFinished {
		t.Errorf("0.2.3's boot over the record = %v, want BootRollbackFinished", a)
	}
	if old.RejectedVersion != "0.2.4" || old.RejectedReason == "" || old.RejectedConfigDigest != "" || old.MigrationVeto != "0.2.4" {
		t.Errorf("0.2.3 reads the record as %+v, want 0.2.4 held back whatever the settings, its migrations vetoed", old)
	}

	// The upgrade: 0.2.3's own install marker, read here.
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte(`{
  "status": "installing",
  "targetVersion": "0.2.4",
  "previousVersion": "0.2.3",
  "attemptedAt": "2026-11-06T12:00:00Z",
  "swapStarted": true
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := DecideBootAction(marker, "0.2.4", time.Now()); a != BootInstallProbation {
		t.Errorf("0.2.4's boot over 0.2.3's install marker = %v, want BootInstallProbation", a)
	}
	res, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
	if err != nil || !res.Counted || res.CountTried != 1 {
		t.Fatalf("0.2.4's admission over 0.2.3's install marker = %+v, %v; want its start counted", res, err)
	}
	if got := mustLoad(t, dir); got.FailedStarts != 1 || got.DataDirOpenedBy != "0.2.4" || got.Status != "installing" {
		t.Errorf("the marker after the admission = %+v, want this start counted and recorded, the install on probation", got)
	}
}

func mustLoad(t *testing.T, dir string) State {
	t.Helper()
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestProbationStatusSaysWhereAnInstallStands: nothing where no install of
// the version reading it awaits its confirmation; ok at 0 and 1, a start
// running counting itself; a warning from 2; a failure at the limit, with a
// refused crash rollback, or with a start that did not answer as ready. The
// doctor's words count the marker as it is, the running start's take itself
// off; neither names a path.
func TestProbationStatusSaysWhereAnInstallStands(t *testing.T) {
	refused, notReady := probationMarker(4), probationMarker(1)
	refused.RollbackRefused = "0.2.4 began a migration of the store, which may have changed it"
	notReady.NotReady = true
	for _, tc := range []struct {
		name               string
		st                 State
		current            string
		level              ProbationLevel
		says, thisStart    string
		saysNot, thisStNot string
	}{
		{"no marker", State{}, "0.2.4", ProbationNone, "", "", "", ""},
		{"another version's install", probationMarker(1), "0.2.9", ProbationNone, "", "", "", ""},
		{"a confirmed install", State{Status: "installed", TargetVersion: "0.2.4", InstalledAt: time.Now()}, "0.2.4", ProbationNone, "", "", "", ""},
		{"no start yet", probationMarker(0), "0.2.4", ProbationOK, "on probation", "this start is confirmed", "not ended cleanly", "did not end cleanly"},
		{"a healthy start running", probationMarker(1), "0.2.4", ProbationOK, "on probation", "this start is confirmed", "not ended cleanly", "did not end cleanly"},
		{"one start before this one", probationMarker(2), "0.2.4", ProbationWarn, "2 of its starts", "1 of its starts before this one", "", ""},
		{"at the limit", probationMarker(3), "0.2.4", ProbationFail, "the next start puts 0.2.3 back", "if this one does not either", "", ""},
		{"a crash rollback refused", refused, "0.2.4", ProbationFail, "the backup is not put back: " + refused.RollbackRefused,
			"the backup is not put back: " + refused.RollbackRefused, "", ""},
		{"a start that did not answer", notReady, "0.2.4", ProbationFail, "did not answer as ready", "did not answer as ready", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ProbationStatus(tc.st, tc.current)
			if p.Level != tc.level {
				t.Fatalf("ProbationStatus = %+v, want level %v", p, tc.level)
			}
			if tc.level == ProbationNone {
				if p.Says != "" || p.ThisStart != "" {
					t.Errorf("ProbationStatus says %+v where no install awaits", p)
				}
				return
			}
			if !strings.Contains(p.Says, tc.says) || (tc.saysNot != "" && strings.Contains(p.Says, tc.saysNot)) {
				t.Errorf("the doctor's words = %q, want them to say %q and not %q", p.Says, tc.says, tc.saysNot)
			}
			if !strings.Contains(p.ThisStart, tc.thisStart) || (tc.thisStNot != "" && strings.Contains(p.ThisStart, tc.thisStNot)) {
				t.Errorf("the running start's words = %q, want them to say %q and not %q", p.ThisStart, tc.thisStart, tc.thisStNot)
			}
			for _, words := range []string{p.Says, p.ThisStart} {
				if strings.ContainsAny(words, `\`) || strings.Contains(words, "/dev") || strings.Contains(words, "/home") {
					t.Errorf("the words %q name a path", words)
				}
			}
		})
	}
}

// TestTheAdmissionCountsTheStartsOfTheInstallsVersion: serve's admission adds
// one to the count where its version is the install's target, in its one
// write with its record, clears NotReady, and counts nothing of another
// version; the count's own write failing, or its lock not taken, is a line
// (CountMissed), never a refusal, while the barrier before it still refuses.
func TestTheAdmissionCountsTheStartsOfTheInstallsVersion(t *testing.T) {
	t.Run("with its record", func(t *testing.T) {
		dir := t.TempDir()
		st := probationMarker(0)
		st.DataDirOpenedBy, st.NotReady = "", true
		mustSave(t, dir, st)
		res, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
		if err != nil || !res.Recorded || !res.Counted || res.CountTried != 1 {
			t.Fatalf("the admission = %+v, %v; want its record and its count in one write", res, err)
		}
		if got := mustLoad(t, dir); got.FailedStarts != 1 || got.DataDirOpenedBy != "0.2.4" || got.NotReady || got != res.State {
			t.Errorf("the marker = %+v (the result's %+v), want the count, the record, NotReady cleared", got, res.State)
		}
	})
	t.Run("alone, over a record made", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		res, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
		if err != nil || res.Recorded || !res.Counted || res.CountTried != 3 {
			t.Fatalf("the admission = %+v, %v; want the count alone", res, err)
		}
	})
	t.Run("of another version", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		res, err := AdmitBoot(t.Context(), dir, "0.2.3", nil)
		if err != nil || res.Counted || res.CountTried != 0 {
			t.Fatalf("the backup's version's admission = %+v, %v; want nothing counted", res, err)
		}
		if got := mustLoad(t, dir); got.FailedStarts != 2 {
			t.Errorf("the marker = %+v, want the count as it was", got)
		}
	})
	t.Run("whose own write fails", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		res, err := AdmitBoot(t.Context(), dir, "0.2.4", func(string, State) error { return errors.New("the disk is full") })
		if err != nil || res.Counted || res.CountTried != 3 || res.CountMissed == nil {
			t.Fatalf("the admission = %+v, %v; want a missed count said, not a refusal, and the count it tried", res, err)
		}
	})
	t.Run("whose record and count fail together", func(t *testing.T) {
		dir := t.TempDir()
		st := probationMarker(0)
		st.DataDirOpenedBy = ""
		mustSave(t, dir, st)
		if _, err := AdmitBoot(t.Context(), dir, "0.2.4", func(string, State) error { return errors.New("the disk is full") }); err == nil {
			t.Fatal("the admission whose record fails = nil, want its refusal, as before")
		}
	})
	t.Run("whose barrier fails", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		t.Cleanup(SetMarkerBarrierForTest(func(string) error { return syscall.EIO }))
		if _, err := AdmitBoot(t.Context(), dir, "0.2.4", nil); err == nil {
			t.Fatal("the admission whose barrier fails = nil, want its refusal: the barrier runs first")
		}
	})
	t.Run("without the lock", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(2))
		t.Cleanup(SetStateTryLockForTest(func(*os.File) error { return syscall.EIO }))
		res, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
		if err != nil || res.Counted || res.CountTried != 0 || res.CountMissed == nil {
			t.Fatalf("the admission without the lock = %+v, %v; want a missed count said, nothing tried", res, err)
		}
	})
}

func mustSave(t *testing.T, dir string, st State) {
	t.Helper()
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
}

// TestAMigrationRecordsThatItBeganThoughTheDataDirIsRecorded: a store open
// with a step pending records that its version began a migration wherever an
// install is armed and it is not the backup's, after serve's admission
// recorded the data dir too, and refuses where the record cannot be written.
func TestAMigrationRecordsThatItBeganThoughTheDataDirIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      State
		current string
		records bool
	}{
		{"after the admission", probationMarker(1), "0.2.4", true},
		{"before any admission", func() State { s := probationMarker(0); s.DataDirOpenedBy = ""; return s }(), "0.2.4", true},
		{"of a third version", probationMarker(1), "0.2.9", true},
		{"of the backup's version", probationMarker(1), "0.2.3", false},
		{"with no install armed", State{RejectedVersion: "0.2.5"}, "0.2.4", false},
		{"over a confirmed install", State{Status: "installed", TargetVersion: "0.2.4", PreviousVersion: "0.2.3", InstalledAt: time.Now()}, "0.2.4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustSave(t, dir, tc.st)
			res, err := DisarmBeforeMigrating(dir, tc.current)
			if err != nil || res.StoreRecorded != tc.records {
				t.Fatalf("the migrating open = %+v, %v; want it recorded: %v", res, err, tc.records)
			}
			if got := mustLoad(t, dir); (got.StoreMigratedBy == tc.current) != tc.records {
				t.Errorf("the marker = %+v, want StoreMigratedBy %s: %v", got, tc.current, tc.records)
			}
			if res.Counted || mustLoad(t, dir).FailedStarts != tc.st.FailedStarts {
				t.Error("a migration counted a start")
			}
		})
	}
	t.Run("a record that cannot be written", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		t.Cleanup(SetMarkerBarrierForTest(func(string) error { return syscall.EIO }))
		if _, err := DisarmBeforeMigrating(dir, "0.2.4"); err == nil {
			t.Fatal("a migrating open whose record's barrier fails = nil, want its refusal")
		}
	})
	t.Run("a lock that cannot be taken", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		t.Cleanup(SetStateTryLockForTest(func(*os.File) error { return syscall.EIO }))
		if _, err := DisarmBeforeMigrating(dir, "0.2.4"); err == nil || !strings.Contains(err.Error(), "began a migration") {
			t.Fatalf("a migrating open without the lock = %v, want its refusal, saying what it could not record", err)
		}
		if got := mustLoad(t, dir); got.StoreMigratedBy != "" {
			t.Errorf("the marker = %+v, want nothing written without the lock", got)
		}
	})
}

// TestTheConfirmationStampsOnlyAnInstallThatAwaitsIt: ConfirmInstall stamps
// an install of the version confirming it installed, clearing the count, the
// refusal and NotReady and keeping the rest; any other marker stays as it is,
// and one that awaits nothing asks for no lock.
func TestTheConfirmationStampsOnlyAnInstallThatAwaitsIt(t *testing.T) {
	now := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st := probationMarker(1)
	st.RollbackRefused, st.NotReady, st.StoreMigratedBy = "a refusal", true, "0.2.4"
	mustSave(t, dir, st)
	confirmed, err := ConfirmInstall(t.Context(), dir, "0.2.4", now)
	if err != nil || !confirmed {
		t.Fatalf("ConfirmInstall = %v, %v; want the install confirmed", confirmed, err)
	}
	want := st
	want.Status, want.InstalledAt = "installed", now
	want.FailedStarts, want.RollbackRefused, want.NotReady = 0, "", false
	if got := mustLoad(t, dir); got != want {
		t.Errorf("the marker = %+v, want %+v", got, want)
	}
	if a := DecideBootAction(mustLoad(t, dir), "0.2.4", now); a != BootCleanupBak {
		t.Errorf("the boot after the confirmation = %v, want BootCleanupBak", a)
	}
	for _, other := range []State{{}, probationMarker(1), {Status: "installing", TargetVersion: "0.2.5", PreviousVersion: "0.2.4"}} {
		dir := t.TempDir()
		mustSave(t, dir, other)
		t.Cleanup(SetStateTryLockForTest(func(*os.File) error { t.Error("a lock asked for a marker that awaits nothing"); return nil }))
		current := "0.2.4"
		if other.TargetVersion == "0.2.4" {
			current = "0.2.3"
		}
		if confirmed, err := ConfirmInstall(t.Context(), dir, current, now); confirmed || err != nil {
			t.Errorf("ConfirmInstall over %+v as %s = %v, %v; want nothing", other, current, confirmed, err)
		}
	}
}

// TestAConfirmationThatWaitsReadsTheMarkerAgain: a confirmation that waits
// for the install marker's lock reads the marker again once it holds it, and
// an install of a newer version recorded meanwhile stays as it is; a stop
// while it waits, and a stop before an uncontended take, write nothing.
func TestAConfirmationThatWaitsReadsTheMarkerAgain(t *testing.T) {
	dir := t.TempDir()
	mustSave(t, dir, probationMarker(1))
	held, err := LockState(t.Context(), dir)
	if err != nil || !held.Held() {
		t.Fatalf("hold the lock: %v", err)
	}
	waits := make(chan struct{}, 1)
	t.Cleanup(SetStateLockWaitingForTest(func(string) {
		select {
		case waits <- struct{}{}:
		default:
		}
	}))
	newer := State{Status: "installing", TargetVersion: "0.2.5", PreviousVersion: "0.2.4", AttemptedAt: time.Now().UTC().Truncate(time.Second), SwapStarted: true}
	answer, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		confirmed, err := ConfirmInstall(context.Background(), dir, "0.2.4", time.Now().UTC())
		if confirmed {
			err = errors.New("it confirmed")
		}
		answer <- err
	}()
	drainOnCleanup(t, held.Release, done, "the confirmation")
	select {
	case <-waits:
	case <-time.After(20 * time.Second):
		t.Fatal("the confirmation never waited for the lock")
	}
	mustSave(t, dir, newer)
	held.Release()
	if err := <-answer; err != nil {
		t.Fatalf("the confirmation over a newer install = %v, want nothing done", err)
	}
	if got := mustLoad(t, dir); got != newer {
		t.Errorf("the marker = %+v, want the newer install as recorded, %+v", got, newer)
	}

	t.Run("a stop while it waits", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		before := mustLoad(t, dir)
		held, err := LockState(t.Context(), dir)
		if err != nil || !held.Held() {
			t.Fatalf("hold the lock: %v", err)
		}
		t.Cleanup(held.Release)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(SetStateLockWaitingForTest(func(string) { cancel() }))
		if confirmed, err := ConfirmInstall(ctx, dir, "0.2.4", time.Now()); confirmed || !errors.Is(err, context.Canceled) {
			t.Fatalf("ConfirmInstall stopped in its wait = %v, %v; want the stop", confirmed, err)
		}
		if got := mustLoad(t, dir); got != before {
			t.Errorf("the marker = %+v, want it as it was", got)
		}
	})
	t.Run("an ended context with a free lock", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		before := mustLoad(t, dir)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if confirmed, err := ConfirmInstall(ctx, dir, "0.2.4", time.Now()); confirmed || !errors.Is(err, context.Canceled) {
			t.Fatalf("ConfirmInstall on an ended context = %v, %v; want the stop", confirmed, err)
		}
		if got := mustLoad(t, dir); got != before {
			t.Errorf("the marker = %+v, want it as it was", got)
		}
	})
	t.Run("a filesystem that keeps no locks", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		t.Cleanup(SetStateTryLockForTest(func(*os.File) error { return syscall.EOPNOTSUPP }))
		confirmed, err := ConfirmInstall(t.Context(), dir, "0.2.4", time.Now())
		if confirmed || !KeepsNoLocks(err) {
			t.Fatalf("ConfirmInstall where no lock is kept = %v, %v; want it refused as such", confirmed, err)
		}
	})
}

// TestATakeBackWritesOnlyTheCountItsStartWrote: the take-back of a start
// that ended cleanly removes that start's count from an install of its
// version awaiting confirmation, and nothing where the install was confirmed
// meanwhile, a newer install replaced it, or the count is not the one the
// start wrote (a count that never landed takes back no earlier start's).
func TestATakeBackWritesOnlyTheCountItsStartWrote(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      State
		counted int
		takes   bool
	}{
		{"its own count", probationMarker(2), 2, true},
		{"a count that did not land", probationMarker(1), 2, false},
		{"no count tried", probationMarker(2), 0, false},
		{"after the confirmation", State{Status: "installed", TargetVersion: "0.2.4", PreviousVersion: "0.2.3", InstalledAt: time.Now()}, 1, false},
		{"after a newer install", State{Status: "installing", TargetVersion: "0.2.5", PreviousVersion: "0.2.4", FailedStarts: 1}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustSave(t, dir, tc.st)
			before := mustLoad(t, dir)
			took, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", tc.counted)
			if err != nil || took != tc.takes {
				t.Fatalf("TakeBackFailedStart = %v, %v; want %v", took, err, tc.takes)
			}
			want := before
			if tc.takes {
				want.FailedStarts--
			}
			if got := mustLoad(t, dir); got != want {
				t.Errorf("the marker = %+v, want %+v", got, want)
			}
		})
	}
	t.Run("a filesystem that keeps no locks", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		t.Cleanup(SetStateTryLockForTest(func(*os.File) error { return syscall.EOPNOTSUPP }))
		if took, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 1); err != nil || !took {
			t.Fatalf("TakeBackFailedStart where no lock is kept = %v, %v; want it taken back, as a disarm goes on", took, err)
		}
	})
}

// TestTheAutoInstallerWaitsForTheRunningInstallsProbation: while the running
// version's own install awaits its confirmation, with no migration begun, and
// the process has been up for less than twice the window, the auto-installer
// defers, at its gate and again under the install marker's lock, and says
// why; a manual install is not held.
func TestTheAutoInstallerWaitsForTheRunningInstallsProbation(t *testing.T) {
	upd, restarts := makeAutoInstallUpdater(t, Options{AutoInstall: true, Uptime: func() time.Duration { return time.Minute }})
	dir := upd.autoInstallOpts.DataDir
	mustSave(t, dir, State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", SwapStarted: true, FailedStarts: 1})
	upd.maybeAutoInstall(context.Background())
	if why := upd.Status().DeferredReason; !strings.Contains(why, "awaits its confirmation") {
		t.Errorf("the auto-installer's deferred reason = %q, want it waiting for 0.0.1's confirmation", why)
	}
	if restarts.Load() != 0 {
		t.Error("the auto-installer restarted")
	}
	opts := *upd.autoInstallOpts
	opts.Automatic = true
	if err := upd.decideUnderLock(opts, "9.9.9"); !errors.Is(err, ErrInstallAwaitsConfirmation) {
		t.Errorf("the decision under the lock for the auto-installer = %v, want it waiting", err)
	}
	opts.Automatic = false
	if err := upd.decideUnderLock(opts, "9.9.9"); err != nil {
		t.Errorf("the decision under the lock for a manual install = %v, want none", err)
	}
}

// TestTheAutoInstallerDoesNotWaitWhereTheBackupCannotBePutBack: a migration
// of the store begun under the running install, which no crash rollback
// passes, holds nothing back.
func TestTheAutoInstallerDoesNotWaitWhereTheBackupCannotBePutBack(t *testing.T) {
	upd, _ := makeAutoInstallUpdater(t, Options{AutoInstall: true, Uptime: func() time.Duration { return time.Minute }})
	mustSave(t, upd.autoInstallOpts.DataDir, State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0",
		SwapStarted: true, FailedStarts: 1, StoreMigratedBy: "0.0.1"})
	upd.maybeAutoInstall(context.Background())
	if why := upd.Status().DeferredReason; strings.Contains(why, "awaits its confirmation") {
		t.Errorf("the auto-installer waits (%q) over a store a migration began on", why)
	}
	opts := *upd.autoInstallOpts
	opts.Automatic = true
	if err := upd.decideUnderLock(opts, "9.9.9"); err != nil {
		t.Errorf("the decision under the lock = %v, want none", err)
	}
}

// TestTheAutoInstallerDoesNotWaitForAnInstallThatNeverAnswered: past twice
// the window, an install that never answered does not wait on itself, since
// a newer release may be its fix; and an install of another version, or a
// confirmed one, holds nothing back.
func TestTheAutoInstallerDoesNotWaitForAnInstallThatNeverAnswered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uptime time.Duration
		st     State
	}{
		{"past twice the window", 2 * ProbationWindow, State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", NotReady: true, FailedStarts: 1}},
		{"an install of another version", time.Minute, State{Status: "installing", TargetVersion: "0.0.2", PreviousVersion: "0.0.1"}},
		{"a confirmed install", time.Minute, State{Status: "installed", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", InstalledAt: time.Now()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upd, _ := makeAutoInstallUpdater(t, Options{AutoInstall: true, Uptime: func() time.Duration { return tc.uptime }})
			mustSave(t, upd.autoInstallOpts.DataDir, tc.st)
			upd.maybeAutoInstall(context.Background())
			if why := upd.Status().DeferredReason; strings.Contains(why, "awaits its confirmation") {
				t.Errorf("the auto-installer waits: %q", why)
			}
		})
	}
	if why := ProbationHolds(State{Status: "installing", TargetVersion: "0.0.1"}, "0.0.1", 2*ProbationWindow-time.Second); why == "" {
		t.Error("ProbationHolds just inside twice the window holds nothing")
	}
}
