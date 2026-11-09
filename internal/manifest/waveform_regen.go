package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A waveform curve proven missing is rendered again on access, waveform only
// (backlog B652): the analysis pool's repair job decodes the source again,
// writes the curve where it belongs, and records the curve on the row
// through CommitWaveformRepair, which writes the three waveform columns and
// nothing else. Its failures are counted apart from the analysis-failure
// debounce (analysis_failure.go, whose three verdicts and seven-day TTL are
// about whether a source is offered for analysis at all), in
// waveform_regen_failures (migration v62).

// RecordedWaveform is one `track_analysis` row that records a curve, as the
// regeneration guard reads it: the source it is keyed on, the path it
// records and the size it recorded there.
type RecordedWaveform struct {
	SourcePath   string
	WaveformPath string
	WaveformSize int64
}

// RecordedWaveforms lists every row that records a curve (its waveform path
// not empty), the catalog the regeneration guard judges. Three columns, not
// AllAnalysisRows' whole row: the guard reads the catalog at most once an
// interval, and the spectrum blobs are of no use to it. Read-only; no s.mu.
func (s *Store) RecordedWaveforms(ctx context.Context) ([]RecordedWaveform, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, waveform_path, waveform_size
		  FROM track_analysis
		 WHERE waveform_path != ''
		 ORDER BY source_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RecordedWaveform
	for rows.Next() {
		var w RecordedWaveform
		if err := rows.Scan(&w.SourcePath, &w.WaveformPath, &w.WaveformSize); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ErrAnalysisChanged is CommitWaveformRepair's answer when the row is no
// longer the one the repair started from: another writer changed it since
// (an analysis that rewrote it, an adoption, a deletion). The repair writes
// nothing to the row then; a changed source goes through ordinary analysis.
var ErrAnalysisChanged = errors.New("analysis row changed since the repair began")

// CommitWaveformRepair records a curve rendered again for base's row, waveform
// only: it writes waveform_path, waveform_tag and waveform_size and nothing
// else, so loudness, true peak, key, tempo, dynamic range, the spectrum and
// its bandwidth, the audio-MD5 verdict and its retry counter stay as they are.
// It writes only while the row is still base: a compare-and-swap on the
// columns every writer of the row moves (an analysis rewrites created_at, an
// adoption waveform_path) and on the source's version and the schema the
// row's measurements were made at, all NOT NULL and read back exactly. A row
// that changed, or is gone, is ErrAnalysisChanged, with nothing written.
//
// `indexed_at` advances only when the new curve's tag differs from the
// row's, through bumpIndexedAtByPathSQL in the same transaction: a client
// keys its cached curve on the tag, so a curve rendered again byte for byte
// is no change a client can see, and a curve that changed is one it must
// fetch again. Reports whether it advanced.
func (s *Store) CommitWaveformRepair(ctx context.Context, base AnalysisRow, path, tag string, size int64) (bumped bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE track_analysis
		   SET waveform_path = ?, waveform_tag = ?, waveform_size = ?
		 WHERE source_path = ?
		   AND waveform_path = ? AND waveform_tag = ? AND waveform_size = ?
		   AND source_mtime_ns = ? AND source_size = ?
		   AND schema_version = ? AND created_at = ?`,
		path, tag, size,
		base.SourcePath,
		base.WaveformPath, base.WaveformTag, base.WaveformSize,
		base.SourceMTimeNS, base.SourceSize,
		base.SchemaVersion, base.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("commit waveform repair: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return false, ErrAnalysisChanged
	}
	bumped = tag != base.WaveformTag
	if bumped {
		if _, err := tx.ExecContext(ctx, bumpIndexedAtByPathSQL, s.now().UnixNano(), base.SourcePath); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if bumped {
		s.noteLibrary(ctx)
	}
	return bumped, nil
}

// WaveformRegenStopAfter is how many regenerations of a curve that failed on
// their own account (the source read, and the curve could not be rendered or
// written) stop its regeneration, until the bridge is another build or the
// source another version. Two: the first may be a moment's fault, and a
// second against the same build and the same bytes says the next try would
// fail the same way, at the cost of a decode.
const WaveformRegenStopAfter = 2

// WaveformRegenFailure is a curve's failed regenerations as recorded: the
// build and the source version they were counted against, how many, when the
// first and the last were, and what the last said.
type WaveformRegenFailure struct {
	SourcePath    string
	Build         string
	SourceMTimeNS int64
	SourceSize    int64
	Failures      int
	FirstAt       int64
	LastAt        int64
	Reason        string
}

// StopsFor reports whether the record stops the curve's regeneration under
// build, for the source at (mtimeNS, size): enough failures, counted against
// that very build and version. A record of another build or version stops
// nothing, and is counted again from one at its next failure.
func (f *WaveformRegenFailure) StopsFor(build string, mtimeNS, size int64) bool {
	return f != nil && f.Failures >= WaveformRegenStopAfter &&
		f.Build == build && f.SourceMTimeNS == mtimeNS && f.SourceSize == size
}

// RecordWaveformRegenFailure counts one regeneration of sourcePath's curve
// that failed on its own account, against build and the source at (mtimeNS,
// size), and returns the count: one more than the record's when it was
// counted against the same build and version, one otherwise (a new build, or
// a changed source, starts again). Durable, so a restart keeps it. Returns 0
// and writes nothing when the analysis row is gone (the record goes with it).
// Touches neither `tracks` nor `indexed_at`: a failed regeneration changes
// nothing a client can see.
func (s *Store) RecordWaveformRegenFailure(ctx context.Context, sourcePath, build string, mtimeNS, size int64, reason string) (int, error) {
	reason = trimReasonToRuneBoundary(reason, analysisFailureReasonMax)
	now := s.now().UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO waveform_regen_failures
			(source_path, build, source_mtime_ns, source_size, failures, first_at, last_at, reason)
		SELECT ?1, ?2, ?3, ?4, 1, ?5, ?5, ?6
		 WHERE EXISTS (SELECT 1 FROM track_analysis WHERE source_path = ?1)
		ON CONFLICT (source_path) DO UPDATE SET
			failures = CASE
				WHEN build = excluded.build AND source_mtime_ns = excluded.source_mtime_ns
				 AND source_size = excluded.source_size
				THEN failures + 1 ELSE 1 END,
			first_at = CASE
				WHEN build = excluded.build AND source_mtime_ns = excluded.source_mtime_ns
				 AND source_size = excluded.source_size
				THEN first_at ELSE excluded.first_at END,
			build           = excluded.build,
			source_mtime_ns = excluded.source_mtime_ns,
			source_size     = excluded.source_size,
			last_at         = excluded.last_at,
			reason          = excluded.reason`,
		sourcePath, build, mtimeNS, size, now, reason); err != nil {
		return 0, fmt.Errorf("record waveform regeneration failure: %w", err)
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT failures FROM waveform_regen_failures WHERE source_path = ?`, sourcePath).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ClearWaveformRegenFailure drops sourcePath's record: called once its curve
// is written again, by a regeneration or an ordinary analysis, so the count
// is of consecutive failures.
func (s *Store) ClearWaveformRegenFailure(ctx context.Context, sourcePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM waveform_regen_failures WHERE source_path = ?`, sourcePath)
	return err
}

// GetWaveformRegenFailure is sourcePath's record, nil when it has none.
// Read-only; no s.mu.
func (s *Store) GetWaveformRegenFailure(ctx context.Context, sourcePath string) (*WaveformRegenFailure, error) {
	var f WaveformRegenFailure
	err := s.db.QueryRowContext(ctx, `
		SELECT source_path, build, source_mtime_ns, source_size, failures, first_at, last_at, reason
		  FROM waveform_regen_failures WHERE source_path = ?`, sourcePath).Scan(
		&f.SourcePath, &f.Build, &f.SourceMTimeNS, &f.SourceSize, &f.Failures, &f.FirstAt, &f.LastAt, &f.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// StoppedWaveformRegens lists the records that stop a curve's regeneration
// under build now: enough failures, counted against build and against the
// source version the analysis row still records. `bridge doctor` lists them
// as "regeneration stopped". Read-only; no s.mu.
func (s *Store) StoppedWaveformRegens(ctx context.Context, build string) ([]WaveformRegenFailure, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.source_path, f.build, f.source_mtime_ns, f.source_size, f.failures,
		       f.first_at, f.last_at, f.reason
		  FROM waveform_regen_failures f
		  JOIN track_analysis a ON a.source_path = f.source_path
		 WHERE f.failures >= ? AND f.build = ?
		   AND f.source_mtime_ns = a.source_mtime_ns AND f.source_size = a.source_size
		 ORDER BY f.source_path`, WaveformRegenStopAfter, build)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WaveformRegenFailure
	for rows.Next() {
		var f WaveformRegenFailure
		if err := rows.Scan(&f.SourcePath, &f.Build, &f.SourceMTimeNS, &f.SourceSize, &f.Failures,
			&f.FirstAt, &f.LastAt, &f.Reason); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SuppressedAnalysisVersion answers SuppressedAnalysisPaths' question for one
// path: whether the decoders' verdicts suppress path now
// (analysisFailureSuppressedSQL), and the file version they were recorded
// against, which the caller compares with its live stat as the candidate walk
// does. The waveform regeneration asks it on a lookup, where the set would be
// read whole for one path. Read-only; no s.mu.
func (s *Store) SuppressedAnalysisVersion(ctx context.Context, path string) (AnalysisSuppression, bool, error) {
	var v AnalysisSuppression
	err := s.db.QueryRowContext(ctx,
		`SELECT analysis_fail_size, analysis_fail_mtime_ns
		   FROM tracks WHERE path = ? AND `+analysisFailureSuppressedSQL,
		path, s.AnalysisFailureCutoff()).Scan(&v.SizeBytes, &v.MTimeNS)
	if errors.Is(err, sql.ErrNoRows) {
		return AnalysisSuppression{}, false, nil
	}
	if err != nil {
		return AnalysisSuppression{}, false, err
	}
	return v, true, nil
}
