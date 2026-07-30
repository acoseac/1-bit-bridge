package enrich

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A memo key has to name every input its ladder reads, and for a while these
// did not: the album key was (artist, album) from a time when that WAS the
// query, and the ladders later grew an albumArtist rung. On a track whose
// artist tag is junk, that rung is the one that answers — so two tracks
// agreeing on (artist, album) and differing in albumArtist shared an entry and
// took whichever answer ran first.
//
// The tests below are behavioural on purpose. A key-shape assertion would pass
// against any two distinct strings and tell us nothing about whether the cache
// actually consults them.

// TestArtistCacheDoesNotCollideAcrossAlbumArtists is the artist half. Both
// tracks carry the same unhelpful artist tag and differ only in albumArtist,
// which is the rung that resolves them.
func TestArtistCacheDoesNotCollideAcrossAlbumArtists(t *testing.T) {
	// Answers only the two composer names; anything else is a clean miss, so
	// rung 1 ("Unknown") falls through to the albumArtist rung exactly as it
	// does in production.
	mbids := map[string]string{
		"Beethoven": "11111111-1111-4111-8111-111111111111",
		"Mahler":    "22222222-2222-4222-8222-222222222222",
	}
	e, _ := newOfflineEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "artist") {
			_, _ = w.Write([]byte(`{"releases":[]}`))
			return
		}
		q, _ := url.QueryUnescape(r.URL.Query().Get("query"))
		for name, id := range mbids {
			if strings.Contains(q, name) {
				_, _ = w.Write([]byte(`{"artists":[{"id":"` + id + `","name":"` + name + `","score":100}]}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"artists":[]}`))
	})

	beethoven := &manifest.Track{Path: "x/1.flac", Artist: "Unknown", AlbumArtist: "Beethoven"}
	mahler := &manifest.Track{Path: "x/2.flac", Artist: "Unknown", AlbumArtist: "Mahler"}

	for _, tr := range []*manifest.Track{beethoven, mahler} {
		if err := e.resolveArtist(context.Background(), tr); err != nil {
			t.Fatalf("resolveArtist(%s): %v", tr.AlbumArtist, err)
		}
	}

	if beethoven.ArtistMBID != mbids["Beethoven"] {
		t.Fatalf("first track got %q, want Beethoven's MBID — the fixture never resolved, "+
			"so the collision below cannot be observed", beethoven.ArtistMBID)
	}
	if mahler.ArtistMBID == beethoven.ArtistMBID {
		t.Errorf("both tracks resolved to %q. They share an artist tag and differ only in "+
			"albumArtist — the rung that answers — so a key omitting albumArtist hands the "+
			"second track the first one's artist", mahler.ArtistMBID)
	}
	if mahler.ArtistMBID != mbids["Mahler"] {
		t.Errorf("second track got %q, want Mahler's MBID", mahler.ArtistMBID)
	}
}

// TestAlbumCacheDoesNotCollideAcrossAlbumArtists is the album half, and the
// one that costs more when it goes wrong: the shared entry carries a release
// MBID, so the loser gets another album's identity and cover.
//
// The shape is ordinary in a classical library, which tags artist with the
// performer and albumArtist with the composer — one orchestra's "Symphony
// No. 5" is one key for Beethoven's and Mahler's alike.
func TestAlbumCacheDoesNotCollideAcrossAlbumArtists(t *testing.T) {
	const (
		beethovenRelease = "33333333-3333-4333-8333-333333333333"
		mahlerRelease    = "44444444-4444-4444-8444-444444444444"
	)
	release := func(id, credit string) string {
		return `{"releases":[{"id":"` + id + `","score":100,"title":"Symphony No. 5",` +
			`"artist-credit":[{"name":"` + credit + `"}]}]}`
	}
	e, _ := newOfflineEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "artist") {
			_, _ = w.Write([]byte(`{"artists":[]}`))
			return
		}
		q, _ := url.QueryUnescape(r.URL.Query().Get("query"))
		switch {
		case strings.Contains(q, "Beethoven"):
			_, _ = w.Write([]byte(release(beethovenRelease, "Beethoven")))
		case strings.Contains(q, "Mahler"):
			_, _ = w.Write([]byte(release(mahlerRelease, "Mahler")))
		default:
			_, _ = w.Write([]byte(`{"releases":[]}`))
		}
	})

	// Identical performer and album title; only the composer differs.
	newTrack := func(path, composer string) *manifest.Track {
		return &manifest.Track{
			Path:        path,
			Artist:      "Berliner Philharmoniker",
			AlbumArtist: composer,
			Album:       "Symphony No. 5",
		}
	}
	first := newTrack("b/1.flac", "Beethoven")
	second := newTrack("m/1.flac", "Mahler")

	for _, tr := range []*manifest.Track{first, second} {
		e.enrichOne(context.Background(), tr)
	}

	if first.MusicBrainzAlbumID != beethovenRelease {
		t.Fatalf("first track got release %q, want Beethoven's — the fixture never "+
			"resolved, so the collision below cannot be observed", first.MusicBrainzAlbumID)
	}
	if second.MusicBrainzAlbumID == first.MusicBrainzAlbumID {
		t.Errorf("both tracks resolved to release %q. Same performer, same album title, "+
			"different composer — a key omitting albumArtist gives the second album the "+
			"first one's identity, and with it the wrong cover", second.MusicBrainzAlbumID)
	}
	if second.MusicBrainzAlbumID != mahlerRelease {
		t.Errorf("second track got release %q, want Mahler's", second.MusicBrainzAlbumID)
	}
}

// TestCacheKeysCollapseWhenAlbumArtistAddsNothing pins the other direction.
// Widening a memo key costs hit rate, and these caches exist so the tracks of
// one album share a single round-trip. Where albumArtist cannot contribute a
// rung — absent, or folding to the artist — the key must stay exactly what it
// has always been.
func TestCacheKeysCollapseWhenAlbumArtistAddsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, artist, albumArtist string
	}{
		{"absent", "Metallica", ""},
		{"identical", "Metallica", "Metallica"},
		{"differs only by case", "Metallica", "metallica"},
		{"differs only by accent", "Yael Naim", "Yael Naïm"},
		{"differs only by punctuation", "Ain't Misbehavin", "Aint Misbehavin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := releaseCacheKey(tc.artist, tc.albumArtist, "Album"),
				cacheKey(tc.artist, "Album"); got != want {
				t.Errorf("releaseCacheKey = %q, want the historic %q — albumArtist adds no "+
					"rung here, so splitting the key only costs sibling-track sharing", got, want)
			}
			if got, want := artistCacheKey(tc.artist, tc.albumArtist),
				"artist\x00"+tc.artist; got != want {
				t.Errorf("artistCacheKey = %q, want the historic %q", got, want)
			}
		})
	}
}

// TestCacheKeysAgreeWithTheLadder is the invariant behind both: a key splits
// exactly when the ladder gains a rung from albumArtist. If these two ever
// disagree, one of the caches is either colliding or missing needlessly.
func TestCacheKeysAgreeWithTheLadder(t *testing.T) {
	for _, tc := range []struct{ artist, albumArtist, album string }{
		{"Metallica", "", "Album"},
		{"Metallica", "Metallica", "Album"},
		{"Berliner Philharmoniker", "Beethoven", "Symphony No. 5"},
		{"CD 01", "Abdullah Ibrahim", "Live"},
		{"Unknown", "Mahler", "Symphony No. 5"},
	} {
		t.Run(tc.artist+"/"+tc.albumArtist, func(t *testing.T) {
			ladderUses := false
			for _, a := range buildReleaseLadder(tc.artist, tc.albumArtist, tc.album) {
				if foldName(a.artist) != foldName(tc.artist) {
					ladderUses = true
				}
			}
			keySplit := releaseCacheKey(tc.artist, tc.albumArtist, tc.album) != cacheKey(tc.artist, tc.album)
			if ladderUses != keySplit {
				t.Errorf("ladder draws on albumArtist = %v but the key splits = %v — "+
					"a key that ignores an input its ladder reads collides; one that "+
					"splits on an input the ladder ignores just misses", ladderUses, keySplit)
			}
		})
	}
}
