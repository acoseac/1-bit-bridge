package api

// The client format gate through the real router (backlog B489): a request
// that declares no format in X-Bridge-Formats is served no WavPack or
// Monkey's Audio row, tombstone or count on any catalog surface (the
// manifest full, paged and delta, /v1/health, search, the smart playlists,
// the playlist files), and one that declares them is; each such response
// varies on the header; a page cursor continues only its own view; and the
// documents a device writes keep a .wv entry for every client. The library
// is scanned for real, from stub files: the .wv and .ape files are files
// their readers refuse, indexed by name with no codec, so every row here is
// held back by its extension alone.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// The gate fixture's library: one track of every kind, each titled so one
// search finds them all, and a playlist file naming three of them.
var gateLibrary = []string{
	"Artist/Album/01 Gate One.flac",
	"Artist/Album/02 Gate Two.wv",
	"Artist/Album/03 Gate Three.ape",
	"Artist/Album/04 Gate Four.WV",
	"Other/05 Gate Five.mp3",
}

// gatedPaths are the library's WavPack and Monkey's Audio rows.
var gatedPaths = []string{"Artist/Album/02 Gate Two.wv", "Artist/Album/03 Gate Three.ape", "Artist/Album/04 Gate Four.WV"}

// formatsFixture is a scanned library behind the real router: its store, the
// server's address and a bearer token.
type formatsFixture struct {
	store *manifest.Store
	hs    *httptest.Server
	token string
	lib   string
}

func newFormatsFixture(t *testing.T) *formatsFixture {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	for _, rel := range gateLibrary {
		writeGateFile(t, lib, rel, "not a real audio file")
	}
	// A playlist naming a playable track, a WavPack one and a Monkey's
	// Audio one.
	writeGateFile(t, lib, "Playlists/Mix.m3u8",
		"#EXTM3U\n../Artist/Album/01 Gate One.flac\n../Artist/Album/02 Gate Two.wv\n../Artist/Album/03 Gate Three.ape\n")
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scanner := manifest.NewScanner([]string{lib}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("gate")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	// These tests ask for many manifests in a row: the per-token limiter
	// would answer 429 past its burst.
	cfg.Limits.Manifest.RequestsPerMinute = intPtr(0)
	srv := New(cfg, tokens, manifest.NewProvider(store, scanner), "fp").
		WithPlaylistFileStore(store).
		WithSmartPlaylistStore(store).
		WithDeviceRegistrar(store).
		WithPlaylistStore(store).
		WithFavoritesStore(store)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &formatsFixture{store: store, hs: hs, token: raw, lib: lib}
}

func writeGateFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// get sends GET target with the fixture's token and each value as an
// X-Bridge-Formats line, and returns the answer and its body.
func (f *formatsFixture) get(t *testing.T, target string, formats ...string) (*http.Response, []byte) {
	t.Helper()
	return f.getWith(t, target, nil, formats...)
}

// getWith is get with extra request headers.
func (f *formatsFixture) getWith(t *testing.T, target string, extra map[string]string, formats ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.hs.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Accept-Encoding", "identity")
	for _, v := range formats {
		req.Header.Add("X-Bridge-Formats", v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// manifestOf decodes a manifest answer.
func manifestOf(t *testing.T, body []byte) ManifestPage {
	t.Helper()
	var m ManifestPage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, body)
	}
	return m
}

func trackPaths(tracks []manifest.Track) []string {
	out := make([]string, 0, len(tracks))
	for _, tr := range tracks {
		out = append(out, tr.Path)
	}
	slices.Sort(out)
	return out
}

// requireVaryOnFormats asserts that resp varies on the declared formats.
func requireVaryOnFormats(t *testing.T, label string, resp *http.Response) {
	t.Helper()
	if !slices.ContainsFunc(varyTokens(resp.Header), func(s string) bool { return strings.EqualFold(s, "X-Bridge-Formats") }) {
		t.Errorf("%s: Vary %q, want X-Bridge-Formats among its tokens", label, resp.Header.Values("Vary"))
	}
}

// gateView is a view the tests ask in: the X-Bridge-Formats lines it sends,
// and which of the library's paths it takes.
type gateView struct {
	name    string
	formats []string
	takes   func(p string) bool
}

var gateViews = []gateView{
	{"no header", nil, func(p string) bool { return !slices.Contains(gatedPaths, p) }},
	{"a token the bridge does not know", []string{"flac, dsd"}, func(p string) bool { return !slices.Contains(gatedPaths, p) }},
	{"wavpack", []string{"wavpack"}, func(p string) bool { return !strings.HasSuffix(p, ".ape") }},
	{"APE in capitals, padded", []string{" APE "}, func(p string) bool { return !strings.EqualFold(filepath.Ext(p), ".wv") }},
	{"both, on one line", []string{"wavpack, ape"}, func(string) bool { return true }},
	{"both, on two lines", []string{"Ape", "WavPack"}, func(string) bool { return true }},
}

// want is the library's paths v takes, sorted.
func (v gateView) want() []string {
	var out []string
	for _, p := range gateLibrary {
		if v.takes(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// TestPendingDeletionsAreEachViewsRows: health's pendingDeletions counts the
// rows a view is served, as tracksIndexed does (round 2, Claude's review:
// PROTOCOL.md says no count of a format a client does not declare reaches
// it). A WavPack file gone from the share is no pending deletion to an app
// that was never served its row.
func TestPendingDeletionsAreEachViewsRows(t *testing.T) {
	f := newFormatsFixture(t)
	gone := []string{"Artist/Album/01 Gate One.flac", "Artist/Album/02 Gate Two.wv"}
	for _, rel := range gone {
		if err := os.Remove(filepath.Join(f.lib, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	scanner := manifest.NewScanner([]string{f.lib}, f.store, "")
	scanner.SetDeleteThreshold(3)
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			want := int64(0)
			for _, rel := range gone {
				if v.takes(rel) {
					want++
				}
			}
			_, body := f.get(t, "/v1/health", v.formats...)
			var h HealthResponse
			if err := json.Unmarshal(body, &h); err != nil {
				t.Fatal(err)
			}
			if h.ScanState == nil || h.ScanState.PendingDeletions != want {
				t.Errorf("pendingDeletions %+v, want %d", h.ScanState, want)
			}
		})
	}
}

// TestTheManifestServesEachViewItsRows: the full manifest, streamed, holds a
// view's rows and its counts (enrichmentProgress), its folders are every
// view's, and it varies on the header; the health count is the view's.
func TestTheManifestServesEachViewItsRows(t *testing.T) {
	f := newFormatsFixture(t)
	var folders []string
	for i, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			resp, body := f.get(t, "/v1/manifest", v.formats...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			requireVaryOnFormats(t, "manifest", resp)
			m := manifestOf(t, body)
			if got, want := trackPaths(m.Tracks), v.want(); !slices.Equal(got, want) {
				t.Errorf("tracks %q, want %q", got, want)
			}
			if m.EnrichmentProgress == nil || m.EnrichmentProgress.TracksTotal != len(v.want()) {
				t.Errorf("enrichmentProgress %+v, want tracksTotal %d", m.EnrichmentProgress, len(v.want()))
			}
			var fs []string
			for _, fo := range m.Folders {
				fs = append(fs, fo.Path)
			}
			if i == 0 {
				folders = fs
			} else if !slices.Equal(fs, folders) {
				t.Errorf("folders %q, want every view's %q", fs, folders)
			}

			hresp, hbody := f.get(t, "/v1/health", v.formats...)
			requireVaryOnFormats(t, "health", hresp)
			var h HealthResponse
			if err := json.Unmarshal(hbody, &h); err != nil {
				t.Fatal(err)
			}
			if h.ScanState == nil || h.ScanState.TracksIndexed != len(v.want()) {
				t.Errorf("tracksIndexed %+v, want %d", h.ScanState, len(v.want()))
			}
			if !slices.Contains(h.Features, "wavpackApe") {
				t.Errorf("features %q lack wavpackApe", h.Features)
			}
		})
	}
	if !slices.Contains(folders, "Artist/Album") {
		t.Errorf("folders %q, want the album's folder listed to every view", folders)
	}
}

// TestEachViewPagesItsOwnRowsAndACursorContinuesOnlyItsView: a paged walk
// collects exactly a view's rows, the first page's total is the view's, and a
// cursor presented with another view is a 410 manifest_cursor_view, never a
// page that would skip the rows the other view did not deliver.
func TestEachViewPagesItsOwnRowsAndACursorContinuesOnlyItsView(t *testing.T) {
	f := newFormatsFixture(t)
	cursors := map[string]string{}
	for _, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			var got []string
			cursor := ""
			for page := 0; ; page++ {
				target := "/v1/manifest?limit=1"
				if cursor != "" {
					target += "&cursor=" + urlquery.Escape(cursor)
				}
				resp, body := f.get(t, target, v.formats...)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("page %d: status %d: %s", page, resp.StatusCode, body)
				}
				requireVaryOnFormats(t, "page", resp)
				m := manifestOf(t, body)
				if page == 0 && (m.Total == nil || *m.Total != len(v.want())) {
					t.Errorf("total %v, want %d", m.Total, len(v.want()))
				}
				got = append(got, trackPaths(m.Tracks)...)
				if m.NextCursor == nil {
					break
				}
				cursor = *m.NextCursor
				if page == 0 {
					cursors[v.name] = cursor
				}
			}
			slices.Sort(got)
			if want := v.want(); !slices.Equal(got, want) {
				t.Errorf("pages hold %q, want %q", got, want)
			}
		})
	}
	for _, c := range []struct{ cursorOf, askedIn string }{
		{"no header", "wavpack"},
		{"wavpack", "no header"},
		{"wavpack", "both, on one line"},
		{"both, on one line", "APE in capitals, padded"},
	} {
		var formats []string
		for _, v := range gateViews {
			if v.name == c.askedIn {
				formats = v.formats
			}
		}
		resp, body := f.get(t, "/v1/manifest?limit=1&cursor="+urlquery.Escape(cursors[c.cursorOf]), formats...)
		var er ErrorResponse
		_ = json.Unmarshal(body, &er)
		if resp.StatusCode != http.StatusGone || er.Error != "manifest_cursor_view" {
			t.Errorf("a %s cursor asked in %s: status %d %q, want 410 manifest_cursor_view", c.cursorOf, c.askedIn, resp.StatusCode, er.Error)
		}
	}
	// The same view's own cursor continues, with the header spelled another
	// way: the view is the set of formats, not the header's bytes.
	if resp, _ := f.get(t, "/v1/manifest?limit=1&cursor="+urlquery.Escape(cursors["both, on one line"]), "APE,WAVPACK"); resp.StatusCode != http.StatusOK {
		t.Errorf("a view's own cursor with its header respelled: status %d, want 200", resp.StatusCode)
	}
}

// TestADeltaServesEachViewItsRowsAndTombstones: a delta holds a view's
// changed rows and its tombstones alone, the deleted .wv and a refused file
// included only where the view takes them.
func TestADeltaServesEachViewItsRowsAndTombstones(t *testing.T) {
	f := newFormatsFixture(t)
	ctx := context.Background()
	// Rows enough that three deletions are a deletion list, never the
	// journal's mass delete (a quarter of the library), which resets its
	// coverage and answers deltaIncomplete instead.
	var filler []*manifest.Track
	for i := 0; i < 20; i++ {
		filler = append(filler, &manifest.Track{Path: "Filler/" + strconv.Itoa(i) + ".flac", Size: 1, ModTime: time.Unix(1, 0).UTC()})
	}
	if err := f.store.UpsertTrackBatch(ctx, filler); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	gone := []string{"Artist/Album/01 Gate One.flac", "Artist/Album/02 Gate Two.wv", "Artist/Album/04 Gate Four.WV"}
	if err := f.store.DeleteTracksBatch(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertTrack(ctx, &manifest.Track{Path: "Artist/Album/03 Gate Three.ape", Size: 2, ModTime: since, Title: "Retagged"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertTrack(ctx, &manifest.Track{Path: "Other/05 Gate Five.mp3", Size: 2, ModTime: since, Title: "Retagged"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			resp, body := f.get(t, "/v1/manifest?since="+urlquery.Escape(since.Format(time.RFC3339Nano)), v.formats...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			requireVaryOnFormats(t, "delta", resp)
			m := manifestOf(t, body)
			var wantTracks, wantDeleted []string
			for _, p := range []string{"Artist/Album/03 Gate Three.ape", "Other/05 Gate Five.mp3"} {
				if v.takes(p) {
					wantTracks = append(wantTracks, p)
				}
			}
			for _, p := range gone {
				if v.takes(p) {
					wantDeleted = append(wantDeleted, p)
				}
			}
			slices.Sort(wantDeleted)
			if got := trackPaths(m.Tracks); !slices.Equal(got, wantTracks) {
				t.Errorf("tracks %q, want %q", got, wantTracks)
			}
			got := append([]string(nil), m.Deleted...)
			slices.Sort(got)
			if !slices.Equal(got, wantDeleted) || m.DeltaIncomplete {
				t.Errorf("deleted %q (incomplete %v), want %q", got, m.DeltaIncomplete, wantDeleted)
			}
		})
	}
}

// TestSearchServesEachViewItsHits: a held-back track is no hit.
func TestSearchServesEachViewItsHits(t *testing.T) {
	f := newFormatsFixture(t)
	if ok, _ := f.store.SearchAvailable(context.Background()); !ok {
		t.Skip("FTS5 not compiled into this driver build")
	}
	for _, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			resp, body := f.get(t, "/v1/search?q=Gate&limit=100", v.formats...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			requireVaryOnFormats(t, "search", resp)
			var sr searchResponse
			if err := json.Unmarshal(body, &sr); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range sr.Tracks {
				got = append(got, h.Path)
			}
			slices.Sort(got)
			if want := v.want(); !slices.Equal(got, want) {
				t.Errorf("hits %q, want %q", got, want)
			}
		})
	}
}

// TestSmartPlaylistsServeEachViewItsItems: an item held back is left out and
// the positions close up, and a family the gate empties is not sent.
func TestSmartPlaylistsServeEachViewItsItems(t *testing.T) {
	f := newFormatsFixture(t)
	items := func(paths ...string) []byte {
		var its []manifest.SmartPlaylistItem
		for i, p := range paths {
			its = append(its, manifest.SmartPlaylistItem{Position: i, Path: p})
		}
		raw, err := json.Marshal(its)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if err := f.store.ReplaceSmartPlaylists(context.Background(), []manifest.StoredSmartPlaylist{
		{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Position: 0, RefreshedAt: 1,
			ItemsJSON: items("Artist/Album/02 Gate Two.wv", "Artist/Album/01 Gate One.flac", "Artist/Album/03 Gate Three.ape", "Other/05 Gate Five.mp3")},
		{Slug: "wavpack-only", Kind: "recentlyPlayed", Title: "Recent", Position: 1, RefreshedAt: 1,
			ItemsJSON: items("Artist/Album/02 Gate Two.wv", "Artist/Album/04 Gate Four.WV")},
	}); err != nil {
		t.Fatal(err)
	}
	for _, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			resp, body := f.get(t, "/v1/smart-playlists", v.formats...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			requireVaryOnFormats(t, "smart playlists", resp)
			var sp smartPlaylistsResponse
			if err := json.Unmarshal(body, &sp); err != nil {
				t.Fatal(err)
			}
			got := map[string][]string{}
			for _, p := range sp.Playlists {
				for i, it := range p.Items {
					if it.Position != i {
						t.Errorf("%s: item %d at position %d, want the positions closed up", p.Slug, i, it.Position)
					}
					got[p.Slug] = append(got[p.Slug], it.Path)
				}
			}
			var wantHeavy []string
			for _, p := range []string{"Artist/Album/02 Gate Two.wv", "Artist/Album/01 Gate One.flac", "Artist/Album/03 Gate Three.ape", "Other/05 Gate Five.mp3"} {
				if v.takes(p) {
					wantHeavy = append(wantHeavy, p)
				}
			}
			if !slices.Equal(got["heavy-rotation"], wantHeavy) {
				t.Errorf("heavy-rotation %q, want %q", got["heavy-rotation"], wantHeavy)
			}
			_, sent := got["wavpack-only"]
			if wantSent := v.takes("Artist/Album/02 Gate Two.wv"); sent != wantSent {
				t.Errorf("the family of WavPack tracks alone sent %v, want %v", sent, wantSent)
			}
		})
	}
}

// TestThePlaylistFilesServeEachViewItsMembers: a line naming a held-back
// track is unresolved, the response varies on the header, a cursor of one
// view is stale in another, and a page's ETag from one view does not answer
// 304 to another.
func TestThePlaylistFilesServeEachViewItsMembers(t *testing.T) {
	f := newFormatsFixture(t)
	etags := map[string]string{}
	for _, v := range gateViews {
		t.Run(v.name, func(t *testing.T) {
			resp, body := f.get(t, "/v1/playlist-files", v.formats...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			requireVaryOnFormats(t, "playlist files", resp)
			var pf playlistFilesResponse
			if err := json.Unmarshal(body, &pf); err != nil {
				t.Fatal(err)
			}
			if len(pf.Playlists) != 1 {
				t.Fatalf("playlists %+v, want the one file", pf.Playlists)
			}
			var wantMembers, wantUnresolved []string
			for _, p := range []string{"Artist/Album/01 Gate One.flac", "Artist/Album/02 Gate Two.wv", "Artist/Album/03 Gate Three.ape"} {
				if v.takes(p) {
					wantMembers = append(wantMembers, p)
				} else {
					wantUnresolved = append(wantUnresolved, "../"+p)
				}
			}
			got := pf.Playlists[0]
			if !slices.Equal(got.Members, wantMembers) || !slices.Equal(got.Unresolved, nonNilStrings(wantUnresolved)) {
				t.Errorf("members %q unresolved %q, want %q and %q", got.Members, got.Unresolved, wantMembers, wantUnresolved)
			}
			etags[v.name] = resp.Header.Get("ETag")
			// Its own ETag is a 304.
			if again, _ := f.getWith(t, "/v1/playlist-files", map[string]string{"If-None-Match": etags[v.name]}, v.formats...); again.StatusCode != http.StatusNotModified {
				t.Errorf("its own ETag: status %d, want 304", again.StatusCode)
			}
		})
	}
	// Another view's ETag is never a 304, though the views may hold the
	// same members (their versions differ).
	for _, c := range []struct{ etagOf, askedIn string }{
		{"wavpack", "no header"}, {"no header", "both, on one line"}, {"a token the bridge does not know", "APE in capitals, padded"},
	} {
		var formats []string
		for _, v := range gateViews {
			if v.name == c.askedIn {
				formats = v.formats
			}
		}
		resp, _ := f.getWith(t, "/v1/playlist-files", map[string]string{"If-None-Match": etags[c.etagOf]}, formats...)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("a %s ETag asked in %s: status %d, want 200", c.etagOf, c.askedIn, resp.StatusCode)
		}
	}
	// "no header" and "a token the bridge does not know" are one view: one
	// version, so one ETag.
	if etags["no header"] != etags["a token the bridge does not know"] {
		t.Errorf("two spellings of the zero view answered ETags %q and %q, want one", etags["no header"], etags["a token the bridge does not know"])
	}
}

// TestADevicesDocumentsKeepAWavPackEntryForEveryClient: the playlists and the
// favorites a device writes are stored and returned as written, whatever the
// reading client declares, so a client that plays neither format reads a .wv
// entry, edits the document and writes it back with the entry kept.
func TestADevicesDocumentsKeepAWavPackEntryForEveryClient(t *testing.T) {
	f := newFormatsFixture(t)
	const dev = "deadbeef"
	put := func(target, body string, formats ...string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, f.hs.URL+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("X-Device-Token", dev)
		for _, v := range formats {
			req.Header.Add("X-Bridge-Formats", v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	const id = "5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a"
	const wv = "Artist/Album/02 Gate Two.wv"
	// A client that plays WavPack writes the playlist.
	if resp := put("/v1/playlists/"+id, `{"id":"`+id+`","name":"Mixed","lastModifiedAt":100,"items":[{"position":0,"path":"`+wv+`"},{"position":1,"path":"Artist/Album/01 Gate One.flac"}]}`, "wavpack"); resp.StatusCode != http.StatusOK {
		t.Fatalf("put from the declaring client: %d", resp.StatusCode)
	}
	// A client that plays neither reads it, entry included...
	resp, body := f.getWith(t, "/v1/playlists/"+id, map[string]string{"X-Device-Token": dev})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), wv) {
		t.Fatalf("get from the undeclaring client: %d %s, want the .wv entry", resp.StatusCode, body)
	}
	// ...renames it and writes back what it read.
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	doc["name"] = "Renamed"
	doc["baseLastModifiedAt"] = doc["lastModifiedAt"]
	doc["lastModifiedAt"] = 200
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if resp := put("/v1/playlists/"+id, string(edited)); resp.StatusCode != http.StatusOK {
		t.Fatalf("put back from the undeclaring client: %d", resp.StatusCode)
	}
	for _, formats := range [][]string{{"wavpack"}, nil} {
		resp, body := f.getWith(t, "/v1/playlists/"+id, map[string]string{"X-Device-Token": dev}, formats...)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), wv) || !strings.Contains(string(body), "Renamed") {
			t.Errorf("get declaring %q after the round trip: %d %s, want the renamed playlist with its .wv entry", formats, resp.StatusCode, body)
		}
	}

	// The favorites document, the same way, through its revisions.
	if resp := put("/v1/favorites", `{"lastModifiedAt":1,"baseRevision":0,"tracks":[{"path":"`+wv+`","favoritedAt":1}],"albums":[]}`, "wavpack"); resp.StatusCode != http.StatusOK {
		t.Fatalf("favorites put from the declaring client: %d", resp.StatusCode)
	}
	resp, body = f.getWith(t, "/v1/favorites", map[string]string{"X-Device-Token": dev})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), wv) {
		t.Fatalf("favorites get from the undeclaring client: %d %s", resp.StatusCode, body)
	}
	var fav struct {
		Revision int64             `json:"revision"`
		Tracks   []json.RawMessage `json:"tracks"`
	}
	if err := json.Unmarshal(body, &fav); err != nil {
		t.Fatal(err)
	}
	tracks := append(fav.Tracks, json.RawMessage(`{"path":"Artist/Album/01 Gate One.flac","favoritedAt":2}`))
	rawTracks, err := json.Marshal(tracks)
	if err != nil {
		t.Fatal(err)
	}
	back := `{"lastModifiedAt":2,"baseRevision":` + strconv.FormatInt(fav.Revision, 10) + `,"tracks":` + string(rawTracks) + `,"albums":[]}`
	if resp := put("/v1/favorites", back); resp.StatusCode != http.StatusOK {
		t.Fatalf("favorites put back from the undeclaring client: %d", resp.StatusCode)
	}
	resp, body = f.getWith(t, "/v1/favorites", map[string]string{"X-Device-Token": dev}, "wavpack")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), wv) || !strings.Contains(string(body), "01 Gate One.flac") {
		t.Errorf("favorites after the round trip: %d %s, want both entries", resp.StatusCode, body)
	}
}

// TestTheFilesystemViewsAreNotHeldBack: /v1/list, /v1/stat and /v1/download
// answer for a .wv path whatever the request declares.
func TestTheFilesystemViewsAreNotHeldBack(t *testing.T) {
	f := newFormatsFixture(t)
	for _, target := range []string{
		"/v1/list?path=" + urlquery.Escape("Artist/Album"),
		"/v1/stat?path=" + urlquery.Escape("Artist/Album/02 Gate Two.wv"),
		"/v1/download?path=" + urlquery.Escape("Artist/Album/02 Gate Two.wv"),
	} {
		resp, body := f.get(t, target)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s with no header: status %d: %s", target, resp.StatusCode, body)
		}
		if strings.HasPrefix(target, "/v1/list") && !strings.Contains(string(body), "02 Gate Two.wv") {
			t.Errorf("%s: %s, want the .wv file listed", target, body)
		}
		if strings.HasPrefix(target, "/v1/download") && string(body) != "not a real audio file" {
			t.Errorf("%s: %q, want the file's bytes", target, body)
		}
	}
}
