// Folder-scoped metadata reads + retries for the admin Library
// Inspector's Atlas layer (tile artwork refs, About-card detail,
// per-folder "retry missing metadata").
//
// COST DISCIPLINE: StreamTrackMetaRefsUnderPrefix and the Distinct*
// UnderPrefix enumerators are json_extract subtree walks — the same
// cost class as AtlasMetaBreakdownCounts / FormatDistribution. They
// are for CLICK-DRIVEN admin endpoints only, always behind the admin
// server's TTL + singleflight cache, and must NEVER run on an SSE
// tick (CLAUDE.md composition-bars discipline).
package manifest

import (
	"context"
	"encoding/json"
	"fmt"
)

// TrackMetaRef is the slim per-track MBID projection the inspector's
// metadata endpoints group by child folder. ArtworkMBID may be a
// MusicBrainz release UUID or a `local-<sha256>` curated-art sentinel
// — both are servable cover refs. ArtworkVersion is the premium-cover
// cache-buster column (empty when unset).
type TrackMetaRef struct {
	Path           string
	ArtworkMBID    string
	ArtistMBID     string
	ReleaseMBID    string
	ArtworkVersion string
}

// StreamTrackMetaRefsUnderPrefix walks every track under `prefix`
// ("" = whole library) and yields the MBID projection per row. The
// callback MUST NOT retain the value past its invocation (the
// StreamTracks contract — the struct is reused across iterations).
// Read-only; no s.mu.
func (s *Store) StreamTrackMetaRefsUnderPrefix(ctx context.Context, prefix string, fn func(TrackMetaRef) error) error {
	q := `
		SELECT t.path,
		       COALESCE(json_extract(t.tags_json, '$.artworkMBID'),        ''),
		       COALESCE(json_extract(t.tags_json, '$.artistMBID'),         ''),
		       COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), ''),
		       COALESCE(t.artwork_version, '')
		  FROM tracks t`
	var args []any
	if prefix != "" {
		// likeEscape escapes %, _, AND the escape char itself, so a
		// folder literally named `Rock \ Metal` can't produce a broken
		// escape sequence (store.go:likeEscape).
		q += `
		 WHERE t.path LIKE ? ESCAPE '\'`
		args = append(args, likeEscape(prefix)+"/%")
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("stream meta refs %q: %w", prefix, err)
	}
	defer rows.Close()
	var ref TrackMetaRef
	for rows.Next() {
		ref = TrackMetaRef{}
		if err := rows.Scan(&ref.Path, &ref.ArtworkMBID, &ref.ArtistMBID,
			&ref.ReleaseMBID, &ref.ArtworkVersion); err != nil {
			return err
		}
		if err := fn(ref); err != nil {
			return err
		}
	}
	return rows.Err()
}

// BookletState is the per-release booklet verdict for the inspector:
// Available = Atlas has a booklet for the release; Fetched = the PDF
// is already in the local cache dir.
type BookletState struct {
	Available bool
	Fetched   bool
}

// BookletStatesIn returns the booklet state for each of the given
// release MBIDs. Absent rows simply don't appear in the map (zero
// value = not available). Single json_each bind (BookletsToCheck
// pattern). Read-only; no s.mu.
func (s *Store) BookletStatesIn(ctx context.Context, mbids []string) (map[string]BookletState, error) {
	if len(mbids) == 0 {
		return map[string]BookletState{}, nil
	}
	blob, err := json.Marshal(mbids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT release_mbid, available, fetched_at IS NOT NULL
		  FROM booklets
		 WHERE release_mbid IN (SELECT value FROM json_each(?))
	`, string(blob))
	if err != nil {
		return nil, fmt.Errorf("booklet states: %w", err)
	}
	defer rows.Close()
	out := make(map[string]BookletState, len(mbids))
	for rows.Next() {
		var mbid string
		var avail, fetched int
		if err := rows.Scan(&mbid, &avail, &fetched); err != nil {
			return nil, err
		}
		out[mbid] = BookletState{Available: avail != 0, Fetched: fetched != 0}
	}
	return out, rows.Err()
}

// ResetEnrichedMissesUnderPrefix is the prefix-scoped variant of
// ResetEnrichedMisses (PR #495): re-queue enriched-but-incomplete
// tracks (missing artworkMBID or artistMBID) UNDER the given folder
// so the enricher re-runs just that slice of the library. Same
// enriched-rows-only predicate — a full MB/CAA re-crawl is never
// triggered — and indexed_at is deliberately untouched (no iOS delta
// churn).
//
// SANCTIONED enriched_at WRITER: this joins the enricher itself and
// the PR #495 library-wide retry pair as the only code allowed to
// write enriched_at (the `WHERE enriched_at = 0` worker queue rides
// on it). It is reachable ONLY from the admin's rate-guarded
// POST /api/library/enrichment/retry. Empty prefix delegates to the
// library-wide ResetEnrichedMisses. Holds s.mu (writer contract).
func (s *Store) ResetEnrichedMissesUnderPrefix(ctx context.Context, prefix string) (int64, error) {
	if prefix == "" {
		return s.ResetEnrichedMisses(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks SET enriched_at = 0
		 WHERE enriched_at > 0
		   AND path LIKE ? ESCAPE '\'
		   AND (COALESCE(json_extract(tags_json, '$.artworkMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.artistMBID'), '') = '')
	`, likeEscape(prefix)+"/%")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DistinctArtistMBIDsUnderPrefix is the folder-scoped variant of
// DistinctArtistMBIDs. Read-only; no s.mu.
func (s *Store) DistinctArtistMBIDsUnderPrefix(ctx context.Context, prefix string) ([]string, error) {
	if prefix == "" {
		return s.DistinctArtistMBIDs(ctx)
	}
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artistMBID')
		  FROM tracks
		 WHERE path LIKE ? ESCAPE '\'
		   AND json_extract(tags_json, '$.artistMBID') IS NOT NULL
		   AND json_extract(tags_json, '$.artistMBID') != ''
	`, likeEscape(prefix)+"/%"))
}

// DistinctReleaseMBIDsUnderPrefix enumerates the distinct release
// UUIDs under a folder: the UNION of UUID-form artworkMBIDs (the
// enricher's cover keys) and musicBrainzAlbumIDs (the booklet /
// About-card key), both excluding the `local-<sha>` curated-art
// sentinels. Mirrors the AtlasMetaBreakdownCounts release-universe
// shape, scoped. Read-only; no s.mu.
func (s *Store) DistinctReleaseMBIDsUnderPrefix(ctx context.Context, prefix string) ([]string, error) {
	// Two static query texts (no fragment concatenation — keeps the
	// scoped form a fully-constant statement with bound params only).
	if prefix == "" {
		return collectStringColumn(s.db.QueryContext(ctx, `
			SELECT DISTINCT json_extract(tags_json, '$.artworkMBID') AS mbid
			  FROM tracks
			 WHERE json_extract(tags_json, '$.artworkMBID') IS NOT NULL
			   AND json_extract(tags_json, '$.artworkMBID') != ''
			   AND json_extract(tags_json, '$.artworkMBID') NOT LIKE 'local-%'
			UNION
			SELECT DISTINCT json_extract(tags_json, '$.musicBrainzAlbumID') AS mbid
			  FROM tracks
			 WHERE json_extract(tags_json, '$.musicBrainzAlbumID') IS NOT NULL
			   AND json_extract(tags_json, '$.musicBrainzAlbumID') != ''
			   AND json_extract(tags_json, '$.musicBrainzAlbumID') NOT LIKE 'local-%'
		`))
	}
	pattern := likeEscape(prefix) + "/%"
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artworkMBID') AS mbid
		  FROM tracks
		 WHERE path LIKE ? ESCAPE '\'
		   AND json_extract(tags_json, '$.artworkMBID') IS NOT NULL
		   AND json_extract(tags_json, '$.artworkMBID') != ''
		   AND json_extract(tags_json, '$.artworkMBID') NOT LIKE 'local-%'
		UNION
		SELECT DISTINCT json_extract(tags_json, '$.musicBrainzAlbumID') AS mbid
		  FROM tracks
		 WHERE path LIKE ? ESCAPE '\'
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') IS NOT NULL
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') != ''
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') NOT LIKE 'local-%'
	`, pattern, pattern))
}

// ResetBookletChecks zeroes check_attempts for the NOT-yet-available
// rows in the given set so the next harvest check cycle re-probes
// them (BookletsToCheck gates on attempts < cap). Rows already
// available are untouched — their lifecycle is the fetch sweep's.
// Returns the number of rows reset. Holds s.mu (writer contract).
func (s *Store) ResetBookletChecks(ctx context.Context, mbids []string) (int64, error) {
	if len(mbids) == 0 {
		return 0, nil
	}
	blob, err := json.Marshal(mbids)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE booklets SET check_attempts = 0
		 WHERE release_mbid IN (SELECT value FROM json_each(?))
		   AND available = 0
	`, string(blob))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
