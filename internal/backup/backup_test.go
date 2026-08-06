package backup_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	_ "modernc.org/sqlite"
)

// writeSnapshotManifest hand-writes a minimal valid manifest.json into
// dir (marking it a "completed" snapshot for List / ReapOrphans). Uses
// the exported Manifest shape so the schema tracks backup.SchemaVersion.
func writeSnapshotManifest(t *testing.T, dir string, files ...string) {
	t.Helper()
	data, err := json.Marshal(backup.Manifest{
		SchemaVersion: backup.SchemaVersion,
		Files:         files,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// Snapshot must capture every present input file, write a manifest
// describing them, and produce a directory under
// <dataDir>/backups/<timestamp>/.
func TestSnapshotCapturesAllProvidedFiles(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)

	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Dir layout
	if !pathExists(t, dst) {
		t.Fatalf("snapshot dir %q does not exist", dst)
	}
	if !pathExists(t, filepath.Join(dst, "bridge.db")) {
		t.Errorf("snapshot missing bridge.db")
	}
	if !pathExists(t, filepath.Join(dst, "tokens.json")) {
		t.Errorf("snapshot missing tokens.json")
	}
	if !pathExists(t, filepath.Join(dst, "server.crt")) {
		t.Errorf("snapshot missing server.crt")
	}
	if !pathExists(t, filepath.Join(dst, "server.key")) {
		t.Errorf("snapshot missing server.key")
	}
	if !pathExists(t, filepath.Join(dst, "bridge.yaml")) {
		t.Errorf("snapshot missing bridge.yaml")
	}
	if !pathExists(t, filepath.Join(dst, backup.ManifestFile)) {
		t.Fatalf("snapshot missing manifest.json")
	}

	// Permissions: snapshot dir 0700, files 0600.
	//
	// POSIX bits are ADVISORY on Windows — Go synthesises 0777/0666 from
	// the read-only attribute, and the real protection is the NTFS ACL on
	// the per-user %LOCALAPPDATA% profile (CLAUDE.md, PR #63). The
	// snapshot still CONTAINS the right files there, which is what the
	// rest of this test checks; only the numeric mode is unassertable.
	if runtime.GOOS != "windows" {
		if got := mode(t, dst); got&0o777 != 0o700 {
			t.Errorf("snapshot dir mode = %o, want 0700", got&0o777)
		}
		for _, name := range []string{"bridge.db", "tokens.json", "server.crt", "server.key"} {
			if got := mode(t, filepath.Join(dst, name)); got&0o777 != 0o600 {
				t.Errorf("snapshot file %s mode = %o, want 0600", name, got&0o777)
			}
		}
	}
}

// VACUUM INTO must produce a working SQLite database that round-trips
// the data we put in the live db. This is what makes a restore
// useful — a snapshot of a corrupt clone is only as good as the
// fidelity of the copy.
func TestSnapshotPreservesManifestDBContents(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)

	// Write a recognizable row to the live db so we can read it
	// back out of the snapshot.
	dbPath := src.ManifestDB
	live, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("open live db: %v", err)
	}
	if _, err := live.Exec(`CREATE TABLE bk_test (k TEXT PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	if _, err := live.Exec(`INSERT INTO bk_test VALUES ('canary','round-trip-me')`); err != nil {
		t.Fatalf("insert canary: %v", err)
	}
	live.Close()

	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	snap, err := sql.Open("sqlite", dsn.File(filepath.Join(dst, "bridge.db"), "mode=ro"))
	if err != nil {
		t.Fatalf("open snapshot db: %v", err)
	}
	defer snap.Close()
	var v string
	if err := snap.QueryRow(`SELECT v FROM bk_test WHERE k='canary'`).Scan(&v); err != nil {
		t.Fatalf("read canary from snapshot: %v", err)
	}
	if v != "round-trip-me" {
		t.Errorf("snapshot lost canary row: got %q", v)
	}
}

// A failed (here: cancelled) Snapshot must reap its partial snapshot
// dir. List skips manifest-less dirs, so a leaked partial — which
// contains a full DB copy — would be invisible to Prune forever and
// accumulate unbounded across failures.
func TestSnapshotFailureReapsPartialDir(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // VACUUM INTO observes the cancellation and fails

	if _, err := backup.Snapshot(ctx, src); err == nil {
		t.Fatalf("Snapshot with cancelled ctx should fail")
	}

	backupsRoot := filepath.Join(dataDir, backup.BackupsDirName)
	entries, err := os.ReadDir(backupsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return // root never created — also fine, nothing leaked
		}
		t.Fatalf("read backups root: %v", err)
	}
	for _, e := range entries {
		t.Errorf("failed snapshot leaked %q under backups root", e.Name())
	}
}

// Restoring into fresh paths must reproduce the live state byte-for-
// byte for the simple files (tokens.json, cert, key, yaml). The
// db gets its own end-to-end test.
func TestRestoreRoundTripsSimpleFiles(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	originalTokens := readBytes(t, src.TokensJSON)
	originalCert := readBytes(t, src.ServerCert)

	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Wipe the live state and restore.
	for _, p := range []string{src.TokensJSON, src.ServerCert, src.ServerKey} {
		if err := os.Remove(p); err != nil {
			t.Fatalf("wipe %s: %v", p, err)
		}
	}
	if err := backup.Restore(dst, backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		ServerCert: src.ServerCert,
		ServerKey:  src.ServerKey,
		BridgeYAML: src.BridgeYAML,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := readBytes(t, src.TokensJSON); string(got) != string(originalTokens) {
		t.Errorf("restored tokens.json differs from original")
	}
	if got := readBytes(t, src.ServerCert); string(got) != string(originalCert) {
		t.Errorf("restored server.crt differs from original")
	}
}

// Restore must delete any pre-restore -wal/-shm sidecars when it
// replaces the manifest DB. Snapshot writes a clean VACUUM INTO db
// (no WAL in the bundle); leaving the old WAL on disk would have
// SQLite replay stale frames onto the restored file and corrupt it.
func TestRestoreRemovesStaleWALSHM(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)

	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Simulate a pre-restore WAL/SHM pair belonging to the live DB.
	walPath := src.ManifestDB + "-wal"
	shmPath := src.ManifestDB + "-shm"
	if err := os.WriteFile(walPath, []byte("stale-wal-frames"), 0o600); err != nil {
		t.Fatalf("write stale wal: %v", err)
	}
	if err := os.WriteFile(shmPath, []byte("stale-shm"), 0o600); err != nil {
		t.Fatalf("write stale shm: %v", err)
	}

	if err := backup.Restore(dst, backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		ServerCert: src.ServerCert,
		ServerKey:  src.ServerKey,
		BridgeYAML: src.BridgeYAML,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if pathExists(t, walPath) {
		t.Errorf("stale %s survived Restore — SQLite would replay it onto the restored DB", walPath)
	}
	if pathExists(t, shmPath) {
		t.Errorf("stale %s survived Restore", shmPath)
	}

	// The restored main DB must match the snapshot's clean copy.
	snapDB := readBytes(t, filepath.Join(dst, backup.ManifestDBFileName))
	if got := readBytes(t, src.ManifestDB); string(got) != string(snapDB) {
		t.Errorf("restored bridge.db (%d bytes) != snapshot bridge.db (%d bytes)", len(got), len(snapDB))
	}
}

// A non-ErrNotExist stat error on a snapshot source file (permission
// flap, symlink loop, transient I/O) MUST abort Restore BEFORE it
// removes the live -wal/-shm sidecars — otherwise it deletes the WAL
// for a source it never actually reads, then fails at copyFile's
// os.Open, leaving the live DB inconsistent. Mirrors Snapshot's
// non-ErrNotExist stat handling.
func TestRestoreNonNotExistStatErrorPreservesLiveWAL(t *testing.T) {
	tmp := t.TempDir()
	snapDir := filepath.Join(tmp, "snap")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Manifest lists bridge.db so Restore reaches the per-file loop.
	writeSnapshotManifest(t, snapDir, backup.ManifestDBFileName)

	// Make the snapshot's bridge.db a self-referential symlink → os.Stat
	// (which follows) returns ELOOP: a non-ErrNotExist error, deterministic
	// and cheap. Skip where symlinks aren't available (e.g. unprivileged
	// Windows) rather than fail.
	srcDB := filepath.Join(snapDir, backup.ManifestDBFileName)
	if err := os.Symlink(srcDB, srcDB); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}

	// Live DB + its WAL/SHM sidecars that MUST survive an aborted restore.
	target := filepath.Join(tmp, "live", "bridge.db")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	wal := target + "-wal"
	shm := target + "-shm"
	for _, p := range []string{target, wal, shm} {
		if err := os.WriteFile(p, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := backup.Restore(snapDir, backup.Targets{ManifestDB: target})
	if err == nil {
		t.Fatal("Restore should have failed on the non-ErrNotExist stat error")
	}
	// The load-bearing assertion: the live WAL/SHM were NOT deleted by
	// the aborted restore.
	if !pathExists(t, wal) {
		t.Error("live -wal was deleted by an aborted Restore")
	}
	if !pathExists(t, shm) {
		t.Error("live -shm was deleted by an aborted Restore")
	}
}

// Restore must refuse a snapshot whose manifest schema doesn't match
// — otherwise a future schema change could silently overwrite live
// state with an incompatible older format.
func TestRestoreRefusesSchemaMismatch(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Hand-corrupt the schema version in the manifest.
	mPath := filepath.Join(dst, backup.ManifestFile)
	raw := readBytes(t, mPath)
	if err := os.WriteFile(mPath, []byte(`{"schemaVersion":999,"files":["tokens.json"]}`), 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	defer func() { _ = os.WriteFile(mPath, raw, 0o600) }() // restore for cleanup

	err = backup.Restore(dst, backup.Targets{TokensJSON: src.TokensJSON})
	if err == nil {
		t.Fatalf("expected error from schema-mismatched snapshot, got nil")
	}
}

// Prune must keep the N most recent snapshots and delete the rest,
// based on the manifest CreatedAt timestamps.
func TestPruneKeepsMostRecent(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	backupsRoot := filepath.Join(dataDir, backup.BackupsDirName)

	// Create five snapshots, each in a distinct second so timestamps
	// don't collide.
	dirs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		dst, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		dirs = append(dirs, dst)
		// Force a 1-second gap so the timestamp strings are distinct.
		time.Sleep(1100 * time.Millisecond)
	}

	res, err := backup.Prune(backupsRoot, 3)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.ReapErr != nil {
		t.Errorf("Prune orphan sweep: %v", res.ReapErr)
	}
	if res.Deleted != 2 {
		t.Errorf("Prune deleted %d, want 2", res.Deleted)
	}

	remaining, err := backup.List(backupsRoot)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 3 {
		t.Errorf("after prune, %d snapshots remain (want 3)", len(remaining))
	}

	// The two oldest dirs must be gone.
	for _, p := range dirs[:2] {
		if pathExists(t, p) {
			t.Errorf("oldest snapshot %q should have been pruned but still exists", p)
		}
	}
	// The three newest must remain.
	for _, p := range dirs[2:] {
		if !pathExists(t, p) {
			t.Errorf("recent snapshot %q should have been retained but is missing", p)
		}
	}
}

// keep <= 0 must be a no-op rather than wiping every snapshot.
// Operator opt-outs go through "no rotation" not "rotate to zero".
func TestPruneNonPositiveKeepIsNoOp(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	backupsRoot := filepath.Join(dataDir, backup.BackupsDirName)

	if _, err := backup.Snapshot(t.Context(), src); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, keep := range []int{0, -1, -100} {
		res, err := backup.Prune(backupsRoot, keep)
		if err != nil {
			t.Fatalf("Prune(keep=%d): %v", keep, err)
		}
		if res.Deleted != 0 {
			t.Errorf("Prune(keep=%d) deleted %d, want 0", keep, res.Deleted)
		}
	}
}

// TestPruneIsBestEffortPastALockedDir locks the best-effort contract:
// if one eligible snapshot dir can't be removed (a lock, a permission
// drift), Prune must still reclaim the OTHER eligible dirs and surface
// the failure via a non-nil (joined) error. A fail-fast return left every
// snapshot behind the first failure un-pruned forever → unbounded disk
// growth on a long-running bridge.
func TestPruneIsBestEffortPastALockedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unremovable-dir injection is POSIX-only")
	}
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	backupsRoot := filepath.Join(dataDir, backup.BackupsDirName)

	// Three snapshots in distinct seconds; keep=1 makes the two oldest
	// eligible for pruning.
	dirs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		dst, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		dirs = append(dirs, dst)
		time.Sleep(1100 * time.Millisecond)
	}

	// Make the oldest dir unremovable by dropping write permission so its
	// contents can't be unlinked. Restore immediately via Cleanup so the
	// t.TempDir teardown can still walk it.
	locked := dirs[0]
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	res, err := backup.Prune(backupsRoot, 1)

	// Root bypasses the perm bits — if the removal succeeded anyway the
	// failure injection didn't take; skip rather than assert falsely (same
	// posture as the atomic-persist permission test).
	if err == nil {
		t.Skip("os.RemoveAll succeeded despite 0o500 (running as root / perms-ignoring FS)")
	}

	// Best-effort: the other eligible dir was still reclaimed even though
	// the locked one failed.
	if res.Deleted != 1 {
		t.Errorf("Prune deleted %d, want 1 (the removable old dir)", res.Deleted)
	}
	if !pathExists(t, locked) {
		t.Errorf("locked dir %q should have survived the failed removal", locked)
	}
	if pathExists(t, dirs[1]) {
		t.Errorf("second-oldest dir %q should have been pruned despite the locked sibling", dirs[1])
	}
	if !pathExists(t, dirs[2]) {
		t.Errorf("newest (kept) dir %q must remain", dirs[2])
	}
}

// ReapOrphans reclaims crash-orphaned partial snapshots — dirs with a
// near-full bridge.db copy but NO manifest.json (writer died between
// the DB copy and the manifest write). `List` skips them, so Prune's
// keep-policy can never see them; they'd accumulate unbounded across
// hard crashes. The grace spares an in-progress snapshot (fresh mtime).
func TestReapOrphansRemovesStaleManifestlessDirsSparesFreshAndValid(t *testing.T) {
	root := t.TempDir()
	grace := time.Hour
	now := time.Now()

	mkdir := func(name string) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	age := func(dir string, d time.Duration) {
		t.Helper()
		ts := now.Add(-d)
		if err := os.Chtimes(dir, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	// (a) Old orphan (no manifest, mtime well past the grace) → reaped.
	oldOrphan := mkdir("2026-01-01T00-00-00Z")
	if err := os.WriteFile(filepath.Join(oldOrphan, backup.ManifestDBFileName), []byte("partial-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(oldOrphan, 2*grace)

	// (b) Fresh orphan (no manifest, recent mtime) → spared (may be an
	// in-progress snapshot).
	freshOrphan := mkdir("2026-01-02T00-00-00Z")
	if err := os.WriteFile(filepath.Join(freshOrphan, backup.ManifestDBFileName), []byte("in-progress-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(freshOrphan, time.Minute)

	// (c) Completed old snapshot (HAS a manifest) → spared regardless of
	// age (Prune's keep-policy owns it, not ReapOrphans).
	validOld := mkdir("2026-01-03T00-00-00Z")
	writeSnapshotManifest(t, validOld, backup.ManifestDBFileName)
	age(validOld, 3*grace)

	reaped, err := backup.ReapOrphans(root, grace)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (only the old orphan)", reaped)
	}
	if pathExists(t, oldOrphan) {
		t.Error("old manifest-less orphan should have been reaped")
	}
	if !pathExists(t, freshOrphan) {
		t.Error("fresh manifest-less dir (possible in-progress snapshot) must be spared")
	}
	if !pathExists(t, validOld) {
		t.Error("completed snapshot (has manifest.json) must be spared regardless of age")
	}
}

// TestReapOrphansSparesUnreadableManifest pins that ReapOrphans reaps only on
// a genuinely ABSENT manifest, never on one it merely failed to read.
//
// The pre-fix gate was `if _, err := readManifest(...); err == nil { continue }`
// — so ANY read error (EACCES, EIO, or a Windows ERROR_SHARING_VIOLATION while
// Defender/Search-Indexer holds the file) classified a COMPLETE snapshot as a
// crash orphan and os.RemoveAll'd it. Silently: Prune discards the count and
// this package has no logger. RemoveAll would unlink the manifest and then
// fail the rmdir, leaving a dir that now really has no manifest — so the next
// prune reaps it outright and a good backup dies over two cycles.
func TestReapOrphansSparesUnreadableManifest(t *testing.T) {
	// Windows has no POSIX mode bits: os.Chmod(…, 0o000) only flips the
	// read-only attribute, so readManifest still SUCCEEDS and the
	// present-but-unreadable state this test needs can't be staged. (The
	// test compiles fine there — os.Geteuid is defined on Windows and
	// returns -1 — it just fails on the final "error must surface"
	// assertion.) A Windows equivalent would need a DACL denying FILE_-
	// READ_DATA via an API this package doesn't import.
	if runtime.GOOS == "windows" {
		t.Skip("windows: chmod 0000 doesn't make a file unreadable, so this fixture can't be staged")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 is still readable, so the unreadable-manifest case can't be staged")
	}
	root := t.TempDir()
	grace := time.Hour

	dir := filepath.Join(root, "2026-01-04T00-00-00Z")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSnapshotManifest(t, dir, backup.ManifestDBFileName)
	manifestPath := filepath.Join(dir, backup.ManifestFile)
	// Make it present-but-unreadable, and age it well past the grace so
	// nothing but the manifest check can be sparing it.
	if err := os.Chmod(manifestPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifestPath, 0o600) })
	ts := time.Now().Add(-3 * grace)
	if err := os.Chtimes(dir, ts, ts); err != nil {
		t.Fatal(err)
	}

	reaped, err := backup.ReapOrphans(root, grace)
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 — an unreadable manifest is not a crash orphan", reaped)
	}
	if !pathExists(t, dir) {
		t.Fatal("snapshot with a present-but-unreadable manifest was deleted")
	}
	// The failure must be reported, not swallowed — silence is how the
	// pre-fix deletion went unnoticed.
	if err == nil {
		t.Error("ReapOrphans returned nil error; the unreadable manifest should surface")
	}
}

// Prune wires ReapOrphans, so a crash-orphaned dir is reclaimed on the
// next snapshot's prune even when the keep-policy would otherwise never
// see it. Runs regardless of the keep value.
func TestPruneReapsCrashOrphans(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	backupsRoot := filepath.Join(dataDir, backup.BackupsDirName)

	// One valid snapshot (has a manifest) → must survive the prune.
	valid, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// A crash orphan: a manifest-less dir with a well-aged mtime.
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

	// keep=5 (> the single valid snapshot) so the keep-policy deletes
	// nothing — the orphan reclamation is entirely ReapOrphans's doing.
	if _, err := backup.Prune(backupsRoot, 5); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pathExists(t, orphan) {
		t.Error("Prune should have reaped the crash-orphaned manifest-less dir")
	}
	if !pathExists(t, valid) {
		t.Error("Prune reaped a valid snapshot")
	}
}

// TestPruneReportsOrphanSweepFailureSeparately pins the split between
// "the prune failed" and "the orphan sweep saw something it can't act on".
//
// A directory whose manifest.json is unreadable (here: truncated, the exact
// residue a crash partway through a non-atomic manifest write used to leave)
// is one ReapOrphans deliberately keeps and reports. That condition is
// PERMANENT for as long as the directory exists, so folding it into Prune's
// error return made every subsequent `bridge backup` exit 1 and made the
// serve-side ticker skip its "pruned N" line — even though the snapshot AND
// the keep-policy prune had both succeeded, and even though nothing the
// operator does through the bridge can clear it.
//
// So: err must stay nil, the condition must still surface via
// PruneResult.ReapErr, and the directory must be kept (its bridge.db is
// hand-recoverable; deleting it is not the sweeper's call to make).
func TestPruneReportsOrphanSweepFailureSeparately(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	backupsRoot := filepath.Join(dataDir, backup.BackupsDirName)

	valid, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	corrupt := filepath.Join(backupsRoot, "2020-01-01T00-00-00Z")
	if err := os.MkdirAll(corrupt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, backup.ManifestDBFileName), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Truncated mid-JSON: readable, but not decodable — neither "present"
	// nor os.ErrNotExist.
	if err := os.WriteFile(filepath.Join(corrupt, backup.ManifestFile),
		[]byte(`{"schemaVersion":1,"fil`), 0o600); err != nil {
		t.Fatal(err)
	}

	// keep=5 (> the one valid snapshot) so the keep-policy has nothing to
	// do and the only possible error source is the orphan sweep.
	res, err := backup.Prune(backupsRoot, 5)
	if err != nil {
		t.Fatalf("Prune must not fail over a directory the orphan sweep can't classify; "+
			"that error is permanent and would fail every future `bridge backup`: %v", err)
	}
	if res.ReapErr == nil {
		t.Error("the undecodable manifest must still be REPORTED, via PruneResult.ReapErr")
	}
	if !pathExists(t, corrupt) {
		t.Error("a dir with an unreadable manifest must be kept, not reaped")
	}
	if !pathExists(t, valid) {
		t.Error("Prune reaped a valid snapshot")
	}
}

// List on a missing or empty backups dir must not error — it's a
// fresh-install state, not a corruption signal.
func TestListAcceptsMissingDir(t *testing.T) {
	out, err := backup.List(filepath.Join(t.TempDir(), "no-such-dir"))
	if err != nil {
		t.Fatalf("List(missing): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("List(missing) = %v, want empty", out)
	}
}

// Snapshot must skip files that don't exist on disk rather than
// blowing up — a fresh `bridge init` may not have minted any
// tokens yet.
func TestSnapshotSkipsMissingOptionalFiles(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	if err := os.Remove(src.TokensJSON); err != nil {
		t.Fatalf("remove tokens.json: %v", err)
	}

	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if pathExists(t, filepath.Join(dst, "tokens.json")) {
		t.Errorf("snapshot should not contain tokens.json when source is missing")
	}
	// Other files still captured.
	if !pathExists(t, filepath.Join(dst, "server.crt")) {
		t.Errorf("snapshot should still contain server.crt")
	}
}

// LooksLikeSnapshotDir is the CLI's pre-flight; an arbitrary
// directory must be rejected, a real snapshot accepted.
func TestLooksLikeSnapshotDir(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	dst, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !backup.LooksLikeSnapshotDir(dst) {
		t.Errorf("LooksLikeSnapshotDir(%q) = false, want true", dst)
	}
	if backup.LooksLikeSnapshotDir(t.TempDir()) {
		t.Errorf("LooksLikeSnapshotDir(empty dir) = true, want false")
	}
}

// --- Helpers ---

// primeLiveState builds a plausible bridge state: an empty SQLite
// db, a tokens.json with one stub token, dummy cert/key files, a
// minimal bridge.yaml. Returns the populated Sources for Snapshot.
func primeLiveState(t *testing.T, dataDir string) backup.Sources {
	t.Helper()

	// Manifest db — a real SQLite file so VACUUM INTO works.
	dbPath := filepath.Join(dataDir, "bridge.db")
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sentinel (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create sentinel: %v", err)
	}
	db.Close()

	tokensPath := filepath.Join(dataDir, "tokens.json")
	if err := os.WriteFile(tokensPath, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write tokens.json: %v", err)
	}

	certPath := filepath.Join(dataDir, "server.crt")
	if err := os.WriteFile(certPath, []byte(`-----BEGIN CERTIFICATE-----stub-----END CERTIFICATE-----`), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyPath := filepath.Join(dataDir, "server.key")
	if err := os.WriteFile(keyPath, []byte(`-----BEGIN PRIVATE KEY-----stub-----END PRIVATE KEY-----`), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	yamlPath := filepath.Join(dataDir, "bridge.yaml")
	if err := os.WriteFile(yamlPath, []byte("libraryRoots:\n  - /tmp\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	return backup.Sources{
		DataDir:    dataDir,
		ManifestDB: dbPath,
		TokensJSON: tokensPath,
		ServerCert: certPath,
		ServerKey:  keyPath,
		BridgeYAML: yamlPath,
	}
}

func pathExists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", p, err)
	return false
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return info.Mode()
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return data
}
