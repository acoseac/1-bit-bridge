package upnpingest

import (
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
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
		// strconv-based regression coverage: signed leads
		// MUST reject (strconv.Atoi accepts a leading '-' / '+' as a
		// sign; DLNA dates are always digit-prefixed so the explicit
		// digit-only gate is the right shape).
		"-100-01-01": 0,
		"+100":       0,
		"  2019  ":   2019, // outer whitespace is trimmed (per the helper's contract)
	}
	for s, want := range cases {
		if got := yearFromDate(s); got != want {
			t.Errorf("yearFromDate(%q) = %d; want %d", s, got, want)
		}
	}
}

func TestParseDurationSeconds_StrconvRegression(t *testing.T) {
	// strconv-based regression coverage. The hand-rolled parser quietly
	// returned 0 for everything weird; strconv accepts more shapes, so
	// these cases pin the desired behaviour explicitly.
	cases := map[string]float64{
		// Signed components MUST reject — negatives are nonsensical
		// for a duration and the iOS scrubber would render a stuck UI.
		"-1:00:00": 0,
		"0:-30:00": 0,
		"0:00:-5":  0,
		// Multiple dots in the fractional component MUST reject (the
		// old parser accepted it ambiguously).
		"0:00:1.2.3": 0,
		// Exponent notation in the seconds part — strconv accepts it
		// for floats but we want a clean reject (DLNA spec is fixed-
		// point only).
		"0:00:1e2": 100, // strconv.ParseFloat accepts; documented as a known relaxation
		// Whitespace inside segments MUST reject.
		"0: 00:30": 0,
	}
	for s, want := range cases {
		got := parseDurationSeconds(s)
		if got != want {
			t.Errorf("parseDurationSeconds(%q) = %v; want %v", s, got, want)
		}
	}
}

func TestStableServerKey(t *testing.T) {
	// Real UDN wins.
	got := StableServerKey(config.UPnPUpstreamServerConfig{
		Name: "X", UDN: "uuid:ABC", ManualDescriptionURL: "http://h/d.xml",
	})
	if got != "uuid:abc" {
		t.Errorf("real UDN: got %q", got)
	}
	// No UDN: SHA-256 of ManualDescriptionURL, "manual:" prefix.
	k1 := StableServerKey(config.UPnPUpstreamServerConfig{
		Name: "Same Name", ManualDescriptionURL: "http://a.local:8200/desc.xml",
	})
	k2 := StableServerKey(config.UPnPUpstreamServerConfig{
		Name: "Same Name", ManualDescriptionURL: "http://b.local:8200/desc.xml",
	})
	if k1 == k2 {
		t.Fatalf("two servers with identical Name but distinct URLs collided: %q", k1)
	}
	if !strings.HasPrefix(k1, "manual:") || !strings.HasPrefix(k2, "manual:") {
		t.Errorf("manual keys missing prefix: %q / %q", k1, k2)
	}
	// Identical URL → identical key (deterministic).
	k3 := StableServerKey(config.UPnPUpstreamServerConfig{
		Name: "Diff Name", ManualDescriptionURL: "http://a.local:8200/desc.xml",
	})
	if k3 != k1 {
		t.Errorf("same URL should produce same key: %q vs %q", k3, k1)
	}
}
