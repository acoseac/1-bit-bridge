// Streaming per-track projection for the duplicates report
// (internal/dupes + the `bridge duplicates` CLI).
//
// COST DISCIPLINE: StreamTrackDupeRefsUnderPrefix is a full-subtree
// json_extract walk — the same cost class as AtlasMetaBreakdownCounts /
// StreamTrackMetaRefsUnderPrefix. It is for CLICK-DRIVEN / CLI surfaces
// and the scanner's post-scan tail only, and must NEVER run on an SSE
// tick (CLAUDE.md composition-bars discipline).
package manifest

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// dupeRefSelect reads the duplicate-grouping projection. Two deliberate
// read-truth choices, both load-bearing:
//
//   - Geometry (sample_rate / bits_per_sample / is_dsd / codec) comes from
//     the REAL v25 columns — that is what they exist for — and a NULL maps
//     to the zero value, which internal/dupes reads as "unknown" and
//     degrades to the inconclusive tier. This deliberately diverges from
//     FormatDistribution, which keeps tags_json as its read-truth; the
//     dupes projection wants the query-accelerator columns because it
//     joins them with several tag fields per row anyway.
//
//   - discNumber / trackNumber are json_extract'ed WITHOUT COALESCE: the
//     iOS client falls back to folder/filename inference ONLY when the
//     tag is ABSENT, and an explicit 0 is a value (`Some(0)`). Collapsing
//     NULL and 0 here would silently merge the two cases and change group
//     membership — the DiscTagged/TrackTagged bools carry the distinction
//     instead (dupes.Row deliberately has no pointer fields: the struct is
//     reused across callback invocations, so a *int would be a retain
//     hazard).
const dupeRefSelect = `
	SELECT t.path,
	       COALESCE(json_extract(t.tags_json, '$.title'),       ''),
	       COALESCE(json_extract(t.tags_json, '$.album'),       ''),
	       COALESCE(json_extract(t.tags_json, '$.albumArtist'), ''),
	       COALESCE(json_extract(t.tags_json, '$.artist'),      ''),
	       COALESCE(json_extract(t.tags_json, '$.year'),        0),
	       json_extract(t.tags_json, '$.discNumber'),
	       json_extract(t.tags_json, '$.trackNumber'),
	       COALESCE(json_extract(t.tags_json, '$.size'),        0),
	       COALESCE(json_extract(t.tags_json, '$.duration'),    0),
	       COALESCE(t.sample_rate, 0),
	       COALESCE(t.bits_per_sample, 0),
	       COALESCE(t.is_dsd, 0),
	       COALESCE(t.codec, ''),
	       t.audio_md5,
	       t.dupe_group_id,
	       t.dupe_tier,
	       t.dupe_suppressed
	  FROM tracks t`

// StreamTrackDupeRefsUnderPrefix walks every track under prefix ("" =
// whole library) and yields the duplicate-grouping projection per row,
// paired with the row's CURRENT dupe-stamp state (so the stamping pass
// can diff desired-vs-current, and the CLI can badge what is actually
// being served right now). UPnP-routed rows are EXCLUDED unless
// includeRouted is set: their lifecycle belongs to the ingest reconcile,
// the duplicate stamping pass must never touch them, and mixing remote
// upstream content into the default report invites acting on rows the
// bridge doesn't own.
//
// The callback MUST NOT retain either value past its invocation (the
// StreamTracks contract — the structs are reused across iterations).
// Read-only; no s.mu.
func (s *Store) StreamTrackDupeRefsUnderPrefix(ctx context.Context, prefix string, includeRouted bool, fn func(dupes.Row, DupeStampState) error) error {
	q := dupeRefSelect
	var (
		conds []string
		args  []any
	)
	if !includeRouted {
		conds = append(conds,
			`NOT EXISTS (SELECT 1 FROM upnp_track_routing r WHERE r.source_path = t.path)`)
	}
	// subtreeRangeBase trims a caller-supplied trailing slash before the
	// bounds append their own, and treats a trims-to-empty prefix as
	// whole-library (the store.go prefix-family guard).
	if base, scoped := subtreeRangeBase(prefix); scoped {
		conds = append(conds,
			`t.path COLLATE BINARY >= ? || '/'`,
			`t.path COLLATE BINARY < ? || '0'`)
		args = append(args, base, base)
	}
	for i, c := range conds {
		if i == 0 {
			q += "\n	 WHERE " + c
		} else {
			q += "\n	   AND " + c
		}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("stream dupe refs %q: %w", prefix, err)
	}
	defer rows.Close()
	var (
		ref        dupes.Row
		st         DupeStampState
		disc       sql.NullInt64
		track      sql.NullInt64
		isDSD      int
		suppressed int
	)
	for rows.Next() {
		ref = dupes.Row{}
		st = DupeStampState{}
		disc, track, isDSD, suppressed = sql.NullInt64{}, sql.NullInt64{}, 0, 0
		if err := rows.Scan(&ref.Path, &ref.Title, &ref.Album, &ref.AlbumArtist,
			&ref.Artist, &ref.Year, &disc, &track, &ref.Size, &ref.Duration,
			&ref.SampleRate, &ref.BitsPerSample, &isDSD, &ref.Codec,
			&ref.AudioMD5, &st.GroupID, &st.Tier, &suppressed); err != nil {
			return err
		}
		if disc.Valid {
			ref.Disc = int(disc.Int64)
			ref.DiscTagged = true
		}
		if track.Valid {
			ref.Track = int(track.Int64)
			ref.TrackTagged = true
		}
		ref.IsDSD = isDSD != 0
		st.Suppressed = suppressed != 0
		if err := fn(ref, st); err != nil {
			return err
		}
	}
	return rows.Err()
}
