package transcode

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The coordinator half of the DSD renditions: the compact tier riding the
// optimize walk behind the wired caps, the faithful `pcm` submit trio, the
// batch kind stamps, the JobSpec facts the pool and chain consume, and the
// scratch pre-flight on the temp volume.

var (
	dsdCapsOn      = DSDRenderCaps{Enabled: true, DecodeDSD: true}
	dsdCapsWithDST = DSDRenderCaps{Enabled: true, DecodeDSD: true, DecodeDST: true}
)

func capsFn(c DSDRenderCaps) func() DSDRenderCaps { return func() DSDRenderCaps { return c } }

// seedDSDBatchFixture plants one folder that exercises every arm of the
// DSD gates: an eligible DSF (44.1k family), a DST DFF (needs the dst
// decoder), a 48k-family DFF, an SACD virtual row (never), a PCM FLAC
// (optimize-eligible at its 48k-family floor, never pcm), and an off-family DSD header (never).
func seedDSDBatchFixture(t *testing.T, s *manifest.Store) {
	t.Helper()
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "DSD"}); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		path, codec, compression string
		rate                     float64
		bits                     int
		isDSD                    bool
	}{
		{"DSD/01.dsf", "DSF", "", 2822400, 1, true},
		{"DSD/02.dff", "DFF", "DST", 2822400, 1, true},
		{"DSD/03.dff", "DFF", "", 3072000, 1, true},
		{"DSD/Disc.iso/st/01.dff", "DFF", "", 2822400, 1, true},
		{"DSD/04.flac", "FLAC", "", 96000, 24, false},
		{"DSD/05.dsf", "DSF", "", 3000000, 1, true},
	}
	for _, r := range rows {
		rate, bits, isDSD := r.rate, r.bits, r.isDSD
		if err := s.UpsertTrack(context.Background(), &manifest.Track{
			Path: r.path, Size: 300_000_000, Codec: r.codec, Compression: r.compression,
			SampleRate: &rate, BitsPerSample: &bits, IsDSD: &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", r.path, err)
		}
	}
}

func seedVariantOn(t *testing.T, s *manifest.Store, path, variantID string, rate, bits int) {
	t.Helper()
	if err := s.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: path, VariantID: variantID, SidecarPath: "/tmp/" + variantID + ".flac", Format: "flac",
		SampleRate: rate, BitsPerSample: bits, SizeBytes: 1, SourceMTimeNS: 1, SourceSize: 300_000_000,
		SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// inflightKeys returns the pool's raw `path|variantID` in-flight keys —
// the variant ID half is what distinguishes the families.
func inflightKeys(p *Pool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.inflight))
	for key := range p.inflight {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func batchRows(t *testing.T, s *manifest.Store) []manifest.UpscaleBatchRow {
	t.Helper()
	rows, err := s.ListUpscaleBatches(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListUpscaleBatches: %v", err)
	}
	return rows
}

func joined(xs []string) string { return strings.Join(xs, "|") }

// TestSubmitOptimize_DSDNeedsWiredCaps — the compact tier: an unwired
// coordinator and one with zero caps skip every DSD row exactly as
// before; with the caps on, the DSF and the 48k DFF join the FLAC under
// their own `optimized-dsd-` ids; the DST DFF joins only with the dst
// decoder; the SACD virtual row and the off-family header never do.
func TestSubmitOptimize_DSDNeedsWiredCaps(t *testing.T) {
	cases := []struct {
		name    string
		wire    func(*Coordinator)
		want    []string
		skipped int
	}{
		{"unwired", func(*Coordinator) {}, []string{"DSD/04.flac|optimized-v2-48000-16"}, 5},
		{"zero caps", func(c *Coordinator) { c.WithDSDRender(capsFn(DSDRenderCaps{})) }, []string{"DSD/04.flac|optimized-v2-48000-16"}, 5},
		{"caps on", func(c *Coordinator) { c.WithDSDRender(capsFn(dsdCapsOn)) },
			[]string{"DSD/01.dsf|optimized-dsd-v1-44100-16", "DSD/03.dff|optimized-dsd-v1-48000-16", "DSD/04.flac|optimized-v2-48000-16"}, 3},
		{"caps with dst", func(c *Coordinator) { c.WithDSDRender(capsFn(dsdCapsWithDST)) },
			[]string{"DSD/01.dsf|optimized-dsd-v1-44100-16", "DSD/02.dff|optimized-dsd-v1-44100-16", "DSD/03.dff|optimized-dsd-v1-48000-16", "DSD/04.flac|optimized-v2-48000-16"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTempStoreForBatch(t)
			t.Cleanup(func() { _ = s.Close() })
			seedDSDBatchFixture(t, s)
			c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
			blockRunner(t, p)
			tc.wire(c)

			res, err := c.SubmitOptimize(context.Background(), "DSD", t.TempDir())
			if err != nil {
				t.Fatalf("SubmitOptimize: %v", err)
			}
			if got := inflightKeys(p); joined(got) != joined(tc.want) {
				t.Errorf("enqueued %v, want %v", got, tc.want)
			}
			if res.EnqueuedCount != len(tc.want) || res.TargetBits != 16 {
				t.Errorf("result = %+v, want %d enqueued at 16 bits", res, len(tc.want))
			}
			rows := batchRows(t, s)
			if len(rows) != 1 || rows[0].Kind != "optimize" || rows[0].TargetBits != 16 || rows[0].SkippedFiles != tc.skipped {
				t.Errorf("batch row = %+v, want kind optimize / 16 bits / %d skipped", rows, tc.skipped)
			}
		})
	}
}

// TestSubmitPCMRender_MixedAlbumSkipsNonDSD — the faithful tier over a
// mixed folder renders the eligible DSD rows at their family's 4× base
// and SKIPS the rest (counted, never an error): the FLAC, the SACD
// virtual row, the off-family header, and the DST DFF until the dst
// decoder is present.
func TestSubmitPCMRender_MixedAlbumSkipsNonDSD(t *testing.T) {
	cases := []struct {
		name    string
		caps    DSDRenderCaps
		want    []string
		skipped int
	}{
		{"caps on", dsdCapsOn, []string{"DSD/01.dsf|pcm-v1-176400-24", "DSD/03.dff|pcm-v1-192000-24"}, 4},
		{"caps with dst", dsdCapsWithDST, []string{"DSD/01.dsf|pcm-v1-176400-24", "DSD/02.dff|pcm-v1-176400-24", "DSD/03.dff|pcm-v1-192000-24"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTempStoreForBatch(t)
			t.Cleanup(func() { _ = s.Close() })
			seedDSDBatchFixture(t, s)
			c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
			blockRunner(t, p)
			c.WithDSDRender(capsFn(tc.caps))

			res, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir())
			if err != nil {
				t.Fatalf("SubmitPCMRender: %v", err)
			}
			if got := inflightKeys(p); joined(got) != joined(tc.want) {
				t.Errorf("enqueued %v, want %v", got, tc.want)
			}
			if res.EnqueuedCount != len(tc.want) || res.TotalFiles != len(tc.want) || res.TargetBits != 24 || res.TargetRate != 0 {
				t.Errorf("result = %+v, want %d enqueued, 24 bits, per-track rate", res, len(tc.want))
			}
			rows := batchRows(t, s)
			if len(rows) != 1 {
				t.Fatalf("batch rows = %+v, want exactly one", rows)
			}
			if r := rows[0]; r.Kind != "pcm" || r.TargetBits != 24 || r.TargetRate != 0 ||
				r.SkippedFiles != tc.skipped || r.TotalFiles != len(tc.want) || r.Status != "running" {
				t.Errorf("batch row = %+v, want kind pcm / 24 bits / %d skipped / %d total / running", r, tc.skipped, len(tc.want))
			}
		})
	}
}

// TestSubmitPCMRender_CoverageIsThePCMPrefixOnly — a `pcm-` variant is
// coverage for the faithful tier and an `optimized-dsd-` one is not,
// while for the compact tier it is the other way round (its coverage
// prefix is `optimized-`, which the DSD family starts with).
func TestSubmitPCMRender_CoverageIsThePCMPrefixOnly(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedDSDBatchFixture(t, s)
	seedVariantOn(t, s, "DSD/01.dsf", "pcm-v1-176400-24", 176400, 24)
	seedVariantOn(t, s, "DSD/03.dff", "optimized-dsd-v1-48000-16", 48000, 16)
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)
	c.WithDSDRender(capsFn(dsdCapsOn))

	pcm, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPCMRender: %v", err)
	}
	if pcm.AlreadyCovered != 1 || pcm.EnqueuedCount != 1 {
		t.Errorf("pcm result = %+v, want the pcm-covered DSF skipped and the optimized-dsd DFF enqueued", pcm)
	}
	if got, want := inflightKeys(p), []string{"DSD/03.dff|pcm-v1-192000-24"}; joined(got) != joined(want) {
		t.Errorf("pcm enqueued %v, want %v", got, want)
	}

	opt, err := c.SubmitOptimize(context.Background(), "DSD", t.TempDir())
	if err != nil {
		t.Fatalf("SubmitOptimize: %v", err)
	}
	if opt.AlreadyCovered != 1 || opt.EnqueuedCount != 2 {
		t.Errorf("optimize result = %+v, want the optimized-dsd DFF covered and DSF + FLAC enqueued", opt)
	}
}

// TestSubmitPCMRender_EmptyBatchCompletesSynchronously — with no caps
// nothing under the folder is a candidate; the row must land completed
// with its kind rather than sit pending for a callback that never comes.
func TestSubmitPCMRender_EmptyBatchCompletesSynchronously(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedDSDBatchFixture(t, s)
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)

	res, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPCMRender: %v", err)
	}
	if res.EnqueuedCount != 0 || res.TotalFiles != 0 || res.TargetBits != 24 {
		t.Errorf("result = %+v, want an empty 24-bit batch", res)
	}
	rows := batchRows(t, s)
	if len(rows) != 1 || rows[0].Status != "completed" || rows[0].Kind != "pcm" || rows[0].SkippedFiles != 6 {
		t.Fatalf("batch rows = %+v, want one completed pcm row with all six skipped", rows)
	}
}

// TestSubmitPCMRender_RequiresResolver mirrors the optimize refusal: no
// resolver, no JobSpec absolute paths, no batch.
func TestSubmitPCMRender_RequiresResolver(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir()); err == nil {
		t.Error("SubmitPCMRender without a resolver = nil error, want a refusal")
	}
	if _, err := c.SubmitPCMRenderPaths(context.Background(), "x", []string{"DSD/01.dsf"}, t.TempDir()); err == nil {
		t.Error("SubmitPCMRenderPaths without a resolver = nil error, want a refusal")
	}
}

// TestSubmitPCMRenderPaths_EnqueuesOnlyTheGivenSet — the identity form
// renders the set it was handed, not the folder around it.
func TestSubmitPCMRenderPaths_EnqueuesOnlyTheGivenSet(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedDSDBatchFixture(t, s)
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)
	c.WithDSDRender(capsFn(dsdCapsWithDST))

	res, err := c.SubmitPCMRenderPaths(context.Background(), "DSD/pick", []string{"DSD/01.dsf", "DSD/04.flac"}, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPCMRenderPaths: %v", err)
	}
	if got, want := inflightKeys(p), []string{"DSD/01.dsf|pcm-v1-176400-24"}; joined(got) != joined(want) {
		t.Errorf("enqueued %v, want %v", got, want)
	}
	if res.EnqueuedCount != 1 {
		t.Errorf("EnqueuedCount = %d, want 1 (the FLAC in the set is skipped, not an error)", res.EnqueuedCount)
	}
}

// TestEnqueuedDSDJobSpecsCarryTheRenderFacts — what the pool and the
// chain consume: the DSD flag, the compression tag, the kind, the bits,
// the family rate, and the scratch directory the coordinator was wired
// with. Read off the specs the workers receive.
func TestEnqueuedDSDJobSpecsCarryTheRenderFacts(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedDSDBatchFixture(t, s)
	p := NewPool(s, 8, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	specs := make(chan JobSpec, 16)
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		specs <- spec
		<-ctx.Done()
		return RunResult{SizeBytes: spec.SourceSize}, nil
	}
	c, err := NewCoordinator(p, s, t.TempDir(), nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatal(err)
	}
	c.WithDSDRender(capsFn(dsdCapsWithDST)).WithRenderTempDir("/scratch/render")

	if _, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir()); err != nil {
		t.Fatalf("SubmitPCMRender: %v", err)
	}
	if _, err := c.SubmitOptimize(context.Background(), "DSD", t.TempDir()); err != nil {
		t.Fatalf("SubmitOptimize: %v", err)
	}
	// 3 pcm + 4 optimize (01, 02, 03 DSD + 04 FLAC) jobs reach the workers.
	got := map[string]JobSpec{}
	deadline := time.After(5 * time.Second)
	for len(got) < 7 {
		select {
		case sp := <-specs:
			got[sp.SourceLibraryRel+"|"+sp.VariantID()] = sp
		case <-deadline:
			t.Fatalf("only %d of 7 specs reached the workers: %v", len(got), got)
		}
	}
	check := func(key string, isDSD bool, compression string, kind JobKind, rate, bits int) {
		t.Helper()
		sp, ok := got[key]
		if !ok {
			t.Errorf("no spec for %s", key)
			return
		}
		if sp.SourceIsDSD != isDSD || sp.SourceCompression != compression || sp.Kind != kind ||
			sp.TargetSampleRate != rate || sp.TargetBits != bits || sp.TempDir != "/scratch/render" {
			t.Errorf("%s: spec = %+v, want isDSD=%v compression=%q kind=%s %d/%d tempDir=/scratch/render",
				key, sp, isDSD, compression, kind, rate, bits)
		}
	}
	check("DSD/01.dsf|pcm-v1-176400-24", true, "", JobKindPCMRender, 176400, 24)
	check("DSD/02.dff|pcm-v1-176400-24", true, "DST", JobKindPCMRender, 176400, 24)
	check("DSD/03.dff|pcm-v1-192000-24", true, "", JobKindPCMRender, 192000, 24)
	check("DSD/01.dsf|optimized-dsd-v1-44100-16", true, "", JobKindOptimize, 44100, 16)
	check("DSD/02.dff|optimized-dsd-v1-44100-16", true, "DST", JobKindOptimize, 44100, 16)
	check("DSD/03.dff|optimized-dsd-v1-48000-16", true, "", JobKindOptimize, 48000, 16)
	check("DSD/04.flac|optimized-v2-48000-16", false, "", JobKindOptimize, 48000, 16)
	// The nominal DSD rate rides SourceSampleRate — what the chain's
	// geometry check and the pool's timeout derive from.
	if sp := got["DSD/03.dff|pcm-v1-192000-24"]; sp.SourceSampleRate != 3072000 || sp.SourceBits != 1 {
		t.Errorf("DSD spec source geometry = %d/%d, want the nominal 3072000/1", sp.SourceSampleRate, sp.SourceBits)
	}
}

// TestBatchRowsCarryTheirKind — the three submit paths stamp the v43
// `kind`, so the Jobs page no longer has to infer optimize from a
// (0, 16) sentinel and can tell a pcm batch from either.
func TestBatchRowsCarryTheirKind(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)
	seedDSDBatchFixture(t, s)
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)
	c.WithDSDRender(capsFn(dsdCapsOn))

	if _, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := c.SubmitOptimize(context.Background(), "Album", t.TempDir()); err != nil {
		t.Fatalf("SubmitOptimize: %v", err)
	}
	if _, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir()); err != nil {
		t.Fatalf("SubmitPCMRender: %v", err)
	}
	kinds := map[string]int{}
	for _, r := range batchRows(t, s) {
		kinds[r.Kind]++
	}
	if kinds["upscale"] != 1 || kinds["optimize"] != 1 || kinds["pcm"] != 1 || len(kinds) != 3 {
		t.Errorf("batch kinds = %v, want one each of upscale / optimize / pcm", kinds)
	}
}

// TestDSDSizeDerivedDurationSec pins the one derivation the pool's timeout
// and the coordinator's scratch pre-flight share.
func TestDSDSizeDerivedDurationSec(t *testing.T) {
	const dsd64StereoHour = 2822400 / 8 * 2 * 3600
	cases := []struct {
		name     string
		size     int64
		rate, ch int
		want     float64
	}{
		{"an hour of stereo DSD64", dsd64StereoHour, 2822400, 2, 3600},
		{"unknown channels assume stereo", dsd64StereoHour, 2822400, 0, 3600},
		{"six channels shorten it", dsd64StereoHour, 2822400, 6, 1200},
		{"the nominal rate, not fs/8", dsd64StereoHour, 352800, 2, 28800},
		{"no size", 0, 2822400, 2, 0},
		{"no rate", dsd64StereoHour, 0, 2, 0},
	}
	for _, c := range cases {
		if got := dsdSizeDerivedDurationSec(c.size, c.rate, c.ch); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBuildPCMRenderCandidates_ScratchIsTheLargestSingleJob — the
// pre-flight budgets the LARGEST intermediate, not the sum, at the tier's
// target rate: an hour of DSD64 stereo is 5.08 GB at 176.4 kHz and
// 1.27 GB at 44.1 kHz, whatever the shorter neighbour needs.
func TestBuildPCMRenderCandidates_ScratchIsTheLargestSingleJob(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)
	c.WithDSDRender(capsFn(dsdCapsOn))
	const hour = 2822400 / 8 * 2 * 3600
	projections := []manifest.TrackProjection{
		{Path: "DSD/long.dsf", Size: hour, SampleRate: 2822400, BitsPerSample: 1, Codec: "DSF", IsDSD: true},
		{Path: "DSD/short.dsf", Size: hour / 6, SampleRate: 2822400, BitsPerSample: 1, Codec: "DSF", IsDSD: true},
		{Path: "DSD/pcm.flac", Size: hour, SampleRate: 96000, BitsPerSample: 24, Codec: "FLAC"},
	}
	pcm := c.buildPCMRenderCandidates("DSD", projections)
	if want := TempBytesForRender(2, 176400, 3600); pcm.maxRenderScratch != want || len(pcm.cands) != 2 {
		t.Errorf("pcm scratch = %d (%d candidates), want %d for the hour-long DSF", pcm.maxRenderScratch, len(pcm.cands), want)
	}
	opt := c.buildOptimizeCandidates("DSD", projections)
	if want := TempBytesForRender(2, 44100, 3600); opt.maxRenderScratch != want || len(opt.cands) != 3 {
		t.Errorf("optimize scratch = %d (%d candidates), want %d — the FLAC adds none", opt.maxRenderScratch, len(opt.cands), want)
	}
	if opt.cands[2].isDSD || opt.cands[0].kind != JobKindOptimize || opt.cands[0].targetBits != 16 {
		t.Errorf("optimize candidates = %+v, want the FLAC un-flagged and the DSD rows at kind optimize / 16", opt.cands)
	}
}

// TestSubmitPCMRender_ScratchPreflightGradesTheTempVolume — a source
// whose intermediate could never fit refuses on the SCRATCH volume, and
// the typed error names that directory rather than the sidecar volume.
func TestSubmitPCMRender_ScratchPreflightGradesTheTempVolume(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	rate, bits, isDSD := 2822400.0, 1, true
	if err := s.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Huge/01.dsf", Size: 1 << 60, Codec: "DSF", SampleRate: &rate, BitsPerSample: &bits, IsDSD: &isDSD,
	}); err != nil {
		t.Fatal(err)
	}
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)
	tempDir := filepath.Join(t.TempDir(), "scratch-not-created-yet")
	c.WithDSDRender(capsFn(dsdCapsOn)).WithRenderTempDir(tempDir)

	_, err := c.SubmitPCMRender(context.Background(), "Huge", t.TempDir())
	var dskErr *InsufficientDiskSpaceError
	if !errors.As(err, &dskErr) {
		t.Fatalf("SubmitPCMRender: want *InsufficientDiskSpaceError, got %v", err)
	}
	if want := renderScratchDir(tempDir); dskErr.Dir != want {
		t.Errorf("disk check graded %q, want the render scratch dir %q", dskErr.Dir, want)
	}
	if rows := batchRows(t, s); len(rows) != 0 {
		t.Errorf("a refused pre-flight must not leave a batch row: %+v", rows)
	}
}
