package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// fileFixture lays down a small library tree and returns an httptest server
// plus a valid bearer token.
//
//	root/
//	  Artist/
//	    Album/
//	      01 Track.flac    (1024 bytes of 0xAA)
//	      02 Other.flac    (512 bytes of 0xBB)
//	    Single.flac        (256 bytes of 0xCC)
//	  .DS_Store            (hidden, should not appear in list)
func fileFixture(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	for _, p := range []string{
		"Artist/Album",
	} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, b byte, n int) {
		data := bytes.Repeat([]byte{b}, n)
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Artist/Album/01 Track.flac", 0xAA, 1024)
	write("Artist/Album/02 Other.flac", 0xBB, 512)
	write("Artist/Single.flac", 0xCC, 256)
	os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("hidden"), 0o644)

	cfg := &config.Config{
		LibraryRoots:  []string{root},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, root
}

func authGet(t *testing.T, hs *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func authGetRange(t *testing.T, hs *httptest.Server, path, token, rng string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- /v1/list ---

func TestListReturnsSortedNonHiddenEntries(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path=Artist/Album", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var entries []Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	if entries[0].Name != "01 Track.flac" || entries[1].Name != "02 Other.flac" {
		t.Errorf("order wrong: %v", entries)
	}
	if entries[0].Path != "Artist/Album/01 Track.flac" {
		t.Errorf("Path = %q", entries[0].Path)
	}
	if entries[0].Size != 1024 {
		t.Errorf("size = %d", entries[0].Size)
	}
	if entries[0].IsDir {
		t.Error("expected file, got dir")
	}
}

func TestListRootListsTopLevel(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path=", tok)
	defer resp.Body.Close()
	var entries []Entry
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 1 || entries[0].Name != "Artist" {
		t.Errorf("root list = %v, want [Artist]", entries)
	}
	if !entries[0].IsDir {
		t.Error("Artist should be a directory")
	}
}

func TestListExcludesDotFiles(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path=", tok)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), ".DS_Store") {
		t.Errorf("list leaked .DS_Store: %s", body)
	}
}

func TestListFileReturns400(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path=Artist/Album/01%20Track.flac", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListNotFoundReturns404(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path=Nonexistent", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestListTraversalReturns400(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path="+url.QueryEscape("../outside"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var er ErrorResponse
	json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "bad_request" {
		t.Errorf("error code = %q", er.Error)
	}
}

func TestListRequiresAuth(t *testing.T) {
	hs, _, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/list?path=", "")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestListMultiRootRendersReachabilityFields(t *testing.T) {
	// One reachable root and one bogus path so the response covers both
	// halves of the new Reachable/Reason additive shape.
	tmp := t.TempDir()
	good := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(tmp, "Audiobooks-missing")
	cfg := &config.Config{
		LibraryRoots:  []string{good, missing},
		ListenAddress: ":7788",
		LibraryName:   "Multi",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp := authGet(t, hs, "/v1/list?path=", raw)
	defer resp.Body.Close()
	var entries []Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2: %+v", len(entries), entries)
	}
	// Sort puts Audiobooks-missing first, Music second.
	if entries[0].Reachable == nil || *entries[0].Reachable {
		t.Errorf("missing root should report Reachable=false, got %+v", entries[0])
	}
	if entries[0].Reason != "not_mounted" {
		t.Errorf("missing root reason = %q, want not_mounted", entries[0].Reason)
	}
	if entries[1].Reachable == nil || !*entries[1].Reachable {
		t.Errorf("healthy root should report Reachable=true, got %+v", entries[1])
	}
	if entries[1].Reason != "" {
		t.Errorf("healthy root should omit Reason, got %q", entries[1].Reason)
	}
}

func TestStatReachableRootReturnsReachableTrue(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "L"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp := authGet(t, hs, "/v1/stat?path=", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var sr StatResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if sr.Reachable == nil || !*sr.Reachable {
		t.Errorf("Reachable = %v, want true ptr", sr.Reachable)
	}
}

func TestStatUnreachableRootReturnsStructuredOffline(t *testing.T) {
	// Multi-root setup where one root path doesn't exist. /v1/stat against
	// that root's basename must come back as 200 + Reachable: false +
	// Reason, NOT as a 404 ErrNotFound from the resolver.
	tmp := t.TempDir()
	good := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(tmp, "Gone")
	cfg := &config.Config{LibraryRoots: []string{good, missing}, ListenAddress: ":7788", LibraryName: "L"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp := authGet(t, hs, "/v1/stat?path=Gone", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (structured offline, not 404)", resp.StatusCode)
	}
	var sr StatResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if sr.Reachable == nil || *sr.Reachable {
		t.Errorf("Reachable = %v, want false ptr", sr.Reachable)
	}
	if sr.Reason != "not_mounted" {
		t.Errorf("Reason = %q, want not_mounted", sr.Reason)
	}
	if !sr.IsDir {
		t.Error("offline root should report IsDir=true so iOS renders it as a directory")
	}
}

func TestHealthReportsRootsArray(t *testing.T) {
	tmp := t.TempDir()
	good := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(tmp, "Gone")
	cfg := &config.Config{LibraryRoots: []string{good, missing}, ListenAddress: ":7788", LibraryName: "L"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hr HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatal(err)
	}
	if len(hr.Roots) != 2 {
		t.Fatalf("len(Roots) = %d, want 2: %+v", len(hr.Roots), hr.Roots)
	}
	// Health response order = configured-roots order (NOT alphabetical
	// like /v1/list — the contract is "same order as resolver.Roots()").
	if hr.Roots[0].Name != "Music" || !hr.Roots[0].Reachable {
		t.Errorf("Roots[0] = %+v, want Music reachable", hr.Roots[0])
	}
	if hr.Roots[1].Name != "Gone" || hr.Roots[1].Reachable {
		t.Errorf("Roots[1] = %+v, want Gone unreachable", hr.Roots[1])
	}
	if hr.Roots[1].Reason != "not_mounted" {
		t.Errorf("Roots[1].Reason = %q, want not_mounted", hr.Roots[1].Reason)
	}
}

func TestListMultiRootEmptyPathReturnsRoots(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "Music")
	b := filepath.Join(tmp, "Audiobooks")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		LibraryRoots:  []string{a, b},
		ListenAddress: ":7788",
		LibraryName:   "Multi",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp := authGet(t, hs, "/v1/list?path=", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var entries []Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	// Sorted CI: Audiobooks before Music.
	if entries[0].Name != "Audiobooks" || entries[1].Name != "Music" {
		t.Errorf("unexpected order / names: %+v", entries)
	}
	if !entries[0].IsDir || !entries[1].IsDir {
		t.Error("root entries must be directories")
	}
}

func TestLessCaseFoldUnicode(t *testing.T) {
	// strings.ToLower-based compare case-folds non-ASCII letters
	// (É → é, Ü → ü, etc.). It does NOT give locale-aware collation —
	// byte-order on UTF-8 still places accented letters after ASCII
	// z — but it fixes the specific bug the old foldByte had: "É"
	// used to compare as raw bytes and totally miss case equivalence
	// with "é". Asserting the case-fold property keeps that regression
	// from returning without committing us to a collation library.
	cases := []struct{ a, b string }{
		{"éclair", "Éclair"}, // identical after fold — expect !less in either direction
		{"APPLE", "apple"},
	}
	for _, c := range cases {
		if lessCaseFold(c.a, c.b) || lessCaseFold(c.b, c.a) {
			t.Errorf("%q and %q should compare equal after case-fold", c.a, c.b)
		}
	}
	// Regression: "Ébène" used to sort after "zoo" because the old
	// ASCII fold left the first byte as 0xC3. With ToLower-based
	// compare, "Ébène" (first rune fold-equal to "ébène") still sorts
	// after "zoo" in UTF-8 byte order — but "Apple" no longer sorts
	// before "Zebra" via the old uppercase-vs-lowercase-byte quirk.
	if !lessCaseFold("Apple", "banana") {
		t.Error("CI: Apple < banana failed")
	}
	if !lessCaseFold("apple", "Banana") {
		t.Error("CI: apple < Banana failed")
	}
}

// --- /v1/stat ---

func TestStatFile(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/stat?path=Artist/Album/01%20Track.flac", tok)
	defer resp.Body.Close()
	var st StatResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.IsDir {
		t.Error("expected file")
	}
	if st.Size != 1024 {
		t.Errorf("size = %d", st.Size)
	}
}

func TestStatDir(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/stat?path=Artist/Album", tok)
	defer resp.Body.Close()
	var st StatResponse
	json.NewDecoder(resp.Body).Decode(&st)
	if !st.IsDir {
		t.Error("expected dir")
	}
}

func TestStatNotFound(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/stat?path=nope/nope", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- /v1/read ---

func TestReadRequiresRangeHeader(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGetRange(t, hs, "/v1/read?path=Artist/Album/01%20Track.flac", tok, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var er ErrorResponse
	json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "range_required" {
		t.Errorf("error code = %q", er.Error)
	}
}

func TestReadReturnsExactBytes(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	// Bytes 0-9: first 10 bytes of the 0xAA-filled file.
	resp := authGetRange(t, hs, "/v1/read?path=Artist/Album/01%20Track.flac", tok, "bytes=0-9")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 10 {
		t.Errorf("len = %d, want 10", len(body))
	}
	for i, b := range body {
		if b != 0xAA {
			t.Errorf("byte %d = 0x%02x, want 0xAA", i, b)
		}
	}
}

func TestReadEndRangeReturnsCorrectBytes(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	// Tail range: last 16 bytes.
	resp := authGetRange(t, hs, "/v1/read?path=Artist/Album/02%20Other.flac", tok, "bytes=-16")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 16 {
		t.Errorf("len = %d, want 16", len(body))
	}
	for _, b := range body {
		if b != 0xBB {
			t.Errorf("byte = 0x%02x, want 0xBB", b)
			break
		}
	}
}

func TestReadOnDirectoryReturns400(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGetRange(t, hs, "/v1/read?path=Artist", tok, "bytes=0-10")
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// --- /v1/download ---

func TestDownloadReturnsFullFileUnranged(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01%20Track.flac", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("accept-ranges = %q", ar)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 1024 {
		t.Errorf("len = %d, want 1024", len(body))
	}
}

func TestDownloadSupportsRange(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGetRange(t, hs, "/v1/download?path=Artist/Album/01%20Track.flac", tok, "bytes=100-199")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 100 {
		t.Errorf("len = %d, want 100", len(body))
	}
}

func TestDownloadNotFound(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/download?path=nope/nope.flac", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDownloadTraversalRejected(t *testing.T) {
	hs, tok, _ := fileFixture(t)
	resp := authGet(t, hs, "/v1/download?path="+url.QueryEscape("../../etc/passwd"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for traversal", resp.StatusCode)
	}
}

// --- large file smoke test — ensure we don't buffer the whole thing ---

func TestDownloadLargeFileStreams(t *testing.T) {
	hs, tok, root := fileFixture(t)
	// Write a 4 MB file of a repeating pattern.
	big := filepath.Join(root, "Artist", "big.flac")
	data := make([]byte, 4*1024*1024)
	for i := range data {
		data[i] = byte(i & 0xFF)
	}
	os.WriteFile(big, data, 0o644)

	resp := authGet(t, hs, "/v1/download?path=Artist/big.flac", tok)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) != len(data) {
		t.Fatalf("body len = %d, want %d", len(body), len(data))
	}
	// Spot-check a few offsets instead of byte-by-byte comparing 4 MB.
	for _, i := range []int{0, 1024, 1024 * 1024, len(data) - 1} {
		if body[i] != data[i] {
			t.Errorf("byte %d = 0x%02x, want 0x%02x", i, body[i], data[i])
		}
	}
}

// --- Range-and-auth: full matrix of "requires auth" for file endpoints ---

func TestFileEndpointsRequireAuth(t *testing.T) {
	hs, _, _ := fileFixture(t)
	for _, path := range []string{
		"/v1/list?path=",
		"/v1/stat?path=Artist",
		"/v1/read?path=Artist/Single.flac",
		"/v1/download?path=Artist/Single.flac",
	} {
		resp := authGet(t, hs, path, "")
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s without token: status = %d, want 401", path, resp.StatusCode)
		}
	}
}

// --- childPath helper ---

func TestChildPathBuilding(t *testing.T) {
	cases := []struct{ parent, name, want string }{
		{"", "A", "A"},
		{"/", "A", "A"},
		{"A", "b", "A/b"},
		{"A/B", "c.flac", "A/B/c.flac"},
	}
	for _, c := range cases {
		got := childPath(c.parent, c.name)
		if got != c.want {
			t.Errorf("childPath(%q,%q) = %q, want %q", c.parent, c.name, got, c.want)
		}
	}
}

// quiet unused-import warning — keeps fmt alive for future assertion helpers.
var _ = fmt.Sprintf

// TestSortEntriesByNameMatchesLessCaseFold pins that the new
// decorate-sort-undecorate helper produces the same ordering as the
// previous per-comparison `sort.Slice + lessCaseFold` shape — same
// UX assumptions ("Apple" before "banana", "Ébène" still sorts after
// "zoo" because the byte-fold quirk persists post-ToLower) — across
// the refactor.
func TestSortEntriesByNameMatchesLessCaseFold(t *testing.T) {
	input := []Entry{
		{Name: "banana"},
		{Name: "Apple"},
		{Name: "éclair"},
		{Name: "zoo"},
		{Name: "Ébène"},
	}

	gotEntries := append([]Entry(nil), input...)
	sortEntriesByName(gotEntries)

	wantEntries := append([]Entry(nil), input...)
	// Reference: the legacy per-call comparator (still defined as
	// lessCaseFold). If the new helper diverges from this on any
	// input, the test fails.
	for i := 0; i < len(wantEntries); i++ {
		for j := i + 1; j < len(wantEntries); j++ {
			if lessCaseFold(wantEntries[j].Name, wantEntries[i].Name) {
				wantEntries[i], wantEntries[j] = wantEntries[j], wantEntries[i]
			}
		}
	}

	for i := range gotEntries {
		if gotEntries[i].Name != wantEntries[i].Name {
			t.Errorf("position %d: got %q, want %q (full got=%v want=%v)",
				i, gotEntries[i].Name, wantEntries[i].Name, gotEntries, wantEntries)
		}
	}
}

// TestSortEntriesByNameDeterministicForFoldEqual locks the
// tie-breaker that prevents fold-equal names ("Apple" / "apple")
// from permuting arbitrarily under sort.Sort. Without it, two
// successive /v1/list calls against the same directory could return
// different orders — causing iOS list flicker. Run repeatedly with
// shuffled input to catch the non-determinism (CodeRabbit on PR #71).
func TestSortEntriesByNameDeterministicForFoldEqual(t *testing.T) {
	cases := []string{"Apple", "apple", "APPLE"}
	// Try each starting permutation and assert a single stable
	// outcome. With the byte-order tie-break, capital letters sort
	// before lowercase ('A' < 'a') so the deterministic order is
	// APPLE → Apple → apple.
	want := []string{"APPLE", "Apple", "apple"}
	for _, perm := range [][3]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}, {0, 2, 1}} {
		entries := []Entry{
			{Name: cases[perm[0]]},
			{Name: cases[perm[1]]},
			{Name: cases[perm[2]]},
		}
		sortEntriesByName(entries)
		for i, w := range want {
			if entries[i].Name != w {
				t.Errorf("perm=%v: position %d = %q, want %q", perm, i, entries[i].Name, w)
			}
		}
	}
}

// fakeVariantStore is a minimal in-memory VariantStore for tests.
// Returns a single record keyed on the configured (sourcePath,
// variantID) tuple; all other lookups return (nil, nil).
type fakeVariantStore struct {
	wantSourcePath string
	wantVariantID  string
	rec            *VariantRecord
}

func (f *fakeVariantStore) LookupVariant(ctx context.Context, sourcePath, variantID string) (*VariantRecord, error) {
	if sourcePath == f.wantSourcePath && variantID == f.wantVariantID {
		return f.rec, nil
	}
	return nil, nil
}

// fileFixtureWithVariant lays down the same library tree as
// fileFixture and additionally writes a sidecar file under tmp,
// returning a server wired with a VariantStore that maps
// (sourcePath, variantID) → that sidecar. The sidecar's recorded
// SourceMTimeNS is offset from the source file's actual mtime by
// `mtimeDriftNS` (positive: sidecar appears newer; negative: source
// appears newer). Sourcesize equals the actual size on disk —
// mtime is the only freshness axis under test here.
func fileFixtureWithVariant(t *testing.T, sourceRel, variantID string, mtimeDriftNS int64) (*httptest.Server, string, string) {
	t.Helper()
	hs, _, root := fileFixture(t)
	hs.Close()

	srcAbs := filepath.Join(root, filepath.FromSlash(sourceRel))
	srcInfo, err := os.Stat(srcAbs)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(t.TempDir(), "variant.bin")
	if err := os.WriteFile(sidecar, bytes.Repeat([]byte{0xEE}, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		LibraryRoots:  []string{root},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	raw, _, _ := store.Mint("test")

	srv := New(cfg, store, nil, "fp")
	srv.WithUpscale(true, &fakeVariantStore{
		wantSourcePath: sourceRel,
		wantVariantID:  variantID,
		rec: &VariantRecord{
			SidecarPath:   sidecar,
			SourceMTimeNS: srcInfo.ModTime().UnixNano() + mtimeDriftNS,
			SourceSize:    srcInfo.Size(),
		},
	})
	hs2 := httptest.NewServer(srv.Handler())
	t.Cleanup(hs2.Close)
	return hs2, raw, root
}

// TestServeVariantToleratesSMBFAT32MtimeGranularity locks in the
// fix for the SMB/FAT32 freshness false-stale (PR #N): the previous
// 1 ms tolerance constant was three orders of magnitude tighter
// than FAT32's 2 s mtime granularity, producing constant 410 Gone
// responses for libraries hosted on a NAS. Now: 2 s drift passes
// (200 OK), 3 s drift trips (410 Gone) — the tolerance has to be
// wide enough to cover real filesystem rounding without missing
// genuine source edits, which always jump by far more than 2 s.
func TestServeVariantToleratesSMBFAT32MtimeGranularity(t *testing.T) {
	cases := []struct {
		name         string
		mtimeDriftNS int64
		wantStatus   int
	}{
		{"exact match passes", 0, http.StatusOK},
		{"sub-second drift passes", 500_000_000, http.StatusOK},
		{"FAT32 1.5s rounding passes", 1_500_000_000, http.StatusOK},
		{"FAT32 2s rounding passes (boundary)", 2_000_000_000, http.StatusOK},
		{"3s drift trips stale gate", 3_000_000_000, http.StatusGone},
		{"30s drift trips stale gate", 30_000_000_000, http.StatusGone},
		// Negative-side cases: the production gate takes the absolute
		// value of the delta, so source-newer-than-record (sidecar
		// minted from an older version of the file) trips at the same
		// thresholds. CodeRabbit on PR #132 — guard against a sign
		// regression where only one direction was checked.
		{"negative sub-second drift passes", -500_000_000, http.StatusOK},
		{"negative FAT32 2s rounding passes (boundary)", -2_000_000_000, http.StatusOK},
		{"negative 3s drift trips stale gate", -3_000_000_000, http.StatusGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs, tok, _ := fileFixtureWithVariant(t,
				"Artist/Album/01 Track.flac", "upscaled-pcm-192", tc.mtimeDriftNS)
			// iOS-shaped encoding (spaces → %20, literal + → %2B). Pre-fix
			// the test used url.QueryEscape which form-encodes spaces as
			// '+' — that relied on the stdlib's now-replaced form-decode
			// to recover the space. Real iOS clients never send form-
			// encoded spaces; using the iOS shape here keeps the test
			// realistic.
			iosPath := strings.ReplaceAll(url.QueryEscape("Artist/Album/01 Track.flac"), "+", "%20")
			resp := authGet(t, hs,
				"/v1/download?path="+iosPath+"&variant=upscaled-pcm-192", tok)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("drift=%dns: status=%d (want %d), body=%s",
					tc.mtimeDriftNS, resp.StatusCode, tc.wantStatus, string(body))
			}
		})
	}
}

// BenchmarkSortEntriesByName1000 measures the alloc savings of the
// decorate-sort-undecorate refactor against a typical large directory
// listing. The previous shape allocated ~2 strings per comparison ×
// O(N log N) comparisons; the new shape allocates one parallel
// []string of length N. Run with -benchmem to see the delta.
func BenchmarkSortEntriesByName1000(b *testing.B) {
	src := make([]Entry, 1000)
	for i := range src {
		// Mix of cases + accents so ToLower has actual work to do.
		src[i] = Entry{Name: fmt.Sprintf("Artist %04d — Tâg", (i*7919)%1000)}
	}
	work := make([]Entry, len(src))
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		copy(work, src)
		sortEntriesByName(work)
	}
}
