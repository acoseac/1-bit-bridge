package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

type stubLyricsStore struct{ rec *LyricsRecord }

func (s stubLyricsStore) LookupLyrics(_ context.Context, _ string) (*LyricsRecord, error) {
	return s.rec, nil
}

// lyricsFixture stages a real source file and a lyrics row, wired through
// the same server construction the waveform/spectrum tests use. `mutate`
// adjusts the record before install; `wire` false leaves the store off.
func lyricsFixture(t *testing.T, wire bool, mutate func(*LyricsRecord, string)) (*httptest.Server, string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist/Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	srcRel := "Artist/Album/01.flac"
	srcAbs := filepath.Join(root, filepath.FromSlash(srcRel))
	if err := os.WriteFile(srcAbs, []byte("audio-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(srcAbs)
	if err != nil {
		t.Fatal(err)
	}
	rec := &LyricsRecord{
		SourcePath: srcRel, Format: "lrc", Synced: true, Body: "[00:01.000]Hello", Language: "en",
		Source: "sylt", Tag: "0123abcd", SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size(),
	}
	if mutate != nil {
		mutate(rec, srcAbs)
	}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("ly")
	srv := New(cfg, store, nil, "fp")
	if wire {
		srv = srv.WithLyrics(stubLyricsStore{rec: rec})
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, srcRel
}

func lyricsGet(t *testing.T, hs *httptest.Server, token, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+"/v1/lyrics?path="+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Bridge-Protocol", "1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestLyricsServesTheDocumentWithETag(t *testing.T) {
	hs, tok, rel := lyricsFixture(t, true, nil)
	resp := lyricsGet(t, hs, tok, rel, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if got := resp.Header.Get("ETag"); got != `"0123abcd"` {
		t.Fatalf("ETag %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("Cache-Control %q", got)
	}
	var doc struct {
		Format   string `json:"format"`
		Synced   bool   `json:"synced"`
		Body     string `json:"body"`
		Language string `json:"language"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != "lrc" || !doc.Synced || doc.Body != "[00:01.000]Hello" || doc.Language != "en" {
		t.Fatalf("document %+v", doc)
	}
	// The conditional request revalidates to 304 — strong, weak (`W/`) and
	// wildcard forms alike (RFC 9110 weak comparison); a foreign tag does not.
	for _, h := range []string{`"0123abcd"`, `W/"0123abcd"`, `*`, `"zzzz", W/"0123abcd"`} {
		resp = lyricsGet(t, hs, tok, rel, map[string]string{"If-None-Match": h})
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("If-None-Match %q: %d", h, resp.StatusCode)
		}
	}
	if resp = lyricsGet(t, hs, tok, rel, map[string]string{"If-None-Match": `"other"`}); resp.StatusCode != 200 {
		t.Fatalf("a non-matching If-None-Match must serve the body: %d", resp.StatusCode)
	}
}

func TestLyricsNotFoundShapes(t *testing.T) {
	hs, tok, rel := lyricsFixture(t, true, func(r *LyricsRecord, _ string) { r.Body = "" })
	if resp := lyricsGet(t, hs, tok, rel, nil); resp.StatusCode != 404 {
		t.Fatalf("empty body → 404, got %d", resp.StatusCode)
	}
	hs2, tok2, rel2 := lyricsFixture(t, false, nil)
	resp := lyricsGet(t, hs2, tok2, rel2, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("store not wired → 404, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !json.Valid(b) || !strings.Contains(string(b), "lyrics_not_found") {
		t.Fatalf("error code: %s", b)
	}
	if resp := lyricsGet(t, hs, tok, "", nil); resp.StatusCode != 400 {
		t.Fatalf("missing path → 400, got %d", resp.StatusCode)
	}
	if resp := lyricsGet(t, hs, "", rel, nil); resp.StatusCode != 401 {
		t.Fatalf("no token → 401, got %d", resp.StatusCode)
	}
}

func TestLyricsStaleWhenTheSourceDrifted(t *testing.T) {
	hs, tok, rel := lyricsFixture(t, true, func(r *LyricsRecord, _ string) { r.SourceSize += 10 })
	resp := lyricsGet(t, hs, tok, rel, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("audio drift → 410, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "lyrics_stale") {
		t.Fatalf("error code: %s", b)
	}
	// A sidecar-sourced row is checked against THE SIDECAR: present and
	// matching → 200; missing → 410 even though the audio is unchanged.
	hsOK, tokOK, relOK := lyricsFixture(t, true, func(r *LyricsRecord, srcAbs string) {
		side := filepath.Join(filepath.Dir(srcAbs), "01.lrc")
		if err := os.WriteFile(side, []byte("[00:01.000]Hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(side)
		r.Source, r.SidecarName = "sidecar-lrc", "01.lrc"
		r.SourceMTimeNS, r.SourceSize = info.ModTime().UnixNano(), info.Size()
	})
	if resp := lyricsGet(t, hsOK, tokOK, relOK, nil); resp.StatusCode != 200 {
		t.Fatalf("matching sidecar → 200, got %d", resp.StatusCode)
	}
	hsGone, tokGone, relGone := lyricsFixture(t, true, func(r *LyricsRecord, _ string) {
		r.Source, r.SidecarName = "sidecar-lrc", "missing.lrc"
	})
	if resp := lyricsGet(t, hsGone, tokGone, relGone, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("missing sidecar → 410, got %d", resp.StatusCode)
	}
	// A stored sidecar name that is not a bare file name never reaches the
	// filesystem: the row reads stale rather than resolving a path.
	hsBad, tokBad, relBad := lyricsFixture(t, true, func(r *LyricsRecord, _ string) {
		r.Source, r.SidecarName = "sidecar-lrc", "../../etc/passwd"
	})
	if resp := lyricsGet(t, hsBad, tokBad, relBad, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("non-bare sidecar name → 410, got %d", resp.StatusCode)
	}
}
