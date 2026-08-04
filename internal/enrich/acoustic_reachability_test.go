package enrich

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestAcousticFallbackReachableWithLocalArtwork pins the fix for a bug that
// made the whole feature inert in production while every unit test passed.
//
// The consult used to sit inside `albumMBID == "" && !HasPrefix(ArtworkMBID,
// "local-")`. That second clause is PR #98's local-artwork contract — a track
// whose cover the scanner curated is not a failure — and it belongs on the
// STAMP, not on reachability. Gating the consult behind it meant any track with
// a folder.jpg fell through to MarkEnriched without ever asking the audio.
//
// On the test host that was 18,306 of 18,429 tracks: the sweeper accepted 455
// fingerprints, re-queued all 455, and not one was consulted. Nothing logged an
// error, and the process-lifetime skip tally was EMPTY — because markSkipped
// lives in the same branch that was being skipped. A green suite plus a silent
// production is exactly the shape this test exists to prevent.
//
// The fixture is the production shape: local artwork present, MusicBrainz
// answering cleanly with no candidates, and a fingerprint answer waiting.
func TestAcousticFallbackReachableWithLocalArtwork(t *testing.T) {
	// Local artwork present, MusicBrainz answering cleanly with nothing: the
	// production shape this bug hid in.
	e, store := newOfflineEnricher(t, nil)
	ctx := context.Background()

	const localSentinel = "local-" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "curated.flac", Size: 1, ModTime: time.Now(),
		Artist: "Ducu Bertzi", Album: "Dor de duca",
		// The scanner already curated a cover from folder.jpg. This is the
		// overwhelmingly common case on a real library.
		ArtworkMBID: localSentinel,
	}); err != nil {
		t.Fatal(err)
	}

	e.WithAcousticFallback(fakeLookup{"curated.flac": {
		ArtistMBID: fbArtistMBID, ArtistName: "Ducu Bertzi", AcoustID: "acid-reach",
	}})
	defer startEnricherForTest(e, 5*time.Second)()

	deadline := time.Now().Add(5 * time.Second)
	var got *manifest.Track
	for time.Now().Before(deadline) {
		tr, gerr := store.GetTrack(ctx, "curated.flac")
		if gerr == nil && tr != nil && tr.ArtistMBID != "" {
			got = tr
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the fingerprint answer was never applied — with local artwork present " +
			"the track reached MarkEnriched without the acoustic fallback being consulted")
	}
	if got.ArtistMBID != fbArtistMBID {
		t.Errorf("ArtistMBID = %q, want the fingerprinted %q", got.ArtistMBID, fbArtistMBID)
	}
	// Write-target discipline survives the reachability change: a fingerprint
	// identifies audio, so it may never name a release.
	if got.MusicBrainzAlbumID != "" {
		t.Errorf("MusicBrainzAlbumID = %q, want empty — a fingerprint cannot identify a release",
			got.MusicBrainzAlbumID)
	}
	// The curated cover is untouched; recovering an artist must not disturb it.
	if got.ArtworkMBID != localSentinel {
		t.Errorf("ArtworkMBID = %q, want the curated sentinel preserved", got.ArtworkMBID)
	}
}
