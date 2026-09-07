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
	// compression is the DSDIFF CMPR name ("DST" for DST-compressed
	// DSDIFF) — the DSD-render gate's decoder-dependent term.
	compression string
	// path, when set, is the seeded track's path UNDER its folder in
	// place of `track<ext>` — the SACD virtual shape needs its
	// `<x>.iso/st/NN.dff` form.
	path string
}

// trackPath is the path the case is seeded (and gated) at.
func (c eligibilityCase) trackPath(folder string) string {
	if c.path != "" {
		return folder + "/" + c.path
	}
	return folder + "/track" + c.ext
}

// eligibilityMatrix covers the allowlist, the codec-empty extension
// fallback, the CarPlay-floor boundaries, DSD, lossy, and unknown
// geometry. Rows must be REALISTIC (e.g. DSD carries DSD-scale rates)
// — the SQL's belt-and-braces is_dsd arm intentionally also excludes
// impossible low-rate DSD rows the Go Submit walk would let through
// on rate alone.
var eligibilityMatrix = []eligibilityCase{
	{name: "flac-cd-floor", codec: "FLAC", ext: ".flac", rate: 44100, bits: 16, isDSD: false},
	{name: "flac-48-floor", codec: "FLAC", ext: ".flac", rate: 48000, bits: 16, isDSD: false},
	{name: "flac-48-24", codec: "FLAC", ext: ".flac", rate: 48000, bits: 24, isDSD: false},
	{name: "flac-88-16", codec: "FLAC", ext: ".flac", rate: 88200, bits: 16, isDSD: false},
	{name: "flac-hires", codec: "FLAC", ext: ".flac", rate: 96000, bits: 24, isDSD: false},
	{name: "flac-at-192-24", codec: "FLAC", ext: ".flac", rate: 192000, bits: 24, isDSD: false},
	{name: "flac-above-target-rate", codec: "FLAC", ext: ".flac", rate: 384000, bits: 24, isDSD: false},
	{name: "flac-32bit", codec: "FLAC", ext: ".flac", rate: 96000, bits: 32, isDSD: false},
	{name: "alac-hires", codec: "ALAC", ext: ".m4a", rate: 96000, bits: 24, isDSD: false},
	{name: "wav-hires", codec: "WAV", ext: ".wav", rate: 96000, bits: 24, isDSD: false},
	{name: "aiff-hires", codec: "AIFF", ext: ".aiff", rate: 96000, bits: 24, isDSD: false},
	{name: "mp3-cd", codec: "MP3", ext: ".mp3", rate: 44100, bits: 16, isDSD: false},
	{name: "aac-cd", codec: "AAC", ext: ".m4a", rate: 44100, bits: 16, isDSD: false},
	{name: "dsf-dsd64", codec: "DSF", ext: ".dsf", rate: 2822400, bits: 1, isDSD: true},
	{name: "codec-empty-flac-ext", codec: "", ext: ".flac", rate: 96000, bits: 24, isDSD: false},
	{name: "codec-empty-mp3-ext", codec: "", ext: ".mp3", rate: 96000, bits: 24, isDSD: false},
	{name: "codec-empty-unknown-geometry", codec: "", ext: ".flac", rate: 0, bits: 0, isDSD: false},
	{name: "known-codec-unknown-geometry", codec: "FLAC", ext: ".flac", rate: 0, bits: 0, isDSD: false},
	// DSD-render rows — the dsdRenderEligibleSQL ⇄ transcode.DSDRenderEligible
	// lockstep. Each row targets one term: rate family (DSD128 in the
	// 44.1k family, DSD64 in the 48k family, an off-family header), the
	// DST arm (eligible only with the dst decoder), the SACD virtual shape
	// (never), a forged isDSD on a lossy row (never — codec AND flag), and
	// the codec-empty extension fallback.
	{name: "dsf-dsd128", codec: "DSF", ext: ".dsf", rate: 5644800, bits: 1, isDSD: true},
	{name: "dff-dsd64-48k", codec: "DFF", ext: ".dff", rate: 3072000, bits: 1, isDSD: true},
	{name: "dff-dst", codec: "DFF", ext: ".dff", rate: 2822400, bits: 1, isDSD: true, compression: "DST"},
	{name: "dsd-odd-rate", codec: "DSF", ext: ".dsf", rate: 3000000, bits: 1, isDSD: true},
	{name: "sacd-virtual", codec: "DFF", ext: ".dff", rate: 2822400, bits: 1, isDSD: true, path: "Album.iso/st/01.dff"},
	{name: "mp3-forged-isdsd", codec: "MP3", ext: ".mp3", rate: 44100, bits: 16, isDSD: true},
	{name: "codec-empty-dsf-ext", codec: "", ext: ".dsf", rate: 2822400, bits: 1, isDSD: true},
}

// dsdRenderStates are the three capability states the DSD-aware
// mirrors are pinned in: PCM-only (the pre-v43 predicate, byte for
// byte), DSD without the dst decoder, DSD with it. `opts` is the SQL
// side, `caps` the Go side; the two must describe the same world.
var dsdRenderStates = []struct {
	name string
	opts manifest.EligibilityOpts
	caps transcode.DSDRenderCaps
}{
	{"pcm-only", manifest.EligibilityOpts{}, transcode.DSDRenderCaps{}},
	{"dsd", manifest.EligibilityOpts{DSDRender: true}, transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true}},
	{"dsd+dst", manifest.EligibilityOpts{DSDRender: true, DST: true}, transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true, DecodeDST: true}},
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
			Path:        c.trackPath(folder),
			Size:        1_000_000,
			Codec:       c.codec,
			IsDSD:       &isDSD,
			Compression: c.compression,
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

// TestEligibilitySQLAgreesWithOptimizeEligible — the optimize
// denominator (optimizeEligibleSQL OR dsdRenderEligibleSQL) must agree
// with transcode.OptimizeEligibleFor on every matrix row in every
// capability state; and in the PCM-only state it must ALSO still be
// exactly transcode.OptimizeEligible, so the pre-v43 numbers are
// byte-identical for a caller that has not been wired to the caps.
func TestEligibilitySQLAgreesWithOptimizeEligible(t *testing.T) {
	srv, _, _ := newTestServer(t)
	paths := seedMatrix(t, srv.deps.Manifest)

	for _, st := range dsdRenderStates {
		counts, err := srv.deps.Manifest.EligibleCountsForFolders(
			context.Background(), paths, 0, 0, st.opts) // optimize arm is target-independent
		if err != nil {
			t.Fatalf("%s: EligibleCountsForFolders: %v", st.name, err)
		}
		for i, c := range eligibilityMatrix {
			p := c.trackPath("x")
			want := transcode.OptimizeEligibleFor(p, c.codec, int(c.rate), c.bits, c.isDSD, c.compression, st.caps)
			if st.name == "pcm-only" {
				if pcmOnly := transcode.OptimizeEligible(p, c.codec, int(c.rate), c.bits); pcmOnly != want {
					t.Errorf("%s/%s: OptimizeEligibleFor with zero caps=%v differs from OptimizeEligible=%v",
						st.name, c.name, want, pcmOnly)
				}
			}
			got := counts[paths[i]].Optimize == 1
			if got != want {
				t.Errorf("%s/%s: SQL optimize-eligible=%v, transcode.OptimizeEligibleFor=%v — mirrors diverged",
					st.name, c.name, got, want)
			}
		}
	}
}

// TestEligibilitySQLAgreesWithPCMRenderEligible — the faithful tier's
// denominator (pcmCoveredOrEligibleSQL) must agree with
// transcode.PCMRenderEligible on every matrix row in every capability
// state. No row carries a `pcm-` variant here, so the EXISTS arm is
// inert and the predicate under test is dsdRenderEligibleSQL alone.
func TestEligibilitySQLAgreesWithPCMRenderEligible(t *testing.T) {
	srv, _, _ := newTestServer(t)
	paths := seedMatrix(t, srv.deps.Manifest)

	for _, st := range dsdRenderStates {
		counts, err := srv.deps.Manifest.EligibleCountsForFolders(
			context.Background(), paths, 0, 0, st.opts)
		if err != nil {
			t.Fatalf("%s: EligibleCountsForFolders: %v", st.name, err)
		}
		eligible := 0
		for i, c := range eligibilityMatrix {
			p := c.trackPath("x")
			want := transcode.PCMRenderEligible(p, c.codec, c.isDSD, int(c.rate), c.compression, st.caps)
			got := counts[paths[i]].PCM == 1
			if got != want {
				t.Errorf("%s/%s: SQL pcm-eligible=%v, transcode.PCMRenderEligible=%v — mirrors diverged",
					st.name, c.name, got, want)
			}
			if want {
				eligible++
			}
		}
		// Vacuity guard: the matrix must actually exercise the arm — a
		// state that grants nothing (pcm-only) and states that grant
		// something (the two DSD states) must both occur.
		if st.name == "pcm-only" && eligible != 0 {
			t.Errorf("pcm-only granted %d rows; the zero caps must grant nothing", eligible)
		}
		if st.name != "pcm-only" && eligible == 0 {
			t.Errorf("%s granted no rows; the matrix has stopped exercising the DSD arm", st.name)
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
		context.Background(), paths, targetRate, targetBits, manifest.EligibilityOpts{})
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
