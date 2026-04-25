package backup_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	_ "modernc.org/sqlite"
)

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
	if got := mode(t, dst); got&0o777 != 0o700 {
		t.Errorf("snapshot dir mode = %o, want 0700", got&0o777)
	}
	for _, name := range []string{"bridge.db", "tokens.json", "server.crt", "server.key"} {
		if got := mode(t, filepath.Join(dst, name)); got&0o777 != 0o600 {
			t.Errorf("snapshot file %s mode = %o, want 0600", name, got&0o777)
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
	live, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
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

	snap, err := sql.Open("sqlite", "file:"+filepath.Join(dst, "bridge.db")+"?mode=ro")
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

	deleted, err := backup.Prune(backupsRoot, 3)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 2 {
		t.Errorf("Prune deleted %d, want 2", deleted)
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
		deleted, err := backup.Prune(backupsRoot, keep)
		if err != nil {
			t.Fatalf("Prune(keep=%d): %v", keep, err)
		}
		if deleted != 0 {
			t.Errorf("Prune(keep=%d) deleted %d, want 0", keep, deleted)
		}
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
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
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
