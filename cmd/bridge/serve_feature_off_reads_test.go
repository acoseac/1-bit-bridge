package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	replayGainDB     float64
	bpm              int
	truePeakDB       float64
	drScore          int
	md5State         string
	bandwidthHz      int
}

// manifestTrackFields holds the fields of one `/v1/manifest` track that the
// feature-off test reads: the renditions, and every field analysis
// measured, from each of the manifest's three analysis splices
// (waveformTagSQL, replayGainSQL, analysisScalarsSQL).
type manifestTrackFields struct {
	Path              string            `json:"path"`
	Variants          []json.RawMessage `json:"variants"`
	WaveformTag       string            `json:"waveformTag"`
	ReplayGainTrackDB *float64          `json:"replayGainTrackDB"`
	KeyRoot           *int              `json:"keyRoot"`
	KeyMode           string            `json:"keyMode"`
	BPM               *int              `json:"bpm"`
	BPMEstimated      bool              `json:"bpmEstimated"`
	TruePeakDB        *float64          `json:"truePeakDB"`
	DRScore           *int              `json:"drScore"`
	AudioMD5State     string            `json:"audioMD5State"`
	BandwidthHz       *int              `json:"bandwidthHz"`
}

// presentOrAbsent renders a manifest field a test compares: the value, or
// "absent" for one the manifest left out.
func presentOrAbsent[T any](p *T) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprint(*p)
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
		variantID:    "optimized-v2-48000-16",
		rendition:    []byte("a rendition made while upscaling was on"),
		curve:        []byte{0x01, 0x02, 0x03, 0x04},
		keyRoot:      7,
		replayGainDB: -7.5,
		bpm:          128,
		truePeakDB:   -0.5,
		drScore:      11,
		md5State:     "verified",
		bandwidthHz:  20000,
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
	keyRoot, replayGain, bpm := made.keyRoot, made.replayGainDB, made.bpm
	truePeak, dr, bandwidth := made.truePeakDB, made.drScore, made.bandwidthHz
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
			ReplayGainTrackDB: &replayGain, BPM: &bpm, TruePeakDB: &truePeak, DRScore: &dr,
			AudioMD5State: made.md5State, BandwidthHz: &bandwidth,
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
	if tr.Variants != nil {
		t.Errorf("the manifest carries variants (%d) with upscaling off, want the field left out", len(tr.Variants))
	}
	for _, c := range []struct{ field, got, want string }{
		{"waveformTag", tr.WaveformTag, made.tag},
		{"replayGainTrackDB", presentOrAbsent(tr.ReplayGainTrackDB), fmt.Sprint(made.replayGainDB)},
		{"keyRoot", presentOrAbsent(tr.KeyRoot), fmt.Sprint(made.keyRoot)},
		{"keyMode", tr.KeyMode, "major"},
		{"bpm", presentOrAbsent(tr.BPM), fmt.Sprint(made.bpm)},
		{"bpmEstimated", fmt.Sprint(tr.BPMEstimated), "true"},
		{"truePeakDB", presentOrAbsent(tr.TruePeakDB), fmt.Sprint(made.truePeakDB)},
		{"drScore", presentOrAbsent(tr.DRScore), fmt.Sprint(made.drScore)},
		{"audioMD5State", tr.AudioMD5State, made.md5State},
		{"bandwidthHz", presentOrAbsent(tr.BandwidthHz), fmt.Sprint(made.bandwidthHz)},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q: switching analysis off must not withdraw what it measured", c.field, c.got, c.want)
		}
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
