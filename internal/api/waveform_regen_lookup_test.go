package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// countingAnalysisStore answers one record and counts which lookup each
// request made.
type countingAnalysisStore struct {
	rec                *AnalysisRecord
	mu                 sync.Mutex
	analysis, waveform int
}

func (s *countingAnalysisStore) LookupAnalysis(context.Context, string) (*AnalysisRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.analysis++
	return s.rec, nil
}

func (s *countingAnalysisStore) LookupWaveform(context.Context, string) (*AnalysisRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waveform++
	return s.rec, nil
}

func (s *countingAnalysisStore) counts() (analysis, waveform int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.analysis, s.waveform
}

// TestOnlyAWaveformRequestAsksForItsCurveToBeRenderedAgain: GET /v1/waveform
// looks its row up through LookupWaveform, the lookup that may ask for a
// missing curve to be rendered again (backlog B652), and GET /v1/spectrum
// through LookupAnalysis, which never does. Both answer a curve that is gone
// as they did: the waveform 410, the spectrum from the row.
func TestOnlyAWaveformRequestAsksForItsCurveToBeRenderedAgain(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist", "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	const rel = "Artist/Album/01.flac"
	src := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.WriteFile(src, []byte("audio-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	store := &countingAnalysisStore{rec: &AnalysisRecord{
		SourcePath: rel, WaveformPath: filepath.Join(tmp, "gone.waveform.bin"), WaveformTag: "deadbeef",
		SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size(), Spectrum: []byte("1BSP-curve"),
	}}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "T"}
	tokens, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := tokens.Mint("regen")
	hs := httptest.NewServer(New(cfg, tokens, nil, "fp").WithAnalysis(func() bool { return true }, store).Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/spectrum?path="+rel, raw)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spectrum: %d, want 200 from the row", resp.StatusCode)
	}
	if analysis, waveform := store.counts(); analysis != 1 || waveform != 0 {
		t.Fatalf("a spectrum request made %d LookupAnalysis and %d LookupWaveform, want 1 and 0: a spectrum "+
			"request asks for no curve to be rendered again", analysis, waveform)
	}
	resp = authGet(t, hs, "/v1/waveform?path="+rel, raw)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("waveform over a curve that is gone: %d, want 410 as before", resp.StatusCode)
	}
	if analysis, waveform := store.counts(); analysis != 1 || waveform != 1 {
		t.Fatalf("after a waveform request: %d LookupAnalysis and %d LookupWaveform, want 1 and 1: the "+
			"waveform request must ask LookupWaveform, the lookup that renders a missing curve again", analysis, waveform)
	}
}
