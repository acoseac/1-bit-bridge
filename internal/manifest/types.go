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
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mtime"`
	Title       string    `json:"title,omitempty"`
	Artist      string    `json:"artist,omitempty"`
	AlbumArtist string    `json:"albumArtist,omitempty"`
	Album       string    `json:"album,omitempty"`
	TrackNumber *int      `json:"trackNumber,omitempty"`
	DiscNumber  *int      `json:"discNumber,omitempty"`
	Year        *int      `json:"year,omitempty"`
	Genre       string    `json:"genre,omitempty"`
	Duration    *float64  `json:"duration,omitempty"`   // seconds
	SampleRate  *float64  `json:"sampleRate,omitempty"` // Hz (e.g. 96000, 2822400)
	// BitsPerSample MUST remain nil for lossy formats (AAC / MP3 /
	// OGG / OPUS / WMA) — the value would be the decoder's container
	// width (e.g. 32 for AAC's Float32 output), not a meaningful
	// integer bit depth of the encoded signal. Surfacing it would
	// re-introduce the iOS PR #371 "M4A 32-bit" Now Playing chip
	// regression. Every site in `extractors.go` that writes this
	// field is gated by `!isLossyCodec(t.Codec)` (see PR-A2).
	// Today FLAC + DSF + DFF + ALAC-via-M4A are the only formats
	// that ACTUALLY populate this — the gate is structural insurance
	// against a future enricher addition.
	BitsPerSample      *int     `json:"bitsPerSample,omitempty"` // 1 for DSD, 16/24/32 for PCM
	IsDSD              *bool    `json:"isDSD,omitempty"`
	ReplayGainTrackDB  *float64 `json:"replayGainTrackDB,omitempty"`
	ReplayGainAlbumDB  *float64 `json:"replayGainAlbumDB,omitempty"`
	MusicBrainzTrackID string   `json:"musicBrainzTrackID,omitempty"`
	MusicBrainzAlbumID string   `json:"musicBrainzAlbumID,omitempty"`
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
	// ArtworkVersion is a content marker for the cover served at
	// /v1/artwork/{ArtworkMBID}. It's COLUMN-derived (spliced from the
	// artwork_version column at read time, never persisted into tags_json)
	// and set ONLY when a premium cover is (re)fetched for a UUID ArtworkMBID
	// — whose URL is stable while its bytes change (CAA → premium upgrade), so
	// iOS otherwise can't tell the cover changed. local-<sha256> ArtworkMBIDs
	// already encode their content, so they leave this empty and iOS versions
	// them by the MBID itself. iOS treats `artworkVersion ?? artworkMBID` as the
	// cover identity and re-fetches when it changes. Additive (omitempty); no
	// ProtocolVersion bump.
	ArtworkVersion string `json:"artworkVersion,omitempty"`
	// BookletTag, when non-empty, advertises that GET
	// /v1/booklet/{musicBrainzAlbumID} serves a PDF album booklet for this
	// track's release; the value is an opaque content tag for client
	// cache-busting (it changes when the booklet's bytes change upstream).
	// COLUMN-derived exactly like ArtworkVersion (spliced from the
	// booklet_tag column at read time, never persisted into tags_json; set
	// only by the Atlas booklet availability loop, which also bumps
	// indexed_at so delta-sync surfaces the change). Keyed by
	// MusicBrainzAlbumID, NOT ArtworkMBID — a locally-curated cover doesn't
	// preclude a booklet. Additive (omitempty); no ProtocolVersion bump.
	BookletTag string `json:"bookletTag,omitempty"`
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

	// Codec is the canonical codec string captured at scan time.
	// Disambiguates ALAC vs AAC for `.m4a` containers (both ship in
	// MP4, only ALAC is bit-exact lossless). v1.2 additive — pre-v1.2
	// iOS clients ignore the field; v1.2+ clients prefer it over
	// extension-derived classification at the AlbumQualityFilter
	// layer. Populated values:
	//   - "ALAC", "AAC"   — captured via internal MP4 sample-description
	//                       walk (`extractMP4Codec`)
	//   - "FLAC"           — set by extractFLACFormatFromReader
	//   - "DSF"            — set by extractDSFWithContext
	//   - "MP3", "OGG"     — set from `tag.FileType()` for those formats
	//                       where dhowden's detection IS reliable
	//   - "" (empty)       — unknown / undetected; iOS falls back to
	//                       extension-derived codec name.
	// `omitempty` so pre-v1.2 clients see no field at all on the wire.
	// Per Gemini A1 / iOS bug review #1.
	Codec string `json:"codec,omitempty"`

	// Classical-metadata fields (PR-D, wire-additive, no
	// ProtocolVersion bump). Pre-PR-D bridges omit all five; the
	// iOS decoder treats absent fields as empty / nil.
	//
	// Composer / Conductor / Work: classical music listeners (who
	// favour DSD / FLAC) heavily rely on these. Many classical
	// taggers store the work title (e.g. "Symphony No. 5 in C
	// minor, Op. 67") in ID3v2 TIT1 (Content Group) and the
	// movement name in TIT2 (Title). Track.Title stays mapped to
	// TIT2; Track.Work adds the grouping axis without overloading
	// existing fields. iOS PR-H's work-grouping helper depends on
	// this exact split.
	Composer  string `json:"composer,omitempty"`
	Conductor string `json:"conductor,omitempty"`
	Work      string `json:"work,omitempty"`

	// OriginalYear distinguishes the year the album was first
	// released (ID3v2 TORY / TDOR; Vorbis ORIGINALYEAR /
	// ORIGINALDATE) from Year (which often holds the pressing /
	// remaster year for catalog re-issues). Many collectors treat
	// OriginalYear as the canonical sort key.
	//
	// Pointer + omitempty so the absent-vs-zero distinction
	// matches Year / TrackNumber / DiscNumber. The presence-gate
	// from PR-B applies: only set when the underlying tag is
	// actually present in the raw map (a returned "0" still counts
	// as present; only absence drops to nil).
	OriginalYear *int `json:"originalYear,omitempty"`

	// BPM (beats per minute). Tag-sourced (dhowden picks up TBPM / BPM /
	// tmpo); when the source has no BPM tag, the offline analyzer's
	// estimated tempo is spliced in (tag-absent-only, like
	// ReplayGainTrackDB — see spliceAnalysisScalars + bpmFromAnalysis).
	// Pointer + omitempty; presence-gated.
	BPM *int `json:"bpm,omitempty"`

	// KeyRoot / KeyMode are the estimated musical key from offline
	// analysis (additive since v1.8): KeyRoot is the tonic 0..11 (C=0),
	// KeyMode is "major"/"minor". There's no curated key tag today, so
	// these are analysis-only — spliced from `track_analysis` at read time
	// (always, when present) and NEVER persisted into `tags_json`
	// (marshalForStorage zeroes them unconditionally, like WaveformTag).
	// Both omitempty so pre-feature iOS / un-estimated tracks ignore them;
	// ProtocolVersion stays 1.
	KeyRoot *int   `json:"keyRoot,omitempty"`
	KeyMode string `json:"keyMode,omitempty"`

	// The wf4 track-quality scalars (additive since v1.9, the
	// transparency batch). All analysis-only — no tag source — so like
	// KeyRoot/KeyMode they are spliced from `track_analysis` at read time
	// and NEVER persisted into `tags_json` (marshalForStorage zeroes them
	// unconditionally). All omitempty; ProtocolVersion stays 1.
	//
	// TruePeakDB: BS.1770-style 4x-oversampled true peak in dB relative
	// to full scale, measured on the bridge's 48 kHz analysis rendering
	// (PROTOCOL.md states the derivation — the LIVE native-rate true
	// peak is iOS's own meter's job). DRScore: the community DR value
	// ("DR12"). AudioMD5State: "verified" | "mismatch" — FLAC audio-MD5
	// verification against the STREAMINFO checksum; absent when not
	// verifiable (non-FLAC, no stored checksum, odd bit depth, tool
	// failure). A mismatch means "modified or corrupt", not proof of
	// corruption — some tag editors rewrite FLAC without updating the
	// checksum.
	TruePeakDB    *float64 `json:"truePeakDB,omitempty"`
	DRScore       *int     `json:"drScore,omitempty"`
	AudioMD5State string   `json:"audioMD5State,omitempty"`

	// WaveformTag signals that an offline-computed peak/RMS waveform
	// sidecar is available for this track (the audio-analysis feature,
	// `bridge analyze`). Non-empty ⇒ iOS can fetch
	// `GET /v1/waveform?path=<path>` to render a scrubber waveform; the
	// value is a short content tag (8 lowercase hex of the sidecar
	// bytes' SHA-256) iOS uses as the cache key — a regenerated waveform
	// (source edited) gets a new tag so an immutable client cache
	// re-fetches.
	//
	// Column-derived like `Enriched` / `Variants`: spliced from
	// `track_analysis.waveform_tag` at read time, NEVER persisted into
	// `tags_json` (see `marshalForStorage`). Additive + omitempty —
	// ProtocolVersion stays 1; pre-feature iOS ignores the unknown key.
	WaveformTag string `json:"waveformTag,omitempty"`

	// replayGainFromAnalysis is an internal, NON-WIRE marker (unexported ⇒
	// json ignores it) set by spliceAnalysisReplayGain when it fills
	// ReplayGainTrackDB from `track_analysis` because the source carried
	// no ReplayGain tag. marshalForStorage scrubs ReplayGainTrackDB ONLY
	// when this is true, so a caller round-tripping a read Track back
	// through a write path can't freeze an analysis-derived value into
	// `tags_json` as a faux curated tag (which would then win over future
	// analysis recomputes/deletes). A genuinely tag-curated ReplayGain
	// leaves this false and survives the write unchanged. Go struct copy
	// preserves unexported fields across packages, so the guard holds even
	// for an external round-trip. (CodeRabbit on #396.)
	replayGainFromAnalysis bool

	// bpmFromAnalysis is the BPM twin of replayGainFromAnalysis: set by
	// spliceAnalysisScalars when it fills BPM from the analyzer's estimate
	// (source had no BPM tag), so marshalForStorage scrubs only the
	// analysis-derived BPM on write-back and a curated TBPM tag survives.
	// KeyRoot/KeyMode need no such marker — they have no tag source, so
	// marshalForStorage zeroes them unconditionally.
	bpmFromAnalysis bool

	// versionStampOnly is an internal, NON-WIRE marker (unexported ⇒ json
	// ignores it — the replayGainFromAnalysis shape) set by the scanner's
	// reExtractUnchanged when a version-stale re-extraction produced a row
	// byte-identical (post post-scan-field merge) to what's stored. The
	// scan writer routes such rows through StampExtractorVersionBatch —
	// advancing extractor_version + resetting missing_count WITHOUT
	// touching indexed_at / enriched_at / tags_json — so an
	// ExtractorVersion bump doesn't surface the entire library in every
	// iOS client's next delta sync nor re-queue full re-enrichment.
	versionStampOnly bool
}

// Variant is one cached alternate rendering of a Track's source. The
// shape mirrors the iOS `BridgeVariant` struct field-for-field; new
// fields here must land on iOS in the same Mirror-PR pair.
//
// `ID` is opaque to clients; today the only producer is `bridge
// upscale`, which mints IDs of the form
// `upscaled-v2-<targetRate>-<targetBits>` (e.g.
// `upscaled-v2-176400-24`). The iOS variant resolver keys on the
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
