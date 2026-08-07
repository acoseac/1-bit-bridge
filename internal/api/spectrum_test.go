package api

import (
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

// spectrumFixture stages a real source file and an analysis row carrying a
// curve, wired through the same server construction the waveform tests use.
// `mutate` adjusts the record before it is installed (drift the freshness
// fields, blank the curve, …).
func spectrumFixture(t *testing.T, mutate func(*AnalysisRecord)) (*httptest.Server, string, string, []byte) {
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
	// An 80-byte `1BSP` blob — the real shape, not a placeholder.
	curve := append([]byte("1BSP"), make([]byte, 76)...)
	rec := &AnalysisRecord{
		SourcePath:    srcRel,
		SourceMTimeNS: info.ModTime().UnixNano(),
		SourceSize:    info.Size(),
		Spectrum:      curve,
	}
	if mutate != nil {
		mutate(rec)
	}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("sp")
	srv := New(cfg, store, nil, "fp").WithAnalysis(true, stubAnalysisStore{rec: rec})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, srcRel, curve
}

// TestSpectrumServesTheCurveVerbatim is the happy path — and "verbatim"
// matters: iOS parses these bytes with the same `1BSP` decoder it uses for
// its own locally-measured profiles, so any re-encoding here would fork the
// format.
func TestSpectrumServesTheCurveVerbatim(t *testing.T) {
	hs, tok, rel, curve := spectrumFixture(t, nil)
	resp := authGet(t, hs, "/v1/spectrum?path="+rel, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(curve) {
		t.Errorf("body is %d bytes, want the %d-byte curve verbatim", len(body), len(curve))
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("no ETag — a client can never revalidate this curve")
	}
	// Same reasoning as TestWaveformIsRevalidatedNotImmutable: the URL's only
	// key is `?path=`, so re-analysis rewrites the body underneath it.
	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q: must not be immutable — the URL carries "+
			"no content tag, so the body can change under it", cc)
	}
	if !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

// TestSpectrumAbsentIsNotFound covers the distinction the feature rests on:
// "the bridge has no measurement" must be 404, and must never be confusable
// with "this file has no content up there".
func TestSpectrumAbsentIsNotFound(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*AnalysisRecord)
	}{
		{"row carries no spectrum (analysis predating wf6)", func(r *AnalysisRecord) { r.Spectrum = nil }},
		{"row carries an empty spectrum", func(r *AnalysisRecord) { r.Spectrum = []byte{} }},
	} {
		hs, tok, rel, _ := spectrumFixture(t, c.mutate)
		resp := authGet(t, hs, "/v1/spectrum?path="+rel, tok)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", c.name, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestSpectrumStaleSourceIsGone — a curve measured from different bytes is
// worse than no curve: it is evidence about a file that no longer exists.
func TestSpectrumStaleSourceIsGone(t *testing.T) {
	hs, tok, rel, _ := spectrumFixture(t, func(r *AnalysisRecord) {
		r.SourceSize = 999_999 // the source is 11 bytes
	})
	resp := authGet(t, hs, "/v1/spectrum?path="+rel, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410 for a drifted source", resp.StatusCode)
	}
}

// TestSpectrumRejectsBadRequests — the same argument + traversal guards as
// /v1/waveform, since it takes the same user-controlled path.
func TestSpectrumRejectsBadRequests(t *testing.T) {
	hs, tok, _, _ := spectrumFixture(t, nil)

	resp := authGet(t, hs, "/v1/spectrum", tok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing path: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = authGet(t, hs, "/v1/spectrum?path=../../etc/passwd", tok)
	if resp.StatusCode == http.StatusOK {
		t.Error("a traversal path was served")
	}
	resp.Body.Close()
}

// TestSpectrumRequiresAuth — /v1/spectrum is bearer-gated like every other
// /v1 route except health and pairing.
func TestSpectrumRequiresAuth(t *testing.T) {
	hs, _, rel, _ := spectrumFixture(t, nil)
	resp, err := http.Get(hs.URL + "/v1/spectrum?path=" + rel)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", resp.StatusCode)
	}
}
