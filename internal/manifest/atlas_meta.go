package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ReleaseAtlasMeta is the rich-tier Atlas metadata cached for a release
// (keyed by MusicBrainz release MBID). Found=false is a TOMBSTONE: the iOS
// app queried Atlas and it had nothing, recorded so the entity isn't
// re-queried on every view. Served via GET /v1/atlas-meta/release/{mbid};
// written via POST /v1/atlas-ingest.
type ReleaseAtlasMeta struct {
	ReleaseMBID string
	Found       bool
	Description string
	RecordLabel string
	Genres      []string
	// Source + SourceURL attribute the winning Description (e.g. "bandcamp" + the
	// album page) so iOS can render "Read more on <source>" for CC-BY-SA / ToS
	// compliance. Empty when no description, or for a tombstone.
	Source     string
	SourceURL  string
	AtlasETag  string
	IngestedAt time.Time
}

// ArtistAtlasMeta is the artist analogue of ReleaseAtlasMeta.
type ArtistAtlasMeta struct {
	ArtistMBID string
	Found      bool
	Bio        string
	BioSummary string
	Genres     []string
	// Source + SourceURL attribute the winning Bio (e.g. "wiki" + the Wikipedia
	// URL). Empty when no bio, or for a tombstone.
	Source     string
	SourceURL  string
	AtlasETag  string
	IngestedAt time.Time
}

// UpsertReleaseAtlasMeta writes (or replaces) the cached metadata for a
// release MBID. ingested_at is stamped bridge-side (s.now()) — the
// m.IngestedAt field is IGNORED on write; the bridge is the source of truth
// for its own cache freshness (avoids client-clock TOCTOU). Concurrent
// ingests of the same MBID from two devices can't violate the PK (UPSERT).
// Holds Store.mu like every writer.
func (s *Store) UpsertReleaseAtlasMeta(ctx context.Context, m ReleaseAtlasMeta) error {
	// A tombstone (Found=false) carries no attribution — enforce the documented
	// "source/sourceUrl omitted on a tombstone" contract at the write boundary so
	// a malformed/stale client value can't leak onto a tombstone read. Covers
	// both the ferry ingest and the harvest sink (CodeRabbit on PR #410).
	if !m.Found {
		m.Source, m.SourceURL = "", ""
	}
	genres, err := marshalGenres(m.Genres)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO release_atlas(release_mbid, description, record_label, genres_json, description_source, description_source_url, found, atlas_etag, ingested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(release_mbid) DO UPDATE SET
			description            = excluded.description,
			record_label           = excluded.record_label,
			genres_json            = excluded.genres_json,
			description_source     = excluded.description_source,
			description_source_url = excluded.description_source_url,
			found                  = excluded.found,
			atlas_etag             = excluded.atlas_etag,
			ingested_at            = excluded.ingested_at
	`, m.ReleaseMBID, m.Description, m.RecordLabel, genres, m.Source, m.SourceURL, boolToInt(m.Found), m.AtlasETag, s.now().UnixNano())
	return err
}

// UpsertArtistAtlasMeta is the artist analogue of UpsertReleaseAtlasMeta.
func (s *Store) UpsertArtistAtlasMeta(ctx context.Context, m ArtistAtlasMeta) error {
	// Tombstone carries no attribution — see UpsertReleaseAtlasMeta.
	if !m.Found {
		m.Source, m.SourceURL = "", ""
	}
	genres, err := marshalGenres(m.Genres)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO artist_atlas(artist_mbid, bio, bio_summary, genres_json, bio_source, bio_source_url, found, atlas_etag, ingested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(artist_mbid) DO UPDATE SET
			bio            = excluded.bio,
			bio_summary    = excluded.bio_summary,
			genres_json    = excluded.genres_json,
			bio_source     = excluded.bio_source,
			bio_source_url = excluded.bio_source_url,
			found          = excluded.found,
			atlas_etag     = excluded.atlas_etag,
			ingested_at    = excluded.ingested_at
	`, m.ArtistMBID, m.Bio, m.BioSummary, genres, m.Source, m.SourceURL, boolToInt(m.Found), m.AtlasETag, s.now().UnixNano())
	return err
}

// GetReleaseAtlasMeta returns the cached metadata for a release MBID, or
// (nil, nil) when no row exists (never checked). A tombstone row returns a
// value with Found=false. Reads are un-mutexed (WAL handles concurrent
// readers).
func (s *Store) GetReleaseAtlasMeta(ctx context.Context, mbid string) (*ReleaseAtlasMeta, error) {
	var desc, label, genres, source, sourceURL, etag string
	var found int
	var ingestedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT description, record_label, genres_json, description_source, description_source_url, found, atlas_etag, ingested_at
		FROM release_atlas WHERE release_mbid = ?
	`, mbid).Scan(&desc, &label, &genres, &source, &sourceURL, &found, &etag, &ingestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ReleaseAtlasMeta{
		ReleaseMBID: mbid,
		Found:       found != 0,
		Description: desc,
		RecordLabel: label,
		Genres:      unmarshalGenres(genres),
		Source:      source,
		SourceURL:   sourceURL,
		AtlasETag:   etag,
		IngestedAt:  time.Unix(0, ingestedAt),
	}, nil
}

// GetArtistAtlasMeta is the artist analogue of GetReleaseAtlasMeta.
func (s *Store) GetArtistAtlasMeta(ctx context.Context, mbid string) (*ArtistAtlasMeta, error) {
	var bio, summary, genres, source, sourceURL, etag string
	var found int
	var ingestedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT bio, bio_summary, genres_json, bio_source, bio_source_url, found, atlas_etag, ingested_at
		FROM artist_atlas WHERE artist_mbid = ?
	`, mbid).Scan(&bio, &summary, &genres, &source, &sourceURL, &found, &etag, &ingestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ArtistAtlasMeta{
		ArtistMBID: mbid,
		Found:      found != 0,
		Bio:        bio,
		BioSummary: summary,
		Genres:     unmarshalGenres(genres),
		Source:     source,
		SourceURL:  sourceURL,
		AtlasETag:  etag,
		IngestedAt: time.Unix(0, ingestedAt),
	}, nil
}

// AtlasMetaBreakdown is the have/missing coverage summary for the rich-tier
// Atlas metadata the bridge caches: artist bios (artist_atlas) and album
// descriptions (release_atlas), each measured against the library's distinct
// MBID universe. Tombstone rows (found=0) count as MISSING — the entity was
// checked and nothing was found, which is exactly the gap the dashboard's
// coverage stats should surface.
type AtlasMetaBreakdown struct {
	// ArtistsTotal is the number of distinct artist MBIDs in the library
	// (the harvest submit universe); ArtistBiosFound counts those with a
	// found=1 artist_atlas row.
	ArtistsTotal    int
	ArtistBiosFound int
	// ReleasesTotal is the distinct release-MBID universe — the union of
	// UUID-form artworkMBIDs and musicBrainzAlbumIDs (the same union the
	// harvest submits via DistinctReleaseMBIDs + DistinctReleaseTextMBIDs,
	// so "missing" here is exactly the set that could ever be filled).
	// ReleaseDescsFound counts those with a found=1 release_atlas row.
	ReleasesTotal     int
	ReleaseDescsFound int
}

// AtlasMetaBreakdownCounts computes the coverage summary in ONE statement so
// the four counts come from a single consistent read. The artist / release
// CTEs are full-table json_extract scans (the MBIDs live in tags_json), so —
// like FormatDistribution and EnrichmentBreakdown — this MUST only be called
// behind a TTL cache + singleflight (admin getEnrichmentMetaSnapshot, 60s),
// NEVER on the SSE fast tick. Read-only, no s.mu (WAL concurrent reader).
func (s *Store) AtlasMetaBreakdownCounts(ctx context.Context) (AtlasMetaBreakdown, error) {
	var b AtlasMetaBreakdown
	err := s.db.QueryRowContext(ctx, `
		WITH artists(mbid) AS (
			SELECT DISTINCT json_extract(tags_json, '$.artistMBID')
			  FROM tracks
			 WHERE json_extract(tags_json, '$.artistMBID') IS NOT NULL
			   AND json_extract(tags_json, '$.artistMBID') != ''
		),
		releases(mbid) AS (
			SELECT DISTINCT json_extract(tags_json, '$.artworkMBID')
			  FROM tracks
			 WHERE json_extract(tags_json, '$.artworkMBID') IS NOT NULL
			   AND json_extract(tags_json, '$.artworkMBID') != ''
			   AND json_extract(tags_json, '$.artworkMBID') NOT LIKE 'local-%'
			UNION
			SELECT DISTINCT json_extract(tags_json, '$.musicBrainzAlbumID')
			  FROM tracks
			 WHERE json_extract(tags_json, '$.musicBrainzAlbumID') IS NOT NULL
			   AND json_extract(tags_json, '$.musicBrainzAlbumID') != ''
			   AND json_extract(tags_json, '$.musicBrainzAlbumID') NOT LIKE 'local-%'
		)
		SELECT
			(SELECT COUNT(*) FROM artists),
			(SELECT COUNT(*) FROM artists a
			  WHERE EXISTS (SELECT 1 FROM artist_atlas m
			                 WHERE m.artist_mbid = a.mbid AND m.found = 1)),
			(SELECT COUNT(*) FROM releases),
			(SELECT COUNT(*) FROM releases r
			  WHERE EXISTS (SELECT 1 FROM release_atlas m
			                 WHERE m.release_mbid = r.mbid AND m.found = 1))
	`).Scan(&b.ArtistsTotal, &b.ArtistBiosFound, &b.ReleasesTotal, &b.ReleaseDescsFound)
	return b, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// marshalGenres encodes a genres slice as a JSON array for storage. A nil/
// empty slice stores "[]" so the column is always valid JSON.
func marshalGenres(g []string) (string, error) {
	if len(g) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalGenres decodes the stored JSON array; a malformed/empty value
// yields nil (the field is omitempty on the wire).
func unmarshalGenres(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
