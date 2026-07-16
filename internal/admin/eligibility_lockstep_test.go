package admin

// Lockstep pins for the eligibility SQL mirrors in
// internal/manifest/eligibility.go. The rollups evaluate
// transcode-eligibility as plain-column SQL (no json_extract on the
// browse hot path — the whole point of the v25 columns), which
// necessarily DUPLICATES the Go gates. These truth-table tests feed
// identical fixtures through both implementations and fail on any
// divergence — the admin package is the only one importing both
// manifest and transcode, so the pin lives here. Unlike the sibling
// handler tests (which stub the transcode closures to avoid the
// import), pulling in the REAL transcode package is load-bearing:
// comparing against a re-stub would pin nothing.

import (
	"context"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

type eligibilityCase struct {
	name  string
	codec string
	ext   string // file extension incl. dot
	rate  float64
	bits  int
	isDSD bool
}

// eligibilityMatrix covers the allowlist, the codec-empty extension
// fallback, the CarPlay-floor boundaries, DSD, lossy, and unknown
// geometry. Rows must be REALISTIC (e.g. DSD carries DSD-scale rates)
// — the SQL's belt-and-braces is_dsd arm intentionally also excludes
// impossible low-rate DSD rows the Go Submit walk would let through
// on rate alone.
var eligibilityMatrix = []eligibilityCase{
	{"flac-cd-floor", "FLAC", ".flac", 44100, 16, false},
	{"flac-48-floor", "FLAC", ".flac", 48000, 16, false},
	{"flac-48-24", "FLAC", ".flac", 48000, 24, false},
	{"flac-88-16", "FLAC", ".flac", 88200, 16, false},
	{"flac-hires", "FLAC", ".flac", 96000, 24, false},
	{"flac-at-192-24", "FLAC", ".flac", 192000, 24, false},
	{"flac-above-target-rate", "FLAC", ".flac", 384000, 24, false},
	{"flac-32bit", "FLAC", ".flac", 96000, 32, false},
	{"alac-hires", "ALAC", ".m4a", 96000, 24, false},
	{"wav-hires", "WAV", ".wav", 96000, 24, false},
	{"aiff-hires", "AIFF", ".aiff", 96000, 24, false},
	{"mp3-cd", "MP3", ".mp3", 44100, 16, false},
	{"aac-cd", "AAC", ".m4a", 44100, 16, false},
	{"dsf-dsd64", "DSF", ".dsf", 2822400, 1, true},
	{"codec-empty-flac-ext", "", ".flac", 96000, 24, false},
	{"codec-empty-mp3-ext", "", ".mp3", 96000, 24, false},
	{"codec-empty-unknown-geometry", "", ".flac", 0, 0, false},
	{"known-codec-unknown-geometry", "FLAC", ".flac", 0, 0, false},
}

// seedMatrix upserts one track per case, each in its own folder so
// the per-folder count is that case's verdict (0 or 1).
func seedMatrix(t *testing.T, store *manifest.Store) []string {
	t.Helper()
	paths := make([]string, len(eligibilityMatrix))
	for i, c := range eligibilityMatrix {
		folder := fmt.Sprintf("F%02d", i)
		paths[i] = folder
		isDSD := c.isDSD
		tr := &manifest.Track{
			Path:  folder + "/track" + c.ext,
			Size:  1_000_000,
			Codec: c.codec,
			IsDSD: &isDSD,
		}
		if c.rate > 0 {
			r := c.rate
			tr.SampleRate = &r
		}
		if c.bits > 0 {
			b := c.bits
			tr.BitsPerSample = &b
		}
		if err := store.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatalf("UpsertTrack %s: %v", c.name, err)
		}
	}
	return paths
}

// TestEligibilitySQLAgreesWithOptimizeEligible — optimizeEligibleSQL
// must agree with transcode.OptimizeEligible on every matrix row.
func TestEligibilitySQLAgreesWithOptimizeEligible(t *testing.T) {
	srv, _, _ := newTestServer(t)
	paths := seedMatrix(t, srv.deps.Manifest)

	counts, err := srv.deps.Manifest.EligibleCountsForFolders(
		context.Background(), paths, 0, 0) // optimize arm is target-independent
	if err != nil {
		t.Fatalf("EligibleCountsForFolders: %v", err)
	}
	for i, c := range eligibilityMatrix {
		want := transcode.OptimizeEligible("x/track"+c.ext, c.codec, int(c.rate), c.bits)
		got := counts[paths[i]].Optimize == 1
		if got != want {
			t.Errorf("%s: SQL optimize-eligible=%v, transcode.OptimizeEligible=%v — mirrors diverged",
				c.name, got, want)
		}
	}
}

// TestEligibilitySQLAgreesWithUpscaleSubmitGate — upscaleEligibleSQL
// must agree with a literal re-statement of Coordinator.Submit's
// candidate gate (internal/transcode/batch.go): NOT lossy
// (manifest.IsLossyCodec — the re-statement calls the REAL shared
// predicate, same as Submit does), known geometry, never downsample
// on either axis, skip exact-at-target. Submit has no DSD arm (real
// DSD falls out via rate > target), so the re-statement doesn't
// either; the matrix keeps DSD rows realistic so the SQL's defensive
// is_dsd arm agrees.
func TestEligibilitySQLAgreesWithUpscaleSubmitGate(t *testing.T) {
	const targetRate, targetBits = 192000, 24
	submitGate := func(codec string, rate float64, bits int) bool {
		r, b := int(rate), bits
		if r <= 0 || b <= 0 {
			return false
		}
		if manifest.IsLossyCodec(codec) {
			return false
		}
		if r > targetRate || b > targetBits {
			return false
		}
		if r == targetRate && b == targetBits {
			return false
		}
		return true
	}

	srv, _, _ := newTestServer(t)
	paths := seedMatrix(t, srv.deps.Manifest)

	counts, err := srv.deps.Manifest.EligibleCountsForFolders(
		context.Background(), paths, targetRate, targetBits)
	if err != nil {
		t.Fatalf("EligibleCountsForFolders: %v", err)
	}
	for i, c := range eligibilityMatrix {
		want := submitGate(c.codec, c.rate, c.bits)
		got := counts[paths[i]].Upscale == 1
		if got != want {
			t.Errorf("%s: SQL upscale-eligible=%v, Submit gate=%v — mirrors diverged",
				c.name, got, want)
		}
	}
}
