package api

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// GET /v1/playlist-files (backlog B431, paged since B442): the shape, the
// cursor and its version, the ETag and 304, the health flags, auth, and the
// error paths.

// fakeFeedVersion is the version a fakePlaylistFiles answers at first.
const fakeFeedVersion = "0123456789abcdef"

// fakePlaylistFiles is a PlaylistFileStore whose answer a test sets, and
// can change between requests: files as one page, or pages by the path
// they start after, at version.
type fakePlaylistFiles struct {
	mu         sync.Mutex
	files      []manifest.PlaylistFile
	pages      map[string]manifest.PlaylistFilePage
	version    string
	incomplete bool
	err        error
	// asked records each read's version and after.
	asked [][2]string
	// block, when set, is closed by the read once it has started; the read
	// then waits for its context and fails with it.
	block chan struct{}
}

func (f *fakePlaylistFiles) PlaylistFilesPage(ctx context.Context, want, after string) (manifest.PlaylistFilePage, error) {
	f.mu.Lock()
	f.asked = append(f.asked, [2]string{want, after})
	current := f.version
	if current == "" {
		current = fakeFeedVersion
	}
	page := manifest.PlaylistFilePage{Files: f.files}
	if f.pages != nil {
		page = f.pages[after]
	}
	page.Version, page.Complete = current, !f.incomplete
	err, block := f.err, f.block
	f.mu.Unlock()
	if block != nil {
		close(block)
		<-ctx.Done()
		return manifest.PlaylistFilePage{}, fmt.Errorf("read playlist files: %w", ctx.Err())
	}
	if err == nil && want != "" && want != current {
		return manifest.PlaylistFilePage{}, manifest.ErrPlaylistFilesChanged
	}
	return page, err
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
	return getPlaylistFilesAfter(t, hs, token, "", ifNoneMatch...)
}

// getPlaylistFilesAfter is getPlaylistFiles for the page after cursor (the
// first page for "").
func getPlaylistFilesAfter(t *testing.T, hs *httptest.Server, token, cursor string, ifNoneMatch ...string) (*http.Response, []byte) {
	t.Helper()
	target := hs.URL + "/v1/playlist-files"
	if cursor != "" {
		target += "?after=" + url.QueryEscape(cursor)
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
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
	// only when true, mtime in UTC to the nanosecond, and on the last page
	// no nextCursor.
	want := `{"version":"` + fakeFeedVersion + `","complete":true,"playlists":[` +
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

func TestPlaylistFilesSaysWhenTheIndexIsIncomplete(t *testing.T) {
	// false is sent, never omitted: it is the answer a client must not reap
	// on. Its flip moves the ETag like any other change.
	fake := &fakePlaylistFiles{incomplete: true}
	hs, token := newPlaylistFilesServer(t, fake)
	first, body := getPlaylistFiles(t, hs, token)
	if got := strings.TrimSpace(string(body)); got != `{"version":"`+fakeFeedVersion+`","complete":false,"playlists":[]}` {
		t.Fatalf("body %s", got)
	}
	fake.mu.Lock()
	fake.incomplete = false
	fake.mu.Unlock()
	resp, body := getPlaylistFiles(t, hs, token, first.Header.Get("ETag"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == first.Header.Get("ETag") ||
		strings.TrimSpace(string(body)) != `{"version":"`+fakeFeedVersion+`","complete":true,"playlists":[]}` {
		t.Fatalf("once complete: status %d, ETag %q (was %q), body %s", resp.StatusCode,
			resp.Header.Get("ETag"), first.Header.Get("ETag"), body)
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
		if f == "playlistFiles" || f == "playlistFilesPaged" {
			t.Errorf("health advertises %s with no index wired", f)
		}
	}
	wired, _ := newPlaylistFilesServer(t, &fakePlaylistFiles{})
	feats := healthOf(t, wired).Features
	found := map[string]bool{}
	for _, f := range feats {
		found[f] = true
	}
	if !found["playlistFiles"] || !found["playlistFilesPaged"] {
		t.Errorf("health does not advertise playlistFiles and playlistFilesPaged with the index wired: %v", feats)
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
	if !doc.Complete || doc.Version == "" || doc.NextCursor != "" {
		t.Errorf("the feed after a full scan: complete %v, version %q, nextCursor %q; want complete, a version, one page",
			doc.Complete, doc.Version, doc.NextCursor)
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
	var moved playlistFilesResponse
	if err := json.Unmarshal(body, &moved); err != nil || moved.Version == doc.Version {
		t.Errorf("after the late track: version %q (was %q), %v; a track added moves the version", moved.Version, doc.Version, err)
	}
}

// twoPages is a feed of two pages: a.m3u, then b.m3u.
func twoPages() map[string]manifest.PlaylistFilePage {
	a := manifest.PlaylistFile{Path: "Playlists/a.m3u", Name: "a", ModTime: time.Unix(1, 0).UTC(),
		Members: []string{"A/01.flac"}, Unresolved: []string{}}
	b := manifest.PlaylistFile{Path: "Playlists/b.m3u", Name: "b", ModTime: time.Unix(2, 0).UTC(),
		Members: []string{"B/01.flac"}, Unresolved: []string{}}
	return map[string]manifest.PlaylistFilePage{
		"":                {Files: []manifest.PlaylistFile{a}, Next: "Playlists/a.m3u"},
		"Playlists/a.m3u": {Files: []manifest.PlaylistFile{b}},
	}
}

// decodePage decodes one page's body, failing the test on a bad one.
func decodePage(t *testing.T, resp *http.Response, body []byte) playlistFilesResponse {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var page playlistFilesResponse
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return page
}

func TestPlaylistFilesPagesFollowTheNextCursor(t *testing.T) {
	fake := &fakePlaylistFiles{pages: twoPages()}
	hs, token := newPlaylistFilesServer(t, fake)
	resp, body := getPlaylistFiles(t, hs, token)
	first := decodePage(t, resp, body)
	if first.NextCursor == "" || len(first.Playlists) != 1 || first.Playlists[0].Path != "Playlists/a.m3u" {
		t.Fatalf("first page %s", body)
	}
	if strings.Contains(first.NextCursor, "Playlists") || strings.ContainsAny(first.NextCursor, "+/=") {
		t.Errorf("nextCursor %q: want it opaque and query-safe (unpadded base64url)", first.NextCursor)
	}
	resp, body = getPlaylistFilesAfter(t, hs, token, first.NextCursor)
	second := decodePage(t, resp, body)
	if second.NextCursor != "" || len(second.Playlists) != 1 || second.Playlists[0].Path != "Playlists/b.m3u" ||
		second.Version != first.Version {
		t.Fatalf("second page %s", body)
	}
	fake.mu.Lock()
	asked := fake.asked
	fake.mu.Unlock()
	if len(asked) != 2 || asked[0] != [2]string{"", ""} || asked[1] != [2]string{fakeFeedVersion, "Playlists/a.m3u"} {
		t.Errorf("the store was asked %q; want the first page, then the page after a.m3u at the first page's version", asked)
	}
}

func TestAPlaylistFilesCursorFromAnotherVersionIs410(t *testing.T) {
	fake := &fakePlaylistFiles{pages: twoPages()}
	hs, token := newPlaylistFilesServer(t, fake)
	resp, body := getPlaylistFiles(t, hs, token)
	first := decodePage(t, resp, body)
	fake.mu.Lock()
	fake.version = "fedcba9876543210"
	fake.mu.Unlock()
	resp, body = getPlaylistFilesAfter(t, hs, token, first.NextCursor)
	var e ErrorResponse
	if resp.StatusCode != http.StatusGone || json.Unmarshal(body, &e) != nil || e.Error != "playlist_files_stale" ||
		e.Message != "the playlist files changed since this cursor; start again from the first page" {
		t.Fatalf("a cursor of the old version: status %d %s; want 410 playlist_files_stale", resp.StatusCode, body)
	}
	resp, body = getPlaylistFiles(t, hs, token)
	if again := decodePage(t, resp, body); again.Version != "fedcba9876543210" || again.NextCursor == "" {
		t.Errorf("the first page again: %s", body)
	}
}

func TestAPlaylistFilesCursorTheBridgeDidNotMakeIs400(t *testing.T) {
	fake := &fakePlaylistFiles{pages: twoPages()}
	hs, token := newPlaylistFilesServer(t, fake)
	resp, body := getPlaylistFiles(t, hs, token)
	real := decodePage(t, resp, body).NextCursor
	raw, err := base64.RawURLEncoding.DecodeString(real)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]byte{}, raw...)
	edited[len(fakeFeedVersion)+1+len("Playlists/")] ^= 1 // one bit of the path
	fake.mu.Lock()
	fake.asked = nil
	fake.mu.Unlock()
	enc := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	for _, cursor := range []string{
		base64.RawURLEncoding.EncodeToString(edited), // the bridge's cursor, one bit of its path changed
		real[:len(real)-4],                           // the bridge's cursor, cut short
		enc(fakeFeedVersion + "\x00Playlists/a.m3u"), // the right fields, no signature
		"%%%",                         // not base64url
		enc("Playlists/a.m3u"),        // no separator
		enc("\x00Playlists/a.m3u"),    // no version
		enc(fakeFeedVersion + "\x00"), // no path
		enc(fakeFeedVersion+"\x00Playlists/a.m3u") + "=", // padded
	} {
		resp, body := getPlaylistFilesAfter(t, hs, token, cursor)
		var e ErrorResponse
		if resp.StatusCode != http.StatusBadRequest || json.Unmarshal(body, &e) != nil || e.Error != "bad_request" ||
			e.Message != "after is not a cursor this bridge gave; start from the first page" {
			t.Errorf("after %q: status %d %s; want 400 bad_request", cursor, resp.StatusCode, body)
		}
	}
	fake.mu.Lock()
	asked := len(fake.asked)
	fake.mu.Unlock()
	if asked != 0 {
		t.Errorf("the store was read %d times for cursors the bridge did not make", asked)
	}
}

func TestEachPlaylistFilesPageAnswers304ToItsOwnETag(t *testing.T) {
	hs, token := newPlaylistFilesServer(t, &fakePlaylistFiles{pages: twoPages()})
	firstResp, body := getPlaylistFiles(t, hs, token)
	first := decodePage(t, firstResp, body)
	secondResp, _ := getPlaylistFilesAfter(t, hs, token, first.NextCursor)
	etag1, etag2 := firstResp.Header.Get("ETag"), secondResp.Header.Get("ETag")
	if !strongETag.MatchString(etag2) || etag1 == etag2 {
		t.Fatalf("ETags %q and %q; want each page its own", etag1, etag2)
	}
	if resp, body := getPlaylistFilesAfter(t, hs, token, first.NextCursor, etag2); resp.StatusCode != http.StatusNotModified ||
		len(body) != 0 || resp.Header.Get("ETag") != etag2 {
		t.Errorf("the second page's own ETag: status %d, %d body bytes; want 304", resp.StatusCode, len(body))
	}
	if resp, _ := getPlaylistFilesAfter(t, hs, token, first.NextCursor, etag1); resp.StatusCode != http.StatusOK {
		t.Errorf("the first page's ETag on the second page: status %d, want 200", resp.StatusCode)
	}
}

// TestAPlaylistFileCostsAtLeastItsJSON: a page's budget is spent by
// manifest.PlaylistFilePageCost, which must be at least what each file takes
// in the page's JSON (its object and the comma after it), whatever the JSON
// escapes, or a page could pass its budget on the wire.
func TestAPlaylistFileCostsAtLeastItsJSON(t *testing.T) {
	odd := "q\"&<x>\\\t\x01\u2028\xff" + "Caf\u00e9"
	for _, f := range []manifest.PlaylistFile{
		{Path: "Playlists/a.m3u", Name: "a", Members: []string{}, Unresolved: []string{}},
		{Path: "Playlists/" + odd + ".m3u", Name: odd, ModTime: time.Date(2026, 10, 16, 9, 12, 44, 123456789, time.UTC),
			Members: []string{"A/" + odd + ".flac", "A/01.flac"}, Unresolved: []string{odd, "../x&y.flac"}},
		{Path: "Playlists/All.m3u", Name: "All", ModTime: time.Date(2026, 10, 1, 0, 0, 0, 999999999, time.UTC),
			Members: []string{}, Unresolved: []string{}, TooLarge: true},
	} {
		raw, err := json.Marshal(playlistFileDTO{Path: f.Path, Name: f.Name, MTime: f.ModTime.UTC(),
			Members: f.Members, Unresolved: f.Unresolved, TooLarge: f.TooLarge})
		if err != nil {
			t.Fatal(err)
		}
		if cost := manifest.PlaylistFilePageCost(f); cost < len(raw)+1 {
			t.Errorf("%q: costs %d, takes %d bytes and a comma in the JSON: %s", f.Path, cost, len(raw), raw)
		}
	}
}
