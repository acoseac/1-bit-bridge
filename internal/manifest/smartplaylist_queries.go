package manifest

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// This file holds the read-path aggregations + the cache read/write that
// back the server-generated smart-playlist feature (GET /v1/smart-playlists).
// The pure generation logic lives in internal/smartplaylist (store-agnostic);
// the daily regenerator wires these rows into that engine and writes the
// result back via ReplaceSmartPlaylists.
//
// Wire-type discipline: the aggregation row structs carry NO json tags (the
// engine/API convert). SmartPlaylistItem is the ONE exception — it is the
// on-disk serialization shape of the `smart_playlists.items_json` blob (the
// sanctioned persisted-blob pattern, like tags_json), so it carries json tags.

// --- smart_playlists cache (migration v18) ---

// SmartPlaylistItem is one item in a generated playlist's cached blob. It
// HAS json tags: it is serialized into / out of `smart_playlists.items_json`
// by the regenerator (producer) and the /v1/smart-playlists handler
// (consumer). Path is the library-relative track path the iOS client
// resolves to a local Track row; Title/Artist are render fallbacks
// (mirrors playlistItemDTO).
type SmartPlaylistItem struct {
	Position int    `json:"position"`
	Path     string `json:"path"`
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
}

// SmartPlaylistHourlyBlob is the items_json shape for the time-of-day family:
// per-UTC-hour item pools the /v1/smart-playlists handler shifts to the
// device's local hour at request time. (Every other family stores a flat
// []SmartPlaylistItem.) Go (un)marshals the int map keys as JSON strings.
type SmartPlaylistHourlyBlob struct {
	Hourly map[int][]SmartPlaylistItem `json:"hourly"`
}

// StoredSmartPlaylist is one cached generated-playlist row. No json tags
// (wire-type discipline) — ItemsJSON carries the serialized
// []SmartPlaylistItem (or, for the time-of-day family, an hour-keyed blob
// the handler special-cases). The API + regenerator wrap/convert it.
type StoredSmartPlaylist struct {
	Slug        string
	Kind        string
	Title       string
	Subtitle    string
	Position    int
	RefreshedAt int64 // UnixNano of the generating run
	ItemsJSON   []byte
	// EnergyJSON is the JSON-encoded []float64 energy envelope (normalized
	// 0..1 loudness contour across member tracks) the iOS waveform-signed
	// cover halo renders. Nil/empty for a family with no analyzed members
	// (the client falls back to a seeded waveform). Migration v20.
	EnergyJSON []byte
	// ModalRateHz is the mix's modal sample rate (tie-break → highest),
	// driving the halo glow color via the Hugo-2 rate-LED palette. 0 =
	// unknown. Migration v20.
	ModalRateHz int
}

// ReplaceSmartPlaylists atomically replaces the entire smart-playlist cache
// with the given snapshot (DELETE-all + batch INSERT in one tx). Wholesale
// replace is correct: the regenerator computes the full populated set each
// run, and a family that drops below threshold must not linger. Holds s.mu
// (writer contract); the snapshot is tiny (a handful of families) so the
// lock hold is negligible — unlike the 50k-item UpsertPlaylist path.
func (s *Store) ReplaceSmartPlaylists(ctx context.Context, snapshot []StoredSmartPlaylist) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; unwind guard otherwise

	if _, err := tx.ExecContext(ctx, `DELETE FROM smart_playlists`); err != nil {
		return err
	}

	if len(snapshot) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO smart_playlists
				(slug, kind, title, subtitle, position, refreshed_at, items_json, energy_json, modal_rate_hz)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range snapshot {
			if p.Slug == "" {
				return errors.New("manifest: ReplaceSmartPlaylists received a row with an empty slug")
			}
			if _, err := stmt.ExecContext(ctx,
				p.Slug, p.Kind, p.Title, p.Subtitle, p.Position, p.RefreshedAt, p.ItemsJSON,
				p.EnergyJSON, p.ModalRateHz); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// LoadSmartPlaylists returns the cached smart playlists ordered by position.
// Read path — no s.mu. An empty result (cold cache before the first
// regeneration) is not an error.
func (s *Store) LoadSmartPlaylists(ctx context.Context) ([]StoredSmartPlaylist, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT slug, kind, title, subtitle, position, refreshed_at, items_json, energy_json, modal_rate_hz
		  FROM smart_playlists
		 ORDER BY position ASC, slug ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredSmartPlaylist
	for rows.Next() {
		var p StoredSmartPlaylist
		if err := rows.Scan(&p.Slug, &p.Kind, &p.Title, &p.Subtitle,
			&p.Position, &p.RefreshedAt, &p.ItemsJSON, &p.EnergyJSON, &p.ModalRateHz); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- play-history aggregations ---

// PlayStatRow aggregates qualifying plays for one track path.
type PlayStatRow struct {
	Path        string
	Plays       int
	LastPlayed  int64 // UnixNano of the most recent qualifying play
	FirstPlayed int64 // UnixNano of the earliest qualifying play in scope
}

// PlayStatsInWindow returns the most-played track paths in [sinceNS, untilNS)
// counting only QUALIFYING plays (duration_used >= minDuration, the "30s
// rule" applied at generation time, never at ingestion). untilNS <= 0 means
// open-ended (up to now). Backs Heavy Rotation (14-day window) and the Daily
// Mix "familiar" set (wider window). Read path — no s.mu.
func (s *Store) PlayStatsInWindow(ctx context.Context, sinceNS, untilNS int64, minDuration float64, limit int) ([]PlayStatRow, error) {
	limit = clampLimit(limit, 100, 5000)
	q := `
		SELECT path, COUNT(*) AS n, MAX(started_at) AS last, MIN(started_at) AS first
		  FROM playback_history
		 WHERE duration_used >= ? AND started_at >= ?`
	args := []any{minDuration, sinceNS}
	if untilNS > 0 {
		q += ` AND started_at < ?`
		args = append(args, untilNS)
	}
	q += ` GROUP BY path ORDER BY n DESC, last DESC LIMIT ?`
	args = append(args, limit)
	return s.scanPlayStats(ctx, q, args...)
}

// PlayStatsForgotten returns track paths with at least minPlays qualifying
// plays whose MOST RECENT qualifying play predates notSinceNS — the
// "nostalgia" (Forgotten Favorites) set: loved before, untouched lately.
// The HAVING uses bare aggregates (not aliases) for portability. Read path
// — no s.mu.
func (s *Store) PlayStatsForgotten(ctx context.Context, minDuration float64, notSinceNS int64, minPlays, limit int) ([]PlayStatRow, error) {
	limit = clampLimit(limit, 100, 5000)
	if minPlays < 1 {
		minPlays = 1
	}
	q := `
		SELECT path, COUNT(*) AS n, MAX(started_at) AS last, MIN(started_at) AS first
		  FROM playback_history
		 WHERE duration_used >= ?
		 GROUP BY path
		HAVING COUNT(*) >= ? AND MAX(started_at) < ?
		 ORDER BY n DESC LIMIT ?`
	return s.scanPlayStats(ctx, q, minDuration, minPlays, notSinceNS, limit)
}

// RecentDistinctPlays returns distinct track paths ordered by most-recent
// qualifying play, newest first — backs Recently Played. Read path — no s.mu.
func (s *Store) RecentDistinctPlays(ctx context.Context, minDuration float64, limit int) ([]PlayStatRow, error) {
	limit = clampLimit(limit, 100, 5000)
	q := `
		SELECT path, COUNT(*) AS n, MAX(started_at) AS last, MIN(started_at) AS first
		  FROM playback_history
		 WHERE duration_used >= ?
		 GROUP BY path
		 ORDER BY MAX(started_at) DESC LIMIT ?`
	return s.scanPlayStats(ctx, q, minDuration, limit)
}

func (s *Store) scanPlayStats(ctx context.Context, q string, args ...any) ([]PlayStatRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayStatRow
	for rows.Next() {
		var r PlayStatRow
		if err := rows.Scan(&r.Path, &r.Plays, &r.LastPlayed, &r.FirstPlayed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HourPathRow is a (UTC hour-of-day, track path, qualifying plays) triple.
type HourPathRow struct {
	Hour  int // 0..23, UTC
	Path  string
	Plays int
}

// PlayCountsByHourPath buckets qualifying plays since sinceNS by UTC
// hour-of-day AND path, so the regenerator can build per-hour pools the
// /v1/smart-playlists handler shifts to the device's local hour at request
// time. ifaceTypes (optional) restricts to specific output routes (e.g.
// CarPlay/Bluetooth for a "commute" mix); nil/empty = all routes.
//
// `started_at` is UnixNano — it MUST be divided by 1e9 before
// datetime(...,'unixepoch'), or SQLite overflows the timestamp and returns
// NULL (silently empty buckets). Read path — no s.mu.
func (s *Store) PlayCountsByHourPath(ctx context.Context, sinceNS int64, minDuration float64, ifaceTypes []string) ([]HourPathRow, error) {
	q := `
		SELECT CAST(strftime('%H', datetime(started_at / 1000000000, 'unixepoch')) AS INTEGER) AS hour,
		       path, COUNT(*) AS n
		  FROM playback_history
		 WHERE duration_used >= ? AND started_at >= ?`
	args := []any{minDuration, sinceNS}
	if len(ifaceTypes) > 0 {
		q += ` AND iface_type IN (` + placeholders(len(ifaceTypes)) + `)`
		for _, t := range ifaceTypes {
			args = append(args, t)
		}
	}
	q += ` GROUP BY hour, path ORDER BY hour ASC, n DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourPathRow
	for rows.Next() {
		var r HourPathRow
		var hour sql.NullInt64 // defensive: a corrupt timestamp could still yield NULL
		if err := rows.Scan(&hour, &r.Path, &r.Plays); err != nil {
			return nil, err
		}
		if !hour.Valid {
			continue
		}
		r.Hour = int(hour.Int64)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventTimeRow is a minimal (started_at, duration_used) pair used for
// session segmentation.
type EventTimeRow struct {
	StartedAt    int64 // UnixNano
	DurationUsed float64
}

// OrderedPlayEvents returns qualifying play events since sinceNS in
// chronological order — the regenerator segments them into listening
// sessions (a gap larger than the session threshold starts a new session) to
// derive the average session length for The Finish Line. Capped to bound
// memory. Read path — no s.mu.
func (s *Store) OrderedPlayEvents(ctx context.Context, sinceNS int64, minDuration float64, limit int) ([]EventTimeRow, error) {
	limit = clampLimit(limit, 5000, 50000)
	rows, err := s.db.QueryContext(ctx, `
		SELECT started_at, duration_used
		  FROM playback_history
		 WHERE duration_used >= ? AND started_at >= ?
		 ORDER BY started_at ASC LIMIT ?
	`, minDuration, sinceNS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventTimeRow
	for rows.Next() {
		var e EventTimeRow
		if err := rows.Scan(&e.StartedAt, &e.DurationUsed); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllPlayedPaths returns the distinct set of track paths with at least one
// qualifying play — used to exclude already-heard tracks from Daily Mix
// discovery. Read path — no s.mu.
func (s *Store) AllPlayedPaths(ctx context.Context, minDuration float64) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT path FROM playback_history WHERE duration_used >= ?
	`, minDuration)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// --- track feature hydration ---

// TrackFeatureRow carries the metadata + analysis scalars the smart-playlist
// engine reasons over for one track. Plain Go types (NULL → nil pointer / "")
// so the pure engine package needn't import database/sql. BPM and
// ReplayGainTrackDB are EFFECTIVE values: a curated tag wins, the analysis
// estimate is the fallback (mirrors the manifest read-path splice contract).
// KeyRoot/KeyMode are analysis-only (no tag source today).
type TrackFeatureRow struct {
	Path              string
	Title             string
	Artist            string
	Album             string
	Genre             string
	Duration          *float64 // seconds
	BPM               *int     // effective (tag wins, else analysis)
	KeyRoot           *int     // 0..11, analysis-only
	KeyMode           string   // "major" / "minor"
	ReplayGainTrackDB *float64 // effective (tag wins, else analysis)
	SampleRate        *int     // Hz from tags_json (drives the per-mix modal-rate glow color)
}

// trackFeatureSelect is the shared projection: metadata from tags_json,
// effective bpm/replaygain (curated tag wins over analysis via COALESCE),
// and the analysis-only key. LEFT JOIN so a track with no analysis row still
// yields metadata (it just won't be harmonic-sequenceable). json1 is built
// into the driver (the v1 migration already indexes json_extract(tags_json)).
const trackFeatureSelect = `
	SELECT t.path,
	       COALESCE(json_extract(t.tags_json, '$.title'),  '') AS title,
	       COALESCE(json_extract(t.tags_json, '$.artist'), '') AS artist,
	       COALESCE(json_extract(t.tags_json, '$.album'),  '') AS album,
	       COALESCE(json_extract(t.tags_json, '$.genre'),  '') AS genre,
	       json_extract(t.tags_json, '$.duration') AS duration,
	       COALESCE(json_extract(t.tags_json, '$.bpm'), ta.bpm) AS bpm,
	       ta.key_root AS key_root,
	       COALESCE(ta.key_mode, '') AS key_mode,
	       COALESCE(json_extract(t.tags_json, '$.replayGainTrackDB'), ta.replaygain_track_db) AS replaygain,
	       CAST(json_extract(t.tags_json, '$.sampleRate') AS INTEGER) AS sample_rate
	  FROM tracks t
	  LEFT JOIN track_analysis ta ON ta.source_path = t.path`

// TrackFeaturesForPaths hydrates metadata + analysis features for a set of
// track paths (the path lists the history aggregations produced). Chunks the
// IN-list to stay well under SQLite's bound-variable limit. Paths not in
// `tracks` (e.g. a since-deleted track) simply don't appear in the result —
// the caller drops them. Read path — no s.mu.
func (s *Store) TrackFeaturesForPaths(ctx context.Context, paths []string) ([]TrackFeatureRow, error) {
	const chunk = 400
	var out []TrackFeatureRow
	for i := 0; i < len(paths); i += chunk {
		end := i + chunk
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[i:end]
		q := trackFeatureSelect + ` WHERE t.path IN (` + placeholders(len(batch)) + `)`
		args := make([]any, len(batch))
		for j, p := range batch {
			args[j] = p
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		part, err := scanTrackFeatures(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

// PlayStatsByInterfaceInWindow returns the most-played track paths in
// [sinceNS, now) filtered to a SPECIFIC iface_type (e.g. "CarPlay" for the
// Drive Mix family). Same "30s rule" + plays-desc / last-played-tiebreak shape
// as PlayStatsInWindow — this is a deliberately separate signature so the
// existing PlayStatsInWindow callers (Heavy Rotation / Familiar / Forgotten)
// aren't widened to take an iface filter. Read path — no s.mu.
func (s *Store) PlayStatsByInterfaceInWindow(ctx context.Context, sinceNS int64, ifaceType string, minDuration float64, limit int) ([]PlayStatRow, error) {
	limit = clampLimit(limit, 100, 5000)
	q := `
		SELECT path, COUNT(*) AS n, MAX(started_at) AS last, MIN(started_at) AS first
		  FROM playback_history
		 WHERE duration_used >= ? AND started_at >= ? AND iface_type = ?
		 GROUP BY path ORDER BY n DESC, last DESC LIMIT ?`
	return s.scanPlayStats(ctx, q, minDuration, sinceNS, ifaceType, limit)
}

// OnRepeatCandidates returns track paths whose per-day repeat behaviour
// suggests sustained obsession — for the "On Repeat" family. A track qualifies
// when, in [sinceNS, now), it has:
//   - at least `minTotalPlays` total qualifying plays (summed across all
//     the user's paired devices), AND
//   - at least `minRepeatDays` distinct CALENDAR DAYS on which the path
//     was played ≥ 2 times (devices aggregated within the day — playing
//     once on iPhone + once on iPad on Monday counts as 2 plays on Monday,
//     and that day contributes to `repeat_days`).
//
// The query is TWO-LEVEL because a single GROUP BY (path, day) with HAVING
// ≥ 2 plays-per-day on a flat path-level aggregate is structurally
// impossible — HAVING would evaluate the total count, not the per-day count.
// The inner CTE groups by (path, day) to isolate daily counts (collapsing
// across devices — the user is one human); the outer aggregation sums total
// plays and counts daily buckets that crossed the threshold.
//
// Important: device_token is NOT in the inner GROUP BY. An earlier version
// included it and double-counted single calendar days with ≥ 2 plays per
// device-day bucket (e.g. iPhone 2 + iPad 2 on Monday surfaced as 2 repeat
// days instead of 1) — CodeRabbit caught it on PR #431. Returns PlayStatRow
// with Plays = total qualifying plays, LastPlayed = max day_last_played,
// FirstPlayed = min day_first_played. Read path — no s.mu.
func (s *Store) OnRepeatCandidates(ctx context.Context, sinceNS int64, minDuration float64, minTotalPlays, minRepeatDays, limit int) ([]PlayStatRow, error) {
	limit = clampLimit(limit, 100, 5000)
	if minTotalPlays < 1 {
		minTotalPlays = 1
	}
	if minRepeatDays < 1 {
		minRepeatDays = 1
	}
	q := `
		WITH per_day AS (
			SELECT path,
			       date(started_at / 1000000000, 'unixepoch') AS day,
			       COUNT(*)        AS plays_that_day,
			       MAX(started_at) AS day_last_played,
			       MIN(started_at) AS day_first_played
			  FROM playback_history
			 WHERE duration_used >= ? AND started_at >= ?
			 GROUP BY path, day
		),
		per_path AS (
			SELECT path,
			       SUM(plays_that_day)                                AS total_plays,
			       SUM(CASE WHEN plays_that_day >= 2 THEN 1 ELSE 0 END) AS repeat_days,
			       MAX(day_last_played)                               AS last_played,
			       MIN(day_first_played)                              AS first_played
			  FROM per_day
			 GROUP BY path
		)
		SELECT path, total_plays, last_played, first_played
		  FROM per_path
		 WHERE total_plays >= ? AND repeat_days >= ?
		 ORDER BY total_plays DESC, repeat_days DESC, last_played DESC
		 LIMIT ?`
	return s.scanPlayStats(ctx, q, minDuration, sinceNS, minTotalPlays, minRepeatDays, limit)
}

// LovedArtistDeepCuts returns library tracks by "loved" artists (≥ lovedMinPlays
// qualifying plays in [lovedSinceNS, now)) that have NO qualifying play in
// [deepCutCutoffNS, now) — for the "From Artists You Love" family. Per-artist
// cap is enforced in SQL via a window function (`ROW_NUMBER() OVER (PARTITION
// BY artist ORDER BY t.path)`), not by a Go-side scan, so a single high-volume
// catalog can't dominate the list.
//
// The returned rows are unplayed-or-stale tracks; Plays/LastPlayed/FirstPlayed
// carry placeholder values (the engine shuffles deterministically by week and
// doesn't read those fields for this family). Path is the sole load-bearing
// column. Read path — no s.mu.
func (s *Store) LovedArtistDeepCuts(ctx context.Context, lovedSinceNS int64, lovedMinPlays int, deepCutCutoffNS int64, minDuration float64, perArtistCap, limit int) ([]PlayStatRow, error) {
	limit = clampLimit(limit, 100, 5000)
	if perArtistCap < 1 {
		perArtistCap = 3
	}
	if lovedMinPlays < 1 {
		lovedMinPlays = 3
	}
	// COALESCE on the JOIN/PARTITION key blocks SQLite from using any index
	// on json_extract(tags_json, '$.artist'); bare json_extract evaluates to
	// NULL for missing artists and `NULL != ''` is falsy, so the empty-string
	// filter is preserved without the COALESCE wrapper (Gemini bot review on
	// PR #431).
	q := `
		WITH loved_artists AS (
			SELECT json_extract(t.tags_json, '$.artist') AS artist,
			       COUNT(*) AS plays_in_window
			  FROM playback_history ph
			  JOIN tracks t ON t.path = ph.path
			 WHERE ph.duration_used >= ?
			   AND ph.started_at  >= ?
			   AND json_extract(t.tags_json, '$.artist') != ''
			 GROUP BY artist
			HAVING plays_in_window >= ?
		),
		recently_played AS (
			SELECT DISTINCT path
			  FROM playback_history
			 WHERE duration_used >= ?
			   AND started_at  >= ?
		),
		eligible AS (
			SELECT t.path,
			       la.artist AS artist,
			       ROW_NUMBER() OVER (
			         PARTITION BY la.artist
			         ORDER BY t.path
			       ) AS rn
			  FROM tracks t
			  JOIN loved_artists la
			    ON la.artist = json_extract(t.tags_json, '$.artist')
			 WHERE t.path NOT IN (SELECT path FROM recently_played)
		)
		SELECT path, 1 AS plays, 0 AS last_played, 0 AS first_played
		  FROM eligible
		 WHERE rn <= ?
		 ORDER BY artist ASC, rn ASC
		 LIMIT ?`
	return s.scanPlayStats(ctx, q,
		minDuration, lovedSinceNS, lovedMinPlays,
		minDuration, deepCutCutoffNS,
		perArtistCap, limit)
}

// QuietSlowTrackFeatures returns the candidate pool for the "Wind Down" mood
// band: tracks whose EFFECTIVE BPM (curated tag wins over analysis) is ≤
// bpmMax AND whose EFFECTIVE ReplayGain is > loudnessMin (less-negative
// ReplayGain = quieter master at -18 LUFS). NULL on either field excludes the
// track.
//
// IMPORTANT — NO restrictive caller `limit`. The engine performs a
// deterministic weekly shuffle over the WHOLE matching cohort and then caps
// the result; if SQL returned only the first N rows by physical scan order,
// tracks past position N would be permanently invisible to the band's
// rotation. A safety-rail LIMIT (`smartPlaylistPoolSafetyLimit`) bounds the
// pool against pathological libraries. Read path — no s.mu.
func (s *Store) QuietSlowTrackFeatures(ctx context.Context, bpmMax int, loudnessMin float64) ([]TrackFeatureRow, error) {
	q := trackFeatureSelect + `
		 WHERE COALESCE(json_extract(t.tags_json, '$.bpm'),                ta.bpm)                 IS NOT NULL
		   AND COALESCE(json_extract(t.tags_json, '$.bpm'),                ta.bpm)                 <= ?
		   AND COALESCE(json_extract(t.tags_json, '$.replayGainTrackDB'),  ta.replaygain_track_db) IS NOT NULL
		   AND COALESCE(json_extract(t.tags_json, '$.replayGainTrackDB'),  ta.replaygain_track_db) > ?
		 ORDER BY t.path ASC
		 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, bpmMax, loudnessMin, smartPlaylistPoolSafetyLimit)
	if err != nil {
		return nil, err
	}
	return scanTrackFeatures(rows)
}

// LoudFastTrackFeatures is the symmetric pool for the "Lift Off" mood band:
// EFFECTIVE BPM ≥ bpmMin AND EFFECTIVE ReplayGain ≤ loudnessMax (more-negative
// = louder master). Same NULL handling + safety-limit rationale as
// QuietSlowTrackFeatures. Read path — no s.mu.
func (s *Store) LoudFastTrackFeatures(ctx context.Context, bpmMin int, loudnessMax float64) ([]TrackFeatureRow, error) {
	q := trackFeatureSelect + `
		 WHERE COALESCE(json_extract(t.tags_json, '$.bpm'),                ta.bpm)                 IS NOT NULL
		   AND COALESCE(json_extract(t.tags_json, '$.bpm'),                ta.bpm)                 >= ?
		   AND COALESCE(json_extract(t.tags_json, '$.replayGainTrackDB'),  ta.replaygain_track_db) IS NOT NULL
		   AND COALESCE(json_extract(t.tags_json, '$.replayGainTrackDB'),  ta.replaygain_track_db) <= ?
		 ORDER BY t.path ASC
		 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, bpmMin, loudnessMax, smartPlaylistPoolSafetyLimit)
	if err != nil {
		return nil, err
	}
	return scanTrackFeatures(rows)
}

// smartPlaylistPoolSafetyLimit guards mood-band selectors against a
// pathological library size. Current realistic libraries are well below;
// pools larger than this are silently truncated (the safety-rail tradeoff).
const smartPlaylistPoolSafetyLimit = 50_000

// AnalyzedTrackFeatures returns the candidate pool for harmonic sequencing +
// Daily Mix discovery: tracks WITH an estimated key (key_root NOT NULL).
// Optional exact genre filter. Ordered by path (deterministic, so the daily
// cache is stable) and capped so a huge library doesn't materialize
// everything — the engine only needs a working set. Read path — no s.mu.
func (s *Store) AnalyzedTrackFeatures(ctx context.Context, genre string, limit int) ([]TrackFeatureRow, error) {
	limit = clampLimit(limit, 5000, 50000)
	q := trackFeatureSelect + ` WHERE ta.key_root IS NOT NULL`
	var args []any
	if genre != "" {
		q += ` AND COALESCE(json_extract(t.tags_json, '$.genre'), '') = ?`
		args = append(args, genre)
	}
	q += ` ORDER BY t.path ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanTrackFeatures(rows)
}

// scanTrackFeatures consumes (and closes) rows shaped by trackFeatureSelect.
func scanTrackFeatures(rows *sql.Rows) ([]TrackFeatureRow, error) {
	defer rows.Close()
	var out []TrackFeatureRow
	for rows.Next() {
		var r TrackFeatureRow
		var dur, rg sql.NullFloat64
		var bpm, keyRoot, sampleRate sql.NullInt64
		if err := rows.Scan(&r.Path, &r.Title, &r.Artist, &r.Album, &r.Genre,
			&dur, &bpm, &keyRoot, &r.KeyMode, &rg, &sampleRate); err != nil {
			return nil, err
		}
		if dur.Valid {
			v := dur.Float64
			r.Duration = &v
		}
		if bpm.Valid {
			v := int(bpm.Int64)
			r.BPM = &v
		}
		if keyRoot.Valid {
			v := int(keyRoot.Int64)
			r.KeyRoot = &v
		}
		if rg.Valid {
			v := rg.Float64
			r.ReplayGainTrackDB = &v
		}
		if sampleRate.Valid {
			v := int(sampleRate.Int64)
			r.SampleRate = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- small local helpers ---

// placeholders returns "?,?,...,?" with n marks for an IN-list.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := strings.Repeat("?,", n)
	return b[:len(b)-1]
}

// clampLimit applies a default (when limit <= 0) and a hard ceiling.
func clampLimit(limit, def, ceiling int) int {
	if limit <= 0 {
		return def
	}
	if limit > ceiling {
		return ceiling
	}
	return limit
}
