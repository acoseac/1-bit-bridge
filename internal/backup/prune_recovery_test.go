package backup_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// A bundle of TestThePruneKeepsWhatRecoveryNeeds, by its database's verdict
// as a reader reads it: a manifest alone, written as raw JSON, so the
// keep-policy, which reads nothing but the manifests, can be put to every
// shape the brief names.
const (
	bundleVerified   = "VERIFIED"
	bundleFilesOnly  = "VERIFIED, files FAILED"
	bundleFailed     = "FAILED"
	bundleIncomplete = "INCOMPLETE"
	bundleLegacy     = "legacy"
	bundlePending    = "PENDING, young"
	bundleStale      = "PENDING, past its bound"
)

// writeBundle writes, under root, a snapshot of kind created age ago, named
// by its place in the list, and answers its folder's name.
func writeBundle(t *testing.T, root string, place int, age time.Duration, kind string) string {
	t.Helper()
	created := time.Now().UTC().Add(-age)
	m := map[string]any{
		"schemaVersion": backup.SchemaVersion,
		"bridgeVersion": "test",
		"createdAt":     created,
		"files":         []string{"bridge.db", "tokens.json"},
	}
	record := func(db, files string, dbReasons ...string) map[string]any {
		v := map[string]any{"format": 1, "databaseState": db, "filesState": files}
		if len(dbReasons) > 0 {
			v["databaseReasons"] = dbReasons
		}
		return v
	}
	if kind != bundleLegacy {
		m["baselines"] = []map[string]any{
			{"name": "bridge.db", "size": 4096, "sha256": "00"},
			{"name": "tokens.json", "size": 2, "sha256": "00"},
		}
	}
	switch kind {
	case bundleVerified:
		m["verification"] = record("VERIFIED", "VERIFIED")
	case bundleFilesOnly:
		v := record("VERIFIED", "FAILED")
		v["filesReasons"] = []string{"hash-mismatch"}
		m["verification"] = v
	case bundleFailed:
		m["verification"] = record("FAILED", "VERIFIED", "quick-check")
	case bundleIncomplete:
		v := record("INCOMPLETE", "INCOMPLETE", "cancelled")
		v["filesReasons"] = []string{"cancelled"}
		m["verification"] = v
	case bundlePending:
		v := record("PENDING", "PENDING")
		v["pendingSince"] = created
		m["verification"] = v
	case bundleStale:
		v := record("PENDING", "PENDING")
		v["pendingSince"] = time.Now().UTC().Add(-3 * time.Hour)
		m["verification"] = v
	case bundleLegacy:
	default:
		t.Fatalf("no bundle kind %q", kind)
	}
	name := fmt.Sprintf("bundle-%02d", place)
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestThePruneKeepsWhatRecoveryNeeds pins the keep-policy's rule (backlog
// B653), each row a backups folder listed newest first: with retention off
// every snapshot stays; with it on the keep newest stay, and beside them,
// beyond keep if they must, the newest snapshot whose database is VERIFIED,
// while none is the newest written before verification (legacy), the newest
// whose database FAILED or did not finish, one at most, and every PENDING
// its bound has not passed. A files verdict of FAILED leaves a database
// VERIFIED the recovery point. Red on main, which kept the keep newest and
// nothing else, in every row but retention off's, which is the rule's
// unchanged half.
func TestThePruneKeepsWhatRecoveryNeeds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keep    int
		bundles []string // newest first
		kept    []int    // by place in bundles
	}{
		{
			name:    "retention off keeps every snapshot",
			keep:    0,
			bundles: []string{bundleFailed, bundleFailed, bundleVerified, bundleLegacy, bundleIncomplete, bundleLegacy},
			kept:    []int{0, 1, 2, 3, 4, 5},
		},
		{
			name:    "keep 1 keeps the newest and the newest verified past it",
			keep:    1,
			bundles: []string{bundleFailed, bundleVerified, bundleVerified, bundleLegacy},
			kept:    []int{0, 1},
		},
		{
			name:    "keep 1 over verified ones keeps the newest alone",
			keep:    1,
			bundles: []string{bundleVerified, bundleVerified, bundleVerified},
			kept:    []int{0},
		},
		{
			name:    "several newer failed ones leave the newest verified and one failed",
			keep:    2,
			bundles: []string{bundleIncomplete, bundleFailed, bundleFailed, bundleFailed, bundleVerified, bundleVerified, bundleLegacy},
			kept:    []int{0, 1, 4},
		},
		{
			name:    "the evidence is the newest failed or unfinished one",
			keep:    1,
			bundles: []string{bundleVerified, bundleVerified, bundleIncomplete, bundleFailed},
			kept:    []int{0, 2},
		},
		{
			name:    "only legacy ones and more new failed ones than keep keep the newest legacy one",
			keep:    1,
			bundles: []string{bundleFailed, bundleFailed, bundleFailed, bundleLegacy, bundleLegacy},
			kept:    []int{0, 3},
		},
		{
			name:    "a legacy one goes once a database is verified",
			keep:    1,
			bundles: []string{bundleFailed, bundleFailed, bundleVerified, bundleLegacy, bundleLegacy},
			kept:    []int{0, 2},
		},
		{
			name:    "a failed files verdict leaves the database the recovery point",
			keep:    1,
			bundles: []string{bundleFailed, bundleFilesOnly, bundleVerified},
			kept:    []int{0, 1},
		},
		{
			name:    "a young pending one is never pruned and a stale one is evidence",
			keep:    1,
			bundles: []string{bundleVerified, bundlePending, bundlePending, bundleStale, bundleFailed},
			kept:    []int{0, 1, 2, 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), backup.BackupsDirName)
			names := make([]string, len(tc.bundles))
			for i, kind := range tc.bundles {
				names[i] = writeBundle(t, root, i, time.Duration(i+1)*time.Minute, kind)
			}
			res, err := backup.Prune(root, tc.keep)
			if err != nil {
				t.Fatalf("Prune(keep=%d): %v", tc.keep, err)
			}
			var want, got []string
			for _, i := range tc.kept {
				want = append(want, names[i])
			}
			entries, err := backup.List(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				got = append(got, e.DirName)
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("after Prune(keep=%d) over %v: kept %v, want %v", tc.keep, tc.bundles, got, want)
			}
			if res.Deleted != len(tc.bundles)-len(tc.kept) {
				t.Errorf("Prune(keep=%d) deleted %d, want %d", tc.keep, res.Deleted, len(tc.bundles)-len(tc.kept))
			}
		})
	}
}
