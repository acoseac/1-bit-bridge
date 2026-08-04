package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestMinIntervalForBase(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		public  time.Duration
		hosts   []string
		want    time.Duration
		comment string
	}{
		{"mb public default", DefaultMusicBrainzBase, PublicMBMinInterval, publicMBHosts, PublicMBMinInterval,
			"the shipped default must keep MB's 1 req/s contract"},
		{"mb subdomain", "https://beta.musicbrainz.org/ws/2", PublicMBMinInterval, publicMBHosts, PublicMBMinInterval,
			"same operator, same policy"},
		{"mb uppercase host", "https://MusicBrainz.ORG/ws/2", PublicMBMinInterval, publicMBHosts, PublicMBMinInterval,
			"host comparison is case-insensitive"},
		{"mb with port", "https://musicbrainz.org:443/ws/2", PublicMBMinInterval, publicMBHosts, PublicMBMinInterval,
			"Hostname() strips the port"},
		{"caa public default", DefaultCoverArtBase, PublicCAAMinInterval, publicCAAHosts, PublicCAAMinInterval,
			"the shipped default must keep CAA's polite floor"},
		{"caa redirect target", "https://ia800207.us.archive.org", PublicCAAMinInterval, publicCAAHosts, PublicCAAMinInterval,
			"CAA redirects into archive.org; same infrastructure"},

		{"atlas mirror mb", "https://atlas.ars.md/ws/2", PublicMBMinInterval, publicMBHosts, SelfHostedMinInterval,
			"the motivating case — a private mirror has no public rate limit"},
		{"atlas mirror caa", "https://atlas.ars.md", PublicCAAMinInterval, publicCAAHosts, SelfHostedMinInterval,
			"same host serves the CAA-shaped routes"},
		{"localhost mirror", "http://127.0.0.1:5000/ws/2", PublicMBMinInterval, publicMBHosts, SelfHostedMinInterval,
			"a local musicbrainz-docker"},

		// Substring near-misses must NOT be mistaken for the public host.
		{"lookalike suffix", "https://notmusicbrainz.org/ws/2", PublicMBMinInterval, publicMBHosts, SelfHostedMinInterval,
			"suffix match is anchored on a dot, so this is not a subdomain"},
		{"lookalike prefix", "https://musicbrainz.org.example.com/ws/2", PublicMBMinInterval, publicMBHosts, SelfHostedMinInterval,
			"the registrable domain here is example.com"},

		// Fail-safe: anything we cannot resolve to a host stays polite.
		{"empty", "", PublicMBMinInterval, publicMBHosts, PublicMBMinInterval, "fail safe"},
		{"relative", "/ws/2", PublicMBMinInterval, publicMBHosts, PublicMBMinInterval, "no host — fail safe"},
		{"garbage", "://:::", PublicMBMinInterval, publicMBHosts, PublicMBMinInterval, "unparseable — fail safe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := minIntervalForBase(tc.base, tc.public, tc.hosts); got != tc.want {
				t.Errorf("minIntervalForBase(%q) = %v, want %v (%s)", tc.base, got, tc.want, tc.comment)
			}
		})
	}
}

// TestNewEnricherDerivesPacingFromClients pins the contract that the pacing
// travels with the base URL. A default-configured bridge must be byte-for-byte
// unchanged; an Atlas-pointed one must stop sleeping against its own server.
func TestNewEnricherDerivesPacingFromClients(t *testing.T) {
	dir := t.TempDir()

	t.Run("public defaults unchanged", func(t *testing.T) {
		e := NewEnricher(nil,
			NewMusicBrainzClient("", "t", nil),
			NewCoverArtClient("", "t", nil),
			nil, filepath.Join(dir, "artwork"))
		if e.MBMinInterval != PublicMBMinInterval {
			t.Errorf("MBMinInterval = %v, want %v — the public MB contract is load-bearing",
				e.MBMinInterval, PublicMBMinInterval)
		}
		if e.CAAMinInterval != PublicCAAMinInterval {
			t.Errorf("CAAMinInterval = %v, want %v", e.CAAMinInterval, PublicCAAMinInterval)
		}
	})

	t.Run("self-hosted mirror paces fast", func(t *testing.T) {
		e := NewEnricher(nil,
			NewMusicBrainzClient("https://atlas.example.internal/ws/2", "t", nil),
			NewCoverArtClient("https://atlas.example.internal", "t", nil),
			nil, filepath.Join(dir, "artwork"))
		if e.MBMinInterval != SelfHostedMinInterval {
			t.Errorf("MBMinInterval = %v, want %v", e.MBMinInterval, SelfHostedMinInterval)
		}
		if e.CAAMinInterval != SelfHostedMinInterval {
			t.Errorf("CAAMinInterval = %v, want %v", e.CAAMinInterval, SelfHostedMinInterval)
		}
	})

	t.Run("nil clients keep the polite default", func(t *testing.T) {
		e := NewEnricher(nil, nil, nil, nil, filepath.Join(dir, "artwork"))
		if e.MBMinInterval != PublicMBMinInterval || e.CAAMinInterval != PublicCAAMinInterval {
			t.Errorf("nil clients gave MB=%v CAA=%v, want the public defaults",
				e.MBMinInterval, e.CAAMinInterval)
		}
	})
}

// TestEnricherResolvesArtistWhenAlbumMisses pins that a release-search miss no
// longer costs the track its artist MBID.
//
// Pre-fix, `albumMBID == ""` returned straight to markSkipped, so the artist
// half — an independent and far more reliable query — never ran. With Atlas's
// measured 50% album hit rate that was roughly half the library losing an
// artist portrait for a reason unrelated to the artist.
func TestEnricherResolvesArtistWhenAlbumMisses(t *testing.T) {
	const artistMBID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			// No album match — the case this test is about.
			io.WriteString(w, `{"releases":[]}`)
		case strings.Contains(r.URL.Path, "/artist/"):
			io.WriteString(w, `{"artists":[{"id":"`+artistMBID+`","score":100,"name":"Artist"}]}`)
		}
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "a.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album Nobody Has",
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	defer startEnricherForTest(e, 5*time.Second)()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && e.skipped.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.skipped.Load() == 0 {
		t.Fatal("track was never marked skipped")
	}

	got, err := store.GetTrack(ctx, "a.flac")
	if err != nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if got.MusicBrainzAlbumID != "" {
		t.Errorf("MusicBrainzAlbumID = %q, want empty (the album genuinely has no match)", got.MusicBrainzAlbumID)
	}
	if got.ArtistMBID != artistMBID {
		t.Errorf("ArtistMBID = %q, want %q — an album miss must not cost the artist resolution",
			got.ArtistMBID, artistMBID)
	}
}
