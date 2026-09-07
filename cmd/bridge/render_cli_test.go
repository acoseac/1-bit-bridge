package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// renderCLIFixture writes a real file (ResolveChecked stats it) and the
// matching manifest row, returning everything classifyUpscaleTrack needs.
func renderCLIFixture(t *testing.T, rel, codec string, rateHz float64, isDSD bool, compression string, durationSec float64, channels int) (*manifest.Store, *bridgefs.Resolver, manifest.Track) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	abs := filepath.Join(lib, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rate := rateHz
	bits := 24
	if isDSD {
		bits = 1
	}
	dsd := isDSD
	tr := manifest.Track{
		Path: rel, Size: 1000, Codec: codec, IsDSD: &dsd,
		SampleRate: &rate, BitsPerSample: &bits, Compression: compression,
	}
	if durationSec > 0 {
		d := durationSec
		tr.Duration = &d
	}
	if channels > 0 {
		c := channels
		tr.Channels = &c
	}
	return store, bridgefs.New([]string{lib}), tr
}

func classifyWith(t *testing.T, store *manifest.Store, resolver *bridgefs.Resolver, track manifest.Track, kind transcode.JobKind, caps transcode.DSDRenderCaps) (*upscaleCandidate, upscaleSkipCounters, int) {
	t.Helper()
	var counters upscaleSkipCounters
	var stderr bytes.Buffer
	p := runUpscaleParams{
		targetRateFlag: "auto",
		targetBits:     24,
		quality:        transcode.QualityVeryHigh,
		kind:           kind,
		dsdCaps:        caps,
		tempDir:        "/scratch/render",
	}
	c, exit := classifyUpscaleTrack(context.Background(), &stderr, store, resolver, track, p, &counters)
	if exit != 0 {
		t.Fatalf("exitCode = %d (want a graceful skip, never a fatal abort); stderr=%q", exit, stderr.String())
	}
	return c, counters, exit
}

var (
	cliCapsOff    = transcode.DSDRenderCaps{}
	cliCapsDSD    = transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true}
	cliCapsDSDDST = transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true, DecodeDST: true}
)

// A DSD source is admitted by the two DSD-bearing kinds ONLY under this
// run's caps, and never by `upscale` — a 1-bit delta-sigma stream has no
// PCM rate to raise, which is the same reason EnqueueOne refuses it
// server-side. Without caps this is byte-for-byte the unconditional
// refusal every pre-rendition build had.
func TestClassifyUpscaleTrack_DSDKindArms(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      transcode.JobKind
		caps      transcode.DSDRenderCaps
		admitted  bool
		wantRate  int
		wantBits  int
		wantVarID string
	}{
		{"upscale never admits DSD, caps or not", transcode.JobKindUpscale, cliCapsDSDDST, false, 0, 0, ""},
		{"optimize without caps refuses", transcode.JobKindOptimize, cliCapsOff, false, 0, 0, ""},
		{"pcm without caps refuses", transcode.JobKindPCMRender, cliCapsOff, false, 0, 0, ""},
		{"optimize under caps takes the compact tier", transcode.JobKindOptimize, cliCapsDSD, true, 44100, 16, "optimized-dsd-v1-44100-16"},
		{"pcm under caps takes the faithful tier", transcode.JobKindPCMRender, cliCapsDSD, true, 176400, 24, "pcm-v1-176400-24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, resolver, track := renderCLIFixture(t, "A/01.dsf", "DSF", 2822400, true, "", 300, 2)
			c, counters, _ := classifyWith(t, store, resolver, track, tc.kind, tc.caps)
			if !tc.admitted {
				if c != nil {
					t.Fatalf("candidate = %+v, want nil (refused)", c.spec)
				}
				if counters.notPCM != 1 {
					t.Errorf("notPCM = %d, want 1 (the refusal is counted, not silent)", counters.notPCM)
				}
				return
			}
			if c == nil {
				t.Fatal("candidate = nil, want an enqueued DSD job")
			}
			if c.spec.TargetSampleRate != tc.wantRate || c.spec.TargetBits != tc.wantBits {
				t.Errorf("target = %d/%d, want %d/%d", c.spec.TargetSampleRate, c.spec.TargetBits, tc.wantRate, tc.wantBits)
			}
			if got := c.spec.VariantID(); got != tc.wantVarID {
				t.Errorf("VariantID = %q, want %q", got, tc.wantVarID)
			}
		})
	}
}

// The CLI's spec carries the same render facts the server-side paths do:
// the flag that selects the two-stage chain, the compression tag that
// budgets a DST decode, the geometry that sizes Stage A scratch and
// grades the decode's completeness, and the scratch directory.
func TestClassifyUpscaleTrack_DSDSpecCarriesTheRenderFacts(t *testing.T) {
	store, resolver, track := renderCLIFixture(t, "A/01.dff", "DFF", 2822400, true, "DST", 612.5, 6)
	c, _, _ := classifyWith(t, store, resolver, track, transcode.JobKindPCMRender, cliCapsDSDDST)
	if c == nil {
		t.Fatal("candidate = nil, want a DST job under DST-capable caps")
	}
	if !c.spec.SourceIsDSD {
		t.Error("SourceIsDSD = false — the spec would take the ordinary sox path and be refused")
	}
	if c.spec.SourceCompression != "DST" {
		t.Errorf("SourceCompression = %q, want DST (the pool doubles the timeout on it)", c.spec.SourceCompression)
	}
	if c.spec.SourceChannels != 6 || c.spec.SourceDurationSec != 612.5 {
		t.Errorf("geometry = %d ch / %.1f s, want 6 / 612.5", c.spec.SourceChannels, c.spec.SourceDurationSec)
	}
	if c.spec.TempDir != "/scratch/render" {
		t.Errorf("TempDir = %q, want the run's scratch dir", c.spec.TempDir)
	}
	// The scratch estimate follows the real channel count, not the
	// stereo fallback — the number the disk budget grades.
	if got, stereo := c.spec.RenderScratchBytes(), transcode.TempBytesForRender(2, 176400, 612.5); got <= stereo {
		t.Errorf("RenderScratchBytes = %d, want > the stereo estimate %d", got, stereo)
	}
}

// DST is a SEPARATE capability: an ffmpeg with the dsd_* decoders and no
// `dst` renders plain DSF / DFF and skips only DST-compressed DSDIFF.
func TestClassifyUpscaleTrack_DSTNeedsItsOwnCapability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		caps     transcode.DSDRenderCaps
		admitted bool
	}{
		{"without the dst decoder", cliCapsDSD, false},
		{"with the dst decoder", cliCapsDSDDST, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, resolver, track := renderCLIFixture(t, "A/01.dff", "DFF", 2822400, true, "DST", 300, 2)
			c, _, _ := classifyWith(t, store, resolver, track, transcode.JobKindPCMRender, tc.caps)
			if (c != nil) != tc.admitted {
				t.Errorf("admitted = %v, want %v", c != nil, tc.admitted)
			}
		})
	}
	// Control: the same caps that refuse DST admit a plain DFF, so the
	// refusal above is about the compression and not the container.
	store, resolver, track := renderCLIFixture(t, "A/02.dff", "DFF", 2822400, true, "", 300, 2)
	if c, _, _ := classifyWith(t, store, resolver, track, transcode.JobKindPCMRender, cliCapsDSD); c == nil {
		t.Error("a plain DFF was refused by DST-less caps — the refusal is not scoped to compression")
	}
}

// A PCM source is skipped by the render kind rather than failing it: a
// `bridge render --filter <album>` over a mixed folder renders its DSD
// tracks and passes over the rest, exactly as the batch coordinator's
// buildPCMRenderCandidates does.
func TestClassifyUpscaleTrack_PCMSourceIsSkippedByTheRenderKind(t *testing.T) {
	store, resolver, track := renderCLIFixture(t, "A/01.flac", "FLAC", 96000, false, "", 300, 2)
	c, counters, _ := classifyWith(t, store, resolver, track, transcode.JobKindPCMRender, cliCapsDSDDST)
	if c != nil {
		t.Fatalf("candidate = %+v, want nil (a PCM source already has a PCM form)", c.spec)
	}
	if counters.notPCM != 1 {
		t.Errorf("notPCM = %d, want 1", counters.notPCM)
	}
	// Control: the SAME FLAC is a normal optimize candidate, so the skip
	// above is the kind's rule and not a broken fixture.
	if c, _, _ := classifyWith(t, store, resolver, track, transcode.JobKindOptimize, cliCapsDSDDST); c == nil {
		t.Error("the 96/24 FLAC was refused by optimize too — the fixture is not a valid PCM candidate")
	}
}

// An off-family DSD rate resolves to no target and is skipped, never an
// abort: the rate is a header we do not trust, and one bogus row must
// not end a library-wide run.
func TestClassifyUpscaleTrack_OffFamilyDSDRateSkips(t *testing.T) {
	store, resolver, track := renderCLIFixture(t, "A/01.dsf", "DSF", 3000000, true, "", 300, 2)
	c, _, exit := classifyWith(t, store, resolver, track, transcode.JobKindPCMRender, cliCapsDSDDST)
	if c != nil || exit != 0 {
		t.Errorf("candidate = %v exit = %d, want a graceful skip", c, exit)
	}
}

// The skip line is worded for the kind. `notPCM` means "this pipeline
// cannot take this source", and for the DSD-only render kind the sources
// it holds ARE PCM — calling them non-PCM would be exactly backwards.
func TestReportUpscaleSummary_SkipWordingFollowsTheKind(t *testing.T) {
	for _, tc := range []struct {
		kind transcode.JobKind
		want string
	}{
		{transcode.JobKindUpscale, "non-PCM"},
		{transcode.JobKindOptimize, "non-PCM"},
		{transcode.JobKindPCMRender, "non-DSD"},
	} {
		var out bytes.Buffer
		reportUpscaleSummaryForKind(&out, 3, 1, upscaleSkipCounters{notPCM: 2}, tc.kind)
		if !bytes.Contains(out.Bytes(), []byte(tc.want)) {
			t.Errorf("kind %s: summary %q does not say %q", tc.kind, out.String(), tc.want)
		}
	}
	// The legacy entry point keeps the historical wording byte-for-byte.
	var legacy bytes.Buffer
	reportUpscaleSummary(&legacy, 3, 1, upscaleSkipCounters{notPCM: 2})
	if !bytes.Contains(legacy.Bytes(), []byte("Skipped 2 non-PCM or unparseable track(s).")) {
		t.Errorf("legacy summary changed: %q", legacy.String())
	}
}

// `bridge render` refuses on the operator flag BEFORE it touches the
// toolchain: the flag's absence is a configuration answer, and saying so
// is more useful than an ffmpeg diagnostic for a feature the operator
// never turned on.
func TestRenderCmd_RefusesWhenTheFlagIsOff(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"libraryName: T\nlibraryRoots:\n  - "+filepath.ToSlash(dir)+
			"\ndataDir: "+filepath.ToSlash(filepath.Join(dir, "data"))+
			"\nlistenAddress: 127.0.0.1:7788\nupscale:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := renderCmd(context.Background(), []string{"--config", cfgPath}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("upscale.dsdRender.enabled")) {
		t.Errorf("stderr does not name the flag to set: %q", stderr.String())
	}
}
