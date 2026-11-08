package backup_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// storeWithTracks primes a live state whose store holds n tracks, so the
// snapshot's database has b-tree pages a damaged one can be one of.
func storeWithTracks(t *testing.T, n int) backup.Sources {
	t.Helper()
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	st, err := manifest.OpenStore(src.ManifestDB)
	if err != nil {
		t.Fatal(err)
	}
	tracks := make([]*manifest.Track, 0, n)
	for i := 0; i < n; i++ {
		tracks = append(tracks, &manifest.Track{
			Path: fmt.Sprintf("Artist %02d/Album/%03d Ünïcode.flac", i/10, i), Size: int64(1000 + i),
			Title: fmt.Sprint("Track ", i), Artist: fmt.Sprint("Artist ", i/10), Album: "Album",
		})
	}
	if err := st.UpsertTrackBatch(t.Context(), tracks); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return src
}

// readManifestT reads the snapshot's manifest.json.
func readManifestT(t *testing.T, dir string) backup.Manifest {
	t.Helper()
	var m backup.Manifest
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, backup.ManifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// writeManifestT writes m as the snapshot's manifest.json.
func writeManifestT(t *testing.T, dir string, m backup.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rebaseline records name's bytes as they are now as its baseline: damage
// the copy carried when the snapshot hashed it.
func rebaseline(t *testing.T, dir, name string) {
	t.Helper()
	data := readBytes(t, filepath.Join(dir, name))
	sum := sha256.Sum256(data)
	m := readManifestT(t, dir)
	for i := range m.Baselines {
		if m.Baselines[i].Name == name {
			m.Baselines[i].Size, m.Baselines[i].SHA256 = int64(len(data)), hex.EncodeToString(sum[:])
		}
	}
	writeManifestT(t, dir, m)
}

// listed is the verdict List reads for the snapshot in dir: the reading the
// pruner, the startup skip, the doctor and the console take.
func listed(t *testing.T, dir string) backup.Verification {
	t.Helper()
	entries, err := backup.List(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.DirName == filepath.Base(dir) {
			return e.Verdict
		}
	}
	t.Fatalf("List does not list %s", filepath.Base(dir))
	return backup.Verification{}
}

// verifyT verifies the snapshot in dir at level and requires the verdict
// recorded.
func verifyT(t *testing.T, dir string, level backup.CheckLevel) backup.VerifyResult {
	t.Helper()
	res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{Level: level})
	if err != nil {
		t.Fatalf("VerifySnapshot: %v", err)
	}
	if !res.Recorded {
		t.Fatalf("VerifySnapshot recorded nothing: %+v", res)
	}
	return res
}

// requireVerdict requires v's two verdicts and their first reasons.
func requireVerdict(t *testing.T, what string, v backup.Verification, db backup.State, dbReason backup.Reason, files backup.State, filesReason backup.Reason) {
	t.Helper()
	if v.DatabaseState != db || (dbReason != "" && !slices.Contains(v.DatabaseReasons, dbReason)) ||
		(dbReason == "" && len(v.DatabaseReasons) != 0) {
		t.Errorf("%s: database %s %v, want %s %q", what, v.DatabaseState, v.DatabaseReasons, db, dbReason)
	}
	if v.FilesState != files || (filesReason != "" && !slices.Contains(v.FilesReasons, filesReason)) ||
		(filesReason == "" && len(v.FilesReasons) != 0) {
		t.Errorf("%s: files %s %v, want %s %q", what, v.FilesState, v.FilesReasons, files, filesReason)
	}
}

// names is what the folder at dir holds, sorted.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestASnapshotRecordsItsBaselinesAndPublishesPending pins the capture's
// half (backlog B653): every captured file's size and SHA-256 are in the
// manifest Snapshot publishes, and its record is PENDING from the start, so
// no bundle is ever without one. Red on main: the manifest held file names
// alone and no record.
func TestASnapshotRecordsItsBaselinesAndPublishesPending(t *testing.T) {
	src := storeWithTracks(t, 20)
	before := time.Now().UTC()
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, dir)
	if len(m.Baselines) != len(m.Files) {
		t.Fatalf("%d baselines for %d files: %+v", len(m.Baselines), len(m.Files), m.Baselines)
	}
	for i, name := range m.Files {
		b := m.Baselines[i]
		data := readBytes(t, filepath.Join(dir, name))
		sum := sha256.Sum256(data)
		if b.Name != name || b.Size != int64(len(data)) || b.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("baseline %d is %+v, want %s's size %d and sha256 %x", i, b, name, len(data), sum)
		}
	}
	v := m.Verification
	if v == nil || v.DatabaseState != backup.StatePending || v.FilesState != backup.StatePending {
		t.Fatalf("the published record is %+v, want both verdicts PENDING", v)
	}
	if v.PendingSince.Before(before.Add(-time.Second)) || v.PendingSince.After(time.Now().Add(time.Second)) {
		t.Errorf("pendingSince %v is not the capture's time", v.PendingSince)
	}
	requireVerdict(t, "a fresh snapshot as List reads it", listed(t, dir), backup.StatePending, "", backup.StatePending, "")
}

// TestAVerifiedSnapshotRecordsItsCheck pins a passing check's record: both
// verdicts VERIFIED, the quick level, the store's user_version and the core
// tables' row counts (information: 0 is a count), the baselines untouched.
func TestAVerifiedSnapshotRecordsItsCheck(t *testing.T) {
	src := storeWithTracks(t, 30)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	baselines := readManifestT(t, dir).Baselines
	res := verifyT(t, dir, "")
	requireVerdict(t, "a snapshot of a sound store", res.Record, backup.StateVerified, "", backup.StateVerified, "")
	rec := readManifestT(t, dir)
	if rec.Verification.Level != backup.CheckQuick || rec.Verification.UserVersion != manifest.LadderTop() {
		t.Errorf("level %q, user_version %d; want quick and %d", rec.Verification.Level, rec.Verification.UserVersion, manifest.LadderTop())
	}
	want := map[string]int64{"tracks": 30, "folders": 0, "playlists": 0, "playlist_items": 0}
	if fmt.Sprint(rec.Verification.Counts) != fmt.Sprint(want) {
		t.Errorf("counts %v, want %v", rec.Verification.Counts, want)
	}
	if !rec.Verification.PendingSince.IsZero() {
		t.Errorf("a verdict keeps the PENDING's time %v", rec.Verification.PendingSince)
	}
	if fmt.Sprint(rec.Baselines) != fmt.Sprint(baselines) {
		t.Errorf("the verification replaced the baselines:\n%v\nwas\n%v", rec.Baselines, baselines)
	}
	requireVerdict(t, "List's reading", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
}

// tracksRootPage is the root page of the snapshot's tracks table.
func tracksRootPage(t *testing.T, db string) int {
	t.Helper()
	conn, err := sql.Open("sqlite", dsn.File(db, "mode=ro&immutable=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var page int
	if err := conn.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type = 'table' AND name = 'tracks'`).Scan(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

// damagePage writes a page type no b-tree page has over page n of the
// database at path (pages are 4096 bytes, as SQLite makes them by default).
func damagePage(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, int64(n-1)*4096); err != nil {
		t.Fatal(err)
	}
}

// TestADamagedDatabasePageFails is the brief's first red: a damaged page of
// the snapshot's database FAILS the database's verdict with its reason, and
// leaves the bundle on disk. Damage after the capture differs from the
// baseline (hash-mismatch); damage the copy carried when it was hashed is
// SQLite's own check's to find (quick-check). Red on main: no verification.
func TestADamagedDatabasePageFails(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inBaseline bool
		want       backup.Reason
	}{
		{"after the capture", false, backup.ReasonHash},
		{"in the copy the capture hashed", true, backup.ReasonQuickCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := storeWithTracks(t, 400)
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dir, backup.ManifestDBFileName)
			damagePage(t, db, tracksRootPage(t, db))
			if tc.inBaseline {
				rebaseline(t, dir, backup.ManifestDBFileName)
			}
			res := verifyT(t, dir, backup.CheckQuick)
			requireVerdict(t, "a damaged page", res.Record, backup.StateFailed, tc.want, backup.StateVerified, "")
			requireVerdict(t, "List's reading", listed(t, dir), backup.StateFailed, tc.want, backup.StateVerified, "")
			if !pathExists(t, db) || !pathExists(t, filepath.Join(dir, backup.ManifestFile)) {
				t.Error("a failed verification removed the bundle's files")
			}
		})
	}
}

// TestAFileWithOneByteChangedFailsTheFilesVerdict is the brief's second
// half of that red: a file the manifest lists, one byte changed and its size
// kept, FAILS the files' verdict (hash-mismatch) naming the file, while the
// database's stays VERIFIED, and the bundle stays on disk.
func TestAFileWithOneByteChangedFailsTheFilesVerdict(t *testing.T) {
	src := storeWithTracks(t, 10)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(dir, "tokens.json")
	data := readBytes(t, tokens)
	data[0] ^= 0x01
	if err := os.WriteFile(tokens, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res := verifyT(t, dir, "")
	requireVerdict(t, "a byte changed in tokens.json", res.Record, backup.StateVerified, "", backup.StateFailed, backup.ReasonHash)
	if got := res.Record.FilesFailed; len(got) != 1 || got[0].Name != "tokens.json" || got[0].Reason != backup.ReasonHash {
		t.Errorf("filesFailed = %+v, want tokens.json hash-mismatch", got)
	}
	if !pathExists(t, tokens) {
		t.Error("a failed verification removed the file")
	}
}

// TestACertificateThatDoesNotMatchItsBaselineStillAnchorsRecovery is the
// brief's two-verdict red: a server.crt that differs from its baseline FAILS
// the files' verdict and leaves the database's VERIFIED, and the snapshot is
// still the newest database-verified one: the prune keeps it as the anchor
// and lets an older fully verified one go.
func TestACertificateThatDoesNotMatchItsBaselineStillAnchorsRecovery(t *testing.T) {
	src := storeWithTracks(t, 10)
	older := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	crt := filepath.Join(dir, "server.crt")
	data := readBytes(t, crt)
	data[len(data)/2] ^= 0x20
	if err := os.WriteFile(crt, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res := verifyT(t, dir, "")
	requireVerdict(t, "a certificate unlike its baseline", res.Record, backup.StateVerified, "", backup.StateFailed, backup.ReasonHash)
	st, err := backup.ReadStatus(t.Context(), filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if nv, ok := st.NewestVerified(); !ok || nv.DirName != filepath.Base(dir) {
		t.Errorf("the newest database-verified snapshot is %q (%v), want %q", nv.DirName, ok, filepath.Base(dir))
	}
	if _, err := backup.Prune(filepath.Dir(dir), 1); err != nil {
		t.Fatal(err)
	}
	if !pathExists(t, dir) || pathExists(t, older) {
		t.Errorf("after a prune to 1: the certificate's snapshot there %v, the older one there %v; want it kept as the anchor and the older one gone",
			pathExists(t, dir), pathExists(t, older))
	}
}

// requireNoOpen fails the test if SQLite opens a snapshot's database before
// restore is called.
func requireNoOpen(t *testing.T) (opened func() bool) {
	t.Helper()
	var mu sync.Mutex
	var did bool
	t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
		mu.Lock()
		did = true
		mu.Unlock()
	}))
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return did
	}
}

// TestASidecarBesideTheDatabaseFailsBeforeSQLiteOpens is the brief's red for
// the open's preconditions: a -wal, -shm or -journal beside the snapshot's
// database FAILS it (sidecar) before SQLite opens anything, whatever the
// entry is.
func TestASidecarBesideTheDatabaseFailsBeforeSQLiteOpens(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, backup.ManifestDBFileName+suffix), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			opened := requireNoOpen(t)
			res := verifyT(t, dir, "")
			requireVerdict(t, "a "+suffix+" beside the database", res.Record, backup.StateFailed, backup.ReasonSidecar, backup.StateVerified, "")
			if opened() {
				t.Error("SQLite opened the database beside a sidecar")
			}
		})
	}
}

// TestALinkInTheBundleFailsBeforeSQLiteOpens: a link where the database or a
// listed file goes is never followed: the database FAILS not-regular before
// SQLite opens, and a linked file FAILS the files' verdict.
func TestALinkInTheBundleFailsBeforeSQLiteOpens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a symbolic link needs a privilege on Windows")
	}
	t.Run("the database", func(t *testing.T) {
		src := storeWithTracks(t, 5)
		dir, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(dir, backup.ManifestDBFileName)
		elsewhere := filepath.Join(t.TempDir(), "copy.db")
		if err := os.Rename(db, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, db); err != nil {
			t.Fatal(err)
		}
		opened := requireNoOpen(t)
		res := verifyT(t, dir, "")
		requireVerdict(t, "a link at the database", res.Record, backup.StateFailed, backup.ReasonNotRegular, backup.StateVerified, "")
		if opened() {
			t.Error("SQLite opened a database reached through a link")
		}
	})
	t.Run("a listed file", func(t *testing.T) {
		src := storeWithTracks(t, 5)
		dir, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		crt := filepath.Join(dir, "server.crt")
		elsewhere := filepath.Join(t.TempDir(), "server.crt")
		if err := os.Rename(crt, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, crt); err != nil {
			t.Fatal(err)
		}
		res := verifyT(t, dir, "")
		requireVerdict(t, "a link at server.crt", res.Record, backup.StateVerified, "", backup.StateFailed, backup.ReasonNotRegular)
	})
}

// TestVerifyingAWALHeaderDatabaseTwiceCreatesNoFile is the brief's red for
// the immutable open: a snapshot whose database's header is in WAL mode (as
// the live store's is; VACUUM INTO's own copy is in rollback mode) verifies
// VERIFIED twice, and neither run creates a file in the bundle: a mode=ro
// open alone creates a -shm and a -wal beside it, which the second run would
// then refuse as sidecars.
func TestVerifyingAWALHeaderDatabaseTwiceCreatesNoFile(t *testing.T) {
	src := storeWithTracks(t, 25)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, backup.ManifestDBFileName)
	live := readBytes(t, src.ManifestDB)
	if live[18] != 2 || live[19] != 2 {
		t.Fatalf("the live store's header reads %d/%d, want WAL mode (2/2)", live[18], live[19])
	}
	if err := os.WriteFile(db, live, 0o600); err != nil {
		t.Fatal(err)
	}
	rebaseline(t, dir, backup.ManifestDBFileName)
	before := names(t, dir)
	for run := 1; run <= 2; run++ {
		res := verifyT(t, dir, backup.CheckQuick)
		requireVerdict(t, fmt.Sprintf("run %d", run), res.Record, backup.StateVerified, "", backup.StateVerified, "")
		if got := names(t, dir); fmt.Sprint(got) != fmt.Sprint(before) {
			t.Fatalf("run %d left the bundle holding %v, it held %v", run, got, before)
		}
	}
	if got := readBytes(t, db); string(got) != string(live) {
		t.Error("a verification changed the database's bytes")
	}
}

// setUserVersion stamps the snapshot's database with user_version v.
func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		t.Fatal(err)
	}
}

// TestTheDatabaseMustBeASchemaThisBridgeKnows is the brief's schema red: a
// user_version past this bridge's ladder FAILS (schema-newer), so does a
// core table its version requires and the store lacks (schema-table), and an
// empty library passes with every count 0.
func TestTheDatabaseMustBeASchemaThisBridgeKnows(t *testing.T) {
	t.Run("a newer user_version", func(t *testing.T) {
		src := storeWithTracks(t, 3)
		dir, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(dir, backup.ManifestDBFileName)
		setUserVersion(t, db, manifest.LadderTop()+1)
		rebaseline(t, dir, backup.ManifestDBFileName)
		res := verifyT(t, dir, "")
		requireVerdict(t, "a newer store", res.Record, backup.StateFailed, backup.ReasonSchemaNewer, backup.StateVerified, "")
	})
	t.Run("a core table missing", func(t *testing.T) {
		src := storeWithTracks(t, 3)
		dir, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(dir, backup.ManifestDBFileName)
		conn, err := sql.Open("sqlite", dsn.File(db, ""))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`DROP TABLE playlist_items`); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		rebaseline(t, dir, backup.ManifestDBFileName)
		res := verifyT(t, dir, "")
		requireVerdict(t, "a store without playlist_items", res.Record, backup.StateFailed, backup.ReasonSchemaTable, backup.StateVerified, "")
	})
	t.Run("an empty library", func(t *testing.T) {
		src := primeLiveState(t, t.TempDir())
		dir, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		res := verifyT(t, dir, backup.CheckFull)
		requireVerdict(t, "an empty library", res.Record, backup.StateVerified, "", backup.StateVerified, "")
		for _, table := range []string{"tracks", "folders", "playlists", "playlist_items"} {
			if n, ok := res.Record.Counts[table]; !ok || n != 0 {
				t.Errorf("%s counts %d (%v), want 0", table, n, ok)
			}
		}
	})
}

// TestTheFullCheckEvaluatesTheExpressionIndexes: integrity_check compares
// every index with its table, the unicode_lower expression indexes among
// them, so it needs the functions the store registers; over a store with
// rows in those indexes the full check passes, the foreign keys too.
func TestTheFullCheckEvaluatesTheExpressionIndexes(t *testing.T) {
	src := storeWithTracks(t, 200)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	res := verifyT(t, dir, backup.CheckFull)
	requireVerdict(t, "the full check", res.Record, backup.StateVerified, "", backup.StateVerified, "")
	if got := readManifestT(t, dir).Verification.Level; got != backup.CheckFull {
		t.Errorf("level %q, want full", got)
	}
}

// TestAQuickCheckNeverUndoesAFullOne pins the record's merge: a verdict
// never replaces one of a higher check level unless it is worse.
func TestAQuickCheckNeverUndoesAFullOne(t *testing.T) {
	src := storeWithTracks(t, 50)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	full := verifyT(t, dir, backup.CheckFull)
	quick := verifyT(t, dir, backup.CheckQuick)
	if quick.Record.Level != backup.CheckFull || !quick.Record.CheckedAt.Equal(full.Record.CheckedAt) {
		t.Errorf("a quick check over a full one left level %q checked at %v, want the full check's at %v",
			quick.Record.Level, quick.Record.CheckedAt, full.Record.CheckedAt)
	}
	db := filepath.Join(dir, backup.ManifestDBFileName)
	damagePage(t, db, tracksRootPage(t, db))
	worse := verifyT(t, dir, backup.CheckQuick)
	requireVerdict(t, "a quick check that fails over a full one", worse.Record, backup.StateFailed, backup.ReasonHash, backup.StateVerified, "")
}

// TestACancelledVerificationWritesIncomplete: a verification its caller
// stops writes INCOMPLETE (cancelled), and the stop is its answer.
func TestACancelledVerificationWritesIncomplete(t *testing.T) {
	src := storeWithTracks(t, 20)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	// One byte a second: the check's first read waits, and the stop finds
	// it there.
	t.Cleanup(backup.SetVerifyReadRateForTest(1))
	started := make(chan struct{})
	var once sync.Once
	t.Cleanup(backup.SetOnLowPriorityForTest(func() { once.Do(func() { close(started) }) }))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a stopped verification answered %v, want the stop", err)
	}
	requireVerdict(t, "a stopped check", listed(t, dir), backup.StateIncomplete, backup.ReasonCancelled, backup.StateIncomplete, backup.ReasonCancelled)
}

// TestAStopWithABlockedSyncReturnsWithinItsBound is the brief's shutdown
// red: a verification stopped while its verdict's write is held in a sync
// that never answers returns within StopWriteBound of the stop, abandoning
// the write, and so does one whose INCOMPLETE meets that sync.
func TestAStopWithABlockedSyncReturnsWithinItsBound(t *testing.T) {
	for _, stopDuring := range []string{"the check", "the write"} {
		t.Run(stopDuring, func(t *testing.T) {
			src := storeWithTracks(t, 20)
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			inSync := make(chan struct{}, 1)
			entered := make(chan struct{})
			var enter sync.Once
			t.Cleanup(func() { close(release) })
			t.Cleanup(backup.SetSyncVerdictForTest(func(string) error {
				enter.Do(func() { close(entered) })
				select {
				case inSync <- struct{}{}:
				default:
				}
				<-release
				return nil
			}))
			// The abandoned write has read the seam before the test puts it
			// back: it is held in it.
			defer func() {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Error("the verdict's write never reached its sync")
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if stopDuring == "the check" {
				t.Cleanup(backup.SetVerifyReadRateForTest(1))
				started := make(chan struct{})
				var once sync.Once
				t.Cleanup(backup.SetOnLowPriorityForTest(func() { once.Do(func() { close(started) }) }))
				go func() { <-started; time.Sleep(20 * time.Millisecond); cancel() }()
			} else {
				go func() { <-inSync; cancel() }()
			}
			done := make(chan time.Time, 1)
			var stoppedAt time.Time
			go func() {
				_ = backup.Finish(ctx, dir, func() int { return 1 }, backup.VerifyOptions{})
				done <- time.Now()
			}()
			<-ctx.Done()
			stoppedAt = time.Now()
			select {
			case end := <-done:
				if took := end.Sub(stoppedAt); took > backup.StopWriteBound+time.Second {
					t.Errorf("Finish returned %v after the stop; its bound is %v", took, backup.StopWriteBound)
				}
			case <-time.After(backup.StopWriteBound + 5*time.Second):
				t.Fatal("Finish did not return after the stop while its write was held in a sync")
			}
		})
	}
}

// TestAStalePendingReadsIncompleteAndAYoungOneSurvivesAPrune: a PENDING
// older than PendingBound reads INCOMPLETE (interrupted) wherever it is
// read, and one younger is never pruned, beyond keep or not.
func TestAStalePendingReadsIncompleteAndAYoungOneSurvivesAPrune(t *testing.T) {
	src := storeWithTracks(t, 5)
	stale, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, stale)
	m.CreatedAt = m.CreatedAt.Add(-2 * time.Hour)
	m.Verification.PendingSince = time.Now().Add(-backup.PendingBound - time.Minute)
	writeManifestT(t, stale, m)
	requireVerdict(t, "a PENDING past its bound", listed(t, stale), backup.StateIncomplete, backup.ReasonInterrupted,
		backup.StateIncomplete, backup.ReasonInterrupted)

	time.Sleep(1100 * time.Millisecond)
	young, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	// Make the young PENDING the oldest snapshot, past keep.
	m = readManifestT(t, young)
	m.CreatedAt = m.CreatedAt.Add(-24 * time.Hour)
	writeManifestT(t, young, m)
	time.Sleep(1100 * time.Millisecond)
	newer := verifiedSnapshot(t, src)
	if _, err := backup.Prune(filepath.Dir(newer), 1); err != nil {
		t.Fatal(err)
	}
	if !pathExists(t, young) {
		t.Error("a PENDING younger than its bound was pruned")
	}
	requireVerdict(t, "the young one", listed(t, young), backup.StatePending, "", backup.StatePending, "")
}

// TestASyncFailureAfterTheVerdictsRenameReadsAsNotVerified is the brief's
// publication red, in this package's readers: a verdict whose folder sync
// fails once its rename has landed answers an error, and no reader reads
// VERIFIED: List (the pruner's and the startup skip's reading) and
// ReadStatus (the doctor's and the console's) read INCOMPLETE (unsynced),
// and the prune takes the snapshot for evidence, not the anchor.
// internal/admin's TestAnUnsyncedVerdictReadsAsNotVerifiedEverywhere reads it
// through the doctor and the console's API too.
func TestASyncFailureAfterTheVerdictsRenameReadsAsNotVerified(t *testing.T) {
	src := storeWithTracks(t, 5)
	anchor := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	restore := backup.SetSyncVerdictForTest(func(string) error { return errors.New("injected: the directory would not sync") })
	_, err = backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
	restore()
	if !errors.Is(err, backup.ErrVerdictNotDurable) {
		t.Fatalf("VerifySnapshot answered %v, want ErrVerdictNotDurable", err)
	}
	if m := readManifestT(t, dir); m.Verification.DatabaseState != backup.StateVerified {
		t.Fatalf("the rename did not land (the record holds %s): the test did not reach the window", m.Verification.DatabaseState)
	}
	requireVerdict(t, "List's reading", listed(t, dir), backup.StateIncomplete, backup.ReasonUnsynced, backup.StateIncomplete, backup.ReasonUnsynced)
	st, err := backup.ReadStatus(t.Context(), filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if nv, ok := st.NewestVerified(); !ok || nv.DirName != filepath.Base(anchor) {
		t.Errorf("the newest database-verified snapshot is %q, want the older anchor %q", nv.DirName, filepath.Base(anchor))
	}
	if _, err := backup.Prune(filepath.Dir(dir), 1); err != nil {
		t.Fatal(err)
	}
	if !pathExists(t, anchor) {
		t.Error("the prune let the anchor go for a snapshot whose verdict is not durable")
	}
}

// TestALegacyManifestReadsUnverified: a snapshot written before
// verification (no baselines, no record) reads UNVERIFIED (no-baseline),
// never verified and never corrupt; a verification checks its files for
// presence alone and records nothing.
func TestALegacyManifestReadsUnverified(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, dir)
	m.Baselines, m.Verification = nil, nil
	writeManifestT(t, dir, m)
	before := readBytes(t, filepath.Join(dir, backup.ManifestFile))
	requireVerdict(t, "a legacy snapshot", listed(t, dir), backup.StateUnverified, backup.ReasonNoBaseline,
		backup.StateUnverified, backup.ReasonNoBaseline)
	res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{Level: backup.CheckFull})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Legacy || res.Recorded {
		t.Errorf("a legacy snapshot's verification: legacy %v, recorded %v; want legacy and nothing recorded", res.Legacy, res.Recorded)
	}
	requireVerdict(t, "a legacy snapshot's check", res.Found, backup.StateUnverified, backup.ReasonNoBaseline,
		backup.StateUnverified, backup.ReasonNoBaseline)
	if string(readBytes(t, filepath.Join(dir, backup.ManifestFile))) != string(before) {
		t.Error("a legacy snapshot's verification rewrote its manifest")
	}
	if err := os.Remove(filepath.Join(dir, "tokens.json")); err != nil {
		t.Fatal(err)
	}
	res, err = backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Found.FilesState != backup.StateUnverified || !slices.Contains(res.Found.FilesReasons, backup.ReasonMissing) {
		t.Errorf("a legacy snapshot missing a listed file reads files %s %v, want UNVERIFIED with missing", res.Found.FilesState, res.Found.FilesReasons)
	}
}

// TestTheReadsArePaced: a verification reads no faster than its rate.
func TestTheReadsArePaced(t *testing.T) {
	src := storeWithTracks(t, 300)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, b := range readManifestT(t, dir).Baselines {
		size += b.Size
	}
	const rate = 1 << 20
	t.Cleanup(backup.SetVerifyReadRateForTest(rate))
	start := time.Now()
	verifyT(t, dir, "")
	// The hash reads every byte once at least, and SQLite's check reads the
	// database again: the lower bound is the hash alone.
	if floor := time.Duration(float64(size) / rate * float64(time.Second) * 0.9); time.Since(start) < floor {
		t.Errorf("a verification of %d bytes at %d B/s took %v, under %v", size, rate, time.Since(start), floor)
	}
}

// TestTheVerificationRunsOnAThreadOfItsOwn: the check's thread is locked to
// its goroutine and its priority lowered first.
func TestTheVerificationRunsOnAThreadOfItsOwn(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan int, 1)
	t.Cleanup(backup.SetOnLowPriorityForTest(func() { ran <- threadPriority() }))
	verifyT(t, dir, "")
	select {
	case p := <-ran:
		if runtime.GOOS == "linux" && p != 19 {
			t.Errorf("the check's thread runs at nice %d, want 19", p)
		}
	default:
		t.Fatal("the check did not run on the lowered thread")
	}
}

// TestAPrunerRacingAVerificationSkipsItsRound is the brief's lock red,
// deterministic: a prune while a verification holds the backups folder's
// lock (parked between its check and its write) deletes nothing and says it
// skipped, and the next prune, once the verification is done, prunes.
func TestAPrunerRacingAVerificationSkipsItsRound(t *testing.T) {
	src := storeWithTracks(t, 5)
	old := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	parked, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(backup.SetAfterCheckForTest(func(string) {
		close(parked)
		<-release
	}))
	done := make(chan error, 1)
	go func() {
		_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
		done <- err
	}()
	<-parked
	res, err := backup.Prune(filepath.Dir(dir), 1)
	if err != nil || !res.Skipped || res.Deleted != 0 || !pathExists(t, old) {
		t.Errorf("a prune beside a verification holding the lock: %+v, %v, the older snapshot there %v; want skipped, nothing deleted",
			res, err, pathExists(t, old))
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res, err = backup.Prune(filepath.Dir(dir), 1)
	if err != nil || res.Skipped || res.Deleted != 1 || pathExists(t, old) {
		t.Errorf("the prune after the verification: %+v, %v; want the older snapshot deleted", res, err)
	}
}

// TestTwoVerificationsOfOneSnapshotWriteOneAtATime: two verifications of one
// snapshot in one process run one after the other, and neither loses or
// downgrades the other's verdict: whichever order, the record ends at the
// full check's.
func TestTwoVerificationsOfOneSnapshotWriteOneAtATime(t *testing.T) {
	src := storeWithTracks(t, 30)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, level := range []backup.CheckLevel{backup.CheckQuick, backup.CheckFull} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{Level: level})
			if err == nil && !res.Recorded {
				err = fmt.Errorf("the %s check recorded nothing", level)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	rec := readManifestT(t, dir).Verification
	if rec.DatabaseState != backup.StateVerified || rec.Level != backup.CheckFull {
		t.Errorf("the record ends %s at %q, want VERIFIED at the full check's level", rec.DatabaseState, rec.Level)
	}
}

// TestAFilesystemThatKeepsNoLocksStillVerifies: where the lock cannot be
// had at all (ENOLCK, EOPNOTSUPP), a verification goes on without it.
func TestAFilesystemThatKeepsNoLocksStillVerifies(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetTryLockBackupsForTest(func(*os.File) error { return errors.ErrUnsupported }))
	res := verifyT(t, dir, "")
	requireVerdict(t, "a verification without locks", res.Record, backup.StateVerified, "", backup.StateVerified, "")
}
