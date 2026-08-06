package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// stubAnalysisStore serves one canned AnalysisRecord for any path.
type stubAnalysisStore struct{ rec *AnalysisRecord }

func (s stubAnalysisStore) LookupAnalysis(ctx context.Context, sourcePath string) (*AnalysisRecord, error) {
	return s.rec, nil
}

// waveformFixture stages a source track plus a sidecar whose recorded
// mtime/size match it (so the freshness gate passes) and wires an
// AnalysisStore that points at both.
func waveformFixture(t *testing.T) (*httptest.Server, string, string) {
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
	sidecar := filepath.Join(tmp, "01.flac.wf")
	body := []byte{0x01, 0x02, 0x03, 0x04}
	if err := os.WriteFile(sidecar, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)

	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("wf")

	srv := New(cfg, store, nil, "fp").WithAnalysis(true, stubAnalysisStore{rec: &AnalysisRecord{
		SourcePath:    srcRel,
		WaveformPath:  sidecar,
		WaveformTag:   hex.EncodeToString(sum[:])[:8],
		SourceMTimeNS: info.ModTime().UnixNano(),
		SourceSize:    info.Size(),
	}})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, srcRel
}

// GET /v1/waveform advertised `immutable, max-age=1yr` on a URL whose
// only key is `?path=` — nothing in the request identifies the BODY, so
// re-analysis (an edited source, a schema bump) rewrites the bytes under
// the same URL. `immutable` tells a conforming client never to
// revalidate, which both pins the stale copy for a year AND makes the
// ETag one line above it dead code. iOS keys its own disk cache on the
// waveform tag, so a new tag misses locally, refetches this URL, and can
// be handed the stale body back by any layer that honoured `immutable`.
//
// `no-cache` is NOT "don't store" — it is "store, but revalidate", which
// is exactly what an ETag'd endpoint at a stable URL wants.
func TestWaveformIsRevalidatedNotImmutable(t *testing.T) {
	hs, tok, rel := waveformFixture(t)
	resp := authGet(t, hs, "/v1/waveform?path="+rel, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q: must not be immutable — the URL "+
			"carries no content tag, so the body can change under it", cc)
	}
	if !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache so the ETag is actually used", cc)
	}
	if et := resp.Header.Get("ETag"); et == "" {
		t.Errorf("ETag missing — it is the revalidation key")
	}
}

// The ETag has to actually work: a conditional re-request must come back
// 304 with no body. Without this, dropping `immutable` would just mean
// re-downloading the sidecar on every fetch.
func TestWaveformConditionalRequestReturns304(t *testing.T) {
	hs, tok, rel := waveformFixture(t)
	first := authGet(t, hs, "/v1/waveform?path="+rel, tok)
	etag := first.Header.Get("ETag")
	first.Body.Close()
	if etag == "" {
		t.Fatal("no ETag on first response")
	}

	req, _ := http.NewRequest("GET", hs.URL+"/v1/waveform?path="+rel, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304 for a matching If-None-Match", resp.StatusCode)
	}
}
