package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// writeSnapshotDir builds the minimal directory shape backup.List
// recognises: a timestamped dir containing a manifest.json with the
// schema version + CreatedAt the function reads, and no verification
// record: a snapshot written before snapshots were verified, which reads
// UNVERIFIED (backlog B653). Real Snapshot writes many more fields, but List
// only consults what `readManifest` requires for ordering / filtering.
func writeSnapshotDir(t *testing.T, root string, createdAt time.Time) string {
	return writeSnapshotDirAt(t, root, createdAt, nil)
}

// writeVerifiedSnapshotDir is writeSnapshotDir with a record whose two
// verdicts are VERIFIED.
func writeVerifiedSnapshotDir(t *testing.T, root string, createdAt time.Time) string {
	return writeSnapshotDirAt(t, root, createdAt, &backup.Verification{
		Format: 1, DatabaseState: backup.StateVerified, FilesState: backup.StateVerified,
		Level: backup.CheckQuick, CheckedAt: createdAt,
	})
}

// writeSnapshotDirAt is writeSnapshotDir with the record rec (nil: none).
func writeSnapshotDirAt(t *testing.T, root string, createdAt time.Time, rec *backup.Verification) string {
	t.Helper()
	dir := filepath.Join(root, createdAt.Format("2006-01-02T15-04-05Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := backup.Manifest{
		SchemaVersion: backup.SchemaVersion,
		CreatedAt:     createdAt,
		Files:         []string{"bridge.db"},
		Verification:  rec,
	}
	if rec != nil {
		m.Baselines = []backup.Baseline{{Name: "bridge.db"}}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startupPassOver is the boot pass's decision over the snapshots under root
// at now, as the ticker reads them (backup.ReadStatus, decideStartupPass).
func startupPassOver(t *testing.T, root string, now time.Time) startupPass {
	t.Helper()
	st, err := backup.ReadStatus(t.Context(), root)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	return decideStartupPass(st, now, startupBackupSkipThreshold)
}

// TestTheStartupPassCapturesWhereThereIsNoSnapshot: the very first start,
// its backups folder not made yet or empty, takes a baseline.
func TestTheStartupPassCapturesWhereThereIsNoSnapshot(t *testing.T) {
	for _, root := range []string{t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist")} {
		if pass := startupPassOver(t, root, time.Now().UTC()); pass.action != startupCapture {
			t.Errorf("over %s the startup pass is %v, want a capture: the very first start MUST write a baseline", filepath.Base(root), pass.action)
		}
	}
}

// TestTheStartupPassSkipsForARecentVerifiedSnapshot: a snapshot from the
// last day whose database is VERIFIED is a recovery point recent enough,
// the newest of them named, an older one never shadowing a newer one.
func TestTheStartupPassSkipsForARecentVerifiedSnapshot(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeVerifiedSnapshotDir(t, root, now.Add(-7*24*time.Hour))
	writeVerifiedSnapshotDir(t, root, now.Add(-2*time.Hour))
	pass := startupPassOver(t, root, now)
	if pass.action != startupSkipVerified {
		t.Fatalf("the startup pass is %v, want the skip for a recent verified snapshot", pass.action)
	}
	if age := now.Sub(pass.snapshot.CreatedAt); age > 3*time.Hour {
		t.Errorf("the skip names a snapshot %v old, want the one 2h old", age)
	}
}

// TestTheStartupPassCapturesPastTheThreshold: a verified snapshot older
// than the last day, or exactly a day old (a strict comparison: CodeRabbit
// on PR #101, so a constant-cadence ticker's perfect edge alignment never
// reads a snapshot taken exactly 24h ago as recent), is no recent recovery
// point, and the boot takes one.
func TestTheStartupPassCapturesPastTheThreshold(t *testing.T) {
	now := time.Now().UTC()
	for _, age := range []time.Duration{26 * time.Hour, 24 * time.Hour} {
		root := t.TempDir()
		writeVerifiedSnapshotDir(t, root, now.Add(-age))
		if pass := startupPassOver(t, root, now); pass.action != startupCapture {
			t.Errorf("a verified snapshot %v old: the startup pass is %v, want a capture", age, pass.action)
		}
	}
}

// TestTheStartupPassFollowsTheNewestRecentSnapshot is the startup rule's
// table (decideStartupPass, backlog B653 and its second round), each row a
// backups folder whose newest snapshot from the last day is of a kind: a
// recent database VERIFIED skips the capture, whatever its files say and
// whatever newer snapshot stands beside it; otherwise a newest recent one
// whose check is owed skips it too, waiting where a live owner holds that
// snapshot and verifying it in place where none does; and a newest recent
// one that FAILED, was written before verification or holds a record this
// bridge cannot read is no reason to skip. Red on main (the round before),
// which captured beside every recent snapshot that was not verified, a
// restart loop's dead captures included.
func TestTheStartupPassFollowsTheNewestRecentSnapshot(t *testing.T) {
	now := time.Now().UTC()
	rec := func(db backup.State, files backup.State, reasons ...backup.Reason) *backup.Verification {
		return &backup.Verification{Format: 1, DatabaseState: db, DatabaseReasons: reasons, FilesState: files}
	}
	pending := &backup.Verification{Format: 1, DatabaseState: backup.StatePending, FilesState: backup.StatePending, PendingSince: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name  string
		rec   *backup.Verification
		owner string // "live", "gone", or "" for no owner lock
		older *backup.Verification
		want  startupAction
	}{
		{name: "written before verification", rec: nil, want: startupCapture},
		{name: "its database failed", rec: rec(backup.StateFailed, backup.StateVerified, backup.ReasonQuickCheck), want: startupCapture},
		{name: "a record this bridge cannot read", rec: &backup.Verification{Format: 99, DatabaseState: backup.StateVerified,
			FilesState: backup.StateVerified}, want: startupCapture},
		{name: "its check did not finish", rec: rec(backup.StateIncomplete, backup.StateIncomplete, backup.ReasonCancelled),
			want: startupVerifyInPlace},
		{name: "its check is pending with no owner to ask", rec: pending, want: startupVerifyInPlace},
		{name: "its capture died before its check", rec: pending, owner: "gone", want: startupVerifyInPlace},
		{name: "its check is queued or under way", rec: pending, owner: "live", want: startupSkipOwned},
		{name: "its database verified and a file not", rec: rec(backup.StateVerified, backup.StateFailed),
			want: startupSkipVerified},
		{name: "a newer failed one beside an older verified one", rec: rec(backup.StateFailed, backup.StateVerified,
			backup.ReasonHash), older: rec(backup.StateVerified, backup.StateVerified), want: startupSkipVerified},
		{name: "a newer pending one beside an older verified one", rec: pending, older: rec(backup.StateVerified,
			backup.StateVerified), want: startupSkipVerified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.older != nil {
				writeSnapshotDirAt(t, root, now.Add(-5*time.Hour), tc.older)
			}
			dir := writeSnapshotDirAt(t, root, now.Add(-time.Hour), tc.rec)
			switch tc.owner {
			case "live":
				release, err := backup.TakeOwnerForTest(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
			case "gone":
				release, err := backup.TakeOwnerForTest(dir)
				if err != nil {
					t.Fatal(err)
				}
				release()
			}
			pass := startupPassOver(t, root, now)
			if pass.action != tc.want {
				t.Errorf("the startup pass is %v, want %v", pass.action, tc.want)
			}
			if tc.want == startupVerifyInPlace || tc.want == startupSkipOwned {
				if pass.snapshot.DirName != filepath.Base(dir) {
					t.Errorf("the pass is about %q, want the newest recent snapshot %q", pass.snapshot.DirName, filepath.Base(dir))
				}
			}
		})
	}
}

// TestBackupCmdReapsOrphansWithRetentionDisabled pins that `--keep 0`
// disables the KEEP-POLICY, not the crash-orphan sweep.
//
// The command used to wrap its whole prune in `if *keep > 0`, so an
// operator who turned retention off also silently turned off orphan
// reclamation — and orphans are the one thing nothing else in the backup
// package can ever remove (no manifest.json means `List` skips them, so the
// keep-policy could never select them either). Each one carries a near-full
// bridge.db copy, so they accumulate unbounded across hard crashes.
//
// Driven through the public `backupCmd` entry point, because the defect was
// in the CALL SITE's guard rather than in backup.Prune — which reaped at
// keep<=0 all along, and was simply never invoked.
func TestBackupCmdReapsOrphansWithRetentionDisabled(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	cfgYAML := "dataDir: " + yamlStr(dir) + "\nlibraryRoots:\n  - " + yamlStr(lib) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// A crash orphan: a well-aged snapshot dir carrying a partial DB copy
	// and no manifest.
	backupsRoot := filepath.Join(dir, backup.BackupsDirName)
	orphan := filepath.Join(backupsRoot, "2020-01-01T00-00-00Z")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, backup.ManifestDBFileName), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if rc := backupCmd([]string{"--config", cfgPath, "--keep", "0"}, &stdout, &stderr); rc != 0 {
		t.Fatalf("backupCmd rc=%d, stderr=%s", rc, stderr.String())
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("crash orphan survived `--keep 0`; nothing else can ever reclaim it (stat err = %v)", err)
	}
}
