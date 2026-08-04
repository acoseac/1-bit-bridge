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

// Enrichment miss facets. These name the three arms of
// enrichmentMissPredicateSQL (store.go) so an operator can ask WHICH
// field a track is short of, not merely that it is short of something.
const (
	MissFacetArtwork = "artwork"
	MissFacetArtist  = "artist"
	MissFacetRelease = "release"
)

// MissFacets reports which enrichment facets this row is missing, in a
// stable order. An empty result means the row is fully matched.
//
// LOCKSTEP MIRROR of enrichmentMissPredicateSQL (store.go): three arms,
// each "field is empty", OR'd together. The SQL already COALESCEs each
// column to ” in StreamTrackMetaRefsUnderPrefix, so an empty string here
// means exactly what `COALESCE(...) = ”` means there.
//
// Keeping these in step is load-bearing: the dashboard's "missing" count,
// the "Retry missing" button, and this enumeration must all describe the
// same set of rows, or the operator is once again reading a number that
// doesn't mean what the button does (the bug #596 fixed). Pinned by
// TestMissFacetsMirrorsTheMissPredicate.
func (r TrackMetaRef) MissFacets() []string {
	var out []string
	if r.ArtworkMBID == "" {
		out = append(out, MissFacetArtwork)
	}
	if r.ArtistMBID == "" {
		out = append(out, MissFacetArtist)
	}
	if r.ReleaseMBID == "" {
		out = append(out, MissFacetRelease)
	}
	return out
}

// IsMiss reports whether the row would be re-queued by "Retry missing".
func (r TrackMetaRef) IsMiss() bool {
	return r.ArtworkMBID == "" || r.ArtistMBID == "" || r.ReleaseMBID == ""
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
	// subtreeRangeBase trims a caller-supplied trailing slash before the
	// bounds append their own, and treats a trims-to-empty prefix as
	// whole-library. The byte-range form needs no LIKE escaping at all:
	// a folder named `Rock \ Metal`, `100% Hits` or `foo_bar` is bound
	// as a plain parameter, so there is no pattern metacharacter to
	// escape and no escape sequence to get wrong.
	//
	// Every production caller here already normalises via
	// normaliseBrowsePath, so the trim is defence in depth — but these
	// four helpers were the ones missed when the same guard was added
	// to the store.go prefix family, and the sibling that lacked it is
	// exactly how the class recurs.
	if base, scoped := subtreeRangeBase(prefix); scoped {
		q += `
		 WHERE t.path COLLATE BINARY >= ? || '/'
		   AND t.path COLLATE BINARY < ? || '0'`
		args = append(args, base, base)
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
	// Unscoped ("" / "/" / "//") delegates to the library-wide reset —
	// decided AFTER the trim, so a slash-only prefix can't fall through
	// to a `LIKE '/%'` that silently resets nothing.
	base, scoped := subtreeRangeBase(prefix)
	if !scoped {
		return s.ResetEnrichedMisses(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, resetEnrichedMissesUnderPrefixSQL, base, base)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DistinctArtistMBIDsUnderPrefix is the folder-scoped variant of
// DistinctArtistMBIDs. Read-only; no s.mu.
func (s *Store) DistinctArtistMBIDsUnderPrefix(ctx context.Context, prefix string) ([]string, error) {
	base, scoped := subtreeRangeBase(prefix)
	if !scoped {
		return s.DistinctArtistMBIDs(ctx)
	}
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artistMBID')
		  FROM tracks
		 WHERE path COLLATE BINARY >= ? || '/'
		   AND path COLLATE BINARY < ? || '0'
		   AND json_extract(tags_json, '$.artistMBID') IS NOT NULL
		   AND json_extract(tags_json, '$.artistMBID') != ''
	`, base, base))
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
	// Which one runs is decided AFTER the trim, so "/" and "//" take
	// the whole-library branch rather than a `LIKE '/%'` that matches
	// nothing.
	base, scoped := subtreeRangeBase(prefix)
	if !scoped {
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
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artworkMBID') AS mbid
		  FROM tracks
		 WHERE path COLLATE BINARY >= ? || '/'
		   AND path COLLATE BINARY < ? || '0'
		   AND json_extract(tags_json, '$.artworkMBID') IS NOT NULL
		   AND json_extract(tags_json, '$.artworkMBID') != ''
		   AND json_extract(tags_json, '$.artworkMBID') NOT LIKE 'local-%'
		UNION
		SELECT DISTINCT json_extract(tags_json, '$.musicBrainzAlbumID') AS mbid
		  FROM tracks
		 WHERE path COLLATE BINARY >= ? || '/'
		   AND path COLLATE BINARY < ? || '0'
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') IS NOT NULL
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') != ''
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') NOT LIKE 'local-%'
	`, base, base, base, base))
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
