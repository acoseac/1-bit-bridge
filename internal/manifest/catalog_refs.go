// Streaming per-track projection for the admin web player's library
// catalog (internal/librarycat).
//
// COST DISCIPLINE: StreamCatalogRefs is a full-table json_extract walk —
// the AtlasMetaBreakdownCounts / FormatDistribution / dupe-refs cost
// class. It backs a CACHED snapshot on a click-driven admin surface and
// must NEVER run on an SSE tick.
//
// WHY NOT WIDEN StreamTrackDupeRefsUnderPrefix, which projects most of
// these columns already:
//
//  1. That method feeds the duplicate-stamping pass on the post-scan
//     tail and the `bridge duplicates` CLI. Six more json_extract
//     fields per row would make a hot post-scan pass pay for data it
//     never reads.
//  2. Its projection is a MIRROR INPUT. Its docblock exists to explain
//     why disc/track carry no COALESCE and why geometry comes from the
//     v25 columns; growing it with genre / composer / MBIDs invites
//     keying the mirror on non-mirror fields.
//  3. It deliberately has NO dupe_suppressed filter, because the dupes
//     report must see suppressed rows. The catalog must not.
//  4. It excludes routed rows by default and yields a paired
//     DupeStampState the catalog has no use for.

package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// CatalogRef is one track's catalog projection. Field-for-field the
// input librarycat.Row needs; the admin adapter copies across.
type CatalogRef struct {
	Path        string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Year        int
	Disc        int
	DiscTagged  bool
	Track       int
	TrackTagged bool
	Size        int64
	MTimeNS     int64
	Duration    float64

	SampleRate    int
	BitsPerSample int
	IsDSD         bool
	Codec         string

	Genre          string
	Composer       string
	ArtworkMBID    string
	ArtworkVersion string
	ReleaseMBID    string
	ArtistMBID     string
	IndexedAt      int64
	RoutedUDN      string
}

// catalogRefSelect reads the catalog projection. Four choices, each
// load-bearing:
//
//   - WHERE t.dupe_suppressed = 0. The player is an admin surface but a
//     LISTENER surface: the operator must not be able to queue a track
//     the phone's manifest doesn't contain, and album track counts must
//     agree with iOS. This is the served-set rule applied one level out
//     from /v1, matching trackFeatureSelect's reasoning.
//
//   - Routed rows are INCLUDED, via LEFT JOIN rather than the usual
//     NOT EXISTS anti-join. On a hybrid library they are the
//     overwhelming majority — excluding them would leave the player
//     showing a rounding error of the collection — and the join hands
//     back server_udn for free, which the online/offline badge needs.
//     upnp_track_routing's PK on source_path keeps it a lookup, not a
//     scan, and yields at most one row per track (no fan-out).
//
//   - Geometry reads from tags_json, NOT the v25 accelerator columns.
//     The dupes projection takes the columns because its tier logic
//     wants "unknown → degrade"; the catalog's quality bucket is
//     wire-facing, and FormatDistribution — the closest wire-facing
//     analog — keeps tags_json as read-truth for exactly that reason.
//
//   - Every projected value is aliased tag_ / row_. There is no GROUP BY
//     here so the v25 alias-shadowing trap cannot fire — but naming an
//     alias `codec` or `is_dsd` beside real columns of those names is
//     how it gets armed for the next person. If a GROUP BY is ever
//     added to this query, use ORDINALS (see FormatDistribution).
//
// No ORDER BY: the fold is order-independent by construction, so
// sorting in SQLite would buy nothing. No prefix scoping: the catalog
// is whole-library by definition, and a scope parameter would only
// invite building N partial catalogs.
const catalogRefSelect = `
	SELECT t.path,
	       COALESCE(json_extract(t.tags_json, '$.title'),              '') AS tag_title,
	       COALESCE(json_extract(t.tags_json, '$.artist'),             '') AS tag_artist,
	       COALESCE(json_extract(t.tags_json, '$.albumArtist'),        '') AS tag_album_artist,
	       COALESCE(json_extract(t.tags_json, '$.album'),              '') AS tag_album,
	       COALESCE(json_extract(t.tags_json, '$.genre'),              '') AS tag_genre,
	       COALESCE(json_extract(t.tags_json, '$.composer'),           '') AS tag_composer,
	       COALESCE(json_extract(t.tags_json, '$.year'),                0) AS tag_year,
	       json_extract(t.tags_json, '$.discNumber')                      AS tag_disc,
	       json_extract(t.tags_json, '$.trackNumber')                     AS tag_track,
	       COALESCE(json_extract(t.tags_json, '$.duration'),            0) AS tag_duration,
	       COALESCE(t.size, 0)                                            AS row_size,
	       COALESCE(t.mtime_ns, 0)                                        AS row_mtime_ns,
	       CAST(COALESCE(json_extract(t.tags_json, '$.sampleRate'),    0) AS INTEGER) AS tag_rate,
	       CAST(COALESCE(json_extract(t.tags_json, '$.bitsPerSample'), 0) AS INTEGER) AS tag_bits,
	       CAST(COALESCE(json_extract(t.tags_json, '$.isDSD'),         0) AS INTEGER) AS tag_is_dsd,
	       COALESCE(json_extract(t.tags_json, '$.codec'),              '') AS tag_codec,
	       COALESCE(json_extract(t.tags_json, '$.artworkMBID'),        '') AS tag_artwork_mbid,
	       COALESCE(t.artwork_version,                                 '') AS row_artwork_version,
	       COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), '') AS tag_release_mbid,
	       COALESCE(json_extract(t.tags_json, '$.artistMBID'),         '') AS tag_artist_mbid,
	       t.indexed_at                                                   AS row_indexed_at,
	       COALESCE(r.server_udn,                                      '') AS row_routed_udn
	  FROM tracks t
	  LEFT JOIN upnp_track_routing r ON r.source_path = t.path
	 WHERE t.dupe_suppressed = 0`

// StreamCatalogRefs walks every SERVED track and yields its catalog
// projection.
//
// The callback MUST NOT retain the value past its invocation — the
// struct is reused across iterations, the same contract StreamTracks
// and StreamTrackDupeRefsUnderPrefix carry. librarycat copies what it
// keeps. Read-only; no s.mu.
func (s *Store) StreamCatalogRefs(ctx context.Context, fn func(CatalogRef) error) error {
	rows, err := s.db.QueryContext(ctx, catalogRefSelect)
	if err != nil {
		return fmt.Errorf("stream catalog refs: %w", err)
	}
	defer rows.Close()
	var (
		ref   CatalogRef
		disc  sql.NullInt64
		track sql.NullInt64
		isDSD int
	)
	for rows.Next() {
		ref = CatalogRef{}
		disc, track, isDSD = sql.NullInt64{}, sql.NullInt64{}, 0
		if err := rows.Scan(&ref.Path, &ref.Title, &ref.Artist, &ref.AlbumArtist,
			&ref.Album, &ref.Genre, &ref.Composer, &ref.Year, &disc, &track,
			&ref.Duration, &ref.Size, &ref.MTimeNS, &ref.SampleRate, &ref.BitsPerSample, &isDSD,
			&ref.Codec, &ref.ArtworkMBID, &ref.ArtworkVersion, &ref.ReleaseMBID,
			&ref.ArtistMBID, &ref.IndexedAt, &ref.RoutedUDN); err != nil {
			return err
		}
		// discNumber / trackNumber are read WITHOUT COALESCE on
		// purpose: the client falls back to folder/filename inference
		// only when the tag is ABSENT, and an explicit 0 is a value.
		// Collapsing NULL and 0 here would silently change album
		// ordering and group membership.
		if disc.Valid {
			ref.Disc = int(disc.Int64)
			ref.DiscTagged = true
		}
		if track.Valid {
			ref.Track = int(track.Int64)
			ref.TrackTagged = true
		}
		ref.IsDSD = isDSD != 0
		if err := fn(ref); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CatalogTrackRow is the hydration projection for album / playlist /
// mix / favourites detail: everything a track row renders, plus the
// variant flags the player's playability decision needs.
//
// Separate from TrackFeaturesForPaths (smartplaylist_queries.go), whose
// shape is right but which lacks disc/track/codec/bits/isDSD — and
// widening THAT would change what every smart-mix family reads.
//
// Deliberately carries NO is-upscaled / is-optimized booleans. A bare
// "a variant row exists" is a lie the player would act on: variants go
// stale when their source is re-encoded, and only a freshness check
// against the source's mtime and size can say whether one is playable.
// VariantsForPaths returns the rows so the caller can decide.
type CatalogTrackRow struct {
	Path        string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Year        int
	Disc        int
	DiscTagged  bool
	Track       int
	TrackTagged bool
	Duration    float64
	Size        int64
	// MTimeNS is the scanner's record of the source file's mtime. It
	// pairs with Size as the freshness key a cached variant stamps at
	// generation time (`track_variants.source_mtime_ns` /
	// `source_size`), so a caller can tell a live sidecar from one
	// whose source has moved on without touching the filesystem —
	// which routed rows have no way to do anyway.
	MTimeNS int64

	SampleRate    int
	BitsPerSample int
	IsDSD         bool
	Codec         string

	ArtworkMBID       string
	ArtworkVersion    string
	ReplayGainTrackDB sql.NullFloat64

	RoutedUDN string
}

const catalogTrackRowSelect = catalogRefSelect + `
	   AND t.path IN (SELECT value FROM json_each(?))`

// CatalogTrackRowsForPaths hydrates a bounded set of paths. Rows that
// no longer exist (or became suppressed between the catalog snapshot
// and now) are simply absent — a stale snapshot degrades to a shorter
// list, never to an error.
//
// The path set travels as ONE bound JSON array consumed by json_each,
// not as N concatenated placeholders: that keeps the statement a true
// Go const (which is what keeps SonarCloud's go:S2077 quiet and, more
// importantly, makes it unrepresentable to interpolate a path), and it
// drops the bind-count ceiling so the chunk size below is about memory
// rather than about SQLite limits.
func (s *Store) CatalogTrackRowsForPaths(ctx context.Context, paths []string) ([]CatalogTrackRow, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	const chunk = 400
	out := make([]CatalogTrackRow, 0, len(paths))
	for start := 0; start < len(paths); start += chunk {
		end := start + chunk
		if end > len(paths) {
			end = len(paths)
		}
		blob, err := json.Marshal(paths[start:end])
		if err != nil {
			return nil, err
		}
		got, err := s.catalogTrackRowChunk(ctx, string(blob))
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func (s *Store) catalogTrackRowChunk(ctx context.Context, blob string) ([]CatalogTrackRow, error) {
	rows, err := s.db.QueryContext(ctx, catalogTrackRowSelect, blob)
	if err != nil {
		return nil, fmt.Errorf("catalog track rows: %w", err)
	}
	defer rows.Close()
	var out []CatalogTrackRow
	for rows.Next() {
		var (
			r     CatalogTrackRow
			ref   CatalogRef
			disc  sql.NullInt64
			track sql.NullInt64
			isDSD int
		)
		if err := rows.Scan(&ref.Path, &ref.Title, &ref.Artist, &ref.AlbumArtist,
			&ref.Album, &ref.Genre, &ref.Composer, &ref.Year, &disc, &track,
			&ref.Duration, &ref.Size, &ref.MTimeNS, &ref.SampleRate, &ref.BitsPerSample, &isDSD,
			&ref.Codec, &ref.ArtworkMBID, &ref.ArtworkVersion, &ref.ReleaseMBID,
			&ref.ArtistMBID, &ref.IndexedAt, &ref.RoutedUDN); err != nil {
			return nil, err
		}
		r = CatalogTrackRow{
			Path: ref.Path, Title: ref.Title, Artist: ref.Artist,
			AlbumArtist: ref.AlbumArtist, Album: ref.Album, Year: ref.Year,
			Duration: ref.Duration, Size: ref.Size,
			SampleRate: ref.SampleRate, BitsPerSample: ref.BitsPerSample,
			IsDSD: isDSD != 0, Codec: ref.Codec,
			ArtworkMBID: ref.ArtworkMBID, ArtworkVersion: ref.ArtworkVersion,
			MTimeNS:   ref.MTimeNS,
			RoutedUDN: ref.RoutedUDN,
		}
		if disc.Valid {
			r.Disc, r.DiscTagged = int(disc.Int64), true
		}
		if track.Valid {
			r.Track, r.TrackTagged = int(track.Int64), true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VariantsForPaths returns every variant row for a bounded set of
// source paths, keyed by source path.
//
// Batched because the alternative is one ListVariantsForPath per track:
// a 20-track album detail would issue 20 queries, and a playlist view
// far more. Same json_each + chunking shape as CatalogTrackRowsForPaths.
//
// Returns the ROWS, not a verdict. Freshness is the caller's call and
// needs the source file's mtime and size, which live on disk, not here.
func (s *Store) VariantsForPaths(ctx context.Context, paths []string) (map[string][]VariantRow, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	const chunk = 400
	out := make(map[string][]VariantRow, len(paths))
	for start := 0; start < len(paths); start += chunk {
		end := start + chunk
		if end > len(paths) {
			end = len(paths)
		}
		blob, err := json.Marshal(paths[start:end])
		if err != nil {
			return nil, err
		}
		if err := s.variantChunkForPaths(ctx, string(blob), out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) variantChunkForPaths(ctx context.Context, blob string, out map[string][]VariantRow) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, variant_id, sidecar_path, format, sample_rate,
		       bits_per_sample, size_bytes, source_mtime_ns, source_size
		  FROM track_variants
		 WHERE source_path IN (SELECT value FROM json_each(?))
		 ORDER BY source_path, variant_id`, blob)
	if err != nil {
		return fmt.Errorf("variants for paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v VariantRow
		if err := rows.Scan(&v.SourcePath, &v.VariantID, &v.SidecarPath, &v.Format,
			&v.SampleRate, &v.BitsPerSample, &v.SizeBytes, &v.SourceMTimeNS,
			&v.SourceSize); err != nil {
			return err
		}
		out[v.SourcePath] = append(out[v.SourcePath], v)
	}
	return rows.Err()
}

// VariantPresence is what one track HAS, by kind, as the player's
// coverage readouts need it.
//
// Presence and freshness are separate booleans because they answer
// different questions. The batch walks skip a track that has a variant
// of the kind regardless of freshness, so PRESENCE is what "covered"
// means for a coverage bar; a stale sidecar is nonetheless one the
// serve path answers 410 for, so it has to be countable on its own.
//
// Freshness compares the variant's stamped source facts against the
// scanner's record of the file, not a live stat: it is the same
// definition autoOptimizeCandidateSQL uses to decide what needs
// regenerating, it costs no filesystem access, and it is the only
// definition available at all for a UPnP-routed row.
type VariantPresence struct {
	Upscaled       bool
	UpscaledFresh  bool
	Optimized      bool
	OptimizedFresh bool
	// Bytes is the total size of this track's sidecars, both kinds.
	Bytes int64
}

// variantPresenceSelect aggregates one row per source path. Kept as a
// shared const so the scoped and whole-library forms cannot drift on
// what counts as present or fresh — the same reason
// trackProjectionSelect exists.
//
// The kind LIKE patterns are version-agnostic by design (`upscaled-%`
// matches both v1 and v2 sidecars); see manifest.VariantKindPrefix*.
const variantPresenceSelect = `
	SELECT tv.source_path,
	       MAX(CASE WHEN tv.variant_id LIKE 'upscaled-%'  THEN 1 ELSE 0 END),
	       MAX(CASE WHEN tv.variant_id LIKE 'upscaled-%'
	                 AND tv.source_mtime_ns = t.mtime_ns
	                 AND tv.source_size     = t.size      THEN 1 ELSE 0 END),
	       MAX(CASE WHEN tv.variant_id LIKE 'optimized-%' THEN 1 ELSE 0 END),
	       MAX(CASE WHEN tv.variant_id LIKE 'optimized-%'
	                 AND tv.source_mtime_ns = t.mtime_ns
	                 AND tv.source_size     = t.size      THEN 1 ELSE 0 END),
	       COALESCE(SUM(tv.size_bytes), 0)
	  FROM track_variants tv
	  JOIN tracks t ON t.path = tv.source_path`

// VariantPresenceForPaths returns coverage for a bounded set of tracks.
// Absent paths simply have no entry — a track with no sidecars is the
// zero value, which is what a caller reading the map wants anyway.
func (s *Store) VariantPresenceForPaths(ctx context.Context, paths []string) (map[string]VariantPresence, error) {
	if len(paths) == 0 {
		return map[string]VariantPresence{}, nil
	}
	out := make(map[string]VariantPresence, len(paths))
	for start := 0; start < len(paths); start += variantPresenceChunk {
		end := start + variantPresenceChunk
		if end > len(paths) {
			end = len(paths)
		}
		blob, err := json.Marshal(paths[start:end])
		if err != nil {
			return nil, err
		}
		rows, err := s.db.QueryContext(ctx, variantPresenceSelect+`
			 WHERE tv.source_path IN (SELECT value FROM json_each(?))
			 GROUP BY tv.source_path`, string(blob))
		if err != nil {
			return nil, fmt.Errorf("variant presence for paths: %w", err)
		}
		if err := scanVariantPresence(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AllVariantPresence returns coverage for every track that has at least
// one sidecar.
//
// Whole-library by design: the album grid needs a per-album answer for
// every tile on the page, and asking per page would mean a query per
// scroll. One pass over `track_variants` — a table with a row per
// generated file, not per track — folds into a map the size of the
// covered set, which on a fully-covered 24k library is tens of
// thousands of entries and a few MB. The caller caches it; see the
// admin coverage snapshot.
func (s *Store) AllVariantPresence(ctx context.Context) (map[string]VariantPresence, error) {
	rows, err := s.db.QueryContext(ctx, variantPresenceSelect+`
		 GROUP BY tv.source_path`)
	if err != nil {
		return nil, fmt.Errorf("variant presence: %w", err)
	}
	out := make(map[string]VariantPresence)
	if err := scanVariantPresence(rows, out); err != nil {
		return nil, err
	}
	return out, nil
}

const variantPresenceChunk = 400

func scanVariantPresence(rows *sql.Rows, out map[string]VariantPresence) error {
	defer rows.Close()
	for rows.Next() {
		var (
			path          string
			up, upFresh   int
			opt, optFresh int
			bytes         int64
		)
		if err := rows.Scan(&path, &up, &upFresh, &opt, &optFresh, &bytes); err != nil {
			return err
		}
		out[path] = VariantPresence{
			Upscaled: up != 0, UpscaledFresh: upFresh != 0,
			Optimized: opt != 0, OptimizedFresh: optFresh != 0,
			Bytes: bytes,
		}
	}
	return rows.Err()
}
