package manifest

import (
	"strconv"
	"testing"
)

// Two raw keys that normalise to one alias read the first non-empty value
// in ascending raw-key order on every extraction. REPLAYGAIN_TRACK_GAIN
// comes before replaygain_track_gain, and MUSICBRAINZ_ALBUM_ID comes
// before MusicBrainz Album Id.
func TestTwoSpellingsOfOneAliasReadOneValueOnEveryExtraction(t *testing.T) {
	const extractions = 50
	cases := []struct {
		name string
		raw  map[string]any
		read func(Track) string
		want string
	}{
		{
			name: "replaygain",
			raw: map[string]any{
				"REPLAYGAIN_TRACK_GAIN": "-6.00 dB",
				"replaygain_track_gain": "-3.00 dB",
			},
			read: trackReplayGain,
			want: "-6.00",
		},
		{
			name: "musicbrainz album id",
			raw: map[string]any{
				"MusicBrainz Album Id": "11111111-1111-4111-8111-111111111111",
				"MUSICBRAINZ_ALBUM_ID": "22222222-2222-4222-8222-222222222222",
			},
			read: func(tr Track) string { return tr.MusicBrainzAlbumID },
			want: "22222222-2222-4222-8222-222222222222",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]int{}
			for i := 0; i < extractions; i++ {
				var tr Track
				populateFromTagMetadata(rawOnlyMetadata{raw: tc.raw}, &tr)
				seen[tc.read(tr)]++
			}
			if len(seen) != 1 {
				t.Fatalf("%d extractions saw %d values: %v", extractions, len(seen), seen)
			}
			for got := range seen {
				if got != tc.want {
					t.Fatalf("stable value %q, want %q (the earlier raw key)", got, tc.want)
				}
			}
		})
	}
}

// A value that trims to nothing is skipped, so the later raw key still
// answers. Ascending order visits REPLAYGAIN_TRACK_GAIN first.
func TestAnEmptyEarlierRawKeyLeavesTheLaterValue(t *testing.T) {
	raw := map[string]any{
		"REPLAYGAIN_TRACK_GAIN": "   ",
		"replaygain_track_gain": "-3.00 dB",
	}
	var tr Track
	populateFromTagMetadata(rawOnlyMetadata{raw: raw}, &tr)
	if got := trackReplayGain(tr); got != "-3.00" {
		t.Fatalf("track gain %q, want -3.00 from the later key", got)
	}
}

// One matching key has nothing to choose. Each spelling, alone, reads
// the value it carries on every extraction.
func TestOneSpellingOfAnAliasReadsTheValueItCarries(t *testing.T) {
	const extractions = 50
	cases := []struct {
		name string
		raw  map[string]any
		read func(Track) string
		want string
	}{
		{
			name: "lowercase replaygain",
			raw:  map[string]any{"replaygain_track_gain": "-3.00 dB"},
			read: trackReplayGain,
			want: "-3.00",
		},
		{
			name: "uppercase replaygain",
			raw:  map[string]any{"REPLAYGAIN_TRACK_GAIN": "-6.00 dB"},
			read: trackReplayGain,
			want: "-6.00",
		},
		{
			name: "spaced album id",
			raw:  map[string]any{"MusicBrainz Album Id": "11111111-1111-4111-8111-111111111111"},
			read: func(tr Track) string { return tr.MusicBrainzAlbumID },
			want: "11111111-1111-4111-8111-111111111111",
		},
		{
			name: "underscored album id",
			raw:  map[string]any{"MUSICBRAINZ_ALBUM_ID": "22222222-2222-4222-8222-222222222222"},
			read: func(tr Track) string { return tr.MusicBrainzAlbumID },
			want: "22222222-2222-4222-8222-222222222222",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < extractions; i++ {
				var tr Track
				populateFromTagMetadata(rawOnlyMetadata{raw: tc.raw}, &tr)
				if got := tc.read(tr); got != tc.want {
					t.Fatalf("extraction %d: %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}

func trackReplayGain(tr Track) string {
	if tr.ReplayGainTrackDB == nil {
		return ""
	}
	return strconv.FormatFloat(*tr.ReplayGainTrackDB, 'f', 2, 64)
}
