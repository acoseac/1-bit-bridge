package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAVetoGoesOnlyWithACheckThatEndedAndEarnsIt pins the rule a write's
// removal of a veto follows (earnsClear, backlog B653's fifth round: Codex's
// P2 and the Claude API's Medium on round 4), over the merge of found into
// the record as it stood: an INCOMPLETE in found never clears, whether found
// replaced the whole record or not; a found that replaced both verdicts
// clears; and a record carried over clears only where no veto stood before
// the verification began and every verdict carried over is a VERIFIED that
// found matched with a VERIFIED (a quick check over a full one), never a
// FAILED. The end-to-end cases reach the first rule only where the second
// would refuse too; a record whose database is VERIFIED beside files a check
// did not end, met by the same, is the case only the first refuses.
func TestAVetoGoesOnlyWithACheckThatEndedAndEarnsIt(t *testing.T) {
	v := func(db, files State, level CheckLevel) Verification {
		return Verification{DatabaseState: db, FilesState: files, Level: level}
	}
	for _, tc := range []struct {
		name      string
		stored    Verification
		found     Verification
		doubted   bool
		wantClear bool
	}{
		{"a full VERIFIED over a full VERIFIED", v(StateVerified, StateVerified, CheckFull), v(StateVerified, StateVerified, CheckFull), false, true},
		{"a FAILED over a VERIFIED", v(StateVerified, StateVerified, CheckQuick), v(StateFailed, StateFailed, CheckQuick), false, true},
		{"a quick VERIFIED over a full one", v(StateVerified, StateVerified, CheckFull), v(StateVerified, StateVerified, CheckQuick), false, true},
		{"a quick VERIFIED over a full one a veto stood beside", v(StateVerified, StateVerified, CheckFull), v(StateVerified, StateVerified, CheckQuick), true, false},
		{"a quick VERIFIED over a full FAILED", v(StateFailed, StateVerified, CheckFull), v(StateVerified, StateVerified, CheckQuick), false, false},
		{"an INCOMPLETE over a VERIFIED", v(StateVerified, StateVerified, CheckQuick), v(StateIncomplete, StateIncomplete, ""), false, false},
		{"files a check did not end, met by the same", v(StateVerified, StateIncomplete, CheckQuick), v(StateVerified, StateIncomplete, CheckQuick), false, false},
		{"an INCOMPLETE over a PENDING a veto stood beside", v(StatePending, StatePending, ""), v(StateIncomplete, StateIncomplete, ""), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := tc.stored
			_, database, files := mergeVerdict(&stored, tc.found)
			if got := earnsClear(tc.found, &stored, database, files, tc.doubted); got != tc.wantClear {
				t.Errorf("earnsClear = %v, want %v (database replaced %v, files %v)", got, tc.wantClear, database, files)
			}
		})
	}
}

// unchangedRecord is a snapshot folder whose record holds the VERIFIED it
// answers, a verdict that merges into it with no change, and a veto in the
// bundle beside it: the branch of a verdict's write that removes a veto over a
// record it leaves as it is (publishVerdict). A check that ended always moves
// the record's times, so that branch removes a veto only for a verdict equal
// to the record, which this writes by hand.
func unchangedRecord(t *testing.T) (dir string, v Verification) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "20260101T000000Z")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v = Verification{
		Format: verificationFormat, Snapshot: filepath.Base(dir), Level: CheckQuick,
		DatabaseState: StateVerified, FilesState: StateVerified, CheckedAt: at, FilesCheckedAt: at,
	}
	data, err := json.MarshalIndent(Manifest{Verification: &v}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vetoFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, v
}

// TestAnUnchangedRecordKeepsItsVetoOnceItsWriteIsGivenUp: a write whose merge
// leaves the record as it is removes the veto beside it only while its step
// has not been given up (backlog B653's fifth round: the give-up is asked
// before every removal of a veto, the unchanged record's included).
func TestAnUnchangedRecordKeepsItsVetoOnceItsWriteIsGivenUp(t *testing.T) {
	for _, tc := range []string{"given up", "not given up"} {
		t.Run(tc, func(t *testing.T) {
			dir, v := unchangedRecord(t)
			veto := filepath.Join(dir, vetoFileName)
			given, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc == "given up" {
				cancel()
			}
			_, written, err := publishOver(dir, v, given, nil)
			if err != nil || written {
				t.Fatalf("publishVerdict = written %v, %v; want nothing written: the test did not reach the unchanged record", written, err)
			}
			_, statErr := os.Lstat(veto)
			if stands := statErr == nil; stands != (tc == "given up") {
				t.Errorf("%s, the veto stands: %v", tc, stands)
			}
		})
	}
}
