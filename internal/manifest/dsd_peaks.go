package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// dsd_peaks (migration v47) records a DSD track's true peak at unity decode
// per RENDER PROFILE: the Stage A intermediate a DSD rendition is measured
// on (transcode.DSDPeakProfile — the Stage A recipe, the tier, the target
// rate and sox's rate quality). It is what the album-level gain reads
// (internal/albumgain): the boost an album's tracks share is the one its
// hottest track allows, so a render needs every album-mate's peak before
// its Stage C, including album-mates that have no rendition yet.
//
// Three writers:
//   - every DSD render, through UpsertVariant (VariantRow.PeakProfile) — its
//     Stage B measures the peak anyway;
//   - the album survey's measure-only pass (UpsertDSDPeak);
//   - the v47 seed, once, from the true_peak_dbtp every DSD rendition row
//     already carried — the column no production code read until now.
//
// A NULL peak is a measurement: the source is digitally silent. A MISSING
// row is "never measured". Freshness is the variants' rule — the source's
// mtime and size at measurement must match the track row — so a re-encoded
// file is measured again, and CASCADE on the tracks PK takes a peak with its
// track.

// DSDPeak is one row of dsd_peaks.
type DSDPeak struct {
	SourcePath string
	Profile    string
	// TruePeakDBTP is the true peak at UNITY decode; nil = digitally silent.
	TruePeakDBTP  *float64
	SourceMTimeNS int64
	SourceSize    int64
	MeasuredAt    int64 // unix ns
}

// upsertDSDPeakSQL is shared by UpsertDSDPeak and UpsertVariant's in-tx
// write, so the two writers cannot disagree about what a peak row holds.
const upsertDSDPeakSQL = `
	INSERT INTO dsd_peaks
		(source_path, profile, true_peak_dbtp, source_mtime_ns, source_size, measured_at)
	VALUES (?,?,?,?,?,?)
	ON CONFLICT (source_path, profile) DO UPDATE SET
		true_peak_dbtp  = excluded.true_peak_dbtp,
		source_mtime_ns = excluded.source_mtime_ns,
		source_size     = excluded.source_size,
		measured_at     = excluded.measured_at`

// UpsertDSDPeak records one measurement. It fails (the FK) when the track
// row is gone, which a caller treats as "this album-mate left the library".
// Holds s.mu per the writer contract.
func (s *Store) UpsertDSDPeak(ctx context.Context, p DSDPeak) error {
	if p.SourcePath == "" || p.Profile == "" {
		return fmt.Errorf("dsd peak: path %q / profile %q must both be set", p.SourcePath, p.Profile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, upsertDSDPeakSQL, p.SourcePath, p.Profile,
		nullFloat(p.TruePeakDBTP), p.SourceMTimeNS, p.SourceSize, p.MeasuredAt)
	return err
}

// freshDSDPeaksChunk bounds one json_each list. An album is a few dozen
// tracks at most, so one chunk is the norm.
const freshDSDPeaksChunk = 500

// freshDSDPeaksSQL is the fresh-peak read: a peak whose source facts still
// match the track row. Binds (profile, a JSON array of paths, one raw path):
// a well-formed path goes in the array, and a path that is not valid UTF-8 is
// bound raw with an empty array, because encoding/json rewrites it
// (splitIllFormedUTF8Paths). One literal statement serves both. Assembling
// either form from shared constants trips SonarCloud's go:S2077 ("dynamically
// formatted SQL"), even through a named const, and a second copy would drift.
const freshDSDPeaksSQL = `
	SELECT p.source_path, p.true_peak_dbtp, p.source_mtime_ns, p.source_size, p.measured_at
	  FROM dsd_peaks p
	  JOIN tracks t ON t.path = p.source_path
	 WHERE p.profile = ?
	   AND (p.source_path IN (SELECT value FROM json_each(?)) OR p.source_path = ?)
	   AND p.source_mtime_ns = t.mtime_ns
	   AND p.source_size     = t.size`

// FreshDSDPeaks returns the peaks recorded for paths on profile whose
// source facts still match the track row — the only peaks a render may
// trust. A path with no fresh peak is absent from the map; a silent one is
// present with a nil TruePeakDBTP.
func (s *Store) FreshDSDPeaks(ctx context.Context, profile string, paths []string) (map[string]DSDPeak, error) {
	out := make(map[string]DSDPeak, len(paths))
	valid, illFormed := splitIllFormedUTF8Paths(paths)
	for start := 0; start < len(valid); start += freshDSDPeaksChunk {
		blob, err := json.Marshal(valid[start:min(start+freshDSDPeaksChunk, len(valid))])
		if err != nil {
			return nil, err
		}
		rows, err := s.db.QueryContext(ctx, freshDSDPeaksSQL, profile, string(blob), nil)
		if err := scanFreshDSDPeaks(rows, err, profile, out); err != nil {
			return nil, err
		}
	}
	// encoding/json replaces an ill-formed byte with U+FFFD, so such a path
	// would come back out of json_each as a different string: it is bound
	// raw instead, one at a time (a filename on Linux is any byte string).
	for _, p := range illFormed {
		rows, err := s.db.QueryContext(ctx, freshDSDPeaksSQL, profile, "[]", p)
		if err := scanFreshDSDPeaks(rows, err, profile, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanFreshDSDPeaks drains one fresh-peak query into out, reporting the
// query's own error first.
func scanFreshDSDPeaks(rows *sql.Rows, qerr error, profile string, out map[string]DSDPeak) error {
	if qerr != nil {
		return fmt.Errorf("fresh dsd peaks: %w", qerr)
	}
	defer rows.Close()
	for rows.Next() {
		pk := DSDPeak{Profile: profile}
		var tp sql.NullFloat64
		if err := rows.Scan(&pk.SourcePath, &tp, &pk.SourceMTimeNS, &pk.SourceSize, &pk.MeasuredAt); err != nil {
			return fmt.Errorf("fresh dsd peaks: %w", err)
		}
		if tp.Valid {
			v := tp.Float64
			pk.TruePeakDBTP = &v
		}
		out[pk.SourcePath] = pk
	}
	return rows.Err()
}

// seedDSDPeaksFromVariantsSQL is migration v47's seed: every DSD rendition
// row already carries the true peak its render measured (true_peak_dbtp,
// v43, NULL only for a digitally silent source), so the album-level gain
// starts with a peak for every track that has a rendition and decodes
// nothing to begin.
//
// The profile is transcode.DSDPeakProfile spelled in SQL — this package
// cannot import transcode. "a1" is transcode.DSDPeakRecipe: every row that
// exists at v47 was decoded by the a1 recipe. rateFlag comes from the
// settings blob (a DSD render always writes it; "-v", the server default,
// covers a blob without one). TestSeedDSDPeaksProfileSpelling and
// transcode's TestDSDPeakProfile pin the same literal, which is what keeps
// the two spellings in step.
//
// ORDER BY created_at DESC under INSERT OR IGNORE keeps the NEWEST row's
// peak when two rows share a profile. Idempotent, so the ladder's re-run
// contract holds.
const seedDSDPeaksFromVariantsSQL = `
	INSERT OR IGNORE INTO dsd_peaks
		(source_path, profile, true_peak_dbtp, source_mtime_ns, source_size, measured_at)
	SELECT v.source_path,
	       'a1|' ||
	       CASE WHEN v.variant_id LIKE 'optimized-dsd-%' THEN 'compact' ELSE 'faithful' END || '|' ||
	       CAST(v.sample_rate AS INTEGER) || '|' ||
	       COALESCE(json_extract(v.sox_settings, '$.rateFlag'), '-v'),
	       v.true_peak_dbtp, v.source_mtime_ns, v.source_size, v.created_at
	  FROM track_variants v
	 WHERE v.applied_gain_db IS NOT NULL
	   AND (v.variant_id LIKE 'optimized-dsd-%' OR v.variant_id LIKE 'pcm-%')
	   AND json_valid(v.sox_settings)
	 ORDER BY v.created_at DESC`

// seedDSDPeaksFromVariants is migration v47's post().
func seedDSDPeaksFromVariants(db *sql.DB) error {
	_, err := db.ExecContext(context.Background(), seedDSDPeaksFromVariantsSQL)
	return err
}
