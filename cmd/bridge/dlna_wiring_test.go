package main

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Test_manifestTrackToDLNATrackInfo_FileExtensionFallback pins the
// CodeRabbit-caught regression on PR #356: UPnP-routed tracks reach
// the helper with `absPath = ""` (resolver-miss sentinel from
// rebuild's bulk-read routing fast-path), and pre-fix the
// `filepath.Ext(absPath)` derivation collapsed to an empty
// FileExtension. DIDL `<res protocolInfo>` MIME resolution + the file
// handler's variant-segment regex both key off the extension; an
// empty FileExtension would have silently mis-rendered the codec on
// every routed track shipped to a renderer. The fallback derives
// from `t.Path` (which always carries the real extension in the
// bridge manifest) when absPath is empty.
//
// Pre-PR-#356 (filesystem-only deploys) is unchanged because the
// fallback is a no-op when absPath is non-empty.
func Test_manifestTrackToDLNATrackInfo_FileExtensionFallback(t *testing.T) {
	cases := []struct {
		name        string
		trackPath   string
		absPath     string
		libraryRoot string
		wantExt     string
	}{
		{
			name:      "filesystem-backed track: derives from absPath (unchanged)",
			trackPath: "Artist/Album/01 - Song.flac",
			absPath:   "/srv/library/Artist/Album/01 - Song.flac",
			wantExt:   ".flac",
		},
		{
			name:      "UPnP-routed track: absPath empty, falls back to t.Path",
			trackPath: "2go/Music/AC-DC/[M] Back In Black [35986104] [1980]/01 - Hells Bells.flac",
			absPath:   "",
			wantExt:   ".flac",
		},
		{
			name:      "UPnP-routed DSF track: falls back, preserves dsf extension",
			trackPath: "2go/Music/DSD64-Album/01 - Track.dsf",
			absPath:   "",
			wantExt:   ".dsf",
		},
		{
			name:      "UPnP-routed DFF track: falls back",
			trackPath: "2go/Music/DSD-Album/01 - Track.dff",
			absPath:   "",
			wantExt:   ".dff",
		},
		{
			name:      "UPnP-routed MP3 track: case-folded lowercase output",
			trackPath: "2go/Music/Old/01 - Track.MP3",
			absPath:   "",
			wantExt:   ".mp3",
		},
		{
			name:      "Filesystem-backed track with uppercase ext: case-folded",
			trackPath: "Artist/Album/01.FLAC",
			absPath:   "/srv/library/Artist/Album/01.FLAC",
			wantExt:   ".flac",
		},
		{
			name:      "Both empty: returns empty extension (defensive — never reached in production)",
			trackPath: "no-extension",
			absPath:   "",
			wantExt:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := manifest.Track{Path: tc.trackPath, Title: "T", Artist: "A", Album: "Alb"}
			got := manifestTrackToDLNATrackInfo(tr, tc.absPath, tc.libraryRoot)
			if got.FileExtension != tc.wantExt {
				t.Errorf("FileExtension = %q; want %q (trackPath=%q, absPath=%q)",
					got.FileExtension, tc.wantExt, tc.trackPath, tc.absPath)
			}
			// AbsolutePath plumbed through unchanged regardless of the
			// extension-fallback path — the fallback ONLY affects
			// FileExtension, never AbsolutePath. (UPnP-routed tracks
			// must keep their empty AbsolutePath so the file handler's
			// upnp fast-path takes over BEFORE the filesystem branch
			// would `os.Open("")`.)
			if got.AbsolutePath != tc.absPath {
				t.Errorf("AbsolutePath = %q; want %q (must NOT be backfilled from t.Path)",
					got.AbsolutePath, tc.absPath)
			}
		})
	}
}
