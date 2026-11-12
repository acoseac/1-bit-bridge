package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The artist image of a portrait lookup the Deezer switch refused (backlog
// B761): the enricher stamped the artist's rows, so enrichment says nothing
// of it, and its portrait batch fetches it once both switches are on. Its
// miss is pending while they are, and the terminal no_image while either is
// off, since nothing fetches it then.

const (
	refusedArtistMBID = "aaaaaaaa-0761-4761-8761-000000000761"
	settledArtistMBID = "bbbbbbbb-0761-4761-8761-000000000761"
)

// refusedPortraitFixture is a server over a real store holding one enriched
// row whose portrait lookup the Deezer switch refused, and one enriched row
// of another artist whose portrait was settled with none. Both switches start
// on. Its artwork cache is the directory it returns, and the store is
// returned so a case can settle the refused row.
func refusedPortraitFixture(t *testing.T) (hs *httptest.Server, token, artDir string, store *manifest.Store, enrichmentOn, deezerOn *atomic.Bool) {
	t.Helper()
	ctx := context.Background()
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
	for _, row := range []struct {
		path, mbid string
		refused    bool
	}{
		{"Refused/Album/01.flac", refusedArtistMBID, true},
		{"Settled/Album/01.flac", settledArtistMBID, false},
	} {
		tr := manifest.Track{Path: row.path, Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album"}
		if err := store.UpsertTrack(ctx, &tr); err != nil {
			t.Fatal(err)
		}
		tr.ArtistMBID = row.mbid
		if row.refused {
			tr.PortraitPending(row.mbid, "Artist")
		} else {
			tr.PortraitSettled()
		}
		if err := store.MarkEnriched(ctx, &tr); err != nil {
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
	enrichmentOn, deezerOn = &atomic.Bool{}, &atomic.Bool{}
	enrichmentOn.Store(true)
	deezerOn.Store(true)
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	srv := New(cfg, tokens, provider, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(provider).
		WithEnrichmentEnabled(enrichmentOn.Load).
		WithDeezerEnabled(deezerOn.Load)
	hs = httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, token, artDir, store, enrichmentOn, deezerOn
}

// TestARefusedPortraitIsPendingWhileBothSwitchesAreOn: through the real
// route over the real store, the artist whose portrait lookup the switch
// refused answers 202 pending while enrichment and Deezer are both on, the
// terminal 404 no_image while either is off, and 200 once the portrait is in
// the cache; an artist settled with none answers no_image throughout. On main
// the refused artist answered no_image whatever the switches said: its rows
// were stamped.
func TestARefusedPortraitIsPendingWhileBothSwitchesAreOn(t *testing.T) {
	hs, token, artDir, _, enrichmentOn, deezerOn := refusedPortraitFixture(t)
	refused, settled := "/v1/artist-image/"+refusedArtistMBID, "/v1/artist-image/"+settledArtistMBID

	wantMiss(t, hs.URL, refused, token, http.StatusAccepted, "pending")
	wantMiss(t, hs.URL, settled, token, http.StatusNotFound, "no_image")

	deezerOn.Store(false)
	wantMiss(t, hs.URL, refused, token, http.StatusNotFound, "no_image")
	deezerOn.Store(true)
	enrichmentOn.Store(false)
	wantMiss(t, hs.URL, refused, token, http.StatusNotFound, "no_image")
	enrichmentOn.Store(true)
	wantMiss(t, hs.URL, refused, token, http.StatusAccepted, "pending")

	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	if err := os.WriteFile(filepath.Join(artDir, "artist-"+refusedArtistMBID+".jpg"), jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	resp := authedGET(t, hs.URL+refused, token)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != string(jpeg) {
		t.Errorf("GET %s = %d (%d bytes) once the portrait is cached, want 200 and its bytes", refused, resp.StatusCode, len(body))
	}
	wantMiss(t, hs.URL, settled, token, http.StatusNotFound, "no_image")
}

// TestARefusedPortraitSettledWithNoneIsTerminal: once the portrait batch
// settles the refused row with Deezer's answer that it has none, the artist's
// miss is the terminal no_image with both switches on.
func TestARefusedPortraitSettledWithNoneIsTerminal(t *testing.T) {
	hs, token, _, store, _, _ := refusedPortraitFixture(t)
	refused := "/v1/artist-image/" + refusedArtistMBID
	wantMiss(t, hs.URL, refused, token, http.StatusAccepted, "pending")

	rows, err := store.PendingPortraits(context.Background(), "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("PendingPortraits = %+v (%v), want the refused row", rows, err)
	}
	if n, err := store.ClearPendingPortraits(context.Background(), rows); err != nil || n != 1 {
		t.Fatalf("ClearPendingPortraits = %d (%v)", n, err)
	}
	wantMiss(t, hs.URL, refused, token, http.StatusNotFound, "no_image")
}

// TestTheArtistImageAnswersEachPortraitStateAsTheProtocolSays: through the
// real route over the real store, an artist whose portrait lookup waits
// answers 202 pending, with its Retry-After, while the name it waits by has
// failed fewer than manifest.PortraitPendingAttempts times, and the 404
// no_image, with none, at that bound, though the row still waits for the
// bridge's own retries; another name of the artist below the bound answers
// 202 again, and once an answer ends that name too, no_image. On main a
// lookup that kept failing answered 202 for good.
func TestTheArtistImageAnswersEachPortraitStateAsTheProtocolSays(t *testing.T) {
	hs, token, _, store, _, _ := refusedPortraitFixture(t)
	ctx := context.Background()
	refused := "/v1/artist-image/" + refusedArtistMBID
	wantMiss(t, hs.URL, refused, token, http.StatusAccepted, "pending")

	at := time.Unix(1_800_000_000, 0)
	for i := 1; i < manifest.PortraitPendingAttempts; i++ {
		if _, err := store.NotePortraitFailure(ctx, refusedArtistMBID, "Artist", at); err != nil {
			t.Fatal(err)
		}
	}
	wantMiss(t, hs.URL, refused, token, http.StatusAccepted, "pending")
	if _, err := store.NotePortraitFailure(ctx, refusedArtistMBID, "Artist", at); err != nil {
		t.Fatal(err)
	}
	wantMiss(t, hs.URL, refused, token, http.StatusNotFound, "no_image")
	if rows, err := store.PendingPortraits(ctx, "", 10); err != nil || len(rows) != 1 {
		t.Fatalf("PendingPortraits = %+v (%v), want the row still waiting for the bridge's own retries", rows, err)
	}

	other := manifest.Track{Path: "Refused/Album/02.flac", Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album"}
	if err := store.UpsertTrack(ctx, &other); err != nil {
		t.Fatal(err)
	}
	other.ArtistMBID = refusedArtistMBID
	other.PortraitPending(refusedArtistMBID, "Another Name")
	if err := store.MarkEnriched(ctx, &other); err != nil {
		t.Fatal(err)
	}
	wantMiss(t, hs.URL, refused, token, http.StatusAccepted, "pending")

	rows, err := store.PendingPortraits(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var answered []manifest.PendingPortrait
	for _, r := range rows {
		if r.Query == "Another Name" {
			answered = append(answered, r)
		}
	}
	if n, err := store.ClearPendingPortraits(ctx, answered); err != nil || n != 1 {
		t.Fatalf("ClearPendingPortraits = %d (%v), want the other name's row", n, err)
	}
	wantMiss(t, hs.URL, refused, token, http.StatusNotFound, "no_image")
}

// TestARefusedPortraitsProbeFaultIsPending: a fault of the portrait probe is
// no answer, so the miss fails open to 202, as the enrichment probe's does.
func TestARefusedPortraitsProbeFaultIsPending(t *testing.T) {
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
		known:       map[string]bool{refusedArtistMBID: true},
		portraitErr: errors.New("database is locked"),
	}
	srv := New(&config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}, tokens, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: filepath.Join(dir, "artwork")}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	wantMiss(t, hs.URL, "/v1/artist-image/"+refusedArtistMBID, token, http.StatusAccepted, "pending")
}
