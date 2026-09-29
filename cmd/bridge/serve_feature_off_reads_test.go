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
	const variantID = "optimized-v2-48000-16"
	b := startConsoleBridge(t, "", func(lib string) {
		writeHiResFLACFixture(t, filepath.Join(lib, filepath.FromSlash(rel)))
	})
	waitForTracksIndexed(t, b.console, b.adminBase, 1, b.stderr)

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

	// What a run with the features on left behind: a rendition and a curve
	// on disk, and their rows, stamped with the source as it is now.
	src, err := os.Stat(filepath.Join(b.lib, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	made := t.TempDir()
	rendition := []byte("a rendition made while upscaling was on")
	renditionPath := filepath.Join(made, "01 One."+variantID+".flac")
	curve := []byte{0x01, 0x02, 0x03, 0x04}
	curvePath := filepath.Join(made, "01 One.flac.waveform.bin")
	for path, body := range map[string][]byte{renditionPath: rendition, curvePath: curve} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(curve)
	tag := hex.EncodeToString(sum[:])[:8]
	keyRoot := 7
	store, err := manifest.OpenStore(manifest.DefaultDBPath(b.dataDir))
	if err != nil {
		t.Fatalf("open serve's store: %v", err)
	}
	err = store.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: rel, VariantID: variantID, SidecarPath: renditionPath,
		Format: "flac", SampleRate: 48000, BitsPerSample: 16, SizeBytes: int64(len(rendition)),
		SourceMTimeNS: src.ModTime().UnixNano(), SourceSize: src.Size(),
	})
	if err == nil {
		err = store.UpsertAnalysis(t.Context(), manifest.AnalysisRow{
			SourcePath: rel, WaveformPath: curvePath, WaveformTag: tag, WaveformSize: int64(len(curve)),
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

	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"feature-off reads"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	if token == "" {
		t.Fatalf("the console's pairing link carries no token: %s", mint.PairURL)
	}

	// The manifest strips the renditions with upscaling off and keeps what
	// analysis measured.
	var m struct {
		Tracks []struct {
			Path        string            `json:"path"`
			Variants    []json.RawMessage `json:"variants"`
			WaveformTag string            `json:"waveformTag"`
			KeyRoot     *int              `json:"keyRoot"`
			KeyMode     string            `json:"keyMode"`
		} `json:"tracks"`
	}
	getJSON(t, b.phone, b.apiBase+"/v1/manifest", token, &m)
	found := false
	for _, tr := range m.Tracks {
		if tr.Path != rel {
			continue
		}
		found = true
		if len(tr.Variants) != 0 {
			t.Errorf("the manifest lists %d renditions with upscaling off, want none", len(tr.Variants))
		}
		if tr.WaveformTag != tag {
			t.Errorf("waveformTag = %q, want %q: switching analysis off must not withdraw what it measured", tr.WaveformTag, tag)
		}
		if tr.KeyRoot == nil || *tr.KeyRoot != keyRoot || tr.KeyMode != "major" {
			t.Errorf("keyRoot/keyMode = %v/%q, want %d/\"major\"", tr.KeyRoot, tr.KeyMode, keyRoot)
		}
	}
	if !found {
		t.Fatalf("the manifest has no track %q", rel)
	}

	// The reads a phone holding a rendition or a curve makes.
	q := urlquery.Escape(rel)
	for _, c := range []struct {
		what, path string
		want       []byte
	}{
		{"the rendition", "/v1/download?path=" + q + "&variant=" + variantID, rendition},
		{"the waveform", "/v1/waveform?path=" + q, curve},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.apiBase+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := b.phone.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("GET %s: read body: %v", c.path, err)
		}
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, c.want) {
			t.Errorf("GET %s with its feature off = %d, %d bytes; want 200 and %s's %d bytes",
				c.path, resp.StatusCode, len(body), c.what, len(c.want))
		}
	}
}
