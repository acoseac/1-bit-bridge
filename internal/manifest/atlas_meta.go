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
