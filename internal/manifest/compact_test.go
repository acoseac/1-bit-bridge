package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

func seedForCompact(t *testing.T, s *Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		tr := &Track{
			Path:    fmt.Sprintf("Artist%02d/Album/%05d Track.flac", i%20, i),
			Title:   fmt.Sprintf("Track number %d with some padding to make rows real", i),
			Artist:  fmt.Sprintf("Artist %02d", i%20),
			Album:   "An Album Of Things",
			Size:    1000,
			ModTime: time.Unix(1, 0),
		}
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
}

// TestCompactReclaimsFreePagesAndShrinksTheFile asserts ALL THREE
// outcomes, and the middle one is the load-bearing assertion: a test that
// only checked freelist_count passes against a compaction that reclaims
// nothing, because the VACUUM's output sits in the WAL until it is
// checkpointed.
func TestCompactReclaimsFreePagesAndShrinksTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	seedForCompact(t, s, 4000)
	for i := 0; i < 3500; i++ {
		if err := s.DeleteTrack(ctx, fmt.Sprintf("Artist%02d/Album/%05d Track.flac", i%20, i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}

	pre, err := s.PageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pre.FreelistCount == 0 {
		t.Fatal("fixture produced no free pages; the test would prove nothing")
	}

	res, err := s.Compact(ctx, nil)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.CheckpointBusy {
		t.Fatal("post-VACUUM checkpoint reported busy in a single-connection test")
	}

	post, err := s.PageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if post.FreelistCount != 0 {
		t.Errorf("freelist_count = %d after compact, want 0", post.FreelistCount)
	}
	// THE assertion. Without the post-VACUUM checkpoint the file is
	// byte-for-byte unchanged here while freelist_count still reads 0.
	if res.AfterBytes >= res.BeforeBytes {
		t.Errorf("file did not shrink: before=%d after=%d — a compaction that reclaims nothing",
			res.BeforeBytes, res.AfterBytes)
	}
	walSize := int64(-1)
	if fi, err := os.Stat(p + "-wal"); err == nil {
		walSize = fi.Size()
	} else if os.IsNotExist(err) {
		walSize = 0
	}
	if walSize != 0 {
		t.Errorf("-wal = %d bytes after compact, want 0 (truncating checkpoint did not land)", walSize)
	}
	t.Logf("before=%d after=%d reclaimed=%d freelist %d -> %d",
		res.BeforeBytes, res.AfterBytes, res.BeforeBytes-res.AfterBytes,
		pre.FreelistCount, post.FreelistCount)
}

func TestCompactRefusesWithoutDiskHeadroom(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedForCompact(t, s, 200)

	// Report less free space than the file itself, let alone 2x.
	_, err = s.Compact(context.Background(), func(string) (int64, error) { return 1, nil })
	if !errors.Is(err, ErrInsufficientDiskSpace) {
		t.Fatalf("want ErrInsufficientDiskSpace, got %v", err)
	}

	// A probe that fails is an error, not a silent skip: not knowing how
	// much room there is is not the same as knowing there is enough.
	sentinel := errors.New("probe exploded")
	_, err = s.Compact(context.Background(), func(string) (int64, error) { return 0, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the probe error to surface, got %v", err)
	}

	// Ample headroom proceeds.
	if _, err := s.Compact(context.Background(), func(string) (int64, error) {
		return 1 << 40, nil
	}); err != nil {
		t.Fatalf("Compact with ample headroom: %v", err)
	}
}

func TestPageStatsIsInternallyConsistent(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedForCompact(t, s, 500)

	ps, err := s.PageStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ps.PageSize <= 0 || ps.PageCount <= 0 {
		t.Fatalf("implausible page stats: %+v", ps)
	}
	if ps.FileBytes != ps.PageSize*ps.PageCount {
		t.Errorf("FileBytes = %d, want PageSize*PageCount = %d", ps.FileBytes, ps.PageSize*ps.PageCount)
	}
	if ps.FreePageBytes != ps.PageSize*ps.FreelistCount {
		t.Errorf("FreePageBytes = %d, want PageSize*FreelistCount = %d",
			ps.FreePageBytes, ps.PageSize*ps.FreelistCount)
	}
}

// TestFreePageBytesIsAFloorNotAnEstimate is the assertion the old
// "internally consistent" test could not make, because recomputing
// PageSize*FreelistCount from PageStats' own output pins the arithmetic
// and says nothing about what the number MEANS.
//
// freelist_count counts only WHOLLY free pages. VACUUM also repacks
// intra-page fragmentation, and scattered row deletion — which is what
// every reaping path in this bridge produces — leaves plenty of that and
// no free pages at all. The console rendered a zero here as "nothing to
// reclaim", on a database a compaction would halve.
//
// Both directions are asserted: the floor must never OVERSTATE what a
// real VACUUM returns, and the scattered case must actually reproduce the
// zero, or the test would be describing a hazard it never triggers.
func TestFreePageBytesIsAFloorNotAnEstimate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		keep          func(i int) bool
		wantZeroFloor bool
	}{
		{"scattered — every second row deleted", func(i int) bool { return i%2 == 1 }, true},
		{"contiguous — a whole leading run deleted", func(i int) bool { return i >= 3500 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "bridge.db")
			s, err := OpenStore(p)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			seedForCompact(t, s, 4000)
			for i := 0; i < 4000; i++ {
				if tc.keep(i) {
					continue
				}
				if err := s.DeleteTrack(ctx, fmt.Sprintf("Artist%02d/Album/%05d Track.flac", i%20, i)); err != nil {
					t.Fatalf("delete %d: %v", i, err)
				}
			}

			ps, err := s.PageStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			floor := ps.FreePageBytes
			if tc.wantZeroFloor && floor != 0 {
				t.Logf("note: scattered deletion left %d free-page bytes; the hazard is milder "+
					"on this build than when measured, but the floor assertion below still holds", floor)
			}

			res, err := s.Compact(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			actual := res.BeforeBytes - res.AfterBytes
			if floor > actual {
				t.Errorf("FreePageBytes = %d OVERSTATES what the compaction returned (%d); "+
					"it is documented as a floor", floor, actual)
			}
			if tc.wantZeroFloor && floor == 0 && actual <= 0 {
				t.Errorf("the scattered fixture reclaimed %d bytes, so it does not reproduce the "+
					"under-report this test exists for", actual)
			}
			t.Logf("%s: floor=%d actual=%d (before=%d after=%d)",
				tc.name, floor, actual, res.BeforeBytes, res.AfterBytes)
		})
	}
}

// TestCompactProbesADirectoryNotTheDatabaseFile pins the CONTRACT rather
// than the symptom, so it fails on every platform instead of only on the
// one where the symptom appears.
//
// Compact handed freeBytes the database FILE. The injected implementation
// is transcode.AvailableDiskSpaceNearest, whose parameter is a directory
// everywhere else in the tree and whose ancestor walk advances only on
// os.IsNotExist — so an existing file goes straight to the platform
// probe. POSIX statfs accepts a regular file, which is why macOS and the
// Linux VPS never noticed; Windows GetDiskFreeSpaceExW opens its argument
// with FILE_DIRECTORY_FILE and returns ERROR_DIRECTORY for a file, so the
// operator's Compact button 500'd on every Windows install.
//
// No existing test could see it: compact_test.go passes nil or a stub, so
// the blocking Windows CI leg has never called the real probe with a file
// path.
func TestCompactProbesADirectoryNotTheDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedForCompact(t, s, 50)

	var probed string
	if _, err := s.Compact(context.Background(), func(d string) (int64, error) {
		probed = d
		return 1 << 40, nil
	}); err != nil {
		t.Fatal(err)
	}
	if probed == "" {
		t.Fatal("the headroom probe was never called; the assertion below would be vacuous")
	}
	fi, err := os.Stat(probed)
	if err != nil {
		t.Fatalf("Compact probed %q, which does not exist: %v", probed, err)
	}
	if !fi.IsDir() {
		t.Errorf("Compact probed %q, which is a FILE; the probe takes a directory and its "+
			"Windows implementation refuses anything else", probed)
	}
	if probed != dir {
		t.Errorf("Compact probed %q, want the database's own directory %q", probed, dir)
	}
}

// TestCompactMeasuresTheWALToo pins that the before/after figures are
// FOOTPRINTS — the main file plus its write-ahead log — not the main file
// alone.
//
// In WAL mode an arbitrary fraction of the database lives in -wal until a
// checkpoint folds it back. Measuring only the main file made the console
// report a NEGATIVE reclamation ("the compaction added 2.3 MB") and made
// the headroom guard grade a number orders of magnitude too small — the
// guard whose entire job is preventing an ENOSPC mid-VACUUM.
func TestCompactMeasuresTheWALToo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	// Small on purpose. The discriminating assertion only needs the WAL to
	// be NON-EMPTY — a `before` measured from the main file alone is then
	// already short by walBytes — so there is no reason to build the
	// 131 MB WAL the first version of this fixture did. That version put
	// real pressure on the temp volume under -race, which is its own
	// hazard on a constrained runner and cost this batch a wedged gate.
	//
	// The ~10 s runtime is NOT the data volume and does not shrink with
	// it: it is `busy_timeout(5000)` twice over, once for each checkpoint
	// the held snapshot refuses. That wait is the point — the busy branch
	// is the only place the clamp below can be observed — so don't
	// "optimise" it by releasing the reader early.
	seedForCompact(t, s, 200)

	// The fixture has to REACH the state, or it pins nothing: reverting
	// `before` to the main file alone leaves a test green unless the WAL
	// actually holds a meaningful share of the database. A second
	// connection parked in a read transaction is what gets there — it
	// holds the old snapshot, so no checkpoint can complete and every
	// subsequent write accumulates in -wal.
	reader, err := sql.Open("sqlite", dsn.File(p, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // the snapshot is released either way
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tracks").Scan(&n); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 120; i++ {
		if err := s.DeleteTrack(ctx, fmt.Sprintf("Artist%02d/Album/%05d Track.flac", i%20, i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}

	mainBytes, err := fileSize(p)
	if err != nil {
		t.Fatal(err)
	}
	walBytes := int64(0)
	if fi, err := os.Stat(p + "-wal"); err == nil {
		walBytes = fi.Size()
	}
	if walBytes == 0 {
		t.Fatal("the held read snapshot did not keep a WAL alive; this test would prove nothing " +
			"about whether the before figure includes it")
	}
	foot, err := dbFootprint(p)
	if err != nil {
		t.Fatal(err)
	}
	if foot != mainBytes+walBytes {
		t.Errorf("dbFootprint = %d, want main %d + wal %d = %d", foot, mainBytes, walBytes, mainBytes+walBytes)
	}

	res, err := s.Compact(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The discriminating assertion: the before figure must count the WAL.
	// Measuring the main file alone is what let a compaction report
	// GROWTH, and what made the headroom guard demand 8,192 bytes free
	// for a vacuum that needed ~4.6 MB.
	if res.BeforeBytes < mainBytes+walBytes {
		t.Errorf("BeforeBytes = %d, less than the footprint at entry (main %d + wal %d = %d); "+
			"the WAL is not a cache and an arbitrary share of the database lives in it",
			res.BeforeBytes, mainBytes, walBytes, mainBytes+walBytes)
	}
	// Writing this fixture is what showed that "a compaction can never
	// report growth" is FALSE. With the reader still parked, the
	// post-checkpoint answers BUSY, so the WAL is not truncated and the
	// vacuum's own output sits in it on top of the original — peak disk
	// really has risen, exactly as this file's header describes. Nothing
	// has been reclaimed yet, and the honest report of that is ZERO.
	if !res.CheckpointBusy {
		t.Fatal("the held snapshot did not make the post-checkpoint busy; the clamp assertion " +
			"below would be testing the ordinary path instead of the one it is for")
	}
	if got := res.ReclaimedBytes(); got != 0 {
		t.Errorf("ReclaimedBytes = %d on a busy checkpoint; nothing has been returned to the "+
			"filesystem yet and a negative figure reads as \"the compaction made it bigger\"", got)
	}
	t.Logf("held-WAL fixture: main=%d wal=%d before=%d after=%d busy=%v reclaimed=%d",
		mainBytes, walBytes, res.BeforeBytes, res.AfterBytes, res.CheckpointBusy, res.ReclaimedBytes())

	// And a missing -wal is not an error.
	if _, err := dbFootprint(filepath.Join(dir, "no-such.db")); err == nil {
		t.Error("dbFootprint on a missing database returned no error")
	}
}

// TestCheckpointOutcomeTreatsACancelledContextAsBusyNotFailed pins the
// branch a race would otherwise own.
//
// If the context dies during the mandatory post-VACUUM checkpoint, the
// vacuum has ALREADY committed. Returning an error there tells the
// operator the button failed after it did the work — the mirror image of
// the failure compact.go's header exists to prevent, and
// indistinguishable from a genuine one.
func TestCheckpointOutcomeTreatsACancelledContextAsBusyNotFailed(t *testing.T) {
	real := errors.New("disk went away")
	for _, tc := range []struct {
		name     string
		busy     bool
		err      error
		wantBusy bool
		wantErr  error
	}{
		{"clean checkpoint", false, nil, false, nil},
		{"genuinely busy", true, nil, true, nil},
		{"context cancelled", false, context.Canceled, true, nil},
		{"deadline exceeded", false, context.DeadlineExceeded, true, nil},
		{"wrapped cancellation", false, fmt.Errorf("pragma: %w", context.Canceled), true, nil},
		{"a real failure still fails", false, real, false, real},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy, err := checkpointOutcome(tc.busy, tc.err)
			if !errors.Is(err, tc.wantErr) && !(err == nil && tc.wantErr == nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if busy != tc.wantBusy {
				t.Errorf("busy = %v, want %v", busy, tc.wantBusy)
			}
		})
	}
}

// TestScanInFlightCoversSubtreeScans pins the difference between the two
// scan predicates. Compact's caller must use the broad one: IsScanning
// deliberately does not see a subtree scan (it drives the admin badge and
// the SSE fast tick, where a watcher-triggered subtree scan is not what
// an operator means by "scanning"), so guarding a VACUUM on it would let
// the vacuum start in the middle of the watcher's writes.
//
// Observed through the duplicate-stamping seam, which fires inside
// ScanSubtree's own tail — i.e. while the scan is genuinely in flight.
func TestScanInFlightCoversSubtreeScans(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	seedTrackDirs(t, filepath.Join(lib, "Artist", "Album"))
	s, sc := newScanFixture(t, lib)

	var inFlight, narrow bool
	var observed bool
	beforeApplyDupeStampsHookForTests = func() {
		observed = true
		inFlight = sc.ScanInFlight()
		narrow = sc.IsScanning()
	}
	t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })

	if _, err := sc.ScanSubtree(context.Background(), lib); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if !observed {
		t.Fatal("the subtree scan never reached its stamping tail; the assertion below would be vacuous")
	}
	if !inFlight {
		t.Error("ScanInFlight was false during a subtree scan — Compact's guard would not fire")
	}
	if narrow {
		t.Error("IsScanning was true during a subtree scan; it is documented as full-scan-only")
	}
	if sc.ScanInFlight() {
		t.Error("ScanInFlight still true after the scan returned")
	}
	_ = s
}

// TestWalCheckpointTruncateWorksOffWAL pins the review question about a
// non-WAL database. Measured across journal modes: the pragma returns one
// row in every mode, so sql.ErrNoRows is unreachable and the suggested
// ErrNoRows branch would be dead code.
//
// A store opened by OpenStore is always WAL, so this drives the pragma
// directly against a DELETE-mode database to cover the case an operator
// could reach by putting dataDir on a mount where WAL is unsupported.
func TestWalCheckpointTruncateWorksOffWAL(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dsn.File(filepath.Join(dir, "t.db"), "_pragma=journal_mode(DELETE)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode == "wal" {
		t.Skip("could not open a non-WAL database; the assertion would be vacuous")
	}
	s := &Store{db: db, now: time.Now}
	busy, err := s.walCheckpointTruncate(context.Background())
	if err != nil {
		t.Fatalf("wal_checkpoint(TRUNCATE) in %s mode: %v — if this is sql.ErrNoRows the "+
			"review finding was right after all", mode, err)
	}
	if busy {
		t.Errorf("busy = true in %s mode; there is no WAL to be busy with", mode)
	}
}
