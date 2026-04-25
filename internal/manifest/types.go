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
// with the rest of the optional fields (every `null` on the wire decodes
// to `nil` on the iOS side, every concrete value to `Some(n)`). Note
// that the underlying `dhowden/tag` library returns 0 for both "tag
// absent" and "tag value is 0" with no way to distinguish the two —
// `populateFromTagMetadata` keeps a `!= 0` guard for these three so a
// real "missing" tag round-trips as `null` rather than an unintended
// `0`. Format-specific extractors that DO know the difference (e.g. a
// future FLAC reader that consumed the Vorbis comment directly rather
// than via dhowden) can override.
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
	// ArtworkMBID is set by the enricher (PR #8) when a front-cover image
	// was retrievable from Cover Art Archive for this track's album. iOS
	// derives the artwork URL from this as /v1/artwork/{ArtworkMBID}.
	ArtworkMBID string `json:"artworkMBID,omitempty"`
	// ArtistMBID is set by the enricher when a matching MusicBrainz artist
	// was found. Used for artist-image endpoints (PR #9).
	ArtistMBID string `json:"artistMBID,omitempty"`
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
}
