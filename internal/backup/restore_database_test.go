package backup_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// restoreMarkerKey is the scan_state row a fixture writes before its
// snapshot ("snapshot") and after it ("later"), which tells the restored
// database from the original.
const restoreMarkerKey = "b653p2b1-restore-marker"

// fixtureHarvest is the harvest state a fixture keeps: a credential and a
// sync position the restore rewinds.
const fixtureHarvest = `{"token":"tok-kept","atlasBaseUrl":"https://atlas.example","expiresAt":"2030-01-01T00:00:00Z",` +
	`"resultCursor":42,"lastSubmitAt":"2026-01-01T00:00:00Z","lastBookletCheckAt":"2026-01-02T00:00:00Z"}`

// dbFixture is a data dir holding a live database (its marker "later"), a
// VERIFIED snapshot of it taken at "snapshot", the config both were taken
// with, and a harvest state, laid out as one of three originals: the
// database with a WAL and a -shm planted beside it, the database alone, or
// no database at all.
type dbFixture struct {
	dataDir, snap, yaml string
	cfg                 []byte
	layout              string
}

// The fixture's layouts.
const (
	withSidecars = "sidecars"
	databaseOnly = "database"
	noDatabase   = "none"
)

// newDBFixture builds a dbFixture of layout.
func newDBFixture(t *testing.T, layout string) *dbFixture {
	t.Helper()
	dataDir := resolvedTestDir(t, t.TempDir())
	src := primeLiveState(t, dataDir)
	setMarker(t, src.ManifestDB, "snapshot")
	snap := verifiedSnapshot(t, src)
	setMarker(t, src.ManifestDB, "later")
	f := &dbFixture{dataDir: dataDir, snap: snap, yaml: src.BridgeYAML, cfg: readBytes(t, src.BridgeYAML), layout: layout}
	db := manifest.DefaultDBPath(dataDir)
	switch layout {
	case withSidecars:
		writeFile(t, db+"-wal", "the original's WAL, which only its database can read")
		writeFile(t, db+"-shm", "the original's -shm")
	case noDatabase:
		if err := os.Remove(db); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dataDir, backup.AtlasHarvestFileName), fixtureHarvest)
	return f
}

// opts is the restore's options over f.
func (f *dbFixture) opts() backup.DatabaseOptions {
	return backup.DatabaseOptions{ConfigPath: f.yaml, ConfigAsRead: f.cfg}
}

// originals is what f's four original names hold now, by name: the bytes of
// each regular file, and nothing for a name that holds none.
func (f *dbFixture) originals(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	db := manifest.DefaultDBPath(f.dataDir)
	for _, p := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		if b, err := os.ReadFile(p); err == nil {
			out[filepath.Base(p)] = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return out
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setMarker writes the marker row into the database at path, closing it
// again so nothing is left beside it.
func setMarker(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO scan_state(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, restoreMarkerKey, value); err != nil {
		t.Fatal(err)
	}
}

// readOnly reads the database at path without making anything beside it.
func readOnly(t *testing.T, path string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro&immutable=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fn(db)
}

// markerOf is the marker the database at path holds.
func markerOf(t *testing.T, path string) string {
	t.Helper()
	var v string
	readOnly(t, path, func(db *sql.DB) {
		if err := db.QueryRow(`SELECT v FROM scan_state WHERE k = ?`, restoreMarkerKey).Scan(&v); err != nil {
			t.Fatalf("read the marker of %s: %v", path, err)
		}
	})
	return v
}

// harvestOf is the harvest state's file decoded.
func harvestOf(t *testing.T, dataDir string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readBytes(t, filepath.Join(dataDir, backup.AtlasHarvestFileName)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// requireNoJournalNoStage requires the journal and every staged name gone.
func requireNoJournalNoStage(t *testing.T, dataDir string) {
	t.Helper()
	db := manifest.DefaultDBPath(dataDir)
	stage := manifest.RestoreStagingPath(dataDir)
	for _, p := range []string{manifest.RestoreJournalPath(db), stage, stage + "-wal", stage + "-shm"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left (%v)", filepath.Base(p), err)
		}
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a staged file is left: %s", e.Name())
		}
	}
}

// TestADatabaseOnlyRestoreReplacesTheDatabaseAndKeepsTheOriginals is T8: the
// restored database holds the snapshot's rows under a new epoch, every
// original is in the run's folder byte for byte with its record, the
// journal and the stage are gone, the other files of the data dir are as
// they were, and the harvest's sync position alone is rewound, its
// credential kept.
func TestADatabaseOnlyRestoreReplacesTheDatabaseAndKeepsTheOriginals(t *testing.T) {
	for _, layout := range []string{withSidecars, databaseOnly, noDatabase} {
		t.Run(layout, func(t *testing.T) {
			f := newDBFixture(t, layout)
			db := manifest.DefaultDBPath(f.dataDir)
			before := f.originals(t)
			snapEpoch := epochOf(t, filepath.Join(f.snap, backup.ManifestDBFileName))
			others := map[string][]byte{}
			for _, name := range []string{"tokens.json", "server.crt", "server.key", "bridge.yaml"} {
				others[name] = readBytes(t, filepath.Join(f.dataDir, name))
			}
			began := time.Now().UnixNano()
			res, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
			if err != nil {
				t.Fatalf("RestoreDatabase: %v", err)
			}
			if got := markerOf(t, db); got != "snapshot" {
				t.Errorf("the restored database's marker = %q, want the snapshot's", got)
			}
			if e := epochOf(t, db); e == snapEpoch {
				t.Errorf("the restored database kept the snapshot's epoch %s", e)
			}
			readOnly(t, db, func(rdb *sql.DB) {
				h, err := manifest.ReadRestoredHistory(t.Context(), rdb)
				if err != nil || h.Coverage < began {
					t.Errorf("the restored database's coverage = %+v (%v), want it restarted at the restore (%d) or later", h, err, began)
				}
			})
			if !strings.HasPrefix(res.Archive, filepath.Join(f.dataDir, backup.OriginalsDirName)+string(filepath.Separator)) {
				t.Fatalf("the originals' folder %s is not in %s", res.Archive, backup.OriginalsDirName)
			}
			for name, data := range before {
				if got := string(readBytes(t, filepath.Join(res.Archive, name))); got != data {
					t.Errorf("the archived %s is not the original", name)
				}
			}
			archived, err := os.ReadDir(res.Archive)
			if err != nil {
				t.Fatal(err)
			}
			if len(archived) != len(before)+1 {
				t.Errorf("the run's folder holds %d entries, want the %d originals and its record", len(archived), len(before))
			}
			var record backup.RestoreJournal
			if err := json.Unmarshal(readBytes(t, filepath.Join(res.Archive, "restore.json")), &record); err != nil {
				t.Fatalf("the record does not decode: %v", err)
			}
			for _, o := range record.Originals {
				if _, there := before[o.Name]; o.Present != there {
					t.Errorf("the record says %s present %v", o.Name, o.Present)
				}
			}
			requireNoJournalNoStage(t, f.dataDir)
			for _, side := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Lstat(db + side); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s stands beside the restored database", filepath.Base(db+side))
				}
			}
			for name, data := range others {
				if !bytes.Equal(readBytes(t, filepath.Join(f.dataDir, name)), data) {
					t.Errorf("%s changed", name)
				}
			}
			h := harvestOf(t, f.dataDir)
			if h["resultCursor"].(float64) != 0 || h["token"] != "tok-kept" || h["atlasBaseUrl"] != "https://atlas.example" ||
				!strings.HasPrefix(h["lastSubmitAt"].(string), "0001-01-01") || !strings.HasPrefix(h["lastBookletCheckAt"].(string), "0001-01-01") {
				t.Errorf("the harvest state after the restore = %v, want its position rewound and its credential kept", h)
			}
			if err := manifest.CheckRestoreJournal(db); err != nil {
				t.Errorf("an opener is refused after the restore: %v", err)
			}
		})
	}
}

// TestADatabaseOnlyRestoreTakesOnlyAVerifiedDatabase is T9's rows as every
// reader reads them: a VERIFIED behind its veto (in the bundle), a FAILED
// record, and a snapshot of no baseline each refuse with nothing staged and
// every original as it was; and the look's refusal before the prompt says
// the same of each.
func TestADatabaseOnlyRestoreTakesOnlyAVerifiedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, snap string)
		want string
	}{
		{"vetoed", func(t *testing.T, snap string) { writeFile(t, filepath.Join(snap, ".verdict-unsynced"), "") }, "unsynced"},
		{"failed", func(t *testing.T, snap string) {
			editManifest(t, snap, func(m map[string]any) { m["verification"].(map[string]any)["databaseState"] = "FAILED" })
		}, "FAILED"},
		{"legacy", func(t *testing.T, snap string) {
			editManifest(t, snap, func(m map[string]any) { delete(m, "verification"); delete(m, "baselines") })
		}, "baseline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFixture(t, withSidecars)
			tc.make(t, f.snap)
			before := f.originals(t)
			look, err := backup.CheckRestoreSource(t.Context(), f.snap, backup.VerifyCmdLockWait)
			if err != nil {
				t.Fatalf("the look: %v", err)
			}
			if why := backup.DatabaseRestoreRefusal(look); !strings.Contains(why, tc.want) {
				t.Errorf("the look's refusal = %q, want it to name %q", why, tc.want)
			}
			_, err = backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was restored") {
				t.Fatalf("RestoreDatabase = %v, want a refusal naming %q", err, tc.want)
			}
			requireUnchanged(t, f, before)
		})
	}
}

// requireUnchanged requires f's originals as they were, and nothing of a
// restore left: no journal, no stage, no folder of originals.
func requireUnchanged(t *testing.T, f *dbFixture, before map[string]string) {
	t.Helper()
	if got := f.originals(t); !mapsEqual(got, before) {
		t.Errorf("the originals changed: %d names now, %d before", len(got), len(before))
	}
	requireNoJournalNoStage(t, f.dataDir)
	if entries, err := os.ReadDir(filepath.Join(f.dataDir, backup.OriginalsDirName)); err == nil && len(entries) > 0 {
		t.Errorf("a folder of originals was made: %v", entries)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestASnapshotChangedAfterItsVerificationIsRefused is T11: one byte of the
// snapshot's database flipped after its VERIFIED refuses at the staged
// copy's comparison with its baseline, nothing moved and nothing left staged.
func TestASnapshotChangedAfterItsVerificationIsRefused(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	p := filepath.Join(f.snap, backup.ManifestDBFileName)
	b := readBytes(t, p)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	before := f.originals(t)
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if err == nil || !strings.Contains(err.Error(), "not the one its verification checked") {
		t.Fatalf("RestoreDatabase = %v, want the baseline's refusal", err)
	}
	requireUnchanged(t, f, before)
}

// TestAFailureWhilePreparingLeavesEveryOriginal is T15's busy row: a
// preparation whose checkpoint answers busy refuses, the staged set
// removed, every original as it was and no journal written.
func TestAFailureWhilePreparingLeavesEveryOriginal(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	t.Cleanup(manifest.SetEpochCheckpointAnswerForTest(func() (int, int, int, error) { return 1, 4, 2, nil }))
	before := f.originals(t)
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if !errors.Is(err, manifest.ErrEpochCheckpointBusy) {
		t.Fatalf("RestoreDatabase = %v, want the busy checkpoint's refusal", err)
	}
	requireUnchanged(t, f, before)
}

// TestARestoreRefusesAFilesystemThatCannotSyncADirectory is the barrier's
// probe before anything moves: a directory sync answered EINVAL, ENOTSUP or
// EOPNOTSUPP (a network or FUSE mount) refuses the restore, naming the
// filesystem's limit, with nothing moved and no journal.
func TestARestoreRefusesAFilesystemThatCannotSyncADirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows syncs no directory at all and goes on under its own residual, backlog B920")
	}
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		t.Run(errno.Error(), func(t *testing.T) {
			f := newDBFixture(t, withSidecars)
			before := f.originals(t)
			restore := atomicwrite.SetSyncDirForTest(func(string) error { return errno })
			_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
			restore()
			if err == nil || !strings.Contains(err.Error(), "refuses to sync a directory") {
				t.Fatalf("RestoreDatabase over a filesystem answering %v = %v, want the barrier's refusal", errno, err)
			}
			requireUnchanged(t, f, before)
		})
	}
}

// TestTheOriginalsAreDurableBeforeTheInstall is T46: an original whose sync
// fails stops the restore at C0, nothing moved and no journal; and a sync of
// the run's folder that fails once its sidecars are in it puts every
// original back before anything is installed.
func TestTheOriginalsAreDurableBeforeTheInstall(t *testing.T) {
	t.Run("an original's sync", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		before := f.originals(t)
		restore := backup.SetOriginalSyncForTest(func(*os.File) error { return errors.New("the disk refused the sync") })
		_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
		restore()
		if err == nil || !strings.Contains(err.Error(), "the disk refused the sync") || !strings.Contains(err.Error(), "nothing was restored") {
			t.Fatalf("RestoreDatabase = %v, want C0's refusal", err)
		}
		requireUnchanged(t, f, before)
	})
	t.Run("the folder's sync after C3", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows syncs no directory, backlog B920")
		}
		f := newDBFixture(t, withSidecars)
		before := f.originals(t)
		var moved, failed atomic.Bool
		t.Cleanup(backup.SetRestoreStepForTest(func(step string) {
			if strings.HasPrefix(step, "C3:") {
				moved.Store(true)
			}
		}))
		t.Cleanup(atomicwrite.SetSyncDirForTest(func(dir string) error {
			if moved.Load() && strings.Contains(dir, backup.OriginalsDirName) && failed.CompareAndSwap(false, true) {
				return syscall.EIO
			}
			return nil
		}))
		_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
		if err == nil || !strings.Contains(err.Error(), "originals were put back") {
			t.Fatalf("RestoreDatabase = %v, want the put-back", err)
		}
		if got := f.originals(t); !mapsEqual(got, before) {
			t.Error("the originals are not back as they were")
		}
		requireNoJournalNoStage(t, f.dataDir)
	})
}

// TestASharingViolationMidSwapPutsBackWhatItMoved is T21's shape on every
// platform, a fault of the swap's rename standing in for the violation that
// outlasts its retries: a failure of C4's rename puts back the sidecars C3
// moved, removes the journal and the run's folder, and names the failure.
func TestASharingViolationMidSwapPutsBackWhatItMoved(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	before := f.originals(t)
	t.Cleanup(backup.SetSwapFaultForTest(func(from, to string) error {
		if filepath.Base(from) == backup.ManifestDBFileName && strings.Contains(to, backup.OriginalsDirName) {
			return errors.New("the database's file is held by another program")
		}
		return nil
	}))
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if err == nil || !strings.Contains(err.Error(), "held by another program") || !strings.Contains(err.Error(), "originals were put back") {
		t.Fatalf("RestoreDatabase = %v, want the put-back naming the failure", err)
	}
	requireUnchanged(t, f, before)
}

// TestAnEntryAtATakenNameStopsTheRestoreAndKeepsTheJournal is T22: a WAL
// made at its live name just before the prepared copy is renamed in sends
// the installed copy back and stops with the journal kept (`forward`); the
// way back refuses while the entry stands, moving nothing, and puts every
// original back once it is gone.
func TestAnEntryAtATakenNameStopsTheRestoreAndKeepsTheJournal(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	before := f.originals(t)
	db := manifest.DefaultDBPath(f.dataDir)
	stage := manifest.RestoreStagingPath(f.dataDir)
	var planted atomic.Bool
	restore := fsutil.SetBeforeNoReplaceRenameForTest(func(from, to string) {
		if from == stage && to == db && planted.CompareAndSwap(false, true) {
			writeFile(t, db+"-wal", "a WAL something opened beside the new database")
		}
	})
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	restore()
	if !errors.Is(err, backup.ErrRestoreStopped) || !strings.Contains(err.Error(), "bridge.db-wal") {
		t.Fatalf("RestoreDatabase = %v, want a stop naming the WAL", err)
	}
	if err := manifest.CheckRestoreJournal(db); !errors.Is(err, manifest.ErrRestoreIncomplete) {
		t.Fatalf("the journal does not stand: %v", err)
	}
	if _, err := os.Lstat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the installed copy was not sent back: %v", err)
	}
	_, err = backup.PlanFinishDatabaseRestore(f.dataDir)
	if !errors.Is(err, backup.ErrRestoreStopped) {
		t.Fatalf("the way back over the planted WAL = %v, want a stop", err)
	}
	if err := os.Remove(db + "-wal"); err != nil {
		t.Fatal(err)
	}
	finishDatabaseRestore(t, f.dataDir)
	if got := f.originals(t); !mapsEqual(got, before) {
		t.Error("the originals are not back as they were")
	}
	requireNoJournalNoStage(t, f.dataDir)
}

// finishDatabaseRestore runs the way back as bridge restore --database-only
// runs it once its operator says yes.
func finishDatabaseRestore(t *testing.T, dataDir string) {
	t.Helper()
	plan, err := backup.PlanFinishDatabaseRestore(dataDir)
	if err != nil {
		t.Fatalf("PlanFinishDatabaseRestore: %v", err)
	}
	if err := backup.FinishDatabaseRestore(dataDir, plan, backup.FinishOptions{}); err != nil {
		t.Fatalf("FinishDatabaseRestore: %v", err)
	}
}

// TestTheHarvestCursorIsResetOnlyOnceTheRestoreIsInstalled is T40: a put-back
// leaves the harvest state byte for byte; a restore whose rewind cannot be
// made keeps `installed` and a rerun completes it; a completion with no
// harvest state makes none; and the restored state's rewind is the clean
// state whatever the file held before, a file that does not decode included.
func TestTheHarvestCursorIsResetOnlyOnceTheRestoreIsInstalled(t *testing.T) {
	t.Run("a put-back", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		harvest := readBytes(t, filepath.Join(f.dataDir, backup.AtlasHarvestFileName))
		t.Cleanup(backup.SetSwapFaultForTest(func(from, to string) error {
			if filepath.Base(from) == backup.ManifestDBFileName && strings.Contains(to, backup.OriginalsDirName) {
				return errors.New("held")
			}
			return nil
		}))
		if _, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts()); err == nil {
			t.Fatal("the restore did not fail")
		}
		if !bytes.Equal(readBytes(t, filepath.Join(f.dataDir, backup.AtlasHarvestFileName)), harvest) {
			t.Error("the put-back touched the harvest state")
		}
	})
	t.Run("a rewind that fails, then its retry", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		opts := f.opts()
		opts.HarvestState = filepath.Join(f.dataDir, "harvest-is-a-directory")
		if err := os.Mkdir(opts.HarvestState, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := backup.RestoreDatabase(f.snap, f.dataDir, opts)
		if !errors.Is(err, backup.ErrRestoreStopped) {
			t.Fatalf("RestoreDatabase with a rewind that fails = %v, want a stop", err)
		}
		j, err := backup.ReadRestoreJournal(f.dataDir)
		if err != nil || j.Phase != backup.PhaseInstalled {
			t.Fatalf("the journal after the failed rewind: %v, %v; want installed", j, err)
		}
		if got := markerOf(t, manifest.DefaultDBPath(f.dataDir)); got != "snapshot" {
			t.Errorf("the installed database's marker = %q", got)
		}
		plan, err := backup.PlanFinishDatabaseRestore(f.dataDir)
		if err != nil || !plan.Complete {
			t.Fatalf("the way back's plan = %+v, %v; want it to complete", plan, err)
		}
		if err := backup.FinishDatabaseRestore(f.dataDir, plan, backup.FinishOptions{}); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		requireNoJournalNoStage(t, f.dataDir)
		if h := harvestOf(t, f.dataDir); h["resultCursor"].(float64) != 0 || h["token"] != "tok-kept" {
			t.Errorf("the harvest after the retry = %v", h)
		}
	})
	t.Run("no harvest state", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		p := filepath.Join(f.dataDir, backup.AtlasHarvestFileName)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if _, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts()); err != nil {
			t.Fatalf("RestoreDatabase: %v", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the completion made a harvest state: %v", err)
		}
	})
	t.Run("a harvest state that does not decode", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		p := filepath.Join(f.dataDir, backup.AtlasHarvestFileName)
		writeFile(t, p, "{not json")
		if _, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts()); err != nil {
			t.Fatalf("RestoreDatabase: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(readBytes(t, p), &m); err != nil || m["resultCursor"].(float64) != 0 {
			t.Errorf("the harvest state after the restore = %q (%v), want the clean state written", readBytes(t, p), err)
		}
	})
}

// TestTheRestoreOriginalsFolderIsKeptAcrossRuns is T24's shape: a second
// database-only restore keeps the first's folder of originals and makes one
// of its own beside it.
func TestTheRestoreOriginalsFolderIsKeptAcrossRuns(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	first, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	firstBytes := readBytes(t, filepath.Join(first.Archive, backup.ManifestDBFileName))
	second, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if second.Archive == first.Archive {
		t.Fatalf("the second run took the first's folder %s", first.Archive)
	}
	if !bytes.Equal(readBytes(t, filepath.Join(first.Archive, backup.ManifestDBFileName)), firstBytes) {
		t.Error("the first run's originals changed")
	}
	entries, err := os.ReadDir(filepath.Join(f.dataDir, backup.OriginalsDirName))
	if err != nil || len(entries) != 2 {
		t.Errorf("restore-originals holds %v (%v), want both runs' folders", entries, err)
	}
}

// TestTheOriginalsSurviveEveryPruneAndSweep is T23: the prune and the orphan
// reap of the backups folder leave a populated folder of originals as it
// is, since it is a sibling of the backups folder, never inside it.
func TestTheOriginalsSurviveEveryPruneAndSweep(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	res, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := dirState(t, res.Archive)
	backups := filepath.Join(f.dataDir, backup.BackupsDirName)
	if _, err := backup.Prune(backups, 1); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := backup.ReapOrphans(backups, 0); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if got := dirState(t, res.Archive); !mapsEqual(got, snapshot) {
		t.Errorf("the folder of originals changed: %v, was %v", got, snapshot)
	}
}

// TestAJournalStandingRefusesANewRestoreBeforeItsSnapshot is §4.1 step 3 at
// the API: a new database-only restore refuses while a journal stands,
// before it reads the snapshot or stages anything.
func TestAJournalStandingRefusesANewRestoreBeforeItsSnapshot(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	j := manifest.RestoreJournalPath(manifest.DefaultDBPath(f.dataDir))
	writeFile(t, j, "{}")
	read := false
	t.Cleanup(backup.SetOnSourceWorkerEndForTest(func(string) { read = true }))
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if !errors.Is(err, manifest.ErrRestoreIncomplete) || read {
		t.Fatalf("RestoreDatabase beside a journal = %v (the snapshot read: %v), want the journal's refusal first", err, read)
	}
	if _, err := os.Lstat(manifest.RestoreStagingPath(f.dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something was staged: %v", err)
	}
}

// journalOf is the journal standing in dataDir, which the test requires.
func journalOf(t *testing.T, dataDir string) *backup.RestoreJournal {
	t.Helper()
	j, err := backup.ReadRestoreJournal(dataDir)
	if err != nil {
		t.Fatalf("the journal: %v", err)
	}
	return j
}

// writeJournal writes j as the journal standing in dataDir.
func writeJournal(t *testing.T, dataDir string, j *backup.RestoreJournal) {
	t.Helper()
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest.RestoreJournalPath(manifest.DefaultDBPath(dataDir)), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// dataDirState is every regular file under dataDir (its snapshots and its
// lock files aside) and its bytes.
func dataDirState(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dataDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dataDir, p)
		if d.IsDir() && rel == backup.BackupsDirName {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && !strings.HasSuffix(rel, ".lock") {
			out[filepath.ToSlash(rel)] = string(readBytes(t, p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// stoppedAt runs, in a process of its own, the database-only restore of f
// and kills that process at step (the crash helper, crashHelperEnv); with
// fault, C4's rename fails first, so the step is one of the put-back's.
func stoppedAt(t *testing.T, f *dbFixture, step string, fault bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestoreDatabaseCrashHelperProcess$", "-test.timeout=2m")
	env := append(os.Environ(), crashHelperEnv+"="+step, crashDataDirEnv+"="+f.dataDir, crashSnapEnv+"="+f.snap, crashConfigEnv+"="+f.yaml)
	if fault {
		env = append(env, crashFaultEnv+"=1")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 || strings.Contains(string(out), "reached the end") {
		t.Fatalf("the helper did not stop at %s: %v\n%s", step, err, out)
	}
}

// The crash helper's environment.
const (
	crashHelperEnv  = "BRIDGE_TEST_RESTORE_CRASH_STEP"
	crashDataDirEnv = "BRIDGE_TEST_RESTORE_CRASH_DATADIR"
	crashSnapEnv    = "BRIDGE_TEST_RESTORE_CRASH_SNAPSHOT"
	crashConfigEnv  = "BRIDGE_TEST_RESTORE_CRASH_CONFIG"
	crashFaultEnv   = "BRIDGE_TEST_RESTORE_CRASH_FAULT"
)

// TestRestoreDatabaseCrashHelperProcess is the process a database-only
// restore is stopped in: it skips unless its environment names a step,
// restores the fixture its environment names, and kills itself (SIGKILL, or
// Windows' termination) as the restore reaches that step, so nothing of the
// restore's own runs after it, deferred or not.
func TestRestoreDatabaseCrashHelperProcess(t *testing.T) {
	want := os.Getenv(crashHelperEnv)
	if want == "" {
		t.Skip("the database-only restore's crash helper: run by its tests alone")
	}
	backup.SetRestoreStepForTest(func(step string) {
		if step == want {
			p, err := os.FindProcess(os.Getpid())
			if err == nil {
				_ = p.Kill()
			}
			select {}
		}
	})
	if os.Getenv(crashFaultEnv) != "" {
		// The database's move out fails (C4), or, where there is none, the
		// prepared copy's move in (C5): either puts the originals back.
		backup.SetSwapFaultForTest(func(from, to string) error {
			if filepath.Base(from) == backup.ManifestDBFileName && strings.Contains(to, backup.OriginalsDirName) ||
				filepath.Base(from) == filepath.Base(manifest.RestoreStagingPath(".")) {
				return errors.New("held by another program")
			}
			return nil
		})
	}
	cfg, err := os.ReadFile(os.Getenv(crashConfigEnv))
	if err != nil {
		fmt.Println("refused", err)
		os.Exit(3)
	}
	_, err = backup.RestoreDatabase(os.Getenv(crashSnapEnv), os.Getenv(crashDataDirEnv),
		backup.DatabaseOptions{ConfigPath: os.Getenv(crashConfigEnv), ConfigAsRead: cfg})
	fmt.Println("reached the end:", err)
	os.Exit(0)
}

// TestARestoreStoppedAtEveryStepFollowsItsStateTable is T19: a restore
// stopped at each step of the swap and of its put-back, in a process of its
// own, over each of the three originals' layouts. Before the journal is
// published and once it is removed, an opener of the live database is
// admitted; between, it is refused (each asked of a copy of the data dir, so
// nothing is opened on the fixture). Then the way back: a restore stopped
// before `installed` is put back, every original byte for byte where it was
// and every absent one absent; one stopped at `installed` is completed, the
// restored database in place and the harvest rewound; one stopped after the
// journal's removal is finished, and the way back has nothing to do. A
// second run of the way back finds no journal.
func TestARestoreStoppedAtEveryStepFollowsItsStateTable(t *testing.T) {
	type row struct {
		step     string
		fault    bool
		journal  bool // a journal stands after the stop
		restored bool // the restored database is what remains
	}
	rows := []row{
		{"C0", false, false, false},
		{"C1", false, false, false},
		{"C2", false, true, false},
		{"C3:bridge.db-wal", false, true, false},
		{"C3:bridge.db-shm", false, true, false},
		{"C3:bridge.db-journal:synced", false, true, false},
		{"C4", false, true, false},
		{"C4:synced", false, true, false},
		{"C5", false, true, false},
		{"C5'", false, true, true},
		{"C6:rewind", false, true, true},
		{"C6:rewound", false, true, true},
		{"C6:journal", false, false, true},
		{"P:published", true, true, false},
		{"P:back:bridge.db-wal", true, true, false},
		{"P:back:bridge.db-shm", true, true, false},
		{"P:journal", true, false, false},
	}
	for _, layout := range []string{withSidecars, databaseOnly, noDatabase} {
		t.Run(layout, func(t *testing.T) {
			template := newDBFixture(t, layout)
			for _, r := range rows {
				moves := r.step
				if layout != withSidecars && (strings.HasPrefix(moves, "C3:") || strings.HasPrefix(moves, "P:back:bridge.db-")) {
					continue // no sidecar to move
				}
				if layout == noDatabase && (moves == "C4" || moves == "C4:synced") {
					continue // no database to move
				}
				t.Run(r.step, func(t *testing.T) {
					f := copyFixture(t, template)
					before := f.originals(t)
					stoppedAt(t, f, r.step, r.fault)
					db := manifest.DefaultDBPath(f.dataDir)
					stands := !errors.Is(statErr(manifest.RestoreJournalPath(db)), os.ErrNotExist)
					if stands != r.journal {
						t.Fatalf("after %s a journal stands: %v, want %v", r.step, stands, r.journal)
					}
					requireAdmission(t, f.dataDir, !r.journal)
					if r.step == "P:published" {
						if j := journalOf(t, f.dataDir); j.Phase != backup.PhasePuttingBack {
							t.Errorf("a put-back stopped once published leaves phase %s, want %s", j.Phase, backup.PhasePuttingBack)
						}
					}
					if r.journal {
						finishDatabaseRestore(t, f.dataDir)
					} else if _, err := backup.PlanFinishDatabaseRestore(f.dataDir); !errors.Is(err, backup.ErrNoRestoreJournal) {
						t.Fatalf("the way back with no journal = %v", err)
					}
					if _, err := backup.PlanFinishDatabaseRestore(f.dataDir); !errors.Is(err, backup.ErrNoRestoreJournal) {
						t.Fatalf("a second way back = %v, want no journal", err)
					}
					if r.restored {
						if got := markerOf(t, db); got != "snapshot" {
							t.Errorf("the database after %s holds %q, want the restored one", r.step, got)
						}
						if h := harvestOf(t, f.dataDir); h["resultCursor"].(float64) != 0 || h["token"] != "tok-kept" {
							t.Errorf("the harvest after %s = %v, want it rewound", r.step, h)
						}
						return
					}
					if got := f.originals(t); !mapsEqual(got, before) {
						t.Errorf("the originals after %s and the way back are not as they were: %v", r.step, keys(got))
					}
					if h := harvestOf(t, f.dataDir); h["resultCursor"].(float64) != 42 {
						t.Errorf("the harvest after %s was rewound: %v", r.step, h)
					}
					if layout != noDatabase {
						if got := markerOf(t, db); got != "later" {
							t.Errorf("the database after %s holds %q, want the original", r.step, got)
						}
					}
					// Stopped once the journal is gone, the put-back is
					// finished, and what it left the next restore removes.
					if r.journal {
						requireNoJournalNoStage(t, f.dataDir)
					}
				})
			}
		})
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func statErr(p string) error {
	_, err := os.Lstat(p)
	return err
}

// copyFixture is a copy of template in a temp dir of its own, its paths
// moved with it.
func copyFixture(t *testing.T, template *dbFixture) *dbFixture {
	t.Helper()
	dir := resolvedTestDir(t, t.TempDir())
	copyTree(t, template.dataDir, dir)
	rel, err := filepath.Rel(template.dataDir, template.snap)
	if err != nil {
		t.Fatal(err)
	}
	return &dbFixture{dataDir: dir, snap: filepath.Join(dir, rel), yaml: filepath.Join(dir, "bridge.yaml"), cfg: template.cfg, layout: template.layout}
}

// requireAdmission opens the store in a copy of dataDir, as an opener of the
// live database would, and requires it admitted or refused by the journal.
func requireAdmission(t *testing.T, dataDir string, admitted bool) {
	t.Helper()
	dir := t.TempDir()
	copyTree(t, dataDir, dir)
	s, err := manifest.OpenStore(manifest.DefaultDBPath(dir))
	if err == nil {
		_ = s.Close()
	}
	switch {
	case admitted && err != nil:
		t.Errorf("an opener is refused where none should be: %v", err)
	case !admitted && !errors.Is(err, manifest.ErrRestoreIncomplete):
		t.Errorf("an opener beside the journal = %v, want ErrRestoreIncomplete", err)
	}
}

// TestTheWayBackRefusesWhatItCannotProve is T44: a journal another version
// began, an archived original replaced by unrelated bytes of its size, the
// staging name taken while the installed copy is live, and a journal that
// is no regular file, too large or of an unknown format each refuse, from a
// `forward` and a `puttingBack` journal alike, with every file and the
// journal byte for byte as they were.
func TestTheWayBackRefusesWhatItCannotProve(t *testing.T) {
	template := newDBFixture(t, withSidecars)
	for _, phase := range []string{backup.PhaseForward, backup.PhasePuttingBack} {
		for _, tc := range []struct {
			name, step string
			edit       func(t *testing.T, f *dbFixture, j *backup.RestoreJournal)
			want       error
		}{
			{"another version", "C4:synced", func(t *testing.T, f *dbFixture, j *backup.RestoreJournal) {
				j.BridgeVersion = "0.0.0-another"
				writeJournal(t, f.dataDir, j)
			}, backup.ErrRestoreByAnotherVersion},
			{"an unrelated database in the folder", "C4:synced", func(t *testing.T, f *dbFixture, j *backup.RestoreJournal) {
				p := filepath.Join(j.ArchiveDir(f.dataDir), backup.ManifestDBFileName)
				b := readBytes(t, p)
				for i := range b {
					b[i] = byte(i)
				}
				fi, _ := os.Stat(p)
				if err := os.WriteFile(p, b, 0o600); err != nil {
					t.Fatal(err)
				}
				_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
			}, backup.ErrRestoreStopped},
			{"the staging name taken beside the installed copy", "C5", func(t *testing.T, f *dbFixture, j *backup.RestoreJournal) {
				writeFile(t, manifest.RestoreStagingPath(f.dataDir), "something at the staging name")
			}, backup.ErrRestoreStopped},
			{"a journal that is a directory", "C4:synced", func(t *testing.T, f *dbFixture, j *backup.RestoreJournal) {
				p := manifest.RestoreJournalPath(manifest.DefaultDBPath(f.dataDir))
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(p, 0o700); err != nil {
					t.Fatal(err)
				}
			}, backup.ErrRestoreJournalUnread},
			{"a journal too large", "C4:synced", func(t *testing.T, f *dbFixture, j *backup.RestoreJournal) {
				writeFile(t, manifest.RestoreJournalPath(manifest.DefaultDBPath(f.dataDir)), strings.Repeat(" ", 70<<10))
			}, backup.ErrRestoreJournalUnread},
			{"a journal of another format", "C4:synced", func(t *testing.T, f *dbFixture, j *backup.RestoreJournal) {
				j.Format = 2
				writeJournal(t, f.dataDir, j)
			}, backup.ErrRestoreJournalUnread},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				f := copyFixture(t, template)
				stoppedAt(t, f, tc.step, false)
				j := journalOf(t, f.dataDir)
				if phase == backup.PhasePuttingBack {
					j.Phase = phase
					writeJournal(t, f.dataDir, j)
				}
				tc.edit(t, f, j)
				state := dataDirState(t, f.dataDir)
				_, err := backup.PlanFinishDatabaseRestore(f.dataDir)
				if !errors.Is(err, tc.want) {
					t.Fatalf("the way back = %v, want %v", err, tc.want)
				}
				if got := dataDirState(t, f.dataDir); !mapsEqual(got, state) {
					t.Error("the way back's refusal changed what the data dir holds")
				}
			})
		}
	}
}

// TestAPutBackClassifiesEveryNameBeforeItMovesOne is T45: a planted file at
// a live name refuses with no rename made (counted at the rename's seam);
// an original already returned and altered before a rerun refuses; an
// original in neither place refuses; with no original database and its
// sidecars archived, the installed copy goes aside before any sidecar
// returns; and the run's folder stays while anything but its record is in
// it.
func TestAPutBackClassifiesEveryNameBeforeItMovesOne(t *testing.T) {
	t.Run("a planted file", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "C4:synced", false)
		writeFile(t, manifest.DefaultDBPath(f.dataDir)+"-shm", "planted")
		var renames atomic.Int32
		t.Cleanup(fsutil.SetBeforeNoReplaceRenameForTest(func(string, string) { renames.Add(1) }))
		plan, err := backup.PlanFinishDatabaseRestore(f.dataDir)
		if !errors.Is(err, backup.ErrRestoreStopped) || plan != nil {
			t.Fatalf("the plan over a planted file = %v", err)
		}
		if renames.Load() != 0 {
			t.Errorf("%d renames were made before the refusal", renames.Load())
		}
		if j := journalOf(t, f.dataDir); j.Phase != backup.PhaseForward {
			t.Errorf("the journal's phase moved to %s", j.Phase)
		}
	})
	t.Run("an original returned and altered", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "P:back:bridge.db-wal", true)
		writeFile(t, manifest.DefaultDBPath(f.dataDir)+"-wal", "altered after its return")
		if _, err := backup.PlanFinishDatabaseRestore(f.dataDir); !errors.Is(err, backup.ErrRestoreStopped) {
			t.Fatalf("the plan over an altered original = %v", err)
		}
	})
	t.Run("an original in neither place", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "C4:synced", false)
		j := journalOf(t, f.dataDir)
		if err := os.Remove(filepath.Join(j.ArchiveDir(f.dataDir), "bridge.db-wal")); err != nil {
			t.Fatal(err)
		}
		_, err := backup.PlanFinishDatabaseRestore(f.dataDir)
		if !errors.Is(err, backup.ErrRestoreStopped) || !strings.Contains(err.Error(), "neither at its name nor in") {
			t.Fatalf("the plan with an original gone = %v", err)
		}
	})
	t.Run("the installed copy goes aside first", func(t *testing.T) {
		f := newDBFixture(t, noDatabase)
		db := manifest.DefaultDBPath(f.dataDir)
		writeFile(t, db+"-wal", "an orphaned WAL")
		before := f.originals(t)
		stoppedAt(t, f, "C5", false)
		var order []string
		t.Cleanup(fsutil.SetBeforeNoReplaceRenameForTest(func(from, to string) { order = append(order, filepath.Base(from)) }))
		finishDatabaseRestore(t, f.dataDir)
		if len(order) != 2 || order[0] != backup.ManifestDBFileName || order[1] != "bridge.db-wal" {
			t.Errorf("the put-back renamed %v, want the installed copy aside before the WAL's return", order)
		}
		if got := f.originals(t); !mapsEqual(got, before) {
			t.Errorf("after the put-back the names hold %v, want %v", keys(got), keys(before))
		}
	})
	t.Run("the run's folder kept while it holds more than its record", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "C4:synced", false)
		j := journalOf(t, f.dataDir)
		archive := j.ArchiveDir(f.dataDir)
		writeFile(t, filepath.Join(archive, "a note of the operator's"), "kept")
		finishDatabaseRestore(t, f.dataDir)
		if _, err := os.Stat(filepath.Join(archive, "restore.json")); err != nil {
			t.Errorf("the run's record went with something else in its folder: %v", err)
		}
	})
	t.Run("an original identical to the prepared copy", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "C4:synced", false)
		j := journalOf(t, f.dataDir)
		archived := filepath.Join(j.ArchiveDir(f.dataDir), backup.ManifestDBFileName)
		stage := manifest.RestoreStagingPath(f.dataDir)
		prepared := readBytes(t, stage)
		if err := os.WriteFile(archived, prepared, 0o600); err != nil {
			t.Fatal(err)
		}
		for i := range j.Originals {
			if j.Originals[i].Name == backup.ManifestDBFileName {
				j.Originals[i].Size, j.Originals[i].SHA256 = j.Prepared.Size, j.Prepared.SHA256
			}
		}
		writeJournal(t, f.dataDir, j)
		// The installed copy in place, then the stop after it is moved
		// aside and the original returned: the live database is then the
		// original, and nothing moves it again.
		if err := os.Rename(stage, manifest.DefaultDBPath(f.dataDir)); err != nil {
			t.Fatal(err)
		}
		if _, err := backup.PlanFinishDatabaseRestore(f.dataDir); err != nil {
			t.Fatalf("the plan with the installed copy live: %v", err)
		}
		finishDatabaseRestore(t, f.dataDir)
		if got := readBytes(t, manifest.DefaultDBPath(f.dataDir)); !bytes.Equal(got, prepared) {
			t.Error("the original identical to the prepared copy is not back at its name")
		}
		requireNoJournalNoStage(t, f.dataDir)
	})
}

// TestAnUnrelatedDatabaseWhereTheInstallWasDueIsNeverTakenForIt is T38: with
// the originals moved out and the staged copy gone, a valid SQLite file put
// at the database's name is neither the prepared copy nor the original, and
// the way back stops with every file and the journal as they were; and a
// WAL beside an installed copy stops the completion of an `installed`
// restore.
func TestAnUnrelatedDatabaseWhereTheInstallWasDueIsNeverTakenForIt(t *testing.T) {
	t.Run("an unrelated database", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "C4:synced", false)
		db := manifest.DefaultDBPath(f.dataDir)
		if err := os.Remove(manifest.RestoreStagingPath(f.dataDir)); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(t.TempDir(), "other.db")
		s, err := manifest.OpenStore(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(db, readBytes(t, other), 0o600); err != nil {
			t.Fatal(err)
		}
		state := dataDirState(t, f.dataDir)
		if _, err := backup.PlanFinishDatabaseRestore(f.dataDir); !errors.Is(err, backup.ErrRestoreStopped) {
			t.Fatalf("the way back over an unrelated database = %v, want a stop", err)
		}
		if got := dataDirState(t, f.dataDir); !mapsEqual(got, state) {
			t.Error("the stop changed what the data dir holds")
		}
	})
	t.Run("a WAL beside the installed copy", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		stoppedAt(t, f, "C5'", false)
		db := manifest.DefaultDBPath(f.dataDir)
		writeFile(t, db+"-wal", "a WAL something opened beside the restored database")
		state := dataDirState(t, f.dataDir)
		_, err := backup.PlanFinishDatabaseRestore(f.dataDir)
		if !errors.Is(err, backup.ErrRestoreStopped) || !strings.Contains(err.Error(), "bridge.db-wal") {
			t.Fatalf("the completion beside a WAL = %v, want a stop naming it", err)
		}
		if got := dataDirState(t, f.dataDir); !mapsEqual(got, state) {
			t.Error("the stop changed what the data dir holds")
		}
	})
}

// TestAnEntryMadeAtAPutBacksTargetIsNeverReplaced is T22's race row: an entry
// made at an original's live name just before the put-back's rename of it
// (fsutil's seam before the call) is never renamed over; the put-back stops
// with the journal kept, the entry's bytes and the archived original as
// they were.
func TestAnEntryMadeAtAPutBacksTargetIsNeverReplaced(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	stoppedAt(t, f, "C4:synced", false)
	j := journalOf(t, f.dataDir)
	wal := manifest.DefaultDBPath(f.dataDir) + "-wal"
	archived := filepath.Join(j.ArchiveDir(f.dataDir), "bridge.db-wal")
	original := readBytes(t, archived)
	var planted atomic.Bool
	t.Cleanup(fsutil.SetBeforeNoReplaceRenameForTest(func(from, to string) {
		if to == wal && planted.CompareAndSwap(false, true) {
			writeFile(t, wal, "made at the instant before the rename")
		}
	}))
	plan, err := backup.PlanFinishDatabaseRestore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	err = backup.FinishDatabaseRestore(f.dataDir, plan, backup.FinishOptions{})
	if !errors.Is(err, backup.ErrRestoreStopped) {
		t.Fatalf("the put-back over an entry made at its target = %v, want a stop", err)
	}
	if got := string(readBytes(t, wal)); got != "made at the instant before the rename" {
		t.Errorf("the entry at the target was replaced: %q", got)
	}
	if !bytes.Equal(readBytes(t, archived), original) {
		t.Error("the archived WAL moved or changed")
	}
	if journalOf(t, f.dataDir).Phase != backup.PhasePuttingBack {
		t.Error("the put-back's stop did not keep its journal at puttingBack")
	}
}

// TestTheSwapsOwnPutBackClassifiesBeforeItMoves is the in-process put-back's
// half of T45: a file planted at a sidecar's live name once C3 moved it, then
// a failure of C4's rename, makes the swap's put-back stop before it moves
// anything, its journal still `forward`.
func TestTheSwapsOwnPutBackClassifiesBeforeItMoves(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	wal := manifest.DefaultDBPath(f.dataDir) + "-wal"
	t.Cleanup(backup.SetRestoreStepForTest(func(step string) {
		if step == "C3:bridge.db-journal:synced" {
			writeFile(t, wal, "planted once the WAL was moved out")
		}
	}))
	t.Cleanup(backup.SetSwapFaultForTest(func(from, to string) error {
		if filepath.Base(from) == backup.ManifestDBFileName && strings.Contains(to, backup.OriginalsDirName) {
			return errors.New("held by another program")
		}
		return nil
	}))
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if !errors.Is(err, backup.ErrRestoreStopped) {
		t.Fatalf("RestoreDatabase = %v, want the put-back's stop", err)
	}
	if j := journalOf(t, f.dataDir); j.Phase != backup.PhaseForward {
		t.Errorf("the put-back published %s before its classification refused", j.Phase)
	}
	if got := string(readBytes(t, wal)); got != "planted once the WAL was moved out" {
		t.Errorf("the planted file changed: %q", got)
	}
}

// TestAConfigChangedDuringThePreparationRestoresNothing is T17's preparation
// row: bridge.yaml edited while the staged copy is prepared, its lock let go,
// refuses at the turn taken again before the record, the stage removed and
// no original moved.
func TestAConfigChangedDuringThePreparationRestoresNothing(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	before := f.originals(t)
	t.Cleanup(backup.SetRestoreStepForTest(func(step string) {
		if step == "B9" {
			writeFile(t, f.yaml, string(f.cfg)+"# edited while the copy was prepared\n")
		}
	}))
	_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
	if err == nil || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("RestoreDatabase with the config edited during its preparation = %v, want a refusal", err)
	}
	requireUnchanged(t, f, before)
}
