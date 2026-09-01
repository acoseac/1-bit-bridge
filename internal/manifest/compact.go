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
)

// PageStats is a snapshot of the database file's page accounting. Sizes
// are derived rather than stat'ed so the numbers are internally
// consistent: FileBytes is what SQLite believes it owns, which can differ
// from the on-disk size while a WAL is outstanding.
type PageStats struct {
	PageSize       int64
	PageCount      int64
	FreelistCount  int64
	FileBytes      int64 // PageSize * PageCount
	ReclaimedBytes int64 // PageSize * FreelistCount — what a Compact would return
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
	ps.ReclaimedBytes = ps.PageSize * ps.FreelistCount
	return ps, nil
}

// CompactResult reports what a Compact actually achieved. BeforeBytes /
// AfterBytes are on-disk sizes of the main database file, so a caller can
// report a real reclamation figure rather than a hoped-for one.
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
// freeBytes reports free space on the volume holding dir. It is injected
// rather than imported because `internal/transcode` (which owns the
// existing implementation) imports `internal/manifest`, so taking it as a
// dependency here would close a cycle. nil skips the headroom check —
// the caller is then asserting it has already checked, or accepting the
// risk.
//
// The context bounds the whole operation. Note what that does and does
// not do: it stops a pathological vacuum from holding s.mu indefinitely.
// It does NOT give concurrent API readers a fast failure — they block on
// SQLite's own locking regardless.
func (s *Store) Compact(ctx context.Context, freeBytes func(dir string) (int64, error)) (CompactResult, error) {
	if s.path == "" {
		return CompactResult{}, errors.New("manifest: Compact requires a file-backed store")
	}

	before, err := fileSize(s.path)
	if err != nil {
		return CompactResult{}, fmt.Errorf("manifest: stat before compact: %w", err)
	}
	if freeBytes != nil {
		avail, err := freeBytes(s.path)
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
	busy, err := s.walCheckpointTruncate(ctx)
	if err != nil {
		return CompactResult{}, fmt.Errorf("manifest: post-VACUUM checkpoint: %w", err)
	}

	after, err := fileSize(s.path)
	if err != nil {
		return CompactResult{}, fmt.Errorf("manifest: stat after compact: %w", err)
	}
	return CompactResult{BeforeBytes: before, AfterBytes: after, CheckpointBusy: busy}, nil
}

// walCheckpointTruncate runs `PRAGMA wal_checkpoint(TRUNCATE)` and
// reports whether SQLite answered busy. The pragma returns three columns
// (busy, log-pages, checkpointed-pages); busy=1 means a reader still held
// the old snapshot and the WAL was left in place.
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
