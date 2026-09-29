package transcode

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// TestDSDPeakProfile pins the profile string. manifest's v47 seed spells the
// same formula in SQL and pins the same "a1|compact|44100|-v" literal
// (TestSeedDSDPeaksProfileSpelling), which is what keeps the two in step: a
// seeded peak must match the profile a render asks for, or the album
// survey decodes every rendered track again.
func TestDSDPeakProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec JobSpec
		want string
	}{
		{"compact", JobSpec{SourceIsDSD: true, Kind: JobKindOptimize, TargetSampleRate: 44100, Quality: QualityVeryHigh}, "a1|compact|44100|-v"},
		{"faithful", JobSpec{SourceIsDSD: true, Kind: JobKindPCMRender, TargetSampleRate: 176400, Quality: QualityVeryHigh}, "a1|faithful|176400|-v"},
		{"48k family at high quality", JobSpec{SourceIsDSD: true, Kind: JobKindOptimize, TargetSampleRate: 48000, Quality: QualityHigh}, "a1|compact|48000|-h"},
		{"not DSD", JobSpec{Kind: JobKindOptimize, TargetSampleRate: 44100}, ""},
		{"no target", JobSpec{SourceIsDSD: true, Kind: JobKindOptimize}, ""},
	} {
		if got := tc.spec.DSDPeakProfile(); got != tc.want {
			t.Errorf("%s: profile %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := DSDPeakProfileFor(JobKindOptimize, 44100, "-v"); got != "a1|compact|44100|-v" {
		t.Errorf("DSDPeakProfileFor = %q", got)
	}
}

func TestAlbumClipGuardedGainDB(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name  string
		peaks []*float64
		want  float64
	}{
		{"the hottest track sets the boost", []*float64{f(-9), f(-3.2), f(-7)}, 2.2},
		{"a compliant album keeps +6", []*float64{f(-12), f(-7.5)}, 6.0},
		{"a silent track does not constrain", []*float64{nil, f(-4)}, 3.0},
		{"a non-finite peak is ignored", []*float64{f(math.NaN()), f(math.Inf(1)), f(-4)}, 3.0},
		{"no measurable peak at all gets the nominal +6", []*float64{nil, nil}, 6.0},
		{"empty", nil, 6.0},
		{"a hot album bottoms out at 0", []*float64{f(-0.5), f(-8)}, 0},
	} {
		if got := AlbumClipGuardedGainDB(tc.peaks); got != tc.want {
			t.Errorf("%s: %.2f, want %.2f", tc.name, got, tc.want)
		}
	}
}

func TestAlbumBoundedGain(t *testing.T) {
	for _, tc := range []struct {
		name         string
		track, album float64
		want         float64
	}{
		{"the album's lower figure wins", 6, 2.5, 2.5},
		{"never above the track's own guard", 3, 5, 3},
		{"clamped to the guard's range", 6, 9, 6},
		{"a negative figure cannot attenuate", 6, -2, 0},
		{"NaN is ignored", 4, math.NaN(), 4},
		{"infinity is ignored", 4, math.Inf(-1), 4},
	} {
		if got := albumBoundedGain(tc.track, tc.album); got != tc.want {
			t.Errorf("%s: %.1f, want %.1f", tc.name, got, tc.want)
		}
	}
}

// fakeGainer records every call the render makes.
type fakeGainer struct {
	mu       sync.Mutex
	gain     float64
	ok       bool
	err      error
	budget   time.Duration
	claims   int
	resolves []fakeResolution
	seenOwn  []*float64
}

type fakeResolution struct {
	tp  *float64
	err error
}

func (f *fakeGainer) SurveyBudget(context.Context, JobSpec) time.Duration { return f.budget }

func (f *fakeGainer) Claim(context.Context, JobSpec) func(*float64, error) {
	f.mu.Lock()
	f.claims++
	f.mu.Unlock()
	return func(tp *float64, err error) {
		f.mu.Lock()
		f.resolves = append(f.resolves, fakeResolution{tp, err})
		f.mu.Unlock()
	}
}

func (f *fakeGainer) AlbumGainDB(_ context.Context, _ JobSpec, own *float64) (float64, bool, error) {
	f.mu.Lock()
	f.seenOwn = append(f.seenOwn, own)
	f.mu.Unlock()
	return f.gain, f.ok, f.err
}

func mintTone(t *testing.T, dir, name string, dbfs float64) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if _, err := dsdtone.MintDSF(p, dsdtone.Tone{RateHz: 2822400, Seconds: 3.0, AmplitudeDBFS: dbfs}); err != nil {
		t.Fatal(err)
	}
	return p
}

// dsdToneSpec is the spec a render of the minted tone at src is queued
// with, stamped with the version of the file a scan records (Run renders a
// file only while it is still that version).
func dsdToneSpec(src, outDir, tempDir string, kind JobKind, rate, bits int) JobSpec {
	return stampedAsScanned(JobSpec{
		SourceAbsPath: src, SourceLibraryRel: "Album/" + filepath.Base(src),
		SourceSampleRate: 2822400, SourceIsDSD: true, SourceChannels: 2, SourceDurationSec: 3.0,
		TargetSampleRate: rate, TargetBits: bits, Quality: QualityVeryHigh,
		OutputDir: outDir, TempDir: tempDir, Kind: kind,
	})
}

// albumGainCase is one render of TestRunDSD_AlbumGain_RealToolchain.
type albumGainCase struct {
	name      string
	gainer    *fakeGainer
	wantGain  float64
	wantScope string
	wantRMS   float64
}

// TestRunDSD_AlbumGain_RealToolchain: Stage C applies the album's boost —
// measured in the published file's level, not just the reported number —
// bounded by the track's own guard, and the render hands the gainer its own
// Stage B peak exactly once.
func TestRunDSD_AlbumGain_RealToolchain(t *testing.T) {
	requireDSDToolchain(t)
	root := t.TempDir()
	src := mintTone(t, filepath.Join(root, "lib", "Album"), "tone-m20.dsf", -20)
	for i, tc := range []albumGainCase{
		{"the album's boost below the track's guard", &fakeGainer{gain: 2.0, ok: true}, 2.0, GainScopeAlbum, -21.01},
		{"an album figure above the guard is bounded by it", &fakeGainer{gain: 9.0, ok: true}, 6.0, GainScopeAlbum, -17.01},
		{"no album keeps the track's guard", &fakeGainer{ok: false}, 6.0, GainScopeTrack, -17.01},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := dsdToneSpec(src, filepath.Join(root, "variants", string(rune('a'+i))), filepath.Join(root, "tmp"),
				JobKindOptimize, 44100, 16)
			spec.AlbumGain = tc.gainer
			r, err := Run(context.Background(), spec)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			checkAlbumGainRender(t, spec, r, tc)
			checkGainerSawTheRendersPeak(t, tc.gainer, r)
		})
	}

	t.Run("a gainer error fails the render and publishes nothing", func(t *testing.T) {
		spec := dsdToneSpec(src, filepath.Join(root, "variants", "err"), filepath.Join(root, "tmp"), JobKindOptimize, 44100, 16)
		spec.AlbumGain = &fakeGainer{err: errors.New("store unreachable")}
		if _, err := Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "album gain") {
			t.Fatalf("err = %v, want the album gain failure", err)
		}
		if _, err := os.Stat(spec.SidecarPath()); !os.IsNotExist(err) {
			t.Errorf("a failed render published %s (stat err %v)", spec.SidecarPath(), err)
		}
	})
	if entries, err := os.ReadDir(renderScratchDir(filepath.Join(root, "tmp"))); err != nil || len(entries) != 0 {
		t.Errorf("scratch left behind: %v (err=%v)", entries, err)
	}
}

// checkAlbumGainRender asserts what one render published and reported: the
// applied gain, the file's level carrying it, the scope and the track's own
// guard in the settings, and the peak profile.
func checkAlbumGainRender(t *testing.T, spec JobSpec, r RunResult, tc albumGainCase) {
	t.Helper()
	if r.AppliedGainDB == nil {
		t.Fatal("a DSD rendition must report its applied gain")
	}
	if *r.AppliedGainDB != tc.wantGain {
		t.Fatalf("applied gain %.1f, want %.1f", *r.AppliedGainDB, tc.wantGain)
	}
	if _, rms := soxStats(t, spec.SidecarPath()); math.Abs(rms-tc.wantRMS) > 0.05 {
		t.Errorf("RMS %.2f dB, want %.2f ± 0.05 — the file must carry the applied gain", rms, tc.wantRMS)
	}
	v, ok := ParseSoxSettings(r.Settings)
	if !ok || v.GainScope != tc.wantScope || v.TrackGainDB == nil || *v.TrackGainDB != 6.0 {
		t.Errorf("settings %+v: want scope %q and the track's own guard 6.0", v, tc.wantScope)
	}
	if r.PeakProfile != "a1|compact|44100|-v" {
		t.Errorf("PeakProfile %q", r.PeakProfile)
	}
}

// checkGainerSawTheRendersPeak: the render claimed its track once, resolved
// the claim with its own Stage B peak, and handed the gainer that peak once.
func checkGainerSawTheRendersPeak(t *testing.T, g *fakeGainer, r RunResult) {
	t.Helper()
	if g.claims != 1 || len(g.resolves) != 1 || g.resolves[0].err != nil || g.resolves[0].tp == nil ||
		r.TruePeakDBTP == nil || *g.resolves[0].tp != *r.TruePeakDBTP {
		t.Errorf("claim: %d claims, resolutions %+v; want one, resolved with the render's own peak %v",
			g.claims, g.resolves, r.TruePeakDBTP)
	}
	if len(g.seenOwn) != 1 || g.seenOwn[0] == nil || r.TruePeakDBTP == nil || *g.seenOwn[0] != *r.TruePeakDBTP {
		t.Errorf("AlbumGainDB saw %v, want the render's own peak once", g.seenOwn)
	}
}

// TestRunDSD_ClaimResolvedWhenTheRenderFailsEarly: a render that fails
// before its Stage B still resolves its claim — with an error — so an
// album-mate's survey waiting on it is never left waiting.
func TestRunDSD_ClaimResolvedWhenTheRenderFailsEarly(t *testing.T) {
	requireDSDToolchain(t)
	root := t.TempDir()
	src := mintTone(t, filepath.Join(root, "lib", "Album"), "tone.dsf", -20)
	spec := dsdToneSpec(src, filepath.Join(root, "variants"), filepath.Join(root, "tmp"), JobKindOptimize, 44100, 16)
	spec.SourceSampleRate = 5644800 // the file is DSD64: a geometry mismatch
	g := &fakeGainer{gain: 2, ok: true}
	spec.AlbumGain = g
	if _, err := Run(context.Background(), spec); !errors.Is(err, ErrDSDGeometryMismatch) {
		t.Fatalf("err = %v, want the geometry refusal", err)
	}
	if g.claims != 1 || len(g.resolves) != 1 || g.resolves[0].err == nil {
		t.Errorf("claims %d, resolutions %+v: want one, resolved with an error", g.claims, g.resolves)
	}
	if len(g.seenOwn) != 0 {
		t.Error("no album decision may be asked for without a Stage B peak")
	}
}

// TestMeasureDSDPeakMatchesTheRender: the survey's measurement is the
// render's own Stages A and B, so it reports exactly the peak a render
// measures, on both tiers, and leaves no scratch behind.
func TestMeasureDSDPeakMatchesTheRender(t *testing.T) {
	requireDSDToolchain(t)
	root := t.TempDir()
	src := mintTone(t, filepath.Join(root, "lib", "Album"), "tone-m6.dsf", -6)
	tempDir := filepath.Join(root, "tmp")
	for _, tier := range []struct {
		kind       JobKind
		rate, bits int
	}{{JobKindOptimize, 44100, 16}, {JobKindPCMRender, 176400, 24}} {
		spec := dsdToneSpec(src, filepath.Join(root, "variants"), tempDir, tier.kind, tier.rate, tier.bits)
		r, err := Run(context.Background(), spec)
		if err != nil {
			t.Fatalf("%s Run: %v", tier.kind, err)
		}
		m, err := MeasureDSDPeak(context.Background(), spec)
		if err != nil {
			t.Fatalf("%s MeasureDSDPeak: %v", tier.kind, err)
		}
		if m == nil || r.TruePeakDBTP == nil || math.Abs(*m-*r.TruePeakDBTP) > 1e-9 {
			t.Errorf("%s: measured %v, render measured %v — they must agree exactly", tier.kind, m, r.TruePeakDBTP)
		}
	}
	if entries, err := os.ReadDir(renderScratchDir(tempDir)); err != nil || len(entries) != 0 {
		t.Errorf("scratch left behind: %v (err=%v)", entries, err)
	}
}

func TestMeasureDSDPeakRefusesWhatRunRefuses(t *testing.T) {
	ctx := context.Background()
	if _, err := MeasureDSDPeak(ctx, JobSpec{SourceLibraryRel: "a.flac", SourceAbsPath: "/x/a.flac"}); err == nil {
		t.Error("a PCM source must be refused")
	}
	_, err := MeasureDSDPeak(ctx, JobSpec{SourceIsDSD: true, SourceLibraryRel: "a.flac", SourceAbsPath: "/x/a.flac",
		TargetSampleRate: 44100, TargetBits: 16})
	if !errors.Is(err, ErrDSDDecodeUnavailable) {
		t.Errorf("err = %v, want ErrDSDDecodeUnavailable for a DSD-flagged file that does not route to the DSD chain", err)
	}
}

// TestPoolGivesDSDJobsTheAlbumGainerAndItsSurveyDeadline: the pool injects
// its gainer into DSD specs when they run — whatever path enqueued them —
// and widens the deadline by the survey the gainer reports. PCM jobs get
// neither.
func TestPoolGivesDSDJobsTheAlbumGainerAndItsSurveyDeadline(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	g := &fakeGainer{budget: 30 * time.Minute}
	p.SetAlbumGainer(g)
	type seen struct {
		gainer AlbumGainer
		budget time.Duration
	}
	got := make(chan seen, 2)
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		d, _ := ctx.Deadline()
		got <- seen{spec.AlbumGain, time.Until(d)}
		return RunResult{}, errors.New("stub runner")
	}
	next := func() seen {
		t.Helper()
		select {
		case s := <-got:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("the job never ran")
			return seen{}
		}
	}

	dsd := JobSpec{SourceLibraryRel: "DSD/01.dsf", SourceAbsPath: "/dev/null/01.dsf", SourceIsDSD: true,
		SourceSampleRate: 2822400, SourceDurationSec: 60, Kind: JobKindOptimize,
		TargetSampleRate: 44100, TargetBits: 16, Quality: QualityVeryHigh, OutputDir: t.TempDir()}
	if err := p.Enqueue(dsd); err != nil {
		t.Fatal(err)
	}
	d := next()
	if d.gainer != g {
		t.Errorf("DSD job ran with gainer %v, want the pool's", d.gainer)
	}
	if want := defaultJobTimeout + 30*time.Minute; d.budget > want || d.budget < want-time.Minute {
		t.Errorf("DSD job deadline %v away, want about %v (base + survey)", d.budget, want)
	}

	pcm := JobSpec{SourceLibraryRel: "PCM/01.flac", SourceAbsPath: "/dev/null/01.flac",
		SourceSampleRate: 96000, Kind: JobKindOptimize, TargetSampleRate: 48000, TargetBits: 16,
		Quality: QualityVeryHigh, OutputDir: t.TempDir()}
	if err := p.Enqueue(pcm); err != nil {
		t.Fatal(err)
	}
	c := next()
	if c.gainer != nil {
		t.Errorf("a PCM job must not carry an album gainer, got %v", c.gainer)
	}
	if c.budget > defaultJobTimeout {
		t.Errorf("PCM job deadline %v away, want at most the base %v", c.budget, defaultJobTimeout)
	}
}

func TestAlbumSurveyTimeout(t *testing.T) {
	if got := albumSurveyTimeout(10*time.Minute, 0); got != 10*time.Minute {
		t.Errorf("no survey: %v", got)
	}
	if got := albumSurveyTimeout(10*time.Minute, 20*time.Minute); got != 30*time.Minute {
		t.Errorf("survey adds: %v", got)
	}
	if got := albumSurveyTimeout(maxJobTimeout, 10*time.Hour); got != 2*maxJobTimeout {
		t.Errorf("capped: %v, want %v", got, 2*maxJobTimeout)
	}
}
