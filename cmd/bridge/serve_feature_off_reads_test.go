package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// TestServeWithItsFeaturesOffServesWhatItMadeBefore boots the real serve
// with upscaling and analysis at their defaults, both off, over a store
// holding a rendition and an analysis row, as a bridge that ran with the
// features on and was then switched off holds them, and asks what a paired
// phone asks (backlog B108).
//
// PROTOCOL.md said the variant download and /v1/waveform answer 404 with
// their feature off, and that the manifest then carries no analysis field.
// Since #781 the handlers refuse only on a missing store, which serve wires
// on every bridge, and the manifest splices the analysis fields with no
// gate at all; the spec now says so, because a client drops a rendition or
// a curve on a 404 and switching generation off is not a request for that.
// The api package pins the handlers over stubs; this test pins the wiring
// that makes it true in production, the adapters and the manifest provider
// included, which only a boot can see.
func TestServeWithItsFeaturesOffServesWhatItMadeBefore(t *testing.T) {
	const rel = "Artist/Album/01 One.flac"
	b := startConsoleBridge(t, "", func(lib string) {
		writeHiResFLACFixture(t, filepath.Join(lib, filepath.FromSlash(rel)))
	})
	waitForTracksIndexed(t, b.console, b.adminBase, 1, b.stderr)
	requireUpscalingAndAnalysisOff(t, b)
	made := seedWhatARunWithTheFeaturesOnLeft(t, b, rel)

	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"feature-off reads"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	if token == "" {
		t.Fatalf("the console's pairing link carries no token: %s", mint.PairURL)
	}

	requireTheManifestKeepsWhatAnalysisMeasured(t, b, token, rel, made)

	// The reads a phone holding a rendition or a curve makes.
	q := urlquery.Escape(rel)
	for _, c := range []struct {
		what, path string
		want       []byte
	}{
		{"the rendition", "/v1/download?path=" + q + "&variant=" + made.variantID, made.rendition},
		{"the waveform", "/v1/waveform?path=" + q, made.curve},
	} {
		status, body := getBytesWithToken(t, b, c.path, token)
		if status != http.StatusOK || !bytes.Equal(body, c.want) {
			t.Errorf("GET %s with its feature off = %d, %d bytes; want 200 and %s's %d bytes",
				c.path, status, len(body), c.what, len(c.want))
		}
	}
}

// madeWhileOn is what a run with upscaling and analysis on left behind for
// one track: a rendition and a curve on disk, and what their rows record.
type madeWhileOn struct {
	variantID        string
	rendition, curve []byte
	tag              string
	keyRoot          int
}

// manifestTrackFields holds the fields of one `/v1/manifest` track that the
// feature-off test reads.
type manifestTrackFields struct {
	Path        string            `json:"path"`
	Variants    []json.RawMessage `json:"variants"`
	WaveformTag string            `json:"waveformTag"`
	KeyRoot     *int              `json:"keyRoot"`
	KeyMode     string            `json:"keyMode"`
}

// requireUpscalingAndAnalysisOff checks the fixture: a bridge booted with the
// defaults advertises neither upscaling nor any analysis flag.
func requireUpscalingAndAnalysisOff(t *testing.T, b *consoleBridge) {
	t.Helper()
	var health api.HealthResponse
	getJSON(t, b.phone, b.apiBase+"/v1/health", "", &health)
	if health.UpscaleEnabled == nil || *health.UpscaleEnabled {
		t.Fatalf("fixture broken: /v1/health does not say upscaling is off; stderr=%s", b.stderr.String())
	}
	for _, f := range []string{"keyTempo", "loudness", "spectrum", "trackQuality", "waveform"} {
		if slices.Contains(health.Features, f) {
			t.Fatalf("fixture broken: /v1/health advertises %q with analysis at its default, off", f)
		}
	}
}

// seedWhatARunWithTheFeaturesOnLeft writes a rendition and a curve for rel
// and records their rows in serve's store, stamped with the source as it is
// now, as a run with the features on would have left them.
func seedWhatARunWithTheFeaturesOnLeft(t *testing.T, b *consoleBridge, rel string) madeWhileOn {
	t.Helper()
	made := madeWhileOn{
		variantID: "optimized-v2-48000-16",
		rendition: []byte("a rendition made while upscaling was on"),
		curve:     []byte{0x01, 0x02, 0x03, 0x04},
		keyRoot:   7,
	}
	sum := sha256.Sum256(made.curve)
	made.tag = hex.EncodeToString(sum[:])[:8]

	src, err := os.Stat(filepath.Join(b.lib, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	renditionPath := filepath.Join(dir, "01 One."+made.variantID+".flac")
	curvePath := filepath.Join(dir, "01 One.flac.waveform.bin")
	for path, body := range map[string][]byte{renditionPath: made.rendition, curvePath: made.curve} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := manifest.OpenStore(manifest.DefaultDBPath(b.dataDir))
	if err != nil {
		t.Fatalf("open serve's store: %v", err)
	}
	keyRoot := made.keyRoot
	err = store.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: rel, VariantID: made.variantID, SidecarPath: renditionPath,
		Format: "flac", SampleRate: 48000, BitsPerSample: 16, SizeBytes: int64(len(made.rendition)),
		SourceMTimeNS: src.ModTime().UnixNano(), SourceSize: src.Size(),
	})
	if err == nil {
		err = store.UpsertAnalysis(t.Context(), manifest.AnalysisRow{
			SourcePath: rel, WaveformPath: curvePath, WaveformTag: made.tag, WaveformSize: int64(len(made.curve)),
			SourceMTimeNS: src.ModTime().UnixNano(), SourceSize: src.Size(),
			SchemaVersion: analyze.WaveformSchemaVersion, KeyRoot: &keyRoot, KeyMode: "major",
		})
	}
	if cerr := store.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("seed the rows: %v", err)
	}
	return made
}

// requireTheManifestKeepsWhatAnalysisMeasured asks the manifest for rel: it
// strips the renditions with upscaling off and keeps what analysis measured.
func requireTheManifestKeepsWhatAnalysisMeasured(t *testing.T, b *consoleBridge, token, rel string, made madeWhileOn) {
	t.Helper()
	var m struct {
		Tracks []manifestTrackFields `json:"tracks"`
	}
	getJSON(t, b.phone, b.apiBase+"/v1/manifest", token, &m)
	i := slices.IndexFunc(m.Tracks, func(tr manifestTrackFields) bool { return tr.Path == rel })
	if i < 0 {
		t.Fatalf("the manifest has no track %q", rel)
	}
	tr := m.Tracks[i]
	if len(tr.Variants) != 0 {
		t.Errorf("the manifest lists %d renditions with upscaling off, want none", len(tr.Variants))
	}
	if tr.WaveformTag != made.tag {
		t.Errorf("waveformTag = %q, want %q: switching analysis off must not withdraw what it measured", tr.WaveformTag, made.tag)
	}
	if tr.KeyRoot == nil || *tr.KeyRoot != made.keyRoot || tr.KeyMode != "major" {
		t.Errorf("keyRoot/keyMode = %v/%q, want %d/\"major\"", tr.KeyRoot, tr.KeyMode, made.keyRoot)
	}
}

// getBytesWithToken sends a GET for path to serve's API with the paired
// token, and returns the status and the whole body.
func getBytesWithToken(t *testing.T, b *consoleBridge, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.apiBase+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := b.phone.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	return resp.StatusCode, body
}
