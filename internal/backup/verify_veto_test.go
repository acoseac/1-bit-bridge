package backup_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// flipLastByte changes the last byte of the bundle's file name.
func flipLastByte(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	data := readBytes(t, p)
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readersVerified reports, for the snapshot in dir, which readers read a
// VERIFIED: the pruner's reading (List), the doctor's lines, and ReadStatus,
// the console's reading, as the newest database-verified snapshot. Each
// names its reader.
func readersVerified(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if v := listed(t, dir); v.DatabaseState == backup.StateVerified || v.FilesState == backup.StateVerified {
		out = append(out, "the pruner reads "+string(v.DatabaseState)+"/"+string(v.FilesState))
	}
	root := filepath.Dir(dir)
	st, err := backup.ReadStatus(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if nv, ok := st.NewestVerified(); ok && nv.DirName == filepath.Base(dir) {
		out = append(out, "the console's reading takes it for the newest verified")
	}
	for _, c := range doctor.BackupChecks(st, nil, backup.ProbeBackupsLock(root), nil, true, time.Now()) {
		switch {
		case c.Name == "backup-newest" && strings.Contains(c.Summary, filepath.Base(dir)) &&
			(strings.Contains(c.Summary, "database VERIFIED") || strings.Contains(c.Summary, "files VERIFIED")):
			out = append(out, "the doctor's newest line reads it verified")
		case c.Name == "backup-verified" && strings.Contains(c.Summary, filepath.Base(dir)):
			out = append(out, "the doctor's verified line names it")
		}
	}
	return out
}

// checksRun counts the checks that reach their thread from now on
// (SetOnLowPriorityForTest), the seam put back at the test's end, once every
// verification it started has returned.
func checksRun(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	t.Cleanup(backup.SetOnLowPriorityForTest(func() { n.Add(1) }))
	return &n
}

// TestAVerifiedRecordLeftForAFailedOneIsVetoedWhenItsWriteFails is backlog
// B653's red for a VERIFIED a later check did not find, its second and third
// rounds: a snapshot verified once, whose database (and in most cases its
// files) fail when it is checked again, is set aside before the check, its
// veto in the bundle, or beside it where the bundle takes no file, kept where
// its owner cannot be given, its folder synced; so whatever the write of what
// the check found does after, no reader (the pruner, the doctor, the console)
// reads a VERIFIED the check did not find: a write refused before its rename
// leaves the old record behind the veto, and one whose rename landed and whose
// folder did not sync leaves what was found, and, that rename lost to a
// crash, the old record behind the veto still. Each is a check that completed
// and whose record was not written, never the refusal of a check
// (ErrRecheckRefused), and leaves the record to the next check, which writes
// it and lets the veto go. Mixed verdicts, the database alone damaged and the
// files VERIFIED (Codex on round 2), take the same path, and a write that
// lands records them, the database FAILED. Every case here finds a veto to be
// had: where none is, the check is refused (the next test), which round 2,
// writing over the VERIFIED with none, was red on. Where no directory can be
// synced no veto precedes the check and no new verdict is recorded
// (TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing), so it
// skips there.
func TestAVerifiedRecordLeftForAFailedOneIsVetoedWhenItsWriteFails(t *testing.T) {
	injected := errors.New("injected: the write failed here")
	inBundle := func(dir string) func(string) error {
		return func(p string) error {
			if filepath.Dir(p) == dir {
				return injected
			}
			return nil
		}
	}
	always := func(string) func(string) error { return func(string) error { return injected } }
	for _, tc := range []struct {
		name string
		// failures, given the bundle's folder: the staging's, the rename's,
		// the veto's (nil each for none).
		stage, rename, veto func(dir string) func(string) error
		// unsynced fails the verdict's barrier once its rename has landed.
		unsynced bool
		// crash puts the record the rename replaced back, as a crash that
		// loses an unsynced rename does.
		crash bool
		// ownerRefused refuses the veto its owner (root over a service's
		// snapshot whose owner cannot be given, root_squash).
		ownerRefused bool
		// filesKept leaves the files as they were: mixed verdicts.
		filesKept bool
		// lands: the write lands, behind the veto that goes once it is durable.
		lands bool
		// beside: the veto stands beside the bundle.
		beside bool
	}{
		{name: "its staging fails", stage: always},
		{name: "its rename fails", rename: always},
		{name: "no veto can be made in the bundle", stage: always, veto: inBundle, beside: true},
		{name: "the veto's owner cannot be given", stage: always, ownerRefused: true},
		{name: "its folder does not sync once its rename has landed", unsynced: true},
		{name: "its rename is lost to a crash", unsynced: true, crash: true},
		{name: "mixed verdicts whose staging fails", stage: always, filesKept: true},
		{name: "mixed verdicts whose rename is lost to a crash", unsynced: true, crash: true, filesKept: true},
		{name: "mixed verdicts whose write lands", filesKept: true, lands: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireDirectorySync(t)
			if tc.ownerRefused && runtime.GOOS == "windows" {
				t.Skip("a file takes its directory's ACL on Windows; no owner is given there")
			}
			src := storeWithTracks(t, 20)
			anchor := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			dir := verifiedSnapshot(t, src)
			mpath := filepath.Join(dir, backup.ManifestFile)
			before := readBytes(t, mpath)
			flipLastByte(t, dir, backup.ManifestDBFileName)
			wantFiles, wantFilesWhy := backup.StateFailed, backup.ReasonHash
			if tc.filesKept {
				wantFiles, wantFilesWhy = backup.StateVerified, ""
			} else {
				flipLastByte(t, dir, "tokens.json")
			}
			var stage, rename, veto func(string) error
			if tc.stage != nil {
				stage = tc.stage(dir)
			}
			if tc.rename != nil {
				rename = tc.rename(dir)
			}
			if tc.veto != nil {
				veto = tc.veto(dir)
			}
			restore := backup.SetVerdictWriteFailuresForTest(stage, rename, veto)
			if tc.unsynced {
				restoreSync := backup.SetSyncVerdictForTest(func(string) error { return injected })
				prev := restore
				restore = func() { restoreSync(); prev() }
			}
			if tc.ownerRefused {
				// The veto takes the owner of the folder it is made in, which
				// is what its owner's give names; refused, as root_squash
				// refuses root.
				bundle, err := os.Stat(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(fsutil.SimulateRootRefusingForTest(4242, 4243, func(dst string) bool {
					info, err := os.Stat(dst)
					return err == nil && os.SameFile(info, bundle)
				}, syscall.EPERM))
			}
			res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
			awaitClean(t)
			restore()
			if res.Found.DatabaseState != backup.StateFailed || res.Found.FilesState != wantFiles {
				t.Fatalf("the check found %s/%s, want FAILED/%s: the test did not reach the case", res.Found.DatabaseState, res.Found.FilesState, wantFiles)
			}
			if errors.Is(err, backup.ErrRecheckRefused) {
				t.Fatalf("a check that ran answered the refusal of one: %v", err)
			}
			switch {
			case tc.lands:
				if err != nil {
					t.Fatalf("a write that lands answered %v", err)
				}
				requireVerdict(t, "the record once written", listed(t, dir), backup.StateFailed, backup.ReasonHash, wantFiles, wantFilesWhy)
				if pathExists(t, filepath.Join(dir, backup.VetoFileName)) || pathExists(t, backup.RootVetoPath(dir)) {
					t.Error("a write that landed and synced left its veto")
				}
				return
			case tc.unsynced:
				if !errors.Is(err, backup.ErrVerdictNotDurable) {
					t.Fatalf("a write whose barrier failed answered %v, want ErrVerdictNotDurable", err)
				}
				if m := readManifestT(t, dir); m.Verification.DatabaseState != backup.StateFailed {
					t.Fatalf("the record holds %s: the rename did not land", m.Verification.DatabaseState)
				}
				if tc.crash {
					if err := os.WriteFile(mpath, before, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			default:
				if err == nil {
					t.Fatal("a write that failed answered no error")
				}
				if m := readManifestT(t, dir); m.Verification.DatabaseState != backup.StateVerified {
					t.Fatalf("the record holds %s: the write was not refused before its rename", m.Verification.DatabaseState)
				}
			}
			if got := readersVerified(t, dir); len(got) > 0 {
				t.Errorf("after the error, %s", strings.Join(got, "; "))
			}
			if beside := pathExists(t, backup.RootVetoPath(dir)); beside != tc.beside {
				t.Errorf("the veto beside the bundle there %v, want %v", beside, tc.beside)
			}
			if inBundle := pathExists(t, filepath.Join(dir, backup.VetoFileName)); inBundle == tc.beside {
				t.Errorf("the veto in the bundle there %v, want %v", inBundle, !tc.beside)
			}
			if _, err := backup.Prune(filepath.Dir(dir), 1); err != nil {
				t.Fatal(err)
			}
			if !pathExists(t, anchor) {
				t.Error("the prune let the older verified snapshot go for one no reader may read verified")
			}
			// The next check writes the record and lets the veto go.
			if _, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{}); err != nil {
				t.Fatalf("the next check: %v", err)
			}
			requireVerdict(t, "the record after the next check", listed(t, dir), backup.StateFailed, backup.ReasonHash, wantFiles, wantFilesWhy)
			if pathExists(t, filepath.Join(dir, backup.VetoFileName)) || pathExists(t, backup.RootVetoPath(dir)) {
				t.Error("the next check's write left a veto standing")
			}
		})
	}
}

// TestARecheckWithNoDurableVetoIsRefusedBeforeItsCheck is backlog B653's
// third round's red for a check of a snapshot whose record holds a VERIFIED
// (the review of its brief: durable invalidation before the recheck begins,
// or no recheck): where no veto can be put in place durably, in the bundle or
// beside it (none can be made; one is made and its folder does not sync; one
// an earlier write left does not sync), the verification checks nothing,
// answers ErrRecheckRefused, writes nothing, and leaves the record as every
// reader read it before: a veto it made goes again, and one an earlier write
// left stays. Mixed verdicts, the database alone damaged (Codex on round 2),
// are refused alike. Red at round 2, which checked such a snapshot, and wrote
// over its VERIFIED with no veto, or returned before its rename with the
// database read VERIFIED. Where no directory can be synced no veto precedes
// the check, which runs and records no new verdict
// (TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing), so it
// skips there.
func TestARecheckWithNoDurableVetoIsRefusedBeforeItsCheck(t *testing.T) {
	injected := errors.New("injected")
	always := func(string) error { return injected }
	for _, tc := range []struct {
		name string
		// veto fails the making of a veto; sync its barrier (nil each: none).
		veto, sync func(string) error
		// left: a veto an earlier write left in the bundle.
		left bool
		// filesKept damages the database alone: mixed verdicts.
		filesKept bool
	}{
		{name: "no veto can be made", veto: always},
		{name: "no veto can be made over mixed verdicts", veto: always, filesKept: true},
		{name: "a veto made does not sync", sync: always},
		{name: "a veto an earlier write left does not sync", sync: always, left: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireDirectorySync(t)
			src := storeWithTracks(t, 20)
			dir := verifiedSnapshot(t, src)
			flipLastByte(t, dir, backup.ManifestDBFileName)
			if !tc.filesKept {
				flipLastByte(t, dir, "tokens.json")
			}
			if tc.left {
				if err := os.WriteFile(filepath.Join(dir, backup.VetoFileName), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mpath := filepath.Join(dir, backup.ManifestFile)
			before := readBytes(t, mpath)
			readBefore := listed(t, dir)
			checks := checksRun(t)
			restore := backup.SetVerdictWriteFailuresForTest(nil, nil, tc.veto)
			if tc.sync != nil {
				restoreSync := backup.SetSyncVetoForTest(tc.sync)
				prev := restore
				restore = func() { restoreSync(); prev() }
			}
			res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
			awaitClean(t)
			restore()
			if !errors.Is(err, backup.ErrRecheckRefused) {
				t.Fatalf("a recheck with no durable veto answered %v, want ErrRecheckRefused", err)
			}
			if n := checks.Load(); n != 0 || res.Found.DatabaseState != "" {
				t.Errorf("a refused recheck checked: %d checks, found %s/%s", n, res.Found.DatabaseState, res.Found.FilesState)
			}
			if got := readBytes(t, mpath); !bytes.Equal(got, before) {
				t.Error("a refused recheck wrote the record")
			}
			if inBundle := pathExists(t, filepath.Join(dir, backup.VetoFileName)); inBundle != tc.left {
				t.Errorf("a veto in the bundle there %v, want %v: the refusal changed what readers read", inBundle, tc.left)
			}
			if pathExists(t, backup.RootVetoPath(dir)) {
				t.Error("the refusal left the veto it made beside the bundle")
			}
			after := listed(t, dir)
			requireVerdict(t, "the record a refused recheck left", after, readBefore.DatabaseState, firstReason(readBefore.DatabaseReasons),
				readBefore.FilesState, firstReason(readBefore.FilesReasons))
			if res.Record.DatabaseState != after.DatabaseState || res.Record.FilesState != after.FilesState {
				t.Errorf("the refusal answered the record %s/%s, which every reader reads %s/%s",
					res.Record.DatabaseState, res.Record.FilesState, after.DatabaseState, after.FilesState)
			}
		})
	}
}

// TestANewVerifiedIsNotWrittenWhereNoVetoCanBeHad: a fresh snapshot's check
// that finds it VERIFIED, where no veto can be made in the bundle or beside
// it, writes nothing (a write of a VERIFIED is made behind its veto or not at
// all, backlog B653's third round), its error says so, and once its owner is
// gone no reader reads it verified.
func TestANewVerifiedIsNotWrittenWhereNoVetoCanBeHad(t *testing.T) {
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	mpath := filepath.Join(c.Dir, backup.ManifestFile)
	before := readBytes(t, mpath)
	restore := backup.SetVerdictWriteFailuresForTest(nil, nil, func(string) error { return errors.New("injected") })
	res := backup.FinishCapture(t.Context(), c, nil, backup.VerifyOptions{})
	awaitClean(t)
	restore()
	if res.Verify.Found.DatabaseState != backup.StateVerified {
		t.Fatalf("the check found %s, want VERIFIED: the test did not reach the case", res.Verify.Found.DatabaseState)
	}
	if res.VerifyErr == nil || res.Verify.Recorded {
		t.Errorf("a VERIFIED with no veto to be had answered %v, recorded %v; want it not written", res.VerifyErr, res.Verify.Recorded)
	}
	if got := readBytes(t, mpath); !bytes.Equal(got, before) {
		t.Error("a VERIFIED with no veto to be had was written")
	}
	if got := readersVerified(t, c.Dir); len(got) > 0 {
		t.Errorf("once its owner is gone, %s", strings.Join(got, "; "))
	}
}

// TestARecordCopiedIntoAnotherFolderReadsUnverified: a snapshot's record
// names the folder it was written for (backlog B653's third round, the review
// of its brief: a record bound to another snapshot is not trusted), so a
// verified snapshot's folder copied whole under another name reads UNVERIFIED
// (other-snapshot) to every reader, the original VERIFIED as before, and the
// prune does not take the copy for the newest verified snapshot.
func TestARecordCopiedIntoAnotherFolderReadsUnverified(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir := verifiedSnapshot(t, src)
	root := filepath.Dir(dir)
	copied := filepath.Join(root, "2999-01-01T00-00-00Z")
	if err := os.MkdirAll(copied, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := os.WriteFile(filepath.Join(copied, e.Name()), readBytes(t, filepath.Join(dir, e.Name())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	requireVerdict(t, "the copy", listed(t, copied), backup.StateUnverified, backup.ReasonOtherSnapshot,
		backup.StateUnverified, backup.ReasonOtherSnapshot)
	requireVerdict(t, "the original", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
	st, err := backup.ReadStatus(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if nv, ok := st.NewestVerified(); !ok || nv.DirName != filepath.Base(dir) {
		t.Errorf("the newest verified snapshot is %q, want the original %q", nv.DirName, filepath.Base(dir))
	}
	if got := readersVerified(t, copied); len(got) > 0 {
		t.Errorf("the copy: %s", strings.Join(got, "; "))
	}
}

// firstReason is reasons' first, or none.
func firstReason(reasons []backup.Reason) backup.Reason {
	if len(reasons) == 0 {
		return ""
	}
	return reasons[0]
}
