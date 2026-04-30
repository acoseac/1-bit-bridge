// Package manifest owns the library index: the SQLite-backed store, per-
// format tag extractors (FLAC / DSF / ALAC / WAV / MP3), and the filesystem
// scanner that drives them. It exposes one JSON serialization — the shape
// that GET /v1/manifest returns — matching Track in the iOS SwiftData
// schema (com.acoseac.dsdplayer/LibraryModels.swift).
package manifest

import "time"

// Track is the on-wire shape for a single track, matching field-for-field
// what the iOS Track model decodes. Optional fields use pointer types so
// JSON null is preserved (the iOS decoder uses Swift's `?` optionals for
// the same reason — a missing year is different from year == 0).
//
// `IsDSD` is a pointer specifically to enable the bridge to emit
// `isDSD: false` when the extractor positively knows the file is PCM
// (FLAC's STREAMINFO, ALAC/M4A audio, MP3) — distinguishing "definitely
// PCM" from "format unknown". `extractFLACFormat` and `extractDSF` set
// the explicit value; the dhowden-tag fallback path leaves it nil.
//
// `TrackNumber`, `DiscNumber`, `Year` are pointers to align the shape
// with the rest of the optional fields. The `dhowden/tag` API returns
// 0 for both "tag absent" and "tag value is 0", so
// `populateFromTagMetadata` propagates the raw value as a non-nil
// pointer regardless — a tag legitimately set to 0 round-trips as
// `Some(0)` rather than getting silently dropped. iOS treats 0 as the
// same sentinel as nil for these fields (no track number, no disc
// number, no year), so user-visible behaviour is unchanged; the wire
// shape just stops lying about which case the extractor saw.
type Track struct {
	Path               string    `json:"path"`
	Size               int64     `json:"size"`
	ModTime            time.Time `json:"mtime"`
	Title              string    `json:"title,omitempty"`
	Artist             string    `json:"artist,omitempty"`
	AlbumArtist        string    `json:"albumArtist,omitempty"`
	Album              string    `json:"album,omitempty"`
	TrackNumber        *int      `json:"trackNumber,omitempty"`
	DiscNumber         *int      `json:"discNumber,omitempty"`
	Year               *int      `json:"year,omitempty"`
	Genre              string    `json:"genre,omitempty"`
	Duration           *float64  `json:"duration,omitempty"`      // seconds
	SampleRate         *float64  `json:"sampleRate,omitempty"`    // Hz (e.g. 96000, 2822400)
	BitsPerSample      *int      `json:"bitsPerSample,omitempty"` // 1 for DSD, 16/24/32 for PCM
	IsDSD              *bool     `json:"isDSD,omitempty"`
	ReplayGainTrackDB  *float64  `json:"replayGainTrackDB,omitempty"`
	ReplayGainAlbumDB  *float64  `json:"replayGainAlbumDB,omitempty"`
	MusicBrainzTrackID string    `json:"musicBrainzTrackID,omitempty"`
	MusicBrainzAlbumID string    `json:"musicBrainzAlbumID,omitempty"`
	// ArtworkMBID identifies cached front-cover artwork. Two value
	// shapes share this field:
	//   - <UUID>          : MusicBrainz release MBID, set by the
	//                       enricher after a successful Cover Art
	//                       Archive (or iTunes fallback) fetch.
	//   - local-<sha256>  : SHA-256 hex of locally-extracted artwork
	//                       bytes — embedded ID3 APIC, or a folder-
	//                       level cover.jpg / folder.jpg adjacent to
	//                       the audio file. Set by the scanner during
	//                       Extract; the enricher leaves the value
	//                       alone (skips both CAA and iTunes fetches).
	// iOS derives the artwork URL as /v1/artwork/{ArtworkMBID} for
	// both shapes — the API handler's regex accepts either. This is a
	// deliberate sentinel hijack to keep the protocol additive (no
	// ProtocolVersion bump and no iOS / wire change for v1.2).
	ArtworkMBID string `json:"artworkMBID,omitempty"`
	// ArtistMBID is set by the enricher when a matching MusicBrainz artist
	// was found. Used for artist-image endpoints (PR #9).
	ArtistMBID string `json:"artistMBID,omitempty"`

	// Enriched reports whether the bridge's enrichment loop has finished
	// processing this track (regardless of outcome — empty MBID lookups
	// still flip the bit, see `markSkipped` in internal/enrich/enricher.go).
	// Pointer type so older clients can distinguish "field absent" (bridge
	// pre-dates the field, treat as fully enriched for back-compat) from
	// `false` (bridge supports the field, this track is genuinely pending).
	// Set during Track deserialization in `Store.ListTracks` /
	// `Store.ListTracksPage` from the `enriched_at != 0` column — the
	// JSON-encoded `tags_json` blob doesn't carry it, so we splice it in
	// from the row's column at read time. Mirrors the pointer convention
	// `IsDSD`, `TrackNumber`, `Year` use for the same "absent vs explicit"
	// disambiguation.
	Enriched *bool `json:"enriched,omitempty"`

	// Variants reports any pre-computed alternate-format renderings of
	// this track that the bridge has cached on disk (today: PCM
	// upscaling via `bridge upscale` or `POST /v1/upscale`; future:
	// PCM→DSD synthesis). Spliced in by the store at read time via a
	// SQLite `json_group_array` correlated subquery against
	// `track_variants` — never stored in `tags_json`. Pre-v1.2 servers
	// omit the field entirely; iOS clients unaware of the field decode
	// cleanly via the lenient default JSONDecoder.
	//
	// **Feature-flag gated**: the manifest provider clears this slice
	// before serialization when `cfg.Upscale.Enabled == false`, so a
	// disabled bridge advertises no variants even if the table has
	// rows (preserves the operator's "off" intent without losing the
	// cached sidecars on disk).
	Variants []Variant `json:"variants,omitempty"`
}

// Variant is one cached alternate rendering of a Track's source. The
// shape mirrors the iOS `BridgeVariant` struct field-for-field; new
// fields here must land on iOS in the same Mirror-PR pair.
//
// `ID` is opaque to clients; today the only producer is `bridge
// upscale`, which mints IDs of the form
// `upscaled-v1-<targetRate>-<targetBits>` (e.g.
// `upscaled-v1-176400-24`). The iOS variant resolver keys on the
// `upscaled-` prefix to slot a variant into the share-level "prefer
// upscaled" toggle; future variant kinds (e.g. `dsd-`) get their
// own slots without touching legacy resolution.
//
// `Label` is a human-readable string the iOS picker renders directly
// (e.g. "Upscaled FLAC 24/176.4"). Server-side construction so the
// label can grow richer (target-rate-aware copy, source format hint)
// without an iOS-side update.
type Variant struct {
	ID            string  `json:"id"`
	Format        string  `json:"format"`
	SampleRate    float64 `json:"sampleRate"`
	BitsPerSample int     `json:"bitsPerSample"`
	SizeBytes     int64   `json:"sizeBytes"`
	Label         string  `json:"label"`
}

// Folder is a lightweight folder record used by the scanner's skip logic
// and included in /v1/manifest so the iOS side can reason about directory
// mtimes without re-listing.
type Folder struct {
	Path    string    `json:"path"`
	ModTime time.Time `json:"mtime"`
}

// Manifest is the top-level JSON returned by GET /v1/manifest.
//
// Two consumption modes:
//
//  1. Full manifest (default, v1.0): caller omits `?limit=` and
//     `?cursor=`. Server ships every track in a single response.
//     `NextCursor` and `Total` are absent; `Folders` is always
//     present.
//  2. Paginated (v1.1, full-manifest only): caller sets `?limit=N`
//     and iterates, passing the prior page's `NextCursor` back as
//     `?cursor=` until the server returns a null cursor.
//     `Folders` + `Total` are sent **only on the first page**
//     (`cursor==""`) — for a 50k-track library with 5k folders,
//     repeating them on every page would balloon the response by
//     ~250k rows of redundant JSON. iOS binds its scan-state UI
//     from the first page and ignores the fields on subsequent
//     pages. `Since`-delta mode is never paginated — deltas are
//     bounded by construction.
type Manifest struct {
	Version      int       `json:"version"`
	GeneratedAt  time.Time `json:"generatedAt"`
	LibraryRoots []string  `json:"libraryRoots"`
	// Folders carries the directory list. Present on non-paginated
	// responses AND on the first page of a pagination run; absent
	// (via omitempty) on subsequent pages. Clients MUST tolerate
	// a missing key on later pages — the first-page snapshot is
	// authoritative for the pagination run.
	Folders []Folder `json:"folders,omitempty"`
	Tracks  []Track  `json:"tracks"`
	// NextCursor carries an opaque token the client sends back as
	// `?cursor=` on the next page request. Null means "this is the
	// last page". Always absent on a full (non-paginated) response.
	NextCursor *string `json:"nextCursor,omitempty"`
	// Total is the full track count across all pages of the current
	// pagination run. Pointer type so `0` distinguishes "paginated
	// empty library" (Total = 0, present on the wire) from
	// "non-paginated response" (Total absent). Only set on the first
	// page of a pagination run.
	Total *int `json:"total,omitempty"`
	// EnrichmentProgress reports library-wide enrichment status as a
	// snapshot at manifest build time. Lets iOS distinguish "missing
	// MBID because the bridge hasn't enriched this track yet" (don't
	// treat as a permanent Deezer-miss; wait for the next sync) from
	// "MBID is genuinely absent" (track was enriched, MB returned no
	// match). Pointer type so older clients (decoders without the
	// field) parse manifests from this server without churning, and
	// older servers' manifests parse fine in newer iOS clients (the
	// decoder leaves it nil and falls back to the conservative "all
	// tracks fully enriched" assumption).
	//
	// Populated only on the **first page** of a paginated full-manifest
	// response (`cursor == ""`) and on every non-paginated response.
	// Same pattern as `Folders` / `Total` — the values are stable for
	// the duration of a pagination run, so iOS snapshots them off the
	// first page and ignores subsequent pages.
	EnrichmentProgress *EnrichmentProgress `json:"enrichmentProgress,omitempty"`
}

// EnrichmentProgress is the per-manifest snapshot of how far the bridge's
// enrichment loop has gotten. iOS uses these counters to render an
// "Enrichment in progress (X / Y)…" UI hint and to suppress permanent
// negative-cache stamping on artists whose MBID hasn't landed yet.
//
// `LastEnrichedAt` is wall-clock UTC of the most recent successful
// `MarkEnriched` call across the whole library. **Pointer type**:
// Go's `omitempty` does NOT drop a zero `time.Time` (`time.Time` is a
// struct, not a value type that satisfies the `IsZero` check `omitempty`
// uses), so a non-pointer field would emit `"0001-01-01T00:00:00Z"` on
// the wire when no track has been enriched yet. The iOS decoder would
// then parse that as a real, very-old date, breaking the freshness gate
// (24 h check on this value) and the "never enriched" sentinel.
// Pointer + `omitempty` correctly omits the field on the absent case.
// iOS gates its "still in progress" assumption on a 24 h freshness
// check on this value — a bridge that went idle a week ago shouldn't
// make the iOS UI claim enrichment is "still happening".
type EnrichmentProgress struct {
	TracksTotal    int        `json:"tracksTotal"`
	TracksEnriched int        `json:"tracksEnriched"`
	LastEnrichedAt *time.Time `json:"lastEnrichedAt,omitempty"`
}
