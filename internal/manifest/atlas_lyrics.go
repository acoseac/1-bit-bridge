package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// The verdicts atlas_lyrics_attempt records. These are Atlas's own
// `lyrics_status` values plus one of ours for a track we could not turn into a
// recording MBID at all — a distinction Atlas cannot make because it never saw
// the question.
const (
	AtlasLyricsAvailable    = "available"
	AtlasLyricsInstrumental = "instrumental"
	AtlasLyricsUnavailable  = "unavailable"
	AtlasLyricsPending      = "pending"
	AtlasLyricsUnresolved   = "unresolved"
)

// AtlasLyricsCandidate is one track the network tier may be able to fill, with
// everything the matcher needs to find it in a release listing.
type AtlasLyricsCandidate struct {
	Path        string
	AlbumMBID   string
	TrackMBID   string // the track's OWN recording MBID from tags, if it has one
	CachedMBID  string // a recording MBID a previous attempt already resolved
	Title       string
	TrackNumber int
	DiscNumber  int
	DurationMS  int64
	Attempts    int
}

// atlasLyricsCandidateSQL selects tracks the network lyrics tier should try.
//
// The gate is the ABSENCE of a track_lyrics row, never the attempt's verdict,
// and that ordering is what makes the tier self-healing. A track whose Atlas
// document was later displaced by a local one — or whose local one was deleted
// by its owner — reappears here the moment it has no row, and the cached
// recording_mbid makes the re-fetch a single call. Gating on the verdict
// instead would strand it: `available` would read as "done" about a track that
// visibly has no lyrics.
//
// `instrumental` is the one verdict that must exclude by STATUS, because it is
// a success that correctly leaves no row behind — without this arm the query
// would re-offer every instrumental track on every sweep forever.
//
// The MBID comparison is the retag invalidation. An attempt records the
// identity it was made against, so a corrected album or recording MBID makes
// the stored verdict stale by construction and the track returns as a
// candidate — including out of `instrumental`, which is otherwise terminal.
//
// Routed and suppressed rows are excluded. A UPnP-routed path does not resolve
// on this filesystem, so /v1/lyrics could never serve what we fetched for it;
// a dupe-suppressed row is not served either, and its winner is a candidate in
// its own right. NOT EXISTS rather than NOT IN, keyed on the routing table's
// PRIMARY KEY, matching every other anti-join in this file.
const atlasLyricsCandidateSQL = `
	SELECT t.path,
	       COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), ''),
	       COALESCE(json_extract(t.tags_json, '$.musicBrainzTrackID'), ''),
	       COALESCE(a.recording_mbid, ''),
	       COALESCE(json_extract(t.tags_json, '$.title'), ''),
	       COALESCE(json_extract(t.tags_json, '$.trackNumber'), 0),
	       COALESCE(json_extract(t.tags_json, '$.discNumber'), 0),
	       CAST(COALESCE(json_extract(t.tags_json, '$.duration'), 0) * 1000 AS INTEGER),
	       COALESCE(a.attempts, 0)
	  FROM tracks t
	  LEFT JOIN atlas_lyrics_attempt a ON a.source_path = t.path
	 WHERE NOT EXISTS (SELECT 1 FROM track_lyrics l WHERE l.source_path = t.path)
	   AND NOT EXISTS (SELECT 1 FROM upnp_track_routing r WHERE r.source_path = t.path)
	   AND t.dupe_suppressed = 0
	   AND (COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), '') != ''
	     OR COALESCE(json_extract(t.tags_json, '$.musicBrainzTrackID'), '') != '')
	   AND (a.source_path IS NULL
	     OR a.album_mbid != COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), '')
	     OR a.recording_mbid_at_attempt != COALESCE(json_extract(t.tags_json, '$.musicBrainzTrackID'), '')
	     OR (a.status != 'instrumental' AND a.next_attempt_at <= ?))
	 ORDER BY COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), ''), t.path
	 LIMIT ?`

// AtlasLyricsCandidates returns tracks with no lyrics that the network tier is
// due to try, ordered by album so a caller can serve a whole release from one
// listing fetch — 9,454 candidates spanned 1,088 releases when this was
// measured, so the grouping is most of the cost.
func (s *Store) AtlasLyricsCandidates(ctx context.Context, now int64, limit int) ([]AtlasLyricsCandidate, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, atlasLyricsCandidateSQL, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	capHint := limit
	if capHint > 1000 {
		capHint = 1000
	}
	out := make([]AtlasLyricsCandidate, 0, capHint)
	for rows.Next() {
		var c AtlasLyricsCandidate
		if err := rows.Scan(&c.Path, &c.AlbumMBID, &c.TrackMBID, &c.CachedMBID,
			&c.Title, &c.TrackNumber, &c.DiscNumber, &c.DurationMS, &c.Attempts); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkAtlasLyricsAttempt records what Atlas was asked and what it said.
//
// `albumMBID` and `trackMBID` are the identity the attempt was made AGAINST —
// the track's tags at the time — not the recording that was resolved from them.
// Storing the answer alone would leave a retag invisible, and the negative
// verdict from a wrong MBID would outlive the correction.
func (s *Store) MarkAtlasLyricsAttempt(ctx context.Context, path, albumMBID, trackMBID,
	resolvedMBID, status string, nextAttemptAt int64) error {
	if path == "" {
		return errors.New("manifest: atlas lyrics attempt needs a path")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO atlas_lyrics_attempt(source_path, album_mbid, recording_mbid_at_attempt,
			recording_mbid, status, attempts, attempted_at, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(source_path) DO UPDATE SET
			album_mbid = excluded.album_mbid,
			recording_mbid_at_attempt = excluded.recording_mbid_at_attempt,
			recording_mbid = excluded.recording_mbid,
			status = excluded.status,
			attempts = CASE
				WHEN atlas_lyrics_attempt.album_mbid != excluded.album_mbid
				  OR atlas_lyrics_attempt.recording_mbid_at_attempt != excluded.recording_mbid_at_attempt
				THEN 1
				ELSE atlas_lyrics_attempt.attempts + 1 END,
			attempted_at = excluded.attempted_at,
			next_attempt_at = excluded.next_attempt_at`,
		path, albumMBID, trackMBID, resolvedMBID, status, s.now().UnixNano(), nextAttemptAt)
	return err
}

// UpsertAtlasLyrics writes a network-sourced document for one track.
//
// It refuses to demote: the row is only taken when the incoming source
// OUTRANKS what is already stored, so a sweep can never displace the
// operator's own sidecar, and two sweeps cannot alternate a track between two
// documents — every write here strict-advances indexed_at, and a flap would
// put the track in every paired device's delta on every cycle. Returns whether
// the row was taken.
//
// The provenance columns are deliberately ZERO. They exist for the 410
// staleness check, which stats the LYRICS SOURCE — a sidecar, else the audio
// file — and a network document has neither. Binding it to the audio file's
// mtime would make an unrelated tag edit answer 410 for a document that never
// came from that file; lyricsSourceDrifted skips network rows instead.
func (s *Store) UpsertAtlasLyrics(ctx context.Context, path string, doc lyrics.Doc, source lyrics.Source) (bool, error) {
	if path == "" {
		return false, errors.New("manifest: atlas lyrics needs a path")
	}
	if !source.IsNetwork() {
		return false, fmt.Errorf("manifest: %q is not a network lyrics source", source)
	}
	if doc.Body == "" {
		return false, errors.New("manifest: refusing to store an empty lyrics body")
	}
	tag := lyrics.Tag(doc)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var oldTag, oldSource string
	hadRow := true
	err = tx.QueryRowContext(ctx, `SELECT tag, source FROM track_lyrics WHERE source_path = ?`,
		path).Scan(&oldTag, &oldSource)
	if errors.Is(err, sql.ErrNoRows) {
		hadRow = false
	} else if err != nil {
		return false, err
	}
	if hadRow && lyrics.Source(oldSource).Rank() <= source.Rank() {
		return false, tx.Commit()
	}
	if hadRow && oldTag == tag {
		// The same document arriving under a better-ranked source. Take the
		// row so the source column tells the truth, but leave indexed_at
		// alone: nothing a client can see has changed.
		if _, err := tx.ExecContext(ctx, `UPDATE track_lyrics
			SET format = ?, synced = ?, body = ?, language = ?, source = ?,
			    sidecar_name = '', source_mtime_ns = 0, source_size = 0
			WHERE source_path = ?`,
			doc.Format, boolToInt(doc.Synced), doc.Body, doc.Language, string(source), path); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}

	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO track_lyrics(source_path, format, synced, body, language, source, sidecar_name, tag,
		                         source_mtime_ns, source_size, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, 0, 0, ?)
		ON CONFLICT(source_path) DO UPDATE SET
			format = excluded.format, synced = excluded.synced, body = excluded.body,
			language = excluded.language, source = excluded.source, sidecar_name = '',
			tag = excluded.tag, source_mtime_ns = 0, source_size = 0,
			indexed_at = excluded.indexed_at`,
		path, doc.Format, boolToInt(doc.Synced), doc.Body, doc.Language, string(source), tag, now); err != nil {
		return false, err
	}
	// The SHARED bump statement, never a hand-rolled CASE — the trap PR #840
	// fell into in this very table, where a per-batch `now` landed every
	// changed row on a cursor clients already held and the track never
	// reached the phone.
	if _, err := tx.ExecContext(ctx, bumpIndexedAtByPathSQL, now, path); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AtlasLyricsStats is the operator-facing rollup of the network tier.
type AtlasLyricsStats struct {
	// Rows currently served from each network source.
	SyncedRows int64
	PlainRows  int64
	// Attempt verdicts, whether or not they left a row.
	ByStatus map[string]int64
	// Tracks with no lyrics at all that carry an MBID we could work from.
	Addressable int64
}

// AtlasLyricsStats reports what the network tier has done and has left to do.
func (s *Store) AtlasLyricsStats(ctx context.Context) (AtlasLyricsStats, error) {
	out := AtlasLyricsStats{ByStatus: map[string]int64{}}
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(source = ?), 0), COALESCE(SUM(source = ?), 0) FROM track_lyrics`,
		string(lyrics.SourceAtlasLRC), string(lyrics.SourceAtlas)).Scan(&out.SyncedRows, &out.PlainRows)
	if err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM atlas_lyrics_attempt GROUP BY status`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return out, err
		}
		out.ByStatus[st] = n
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tracks t
		 WHERE NOT EXISTS (SELECT 1 FROM track_lyrics l WHERE l.source_path = t.path)
		   AND NOT EXISTS (SELECT 1 FROM upnp_track_routing r WHERE r.source_path = t.path)
		   AND t.dupe_suppressed = 0
		   AND (COALESCE(json_extract(t.tags_json, '$.musicBrainzAlbumID'), '') != ''
		     OR COALESCE(json_extract(t.tags_json, '$.musicBrainzTrackID'), '') != '')`).
		Scan(&out.Addressable)
	return out, err
}
