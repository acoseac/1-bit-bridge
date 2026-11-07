package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The artwork and artist-image misses while enrich.enabled is off (backlog
// B617): a row that waits for the enricher is no promise then, so its miss
// is the terminal no_image, until the switch is on again.

const (
	offArtistMBID = "aaaaaaaa-1111-4111-8111-111111111111"
	offCoverMBID  = "bbbbbbbb-2222-4222-8222-222222222222"
	offLocalKey   = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// enrichmentOffFixture is a server over a real store holding two rows the
// enricher has not reached: one with an artist and a cover MBID, one with
// a scanner-owned local- cover. The switch starts off. Its artwork cache is
// the directory it returns.
func enrichmentOffFixture(t *testing.T) (hs *httptest.Server, token, artDir string, on *atomic.Bool) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	artDir = filepath.Join(dir, "artwork")
	for _, d := range []string{lib, artDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, tr := range []manifest.Track{
		{Path: "Artist/Album/01.flac", Artist: "Artist", Album: "Album", ArtistMBID: offArtistMBID, ArtworkMBID: offCoverMBID},
		{Path: "Artist/Album/02.flac", Artist: "Artist", Album: "Album", ArtworkMBID: offLocalKey},
	} {
		tr.Size, tr.ModTime = 1, time.Now()
		if err := store.UpsertTrack(context.Background(), &tr); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err = tokens.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	provider := manifest.NewProvider(store, manifest.NewScanner([]string{lib}, store, artDir))
	on = &atomic.Bool{}
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	srv := New(cfg, tokens, provider, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(provider).
		WithEnrichmentEnabled(on.Load)
	hs = httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, token, artDir, on
}

// wantMiss asks path with the bearer token and checks the status and the
// error code of the answer, and that only a 202 carries Retry-After.
func wantMiss(t *testing.T, base, path, token string, status int, code string) {
	t.Helper()
	resp := authedGET(t, base+path, token)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status || !strings.Contains(string(body), `"`+code+`"`) {
		t.Errorf("GET %s = %d %s, want %d %s", path, resp.StatusCode, body, status, code)
	}
	if hasRetry := resp.Header.Get("Retry-After") != ""; hasRetry != (status == http.StatusAccepted) {
		t.Errorf("GET %s: Retry-After %q on a %d", path, resp.Header.Get("Retry-After"), resp.StatusCode)
	}
}

// TestAnImageMissIsTerminalWhileEnrichmentIsOff: through the real routes, a
// row waiting for the enricher answers its artist image and its cover with
// the terminal 404 no_image while enrich.enabled is off, a scanner-owned
// local- cover still answers 202 (the scanner, not the enricher, restores
// it), and once the switch is on the next request answers 202 again.
func TestAnImageMissIsTerminalWhileEnrichmentIsOff(t *testing.T) {
	hs, token, _, on := enrichmentOffFixture(t)

	wantMiss(t, hs.URL, "/v1/artist-image/"+offArtistMBID, token, http.StatusNotFound, "no_image")
	wantMiss(t, hs.URL, "/v1/artwork/"+offCoverMBID, token, http.StatusNotFound, "no_image")
	wantMiss(t, hs.URL, "/v1/artwork/"+offLocalKey, token, http.StatusAccepted, "pending")

	on.Store(true)
	wantMiss(t, hs.URL, "/v1/artist-image/"+offArtistMBID, token, http.StatusAccepted, "pending")
	wantMiss(t, hs.URL, "/v1/artwork/"+offCoverMBID, token, http.StatusAccepted, "pending")
	wantMiss(t, hs.URL, "/v1/artwork/"+offLocalKey, token, http.StatusAccepted, "pending")

	t.Run("a probe fault still answers 202", func(t *testing.T) {
		dir := t.TempDir()
		tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
		if err != nil {
			t.Fatal(err)
		}
		token, _, err := tokens.Mint("phone")
		if err != nil {
			t.Fatal(err)
		}
		probe := fakeMBIDProbe{
			known:      map[string]bool{offCoverMBID: true, offArtistMBID: true},
			pendingErr: errors.New("database is locked"),
		}
		srv := New(&config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}, tokens, nil, "fp").
			WithArtworkDirs(fakeArtworkDirs{dir: filepath.Join(dir, "artwork")}).
			WithMBIDProbe(probe).
			WithEnrichmentEnabled(func() bool { return false })
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		wantMiss(t, hs.URL, "/v1/artwork/"+offCoverMBID, token, http.StatusAccepted, "pending")
		wantMiss(t, hs.URL, "/v1/artist-image/"+offArtistMBID, token, http.StatusAccepted, "pending")
	})
}

// TestAFetchedCoverAndPortraitAreStillServedWhileOff: the switch deletes
// nothing and withholds nothing fetched before it: a cover and a portrait in
// the artwork cache answer 200 while enrich.enabled is off.
func TestAFetchedCoverAndPortraitAreStillServedWhileOff(t *testing.T) {
	hs, token, artDir, _ := enrichmentOffFixture(t)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	for _, name := range []string{offCoverMBID + "-1200.jpg", "artist-" + offArtistMBID + ".jpg"} {
		if err := os.WriteFile(filepath.Join(artDir, name), jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/v1/artwork/" + offCoverMBID, "/v1/artist-image/" + offArtistMBID} {
		resp := authedGET(t, hs.URL+path, token)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != string(jpeg) {
			t.Errorf("GET %s = %d (%d bytes) while enrichment is off, want 200 and the cached bytes",
				path, resp.StatusCode, len(body))
		}
	}
}
