package manifest

// Database compaction — reclaiming the free pages that accumulate as rows
// are reaped, and returning them to the filesystem.
//
// Nothing in the bridge has ever compacted the live database. `VACUUM
// INTO` exists in the backup path, but that writes a *copy*; the file the
// bridge runs on only grows. Every reaping path added since — duplicate
// suppression, the missing-track threshold reap, trash purge, the device
// registration sweep — adds to the freelist and nothing takes it away.
// Measured on a live install: 946 free pages out of 9,410, 10% of a
// 36.7 MiB file, with `auto_vacuum = NONE`.
//
// **`PRAGMA wal_checkpoint(TRUNCATE)` MUST run AFTER the `VACUUM`.** This
// is the whole subtlety of the file. In WAL mode the vacuum's own output
// lands in the WAL, so without a post-vacuum checkpoint the main database
// file does not shrink by a single byte and peak disk *rises*. Measured
// on a seeded 8,000-track store with 7,000 deleted:
//
//	stage                            .db          -wal
//	after deletes                    5,623,808      226,632
//	after VACUUM, no checkpoint      5,623,808    2,813,992   <- no gain
//	after wal_checkpoint(TRUNCATE)   2,572,288            0
//
// A checkpoint *before* the vacuum is optional hygiene (it keeps peak
// disk a little lower); the one after is the feature. A review proposed
// the pre-vacuum checkpoint alone, which would have shipped a button that
// reports success and reclaims nothing.
//
// **`auto_vacuum = INCREMENTAL` was considered and rejected.** It cannot
// be changed on an existing database without a full VACUUM anyway, so
// setting it for new databases alone would create two populations that
// behave differently — strictly worse than one honest, operator-triggered
// compaction.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PageStats is a snapshot of the database file's page accounting. Sizes
// are derived rather than stat'ed so the numbers are internally
// consistent: FileBytes is what SQLite believes it owns, which can differ
// from the on-disk size while a WAL is outstanding.
type PageStats struct {
	PageSize      int64
	PageCount     int64
	FreelistCount int64
	FileBytes     int64 // PageSize * PageCount

	// FreePageBytes is PageSize * FreelistCount: the bytes sitting in
	// pages that are WHOLLY free. It is a FLOOR on what a compaction
	// would return, never an estimate of it, and rendering it as one is
	// how an operator gets told there is nothing to reclaim on a database
	// a VACUUM would halve.
	//
	// The gap is not academic. `freelist_count` counts only pages no live
	// row occupies at all; VACUUM additionally repacks INTRA-PAGE
	// fragmentation, and scattered row deletion is what every reaping
	// path in this bridge produces — duplicate suppression, the
	// missing-track threshold reap, DeleteTracksByPrefix, trash purge.
	// Measured on a 72,474,624-byte store:
	//
	//	deletion pattern        freelist_count   FreePageBytes   VACUUM returned
	//	every 2nd row                        0               0        36,233,216
	//	9 of every 10                   15,536      63,635,456        65,220,608
	//	the first 90% (contiguous)      15,920      65,208,320        65,216,512
	//
	// The first row is the one that matters: the panel said "nothing to
	// reclaim" about half a file. Never present a zero here as an answer
	// to "should I compact"; it is an answer to "are there whole free
	// pages", which is a different and much narrower question.
	FreePageBytes int64
}

// PageStats reads the page accounting. Read path — no s.mu.
func (s *Store) PageStats(ctx context.Context) (PageStats, error) {
	var ps PageStats
	for _, q := range []struct {
		pragma string
		dst    *int64
	}{
		{"PRAGMA page_size", &ps.PageSize},
		{"PRAGMA page_count", &ps.PageCount},
		{"PRAGMA freelist_count", &ps.FreelistCount},
	} {
		if err := s.db.QueryRowContext(ctx, q.pragma).Scan(q.dst); err != nil {
			return PageStats{}, fmt.Errorf("manifest: %s: %w", q.pragma, err)
		}
	}
	ps.FileBytes = ps.PageSize * ps.PageCount
	ps.FreePageBytes = ps.PageSize * ps.FreelistCount
	return ps, nil
}

// CompactResult reports what a Compact actually achieved.
//
// BeforeBytes / AfterBytes are on-disk FOOTPRINTS — the main database
// file plus its write-ahead log — so a caller can report a real
// reclamation figure rather than a hoped-for one. Measuring the main file
// alone made this lie: in WAL mode an arbitrary fraction of the database
// lives in `-wal` until a checkpoint folds it back, and `after` is taken
// once the mandatory post-VACUUM checkpoint has truncated the WAL to
// zero. Measured against a store whose WAL could not be checkpointed,
// with the main-file-only form:
//
//	pre : .db=4,096       -wal=222,088,632
//	post: .db=2,334,720   -wal=0
//	reported: {"beforeBytes":4096,"afterBytes":2334720,"reclaimedBytes":-2330624}
//
// i.e. the console said the compaction ADDED 2.3 MB. See dbFootprint.
type CompactResult struct {
	BeforeBytes int64
	AfterBytes  int64
	// CheckpointBusy is true when the post-VACUUM
	// `wal_checkpoint(TRUNCATE)` could not complete because a reader
	// still held the old snapshot. The vacuum succeeded, but the file
	// will not have shrunk yet — it will once the WAL is checkpointed,
	// which happens on its own eventually. Surfaced rather than
	// swallowed, because "compaction succeeded and reclaimed nothing"
	// otherwise looks like a bug in the compaction.
	CheckpointBusy bool
}

// ErrInsufficientDiskSpace is returned when the volume holding the
// database has less free space than the compaction needs.
var ErrInsufficientDiskSpace = errors.New("manifest: not enough free disk space to compact")

// compactHeadroomFactor is the multiple of the current database size that
// must be free before a compaction is attempted. `VACUUM` writes a
// complete temporary copy alongside the original, so the peak requirement
// is a full second copy; 2x is that plus nothing, which is already tight
// — the WAL grows too (see the table above), so this is a floor rather
// than a comfortable margin.
const compactHeadroomFactor = 2

// Compact reclaims free pages and returns them to the filesystem.
//
// Holds s.mu for the duration: this is a writer, and it is a long one.
// Callers MUST refuse to call it while a library scan is in flight —
// `Scanner.ScanInFlight` is the predicate — because a scan holds s.mu in
// bursts for every batch flush and would serialise behind the vacuum for
// its whole duration.
//
// **That caller check is a GUARD, not mutual exclusion, and the
// distinction is worth stating rather than leaving implied.** It runs in
// ONE direction and check-then-act: a scan starting between the caller's
// check and the lock below — the 6-hour periodic timer, an fsnotify
// subtree scan, an upload commit, a second tab's POST /api/scan — blocks
// on s.mu for the vacuum's whole duration, which is exactly the outcome
// the guard exists to prevent. Nothing refuses or defers a scan while a
// compaction is running, and two concurrent Compacts serialise into two
// full vacuums rather than one.
//
// An interprocess-style flag was considered and not added, for the same
// reason `bridge restore` and `manifest clear-missing` narrow their
// window rather than taking a lock: the consequence here is a STALL, not
// corruption — s.mu genuinely serialises them — and a compaction-in-
// flight flag the scanner had to honour would give a wedged vacuum the
// power to silence the scanner. Say which it is; do not imply the
// stronger thing.
//
// freeBytes reports free space on the volume holding a DIRECTORY. It is injected
// rather than imported because `internal/transcode` (which owns the
// existing implementation) imports `internal/manifest`, so taking it as a
// dependency here would close a cycle. nil skips the headroom check —
// the caller is then asserting it has already checked, or accepting the
// risk.
//
// It is handed `filepath.Dir(s.path)`, NOT the database file. The
// parameter is a directory in every other caller of the injected
// implementation (`transcode.AvailableDiskSpaceNearest`), and its own
// ancestor walk advances only on `os.IsNotExist` — so a file path, which
// exists, is passed straight through to the platform probe. POSIX
// `statfs(2)` accepts a regular file, which is why the file-path form
// worked on macOS and the Linux VPS; Windows `GetDiskFreeSpaceExW` takes
// `lpDirectoryName` and opens it with `FILE_DIRECTORY_FILE`, so a regular
// file returns STATUS_NOT_A_DIRECTORY -> ERROR_DIRECTORY (267) --- the
// directory name is invalid --- and every POST /api/database/compact on
// a Windows install 500'd, always.
//
// No test caught it because none drives the real probe: compact_test.go
// passes nil or a stub, so the blocking Windows CI leg has never called
// AvailableDiskSpace with a file path. The replacement test asserts the
// CONTRACT rather than the symptom, so it fails on every platform.

// The context bounds the whole operation. Note what that does and does
// not do: it stops a pathological vacuum from holding s.mu indefinitely.
// It does NOT give concurrent API readers a fast failure — they block on
// SQLite's own locking regardless.
func (s *Store) Compact(ctx context.Context, freeBytes func(dir string) (int64, error)) (CompactResult, error) {
	if s.path == "" {
		return CompactResult{}, errors.New("manifest: Compact requires a file-backed store")
	}

	before, err := dbFootprint(s.path)
	if err != nil {
		return CompactResult{}, fmt.Errorf("manifest: measure before compact: %w", err)
	}
	if freeBytes != nil {
		avail, err := freeBytes(filepath.Dir(s.path))
		if err != nil {
			return CompactResult{}, fmt.Errorf("manifest: probe free space: %w", err)
		}
		if need := before * compactHeadroomFactor; avail < need {
			return CompactResult{}, fmt.Errorf("%w: need %d bytes free, have %d",
				ErrInsufficientDiskSpace, need, avail)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Pre-checkpoint: optional hygiene. Folding the existing WAL in first
	// keeps peak disk lower during the vacuum. A busy result here is not
	// an error — the vacuum works either way.
	_, _ = s.walCheckpointTruncate(ctx)

	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return CompactResult{}, fmt.Errorf("manifest: VACUUM: %w", err)
	}

	// Post-checkpoint: MANDATORY. Without it the vacuum's output sits in
	// the WAL and the main file does not shrink at all.
	//
	// A CONTEXT CANCELLATION here is not a failed compaction. The VACUUM
	// above has already committed; only the checkpoint was interrupted,
	// and the WAL folds back on its own soon after. Returning an error
	// would tell the operator the button failed AFTER it had done the
	// work — the mirror image of the failure this file's header exists to
	// prevent, and indistinguishable from a genuine one. It is reported
	// as CheckpointBusy instead, which is precisely what that field
	// already means: the vacuum succeeded, the file has not shrunk yet,
	// and it will.
	busy, err := s.walCheckpointTruncate(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		busy = true
	case err != nil:
		return CompactResult{}, fmt.Errorf("manifest: post-VACUUM checkpoint: %w", err)
	}

	after, err := dbFootprint(s.path)
	if err != nil {
		return CompactResult{}, fmt.Errorf("manifest: measure after compact: %w", err)
	}
	return CompactResult{BeforeBytes: before, AfterBytes: after, CheckpointBusy: busy}, nil
}

// dbFootprint reports what this database currently occupies on disk: the
// main file plus its write-ahead log.
//
// The WAL is not a cache. In WAL mode an arbitrary fraction of the
// database lives there until a checkpoint folds it back, so the main
// file's size alone is not "how big the database is" — and a single open
// reader is enough to make `wal_checkpoint(TRUNCATE)` answer busy and
// leave it there. Both places that number is used were wrong without
// this: the reclamation figure could go NEGATIVE (see CompactResult), and
// the headroom guard — whose whole job is preventing an ENOSPC mid-VACUUM
// — graded the wrong number and could under-demand by orders of
// magnitude, once measured demanding 8,192 bytes free for a vacuum that
// needed ~4.6 MB.
//
// A missing `-wal` is normal, not an error: it is absent off WAL, and
// between a checkpoint and the next write.
func dbFootprint(path string) (int64, error) {
	total, err := fileSize(path)
	if err != nil {
		return 0, err
	}
	switch n, err := fileSize(path + "-wal"); {
	case err == nil:
		total += n
	case !os.IsNotExist(err):
		return 0, err
	}
	return total, nil
}

// walCheckpointTruncate runs `PRAGMA wal_checkpoint(TRUNCATE)` and
// reports whether SQLite answered busy. The pragma returns three columns
// (busy, log-pages, checkpointed-pages); busy=1 means a reader still held
// the old snapshot and the WAL was left in place.
//
// A NON-WAL database is safe here and needs no special case, which was
// raised in review and is measured rather than assumed: under
// modernc.org/sqlite the pragma returns exactly ONE row in every journal
// mode — `(0, 0, 0)` under WAL, `(0, -1, -1)` under DELETE and MEMORY —
// so sql.ErrNoRows is not reachable and an ErrNoRows branch would be dead
// code. busy reads 0 off WAL, which is the honest answer: there is no WAL
// to leave behind, and VACUUM has already rewritten the file directly.
func (s *Store) walCheckpointTruncate(ctx context.Context) (busy bool, err error) {
	var b, logPages, checkpointed int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
		Scan(&b, &logPages, &checkpointed); err != nil {
		return false, err
	}
	return b != 0, nil
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
