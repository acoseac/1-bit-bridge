package main

import (
	"strings"
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
	// Four orthogonal cases — covering filesystem (absPath wins), the
	// routed-sentinel fallback to t.Path, case-folding on uppercase
	// extensions (one case suffices; the helper applies ToLower to
	// whichever source it picked, so testing both absPath and t.Path
	// flavors would be redundant), and the defensive both-empty edge
	// case. Earlier drafts tested the routed fallback separately for
	// .flac / .dsf / .dff / .mp3; collapsed because `filepath.Ext`
	// doesn't care about the extension's letters — the fallback
	// either works for all extensions or none. Per SonarCloud
	// duplicated-lines reduction on PR #356 round-5.
	cases := []struct {
		name      string
		trackPath string
		absPath   string
		wantExt   string
	}{
		{"filesystem-backed: derives from absPath", "Artist/Album/01.flac", "/srv/library/01.flac", ".flac"},
		{"routed sentinel: falls back to t.Path", "2go/Music/AC-DC/01 - Hells Bells.flac", "", ".flac"},
		{"case-fold: uppercase extension → lowercase", "2go/Music/Old/01.MP3", "", ".mp3"},
		{"defensive: no extension at all → empty", "no-extension", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := manifest.Track{Path: tc.trackPath, Title: "T", Artist: "A", Album: "Alb"}
			got := manifestTrackToDLNATrackInfo(tr, tc.absPath, "")
			if got.FileExtension != tc.wantExt {
				t.Errorf("FileExtension = %q; want %q", got.FileExtension, tc.wantExt)
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

// Test_manifestTrackToDLNATrackInfo_ArtworkKeyIsTheAppsRetryKey pins the
// cross-repo agreement behind `/dlna/artwork/{key}`: the CDS keys a cover
// the way the iOS client persists `Album.artworkHash` — `artworkVersion ??
// artworkMBID`, the `/v1/artwork/{key}` segment — so the bridge's own DIDL
// and an app emitting `albumArtURI` for a renderer compose the same URL.
func Test_manifestTrackToDLNATrackInfo_ArtworkKeyIsTheAppsRetryKey(t *testing.T) {
	cases := []struct {
		name    string
		mbid    string
		version string
		want    string
	}{
		{"no cover identity → no key", "", "", ""},
		{"MBID only → the MBID", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
		{"scanner local hash → the hash", "local-" + strings.Repeat("ab", 32), "", "local-" + strings.Repeat("ab", 32)},
		{"version alias present → the alias wins, as on the phone", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "0123456789abcdef", "0123456789abcdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := manifest.Track{Path: "A/B/01.flac", ArtworkMBID: tc.mbid, ArtworkVersion: tc.version}
			if got := manifestTrackToDLNATrackInfo(tr, "/srv/A/B/01.flac", "").ArtworkKey; got != tc.want {
				t.Errorf("ArtworkKey = %q, want %q", got, tc.want)
			}
		})
	}
}
