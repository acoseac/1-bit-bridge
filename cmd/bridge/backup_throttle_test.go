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
// schema version + CreatedAt the function reads. Real Snapshot writes
// many more fields, but List only consults what `readManifest`
// requires for ordering / filtering.
func writeSnapshotDir(t *testing.T, root string, createdAt time.Time) string {
	t.Helper()
	dir := filepath.Join(root, createdAt.Format("2006-01-02T15-04-05Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := backup.Manifest{
		SchemaVersion: backup.SchemaVersion,
		CreatedAt:     createdAt,
		Files:         []string{"bridge.db"},
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

func TestStartupSnapshotShouldSkip_NoExistingSnapshots(t *testing.T) {
	root := t.TempDir()
	skip, latest, err := startupSnapshotShouldSkip(root, time.Now().UTC(), 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if skip {
		t.Errorf("skip = true, want false (no snapshots exist; the very first startup MUST write a baseline)")
	}
	if !latest.IsZero() {
		t.Errorf("latest = %v, want zero time", latest)
	}
}

func TestStartupSnapshotShouldSkip_RecentSnapshotWithinThreshold(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeSnapshotDir(t, root, now.Add(-2*time.Hour))

	skip, latest, err := startupSnapshotShouldSkip(root, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !skip {
		t.Errorf("skip = false, want true (a snapshot 2h old is well inside the 24h threshold)")
	}
	if latest.IsZero() {
		t.Errorf("latest = zero, want the recent snapshot's timestamp")
	}
}

func TestStartupSnapshotShouldSkip_OldSnapshotPastThreshold(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeSnapshotDir(t, root, now.Add(-26*time.Hour))

	skip, _, err := startupSnapshotShouldSkip(root, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if skip {
		t.Errorf("skip = true, want false (a 26h-old snapshot is past the 24h threshold; baseline should refresh)")
	}
}

func TestStartupSnapshotShouldSkip_SnapshotExactlyAtThresholdDoesNotSkip(t *testing.T) {
	// Boundary case (CodeRabbit on PR #101): the helper uses a strict
	// `<` comparison, so a snapshot whose age equals the threshold
	// exactly should NOT be treated as recent. Pin the boundary
	// behaviour here so a future drift to `<=` doesn't silently
	// re-introduce the "we wrote a snapshot exactly 24h ago, no need
	// to write another one ever again" failure mode under the perfect
	// edge alignment a constant-cadence ticker would hit.
	root := t.TempDir()
	now := time.Now().UTC()
	writeSnapshotDir(t, root, now.Add(-24*time.Hour))

	skip, _, err := startupSnapshotShouldSkip(root, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if skip {
		t.Errorf("skip = true, want false (exactly-at-threshold must not be treated as recent)")
	}
}

func TestStartupSnapshotShouldSkip_PicksMostRecentNotOldest(t *testing.T) {
	// backup.List sorts newest-first; the throttle must consult that
	// ordering so a one-week-old snapshot doesn't shadow a two-hour-
	// old one.
	root := t.TempDir()
	now := time.Now().UTC()
	writeSnapshotDir(t, root, now.Add(-7*24*time.Hour))
	writeSnapshotDir(t, root, now.Add(-2*time.Hour))

	skip, latest, err := startupSnapshotShouldSkip(root, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !skip {
		t.Errorf("skip = false, want true (most recent is 2h old, well inside 24h)")
	}
	if age := now.Sub(latest); age > 3*time.Hour {
		t.Errorf("latest age = %v, want close to 2h (helper picked the older snapshot)", age)
	}
}

func TestStartupSnapshotShouldSkip_MissingBackupsRootIsNotAnError(t *testing.T) {
	// First-ever startup: no `<dataDir>/backups/` exists yet. List
	// returns (nil, nil) on ErrNotExist — the throttle must treat
	// that as "no snapshots, write the baseline" not as a failure.
	skip, _, err := startupSnapshotShouldSkip(filepath.Join(t.TempDir(), "does-not-exist"), time.Now().UTC(), 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v, want nil (missing backups dir is not an error)", err)
	}
	if skip {
		t.Errorf("skip = true, want false (no snapshots → MUST write the first one)")
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
