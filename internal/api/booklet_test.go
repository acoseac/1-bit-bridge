package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

type fakeBookletStore struct {
	rows map[string]*manifest.BookletRow
}

func (f fakeBookletStore) GetBooklet(_ context.Context, mbid string) (*manifest.BookletRow, error) {
	return f.rows[mbid], nil
}

// bookletFixture builds a server with the booklet surface wired: one
// available+cached release, one available-but-not-downloaded, and one
// checked-unavailable. Returns server, token, cache dir, and the nudge log.
func bookletFixture(t *testing.T) (*httptest.Server, string, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	bkDir := filepath.Join(dir, "booklets")
	if err := os.MkdirAll(bkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BookletPath(bkDir, bookletCachedMBID), []byte("%PDF-1.4 fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	var nudged []string
	srv := New(cfg, store, nil, "fp").WithBooklets(fakeBookletStore{rows: map[string]*manifest.BookletRow{
		bookletCachedMBID:  {ReleaseMBID: bookletCachedMBID, Available: true, Etag: "tag-1"},
		bookletPendingMBID: {ReleaseMBID: bookletPendingMBID, Available: true, Etag: "tag-2"},
		bookletMissMBID:    {ReleaseMBID: bookletMissMBID, Available: false},
	}}, bkDir, func(mbid string) { nudged = append(nudged, mbid) })
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, bkDir, &nudged
}

const (
	bookletCachedMBID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	bookletPendingMBID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	bookletMissMBID    = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

func TestBookletServesCachedPDF(t *testing.T) {
	hs, tok, _, _ := bookletFixture(t)
	resp := authedGET(t, hs.URL+"/v1/booklet/"+bookletCachedMBID, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content-type = %q", ct)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("accept-ranges = %q, want bytes (ServeContent range support)", ar)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "%PDF-1.4 fixture" {
		t.Errorf("body = %q", body)
	}
}

func TestBookletPendingReturns202AndNudges(t *testing.T) {
	hs, tok, _, nudged := bookletFixture(t)
	resp := authedGET(t, hs.URL+"/v1/booklet/"+bookletPendingMBID, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Errorf("retry-after = %q, want 30", ra)
	}
	if len(*nudged) != 1 || (*nudged)[0] != bookletPendingMBID {
		t.Errorf("nudge log = %v, want the pending MBID prioritized", *nudged)
	}
}

func TestBookletUnavailableAndUnknownReturn404(t *testing.T) {
	hs, tok, _, _ := bookletFixture(t)
	for _, mbid := range []string{bookletMissMBID, "dddddddd-dddd-4ddd-8ddd-dddddddddddd"} {
		resp := authedGET(t, hs.URL+"/v1/booklet/"+mbid, tok)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", mbid, resp.StatusCode)
		}
	}
}

func TestBookletRejectsBadMBID(t *testing.T) {
	hs, tok, _, _ := bookletFixture(t)
	resp := authedGET(t, hs.URL+"/v1/booklet/not-a-uuid", tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBookletRequiresAuth(t *testing.T) {
	hs, _, _, _ := bookletFixture(t)
	resp, err := http.Get(hs.URL + "/v1/booklet/" + bookletCachedMBID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHealthAdvertisesBookletsFeature pins the flag: present iff the booklet
// surface is wired, and alpha-sorted in the features list.
func TestHealthAdvertisesBookletsFeature(t *testing.T) {
	hs, tok, _, _ := bookletFixture(t)
	resp := authedGET(t, hs.URL+"/v1/health", tok)
	defer resp.Body.Close()
	var health struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	found := false
	sorted := true
	for i, f := range health.Features {
		if f == "booklets" {
			found = true
		}
		if i > 0 && health.Features[i-1] > f {
			sorted = false
		}
	}
	if !found {
		t.Errorf("features = %v, want booklets present", health.Features)
	}
	if !sorted {
		t.Errorf("features not alpha-sorted: %v", health.Features)
	}
}
