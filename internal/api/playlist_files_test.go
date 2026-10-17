package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// GET /v1/playlist-files (backlog B431): the shape, the ETag and 304, the
// health flag, auth, and the error paths.

// fakePlaylistFiles is a PlaylistFileStore whose answer a test sets, and
// can change between requests.
type fakePlaylistFiles struct {
	mu    sync.Mutex
	files []manifest.PlaylistFile
	err   error
	// block, when set, is closed by the read once it has started; the read
	// then waits for its context and fails with it.
	block chan struct{}
}

func (f *fakePlaylistFiles) PlaylistFiles(ctx context.Context) ([]manifest.PlaylistFile, error) {
	f.mu.Lock()
	files, err, block := f.files, f.err, f.block
	f.mu.Unlock()
	if block != nil {
		close(block)
		<-ctx.Done()
		return nil, fmt.Errorf("read playlist files: %w", ctx.Err())
	}
	return files, err
}

func (f *fakePlaylistFiles) set(files []manifest.PlaylistFile) {
	f.mu.Lock()
	f.files = files
	f.mu.Unlock()
}

// newPlaylistFilesServer is a Server with st as its playlist-file store (none
// when st is nil), behind the real router, and a bearer token for it.
func newPlaylistFilesServer(t *testing.T, st PlaylistFileStore) (*httptest.Server, string) {
	t.Helper()
	router, token := newPlaylistFilesRouter(t, st)
	hs := httptest.NewServer(router)
	t.Cleanup(hs.Close)
	return hs, token
}

// newPlaylistFilesRouter is newPlaylistFilesServer's router, for a test
// that wraps it.
func newPlaylistFilesRouter(t *testing.T, st PlaylistFileStore) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("playlist-files")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	srv := New(cfg, tokens, nil, "fp")
	if st != nil {
		srv.WithPlaylistFileStore(st)
	}
	return srv.Handler(), raw
}

// getPlaylistFiles sends GET /v1/playlist-files with token and each
// If-None-Match line, and returns the answer and its body.
func getPlaylistFiles(t *testing.T, hs *httptest.Server, token string, ifNoneMatch ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/playlist-files", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, v := range ifNoneMatch {
		req.Header.Add("If-None-Match", v)
	}
	// Plain JSON: the transport's own gzip would hide the encoding.
	req.Header.Set("Accept-Encoding", "identity")
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

var twoPlaylistFiles = []manifest.PlaylistFile{
	{
		Path: "Playlists/Road Trip.m3u8", Name: "Road Trip",
		ModTime:    time.Date(2026, 10, 16, 9, 12, 44, 123456789, time.UTC),
		Members:    []string{"Artist/Album/01 One.flac", "Artist/Album/01 One.flac"},
		Unresolved: []string{"../Missing/03.flac"},
	},
	{
		Path: "Playlists/All.m3u", Name: "All",
		ModTime:  time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("CEST", 2*3600)),
		TooLarge: true,
	},
}

var strongETag = regexp.MustCompile(`^"[0-9a-f]{64}"$`)

func TestPlaylistFilesServesTheFeedWithAStrongETag(t *testing.T) {
	hs, token := newPlaylistFilesServer(t, &fakePlaylistFiles{files: twoPlaylistFiles})
	resp, body := getPlaylistFiles(t, hs, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
	if et := resp.Header.Get("ETag"); !strongETag.MatchString(et) {
		t.Errorf("ETag %q, want a quoted SHA-256", et)
	}
	// The exact document: field names, array members never null, tooLarge
	// only when true, mtime in UTC to the nanosecond.
	want := `{"playlists":[` +
		`{"path":"Playlists/Road Trip.m3u8","name":"Road Trip","mtime":"2026-10-16T09:12:44.123456789Z",` +
		`"members":["Artist/Album/01 One.flac","Artist/Album/01 One.flac"],"unresolved":["../Missing/03.flac"]},` +
		`{"path":"Playlists/All.m3u","name":"All","mtime":"2026-09-30T22:00:00Z","members":[],"unresolved":[],"tooLarge":true}` +
		`]}`
	if got := strings.TrimSpace(string(body)); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

func TestPlaylistFilesAnswers304ToAMatchingETag(t *testing.T) {
	hs, token := newPlaylistFilesServer(t, &fakePlaylistFiles{files: twoPlaylistFiles})
	first, _ := getPlaylistFiles(t, hs, token)
	etag := first.Header.Get("ETag")
	for _, inm := range [][]string{
		{etag},
		{"W/" + etag},
		{`"0000", ` + etag},
		{`"0000"`, etag}, // several header lines are one list
		{"*"},
	} {
		resp, body := getPlaylistFiles(t, hs, token, inm...)
		if resp.StatusCode != http.StatusNotModified || len(body) != 0 || resp.Header.Get("ETag") != etag {
			t.Errorf("If-None-Match %q: status %d, %d body bytes, ETag %q; want 304, empty, %s",
				inm, resp.StatusCode, len(body), resp.Header.Get("ETag"), etag)
		}
	}
	if resp, _ := getPlaylistFiles(t, hs, token, `"0000"`); resp.StatusCode != http.StatusOK {
		t.Errorf("a stale ETag: status %d, want 200", resp.StatusCode)
	}
}

func TestPlaylistFilesETagMovesWithTheFeed(t *testing.T) {
	fake := &fakePlaylistFiles{files: twoPlaylistFiles}
	hs, token := newPlaylistFilesServer(t, fake)
	first, _ := getPlaylistFiles(t, hs, token)
	resolved := append([]manifest.PlaylistFile(nil), twoPlaylistFiles...)
	resolved[0].Members = append([]string{}, resolved[0].Members...)
	resolved[0].Members = append(resolved[0].Members, "Missing/03.flac")
	resolved[0].Unresolved = []string{}
	fake.set(resolved)
	resp, body := getPlaylistFiles(t, hs, token, first.Header.Get("ETag"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == first.Header.Get("ETag") {
		t.Fatalf("after a member resolved: status %d, ETag %q (was %q)", resp.StatusCode,
			resp.Header.Get("ETag"), first.Header.Get("ETag"))
	}
	if !strings.Contains(string(body), `"Missing/03.flac"`) {
		t.Errorf("body %s", body)
	}
}

func TestPlaylistFilesNeedsABearerToken(t *testing.T) {
	hs, _ := newPlaylistFilesServer(t, &fakePlaylistFiles{files: twoPlaylistFiles})
	for _, token := range []string{"", "not-a-token"} {
		resp, body := getPlaylistFiles(t, hs, token)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"unauthorized"`) {
			t.Errorf("token %q: status %d %s, want 401 unauthorized", token, resp.StatusCode, body)
		}
	}
}

func TestPlaylistFilesWithoutTheIndexIs404AndUnadvertised(t *testing.T) {
	hs, token := newPlaylistFilesServer(t, nil)
	resp, body := getPlaylistFiles(t, hs, token)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `"playlist_files_not_supported"`) {
		t.Errorf("status %d %s, want 404 playlist_files_not_supported", resp.StatusCode, body)
	}
	for _, f := range healthOf(t, hs).Features {
		if f == "playlistFiles" {
			t.Error("health advertises playlistFiles with no index wired")
		}
	}
	wired, _ := newPlaylistFilesServer(t, &fakePlaylistFiles{})
	feats := healthOf(t, wired).Features
	found := false
	for _, f := range feats {
		found = found || f == "playlistFiles"
	}
	if !found {
		t.Errorf("health does not advertise playlistFiles with the index wired: %v", feats)
	}
	assertAlphaSorted(t, feats)
}

func TestAPlaylistFilesStoreFailureAnswersAFixedMessage(t *testing.T) {
	buf := withTestSlog(t)
	leak := errors.New("read playlist files: sql: database is closed (/home/operator/bridge/data/bridge.db)")
	hs, token := newPlaylistFilesServer(t, &fakePlaylistFiles{err: leak})
	resp, body := getPlaylistFiles(t, hs, token)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var e ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil || e.Error != "internal" ||
		e.Message != "the bridge couldn't read its playlist files" {
		t.Errorf("body %s (%v)", body, err)
	}
	for _, frag := range []string{"sql", "closed", "bridge.db", "operator"} {
		if strings.Contains(string(body), frag) {
			t.Errorf("the 500 body carries %q: %s", frag, body)
		}
	}
	logged := buf.String()
	if strings.Count(logged, `msg="request failed"`) != 1 || !strings.Contains(logged, "level=ERROR") ||
		!strings.Contains(logged, "database is closed") {
		t.Errorf("want one request-failed line at ERROR with the error:\n%s", logged)
	}
}

func TestAClientAbortDuringThePlaylistFilesReadIsInfoAnd499(t *testing.T) {
	buf := withTestSlog(t)
	started := make(chan struct{})
	router, token := newPlaylistFilesRouter(t, &fakePlaylistFiles{block: started})
	// The access line is the router's last write. finished closes once the
	// router has returned, so the test reads a settled buffer.
	finished := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		close(finished)
	}))
	t.Cleanup(hs.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/v1/playlist-files", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		<-started
		cancel()
	}()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the request finished; the cancel did not reach it")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not finish after the client canceled")
	}
	logged := buf.String()
	if !strings.Contains(logged, "status=499") {
		t.Errorf("no line at 499:\n%s", logged)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("a client abort logged at ERROR:\n%s", logged)
	}
	if strings.Count(logged, "level=INFO") < 2 || !strings.Contains(logged, `msg="request failed"`) {
		t.Errorf("want the request-failed line and the access line at INFO:\n%s", logged)
	}
}

func TestPlaylistFilesThroughTheRealIndex(t *testing.T) {
	// The real store and scanner behind the real router: a playlist file
	// indexed by a scan, gzip on the wire, and a member whose track is
	// indexed later moving the ETag with no new read of the file.
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	for rel, body := range map[string]string{
		"Artist/Album/01.flac": "not-a-real-flac",
		"Playlists/mix.m3u8":   "#EXTM3U\n../Artist/Album/01.flac\n../Late/02.flac\n",
	} {
		abs := filepath.Join(lib, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := manifest.NewScanner([]string{lib}, store, "").Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs, token := newPlaylistFilesServer(t, store)

	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/playlist-files", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, Content-Encoding %q; want 200 gzip", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc playlistFilesResponse
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Playlists) != 1 || doc.Playlists[0].Path != "Playlists/mix.m3u8" || doc.Playlists[0].Name != "mix" ||
		strings.Join(doc.Playlists[0].Members, "|") != "Artist/Album/01.flac" ||
		strings.Join(doc.Playlists[0].Unresolved, "|") != "../Late/02.flac" {
		t.Fatalf("feed %+v", doc.Playlists)
	}
	before := resp.Header.Get("ETag")

	if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: "Late/02.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	after, body := getPlaylistFiles(t, hs, token, before)
	if after.StatusCode != http.StatusOK || after.Header.Get("ETag") == before {
		t.Fatalf("after the late track: status %d, ETag %q (was %q)", after.StatusCode, after.Header.Get("ETag"), before)
	}
	if !strings.Contains(string(body), `"members":["Artist/Album/01.flac","Late/02.flac"],"unresolved":[]`) {
		t.Errorf("body %s", body)
	}
}
