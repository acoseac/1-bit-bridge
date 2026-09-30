package enrich

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestApplyAcousticFallbackKeepsARecordingIDTheFileCarries: the fallback wrote
// the fingerprint's recording id over one the file's tags carry, and the
// version-stale merge, which keeps a file's valid id over any post-scan value,
// put the file's back on every ExtractorVersion bump, re-enriching the row
// each time (backlog B188). A file's valid id stays; no id, or one that is no
// MBID, takes the fingerprint's.
func TestApplyAcousticFallbackKeepsARecordingIDTheFileCarries(t *testing.T) {
	const tagged = "0b6b0c5e-2b5a-4b1c-9c6d-6a3c2f0e9d11"
	for _, c := range []struct{ name, carries, want string }{
		{"a valid id the file carries", tagged, tagged},
		{"no id", "", fbRecMBID},
		{"a value that is no MBID", "not-an-mbid", fbRecMBID},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &Enricher{acoustic: fakeLookup{"a.flac": {
				ArtistMBID: fbArtistMBID, ArtistName: "M83", RecordingMBID: fbRecMBID, AcoustID: "acid-1",
			}}}
			tr := &manifest.Track{Path: "a.flac", Artist: "An Unknown Artist", MusicBrainzTrackID: c.carries}
			if _, o := e.applyAcousticFallback(context.Background(), tr); o != acousticApplied {
				t.Fatalf("outcome = %v, want applied", o)
			}
			if tr.MusicBrainzTrackID != c.want {
				t.Errorf("MusicBrainzTrackID = %q, want %q", tr.MusicBrainzTrackID, c.want)
			}
			if tr.ArtistMBID != fbArtistMBID {
				t.Errorf("ArtistMBID = %q, want the fingerprint's %q", tr.ArtistMBID, fbArtistMBID)
			}
		})
	}
}
