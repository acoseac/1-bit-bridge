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
type Track struct {
	Path               string    `json:"path"`
	Size               int64     `json:"size"`
	ModTime            time.Time `json:"mtime"`
	Title              string    `json:"title,omitempty"`
	Artist             string    `json:"artist,omitempty"`
	AlbumArtist        string    `json:"albumArtist,omitempty"`
	Album              string    `json:"album,omitempty"`
	TrackNumber        int       `json:"trackNumber,omitempty"`
	DiscNumber         int       `json:"discNumber,omitempty"`
	Year               int       `json:"year,omitempty"`
	Genre              string    `json:"genre,omitempty"`
	Duration           *float64  `json:"duration,omitempty"`      // seconds
	SampleRate         *float64  `json:"sampleRate,omitempty"`    // Hz (e.g. 96000, 2822400)
	BitsPerSample      *int      `json:"bitsPerSample,omitempty"` // 1 for DSD, 16/24/32 for PCM
	IsDSD              bool      `json:"isDSD,omitempty"`
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
type Manifest struct {
	Version      int       `json:"version"`
	GeneratedAt  time.Time `json:"generatedAt"`
	LibraryRoots []string  `json:"libraryRoots"`
	Folders      []Folder  `json:"folders"`
	Tracks       []Track   `json:"tracks"`
}
