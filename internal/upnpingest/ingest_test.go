package upnpingest

import (
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

func TestDecideSkipWalk_PureLogic(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	walked := now.Add(-2 * time.Hour)
	old := now.Add(-48 * time.Hour)

	cases := []struct {
		name     string
		current  string
		stored   string
		lastWalk time.Time
		backstop time.Duration
		force    bool
		wantSkip bool
	}{
		{"id matches + within backstop → skip", "42", "42", walked, 24 * time.Hour, false, true},
		{"id matches but past backstop → walk", "42", "42", old, 24 * time.Hour, false, false},
		{"id moved → walk", "43", "42", walked, 24 * time.Hour, false, false},
		{"current id empty (server error) → walk", "", "42", walked, 24 * time.Hour, false, false},
		{"current id is '0' (MiniDLNA verbatim) → walk", "0", "0", walked, 24 * time.Hour, false, false},
		{"forceWalk overrides everything → walk", "42", "42", walked, 24 * time.Hour, true, false},
		{"no prior walk → walk", "42", "42", time.Time{}, 24 * time.Hour, false, false},
		{"backstop disabled (negative) + ids match → skip", "42", "42", time.Time{}, -1, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideSkipWalk(c.current, c.stored, c.lastWalk, now, c.backstop, c.force)
			if got != c.wantSkip {
				t.Fatalf("got %v; want %v", got, c.wantSkip)
			}
		})
	}
}

func TestBuildTrackAndRouting_MapsFields(t *testing.T) {
	now := time.Unix(2_000_000, 0).UTC()
	w := upnp.Walked{
		Path:     "Chord/Music/Artist/Album/01 - Title.flac",
		ObjectID: "64$0$1", ParentObjectID: "64$0",
		Res:           "http://h:8200/MediaItems/5.flac",
		ProtocolInfo:  "http-get:*:audio/x-flac:*",
		Size:          12345,
		Duration:      "0:04:30.000",
		SampleRate:    44100,
		BitsPerSample: 16,
		Channels:      2,
		Title:         "Title",
		Artist:        "Artist",
		Album:         "Album",
		TrackNumber:   1,
		Date:          "2019-01-01",
		AlbumPath:     "Chord/Music/Artist/Album",
	}
	tr, rt := buildTrackAndRouting(w, "uuid:abc", now)
	if tr.Path != w.Path || tr.Size != 12345 || !tr.ModTime.Equal(now) {
		t.Fatalf("Track core fields: %+v", tr)
	}
	if tr.Title != "Title" || tr.Artist != "Artist" || tr.Album != "Album" {
		t.Errorf("metadata: %+v", tr)
	}
	if tr.Codec != "FLAC" {
		t.Errorf("Codec = %q; want FLAC", tr.Codec)
	}
	if tr.Enriched == nil || !*tr.Enriched {
		t.Errorf("Enriched = %v; want true", tr.Enriched)
	}
	if tr.TrackNumber == nil || *tr.TrackNumber != 1 {
		t.Errorf("TrackNumber = %v; want 1", tr.TrackNumber)
	}
	if tr.Duration == nil || *tr.Duration != 270 {
		t.Errorf("Duration = %v; want 270.0", tr.Duration)
	}
	if tr.Year == nil || *tr.Year != 2019 {
		t.Errorf("Year = %v; want 2019", tr.Year)
	}
	if tr.IsDSD != nil {
		t.Errorf("IsDSD should be nil for FLAC; got %v", tr.IsDSD)
	}
	// Routing sidecar.
	if rt.SourcePath != w.Path || rt.ServerUDN != "uuid:abc" ||
		rt.ObjectID != "64$0$1" || rt.ParentObjectID != "64$0" ||
		rt.ResURL != w.Res || rt.ProtocolInfo != w.ProtocolInfo ||
		!rt.LastSeenAt.Equal(now) {
		t.Fatalf("routing: %+v", rt)
	}
}

func TestBuildTrackAndRouting_DSF_IsDSDStampedTrue(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	w := upnp.Walked{
		Path:         "Chord/Music/X/01 - DSD.dsf",
		Res:          "http://h/MediaItems/6936.dsf",
		ProtocolInfo: "http-get:*:audio/x-dsf:*",
	}
	tr, _ := buildTrackAndRouting(w, "uuid:x", now)
	if tr.Codec != "DSF" {
		t.Errorf("Codec = %q; want DSF", tr.Codec)
	}
	if tr.IsDSD == nil || !*tr.IsDSD {
		t.Errorf("IsDSD = %v; want *true", tr.IsDSD)
	}
}

func TestCodecFromExtension_KnownAndUnknown(t *testing.T) {
	cases := map[string]string{
		"x.flac":  "FLAC",
		"x.dsf":   "DSF",
		"x.dff":   "DFF",
		"x.wav":   "WAV",
		"x.aiff":  "AIFF",
		"x.aif":   "AIFF",
		"x.mp3":   "MP3",
		"x.ogg":   "OGG",
		"x.opus":  "OPUS",
		"x.m4a":   "", // intentional: ALAC vs AAC ambiguity
		"x.mp4":   "",
		"x.weird": "",
		"x":       "",
	}
	for p, want := range cases {
		if got := codecFromExtension(p); got != want {
			t.Errorf("codecFromExtension(%q) = %q; want %q", p, got, want)
		}
	}
}

func TestParseDurationSeconds(t *testing.T) {
	cases := map[string]float64{
		"0:04:52.162":     4*60 + 52.162,
		"0:00:30":         30,
		"1:02:03.5":       1*3600 + 2*60 + 3.5,
		"":                0,
		"bad":             0,
		"0:bad:0":         0,
		"NOT_IMPLEMENTED": 0,
	}
	for s, want := range cases {
		got := parseDurationSeconds(s)
		if got != want {
			t.Errorf("parseDurationSeconds(%q) = %v; want %v", s, got, want)
		}
	}
}

func TestYearFromDate(t *testing.T) {
	cases := map[string]int{
		"2019-01-01": 2019,
		"1968":       1968,
		"":           0,
		"x":          0,
		"abc-def":    0,
	}
	for s, want := range cases {
		if got := yearFromDate(s); got != want {
			t.Errorf("yearFromDate(%q) = %d; want %d", s, got, want)
		}
	}
}
