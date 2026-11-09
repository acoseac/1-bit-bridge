package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	for _, c := range doctor.BackupChecks(st, nil, backup.ProbeBackupsLock(root), true, time.Now()) {
		switch {
		case c.Name == "backup-newest" && strings.Contains(c.Summary, filepath.Base(dir)) && strings.Contains(c.Summary, "database VERIFIED"):
			out = append(out, "the doctor's newest line reads it verified")
		case c.Name == "backup-verified" && strings.Contains(c.Summary, filepath.Base(dir)):
			out = append(out, "the doctor's verified line names it")
		}
	}
	return out
}

// TestAVerifiedRecordLeftForAFailedOneIsVetoedWhenItsWriteFails is backlog
// B653's second round's red for a VERIFIED a later check did not find: a
// snapshot verified once, whose database and files both fail when it is
// checked again, keeps its VERIFIED record wherever that check's write
// fails before its rename, and no reader (the pruner, the doctor, the
// console) reads that VERIFIED after the error: the veto goes in place
// before the staging, in the bundle, or beside it where the bundle takes no
// file, and stays where its owner cannot be given. Where neither veto can be
// made, the guarantee is the narrowed one, as built: the write goes on, and
// lands the FAILED; only where it then fails too does the record keep its
// VERIFIED, the error saying so (ErrRecordKept). Each failed write leaves
// the record to the next check, which writes it and lets the veto go. Red
// on the round before, which put a veto only over a record that would hold
// a VERIFIED, so a failed write left the old one readable everywhere.
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
	always := func(string) error { return injected }
	for _, tc := range []struct {
		name string
		// failures, given the bundle's folder: the staging's, the rename's,
		// the veto's (nil each for none).
		stage, rename, veto func(dir string) func(string) error
		// ownerRefused refuses the veto its owner (root over a service's
		// snapshot whose owner cannot be given, root_squash).
		ownerRefused bool
		// kept: the write's error says the record keeps its VERIFIED.
		kept bool
		// lands: the write lands, the record FAILED.
		lands bool
		// beside: the veto stands beside the bundle.
		beside bool
	}{
		{name: "its staging fails", stage: func(string) func(string) error { return always }},
		{name: "its rename fails", rename: func(string) func(string) error { return always }},
		{name: "no veto can be made in the bundle", stage: func(string) func(string) error { return always }, veto: inBundle, beside: true},
		{name: "the veto's owner cannot be given", stage: func(string) func(string) error { return always }, ownerRefused: true},
		{name: "no veto can be made and the write lands", veto: func(string) func(string) error { return always }, lands: true},
		{name: "no veto can be made and the write fails", stage: func(string) func(string) error { return always },
			veto: func(string) func(string) error { return always }, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ownerRefused && runtime.GOOS == "windows" {
				t.Skip("a file takes its directory's ACL on Windows; no owner is given there")
			}
			src := storeWithTracks(t, 20)
			anchor := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			dir := verifiedSnapshot(t, src)
			flipLastByte(t, dir, backup.ManifestDBFileName)
			flipLastByte(t, dir, "tokens.json")
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
			restore()
			if res.Found.DatabaseState != backup.StateFailed || res.Found.FilesState != backup.StateFailed {
				t.Fatalf("the check found %s/%s, want FAILED/FAILED: the test did not reach the case", res.Found.DatabaseState, res.Found.FilesState)
			}
			switch {
			case tc.lands:
				if err != nil {
					t.Fatalf("a write that lands answered %v", err)
				}
				requireVerdict(t, "the record once written", listed(t, dir), backup.StateFailed, backup.ReasonHash, backup.StateFailed, backup.ReasonHash)
			case tc.kept:
				if !errors.Is(err, backup.ErrRecordKept) {
					t.Fatalf("a write that failed with no veto answered %v, want ErrRecordKept", err)
				}
				// The narrowed claim, as built: the one case a reader still
				// reads the VERIFIED, and the error says so.
				if got := readersVerified(t, dir); len(got) == 0 {
					t.Error("ErrRecordKept answered while no reader reads the VERIFIED it says the record keeps")
				}
			default:
				if err == nil || errors.Is(err, backup.ErrRecordKept) {
					t.Fatalf("a write that failed behind its veto answered %v", err)
				}
				if got := readersVerified(t, dir); len(got) > 0 {
					t.Errorf("after the error, %s", strings.Join(got, "; "))
				}
				if m := readManifestT(t, dir); m.Verification.DatabaseState != backup.StateVerified {
					t.Fatalf("the record holds %s: the write was not refused before its rename", m.Verification.DatabaseState)
				}
				if beside := pathExists(t, backup.RootVetoPath(dir)); beside != tc.beside {
					t.Errorf("the veto beside the bundle there %v, want %v", beside, tc.beside)
				}
				if _, err := backup.Prune(filepath.Dir(dir), 1); err != nil {
					t.Fatal(err)
				}
				if !pathExists(t, anchor) {
					t.Error("the prune let the older verified snapshot go for one whose VERIFIED stands behind a veto")
				}
				// The next check writes the record and lets the veto go.
				if _, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{}); err != nil {
					t.Fatalf("the next check: %v", err)
				}
				requireVerdict(t, "the record after the next check", listed(t, dir), backup.StateFailed, backup.ReasonHash, backup.StateFailed, backup.ReasonHash)
				if pathExists(t, filepath.Join(dir, backup.VetoFileName)) || pathExists(t, backup.RootVetoPath(dir)) {
					t.Error("the next check's write left a veto standing")
				}
			}
		})
	}
}
