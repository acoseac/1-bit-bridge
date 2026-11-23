package dlna

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// browseGoldenTracks are the tracks the golden was captured for, from the
// ContentDirectory's builder at the base before B634 exported it.
func browseGoldenTracks() []TrackInfo {
	return []TrackInfo{
		{TrackID: "0123456789abcdef", AbsolutePath: "/music/A & B/01 <One> \"x\" 'y'.flac", RelativePath: "A & B/01 <One> \"x\" 'y'.flac",
			Title: "One & <Two> \"q\" 'a'", Artist: "Ar & tist", AlbumArtist: "Album <Artist>", Album: "Al'bum", Composer: "Co\"mposer",
			Genre: "Jazz & Blues", Codec: "FLAC", FileExtension: ".flac", Size: 123456789, DurationSeconds: 245.678, SampleRateHz: 96000,
			BitsPerSample: 24, Channels: 2, Year: 2019, TrackNumber: 7, ArtworkKey: "a1b2c3d4",
			Variants: []VariantInfo{
				{VariantID: "upscaled-v2-176400-24", AbsolutePath: "/v/up.flac", FileExtension: ".flac", Size: 999, BitDepth: 24, SampleRate: 176400},
				{VariantID: "optimized-v2-48000-16", AbsolutePath: "/v/opt.flac", FileExtension: ".flac", Size: 555, BitDepth: 16, SampleRate: 48000},
			}},
		{TrackID: "fedcba9876543210", AbsolutePath: "/music/D/02.dsf", RelativePath: "D/02.dsf", Title: "DSD", Artist: "X", Album: "Y",
			Codec: "DSF", FileExtension: ".dsf", Size: 777, DurationSeconds: 60, SampleRateHz: 2822400, BitsPerSample: 1, Channels: 2,
			IsDSD: true, ArtworkKey: "deadbeef",
			Variants: []VariantInfo{{VariantID: "upscaled-v2-176400-24", AbsolutePath: "/v/d.flac", FileExtension: ".flac", Size: 1, BitDepth: 24, SampleRate: 176400}}},
	}
}

// browseGolden is DIDLForTrack(t.toDIDLOpts(url, ua, parent, art)) for
// each golden track and request, as the builder answered at the base before
// B634 split its two bases and exported it (captured from that tree).
var browseGolden = []struct {
	track           int
	url, ua, parent string
	art             bool
	want            string
}{
	{0, "http://192.168.1.20:7790", "BubbleUPnP/3.7.1", "123456", true,
		"<item id=\"0123456789abcdef\" parentID=\"123456\" restricted=\"1\"><dc:title>One &amp; &lt;Two&gt; &quot;q&quot; &apos;a&apos;</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class><upnp:artist>Ar &amp; tist</upnp:artist><upnp:artist role=\"AlbumArtist\">Album &lt;Artist&gt;</upnp:artist><upnp:album>Al&apos;bum</upnp:album><upnp:author role=\"Composer\">Co&quot;mposer</upnp:author><upnp:genre>Jazz &amp; Blues</upnp:genre><dc:date>2019-01-01</dc:date><upnp:originalTrackNumber>7</upnp:originalTrackNumber><upnp:albumArtURI>http://192.168.1.20:7790/dlna/artwork/a1b2c3d4</upnp:albumArtURI><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"123456789\" duration=\"0:04:05.678\" sampleFrequency=\"96000\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/0123456789abcdef</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"555\" duration=\"0:04:05.678\" sampleFrequency=\"48000\" bitsPerSample=\"16\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/0123456789abcdef/variant-optimized-v2-48000-16.flac</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"999\" duration=\"0:04:05.678\" sampleFrequency=\"176400\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/0123456789abcdef/variant-upscaled-v2-176400-24.flac</res></item>"},
	{0, "http://[fd00::20]:7790", "Music Player Daemon 0.23.12", "654321", false,
		"<item id=\"0123456789abcdef\" parentID=\"654321\" restricted=\"1\"><dc:title>One &amp; &lt;Two&gt; &quot;q&quot; &apos;a&apos;</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class><upnp:artist>Ar &amp; tist</upnp:artist><upnp:artist role=\"AlbumArtist\">Album &lt;Artist&gt;</upnp:artist><upnp:album>Al&apos;bum</upnp:album><upnp:author role=\"Composer\">Co&quot;mposer</upnp:author><upnp:genre>Jazz &amp; Blues</upnp:genre><dc:date>2019-01-01</dc:date><upnp:originalTrackNumber>7</upnp:originalTrackNumber><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"123456789\" duration=\"0:04:05.678\" sampleFrequency=\"96000\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://[fd00::20]:7790/dlna/file/0123456789abcdef</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"555\" duration=\"0:04:05.678\" sampleFrequency=\"48000\" bitsPerSample=\"16\" nrAudioChannels=\"2\">http://[fd00::20]:7790/dlna/file/0123456789abcdef/variant-optimized-v2-48000-16.flac</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"999\" duration=\"0:04:05.678\" sampleFrequency=\"176400\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://[fd00::20]:7790/dlna/file/0123456789abcdef/variant-upscaled-v2-176400-24.flac</res></item>"},
	{0, "http://192.168.1.20:7790", "foobar2000/2.1", "0", true,
		"<item id=\"0123456789abcdef\" parentID=\"0\" restricted=\"1\"><dc:title>One &amp; &lt;Two&gt; &quot;q&quot; &apos;a&apos;</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class><upnp:artist>Ar &amp; tist</upnp:artist><upnp:artist role=\"AlbumArtist\">Album &lt;Artist&gt;</upnp:artist><upnp:album>Al&apos;bum</upnp:album><upnp:author role=\"Composer\">Co&quot;mposer</upnp:author><upnp:genre>Jazz &amp; Blues</upnp:genre><dc:date>2019-01-01</dc:date><upnp:originalTrackNumber>7</upnp:originalTrackNumber><upnp:albumArtURI>http://192.168.1.20:7790/dlna/artwork/a1b2c3d4</upnp:albumArtURI><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"123456789\" duration=\"0:04:05.678\" sampleFrequency=\"96000\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/0123456789abcdef</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"555\" duration=\"0:04:05.678\" sampleFrequency=\"48000\" bitsPerSample=\"16\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/0123456789abcdef/variant-optimized-v2-48000-16.flac</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"999\" duration=\"0:04:05.678\" sampleFrequency=\"176400\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/0123456789abcdef/variant-upscaled-v2-176400-24.flac</res></item>"},
	{1, "http://192.168.1.20:7790", "BubbleUPnP/3.7.1", "123456", true,
		"<item id=\"fedcba9876543210\" parentID=\"123456\" restricted=\"1\"><dc:title>DSD</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class><upnp:artist>X</upnp:artist><upnp:album>Y</upnp:album><upnp:albumArtURI>http://192.168.1.20:7790/dlna/artwork/deadbeef</upnp:albumArtURI><res protocolInfo=\"http-get:*:audio/x-dsf:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"777\" duration=\"0:01:00.000\" sampleFrequency=\"2822400\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/fedcba9876543210</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"1\" duration=\"0:01:00.000\" sampleFrequency=\"176400\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/fedcba9876543210/variant-upscaled-v2-176400-24.flac</res></item>"},
	{1, "http://[fd00::20]:7790", "Music Player Daemon 0.23.12", "654321", false,
		"<item id=\"fedcba9876543210\" parentID=\"654321\" restricted=\"1\"><dc:title>DSD</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class><upnp:artist>X</upnp:artist><upnp:album>Y</upnp:album><res protocolInfo=\"http-get:*:audio/x-dsf:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"777\" duration=\"0:01:00.000\" sampleFrequency=\"2822400\" nrAudioChannels=\"2\">http://[fd00::20]:7790/dlna/file/fedcba9876543210</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"1\" duration=\"0:01:00.000\" sampleFrequency=\"176400\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://[fd00::20]:7790/dlna/file/fedcba9876543210/variant-upscaled-v2-176400-24.flac</res></item>"},
	{1, "http://192.168.1.20:7790", "foobar2000/2.1", "0", true,
		"<item id=\"fedcba9876543210\" parentID=\"0\" restricted=\"1\"><dc:title>DSD</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class><upnp:artist>X</upnp:artist><upnp:album>Y</upnp:album><upnp:albumArtURI>http://192.168.1.20:7790/dlna/artwork/deadbeef</upnp:albumArtURI><res protocolInfo=\"http-get:*:audio/x-dsf:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"777\" duration=\"0:01:00.000\" sampleFrequency=\"2822400\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/fedcba9876543210</res><res protocolInfo=\"http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000\" size=\"1\" duration=\"0:01:00.000\" sampleFrequency=\"176400\" bitsPerSample=\"24\" nrAudioChannels=\"2\">http://192.168.1.20:7790/dlna/file/fedcba9876543210/variant-upscaled-v2-176400-24.flac</res></item>"},
}

// TestTheBrowseItemIsTheSameBytesAfterTheExport pins the export of the item
// builder (B634): Browse's and Search's item, the one server URL its request
// names standing for both bases and the source's and the variants' <res>,
// is byte for byte what the builder wrote before the export, and the exported
// builder given that one URL as both bases writes the same bytes.
func TestTheBrowseItemIsTheSameBytesAfterTheExport(t *testing.T) {
	tracks := browseGoldenTracks()
	for _, g := range browseGolden {
		tr := tracks[g.track]
		if got := DIDLForTrack(tr.toDIDLOpts(g.url, g.ua, g.parent, g.art)); got != g.want {
			t.Errorf("track %d at %s (%s, art %v): Browse's item changed:\n got %q\nwant %q", g.track, g.url, g.ua, g.art, got, g.want)
		}
		o := DIDLItemOpts{TrackBase: g.url, UserAgent: g.ua, ParentID: g.parent}
		if g.art {
			o.ArtworkBase = g.url
		}
		if got := tr.DIDLItem(o); got != g.want {
			t.Errorf("track %d at %s: the exported builder on one base writes %q, want Browse's %q", g.track, g.url, got, g.want)
		}
	}
}

// stubArtwork is an artwork source that serves nothing: the route mounted.
type stubArtwork struct{}

func (stubArtwork) ServeArtwork(w http.ResponseWriter, _ *http.Request, _ string) {
	http.NotFound(w, nil)
}

// TestTheArtworkBaseIsTheListenersOwnEndpoint pins Server.ArtworkBase: while
// the listener runs with its artwork route mounted, an address it takes
// connections on gives the listener's own endpoint there, on the port it
// bound; an address it does not take, a listener without the route, and a
// stopped one give none.
func TestTheArtworkBaseIsTheListenersOwnEndpoint(t *testing.T) {
	loopback := netip.MustParseAddr("127.0.0.1")
	s := startLoopbackDLNA(t, ServerConfig{Artwork: stubArtwork{}}, nil)
	_, port, err := net.SplitHostPort(s.cfg.ListenAddress)
	if err != nil {
		t.Fatal(err)
	}
	base, ok := s.ArtworkBase(loopback)
	if !ok || base != "http://127.0.0.1:"+port {
		t.Fatalf("ArtworkBase(127.0.0.1) = %q, %v; want the listener's own endpoint, http://127.0.0.1:%s", base, ok, port)
	}
	if base, ok := s.ArtworkBase(netip.MustParseAddr("192.168.1.20")); ok {
		t.Errorf("an address the listener does not take gave %q", base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if base, ok := s.ArtworkBase(loopback); ok {
		t.Errorf("a stopped listener gave %q", base)
	}
	plain := startLoopbackDLNA(t, ServerConfig{}, nil)
	if base, ok := plain.ArtworkBase(loopback); ok {
		t.Errorf("a listener without the artwork route gave %q", base)
	}
}
