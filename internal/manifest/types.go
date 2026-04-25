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
// `IsDSD`, `TrackNumber`, `DiscNumber`, `Year` are pointer types for the
// same reason: with a non-pointer + `omitempty`, the encoder silently
// drops `false` / `0` from the wire, leaving the iOS decoder unable to
// distinguish "the extractor saw an explicit zero" from "the extractor
// found no tag". Pointers preserve the `null` vs `0` vs `1` distinction
// every other optional field already gets.
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
