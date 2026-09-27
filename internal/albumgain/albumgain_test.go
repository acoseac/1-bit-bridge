package albumgain

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// ── fakes ──────────────────────────────────────────────────────────────────

type fakeCatalog struct {
	refs    []manifest.CatalogRef
	streams atomic.Int32
}

func (f *fakeCatalog) StreamDSDCatalogRefs(_ context.Context, fn func(manifest.CatalogRef) error) error {
	f.streams.Add(1)
	for _, r := range f.refs {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

type fakePeaks struct {
	mu      sync.Mutex
	rows    map[claimKey]manifest.DSDPeak
	upserts []manifest.DSDPeak
}

func newFakePeaks() *fakePeaks { return &fakePeaks{rows: map[claimKey]manifest.DSDPeak{}} }

func (f *fakePeaks) FreshDSDPeaks(_ context.Context, profile string, paths []string) (map[string]manifest.DSDPeak, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]manifest.DSDPeak{}
	for _, p := range paths {
		if row, ok := f.rows[claimKey{path: p, profile: profile}]; ok {
			out[p] = row
		}
	}
	return out, nil
}

func (f *fakePeaks) UpsertDSDPeak(_ context.Context, p manifest.DSDPeak) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[claimKey{path: p.SourcePath, profile: p.Profile}] = p
	f.upserts = append(f.upserts, p)
	return nil
}

func (f *fakePeaks) put(path, profile string, tp *float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[claimKey{path: path, profile: profile}] = manifest.DSDPeak{SourcePath: path, Profile: profile, TruePeakDBTP: tp}
}

type fakeMeasurer struct {
	mu    sync.Mutex
	peaks map[string]*float64
	errs  map[string]error
	calls map[string]int
	delay time.Duration
}

func (f *fakeMeasurer) measure(ctx context.Context, j transcode.JobSpec) (*float64, error) {
	f.mu.Lock()
	f.calls[j.SourceLibraryRel]++
	tp, err := f.peaks[j.SourceLibraryRel], f.errs[j.SourceLibraryRel]
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return tp, err
}

func (f *fakeMeasurer) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func (f *fakeMeasurer) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func fp(v float64) *float64 { return &v }

const dsd64, dsd128, dsd256, dsd64x48 = 2822400, 5644800, 11289600, 3072000

func dsdRef(path, album, albumArtist string, year, rate int) manifest.CatalogRef {
	return manifest.CatalogRef{
		Path: path, Title: path, Album: album, AlbumArtist: albumArtist, Artist: albumArtist,
		Year: year, SampleRate: rate, BitsPerSample: 1, IsDSD: true, Codec: "DSF",
		Duration: 300, Size: 1 << 20, MTimeNS: 1,
	}
}

type harness struct {
	cat   *fakeCatalog
	peaks *fakePeaks
	meas  *fakeMeasurer
	rates map[string]int
	now   time.Time
	r     *Resolver
}

func newHarness(t *testing.T, refs ...manifest.CatalogRef) *harness {
	t.Helper()
	h := &harness{
		cat:   &fakeCatalog{refs: refs},
		peaks: newFakePeaks(),
		meas:  &fakeMeasurer{peaks: map[string]*float64{}, errs: map[string]error{}, calls: map[string]int{}},
		rates: map[string]int{},
		now:   time.Unix(1_800_000_000, 0),
	}
	for _, ref := range refs {
		h.rates[ref.Path] = ref.SampleRate
	}
	r, err := New(Config{
		Catalog: h.cat,
		Peaks:   h.peaks,
		SpecFor: func(_ context.Context, path string, like transcode.JobSpec) (transcode.JobSpec, error) {
			rate, ok := h.rates[path]
			if !ok {
				return transcode.JobSpec{}, errors.New("no such track")
			}
			s := like
			s.SourceLibraryRel, s.SourceAbsPath, s.SourceSampleRate = path, "/lib/"+path, rate
			s.AlbumGain = nil
			return s, nil
		},
		Measure: h.meas.measure,
		Now:     func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	return h
}

func compactSpec(path string, rate int) transcode.JobSpec {
	return transcode.JobSpec{
		SourceLibraryRel: path, SourceAbsPath: "/lib/" + path, SourceIsDSD: true, SourceSampleRate: rate,
		SourceMTimeNS: 7, SourceSize: 9,
		Kind: transcode.JobKindOptimize, TargetSampleRate: transcode.TargetRateForOptimize(rate), TargetBits: 16,
		Quality: transcode.QualityVeryHigh,
	}
}

func faithfulSpec(path string, rate int) transcode.JobSpec {
	s := compactSpec(path, rate)
	s.Kind, s.TargetSampleRate, s.TargetBits = transcode.JobKindPCMRender, transcode.TargetRateForPCMRender(rate), 24
	return s
}

// ── membership ─────────────────────────────────────────────────────────────

// TestIndexGroupsLikeTheAdminCatalog pins the album identity to the admin
// catalog's own: two DSD tracks share an album in the index exactly when
// librarycat puts them in one album. Case / spacing of the album artist,
// discs of one set, a compilation, a folder-named untagged album and a
// year split all exercise dupes.Resolve. Routed rows and SACD virtual
// tracks are left out of the index — they can never be rendered here.
func TestIndexGroupsLikeTheAdminCatalog(t *testing.T) {
	refs := []manifest.CatalogRef{
		dsdRef("Pink Floyd/DSOTM/01.dsf", "The Dark Side of the Moon", "Pink Floyd", 1973, dsd64),
		dsdRef("Pink Floyd/DSOTM/02.dsf", "The Dark Side of the Moon", "pink floyd ", 1973, dsd64),
		dsdRef("Box/CD1/01.dsf", "Box Set", "Artist B", 2001, dsd64),
		dsdRef("Box/CD2/01.dsf", "Box Set", "Artist B", 2001, dsd64),
		dsdRef("Comp/01.dsf", "Hits", "Various Artists", 1999, dsd64),
		dsdRef("Comp/02.dsf", "Hits", "Various Artists", 1999, dsd64),
		dsdRef("Artist D/Untagged Folder/01.dsf", "", "", 0, dsd64),
		dsdRef("Artist D/Untagged Folder/02.dsf", "", "", 0, dsd64),
		dsdRef("Same/1990/01.dsf", "Same Title", "Same Artist", 1990, dsd64),
		dsdRef("Same/2000/01.dsf", "Same Title", "Same Artist", 2000, dsd64),
	}
	refs[3].Disc, refs[3].DiscTagged = 2, true
	refs[2].Disc, refs[2].DiscTagged = 1, true
	refs[5].Artist = "Someone Else"
	routed := dsdRef("Pink Floyd/DSOTM/03.dsf", "The Dark Side of the Moon", "Pink Floyd", 1973, dsd64)
	routed.RoutedUDN = "uuid:server"
	virtual := dsdRef("Pink Floyd/SACD.iso/st/01.dff", "The Dark Side of the Moon", "Pink Floyd", 1973, dsd64)
	all := append(append([]manifest.CatalogRef{}, refs...), routed, virtual)

	h := newHarness(t, all...)
	ix, err := h.r.currentIndex(context.Background(), refs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, in := ix.albumOf[routed.Path]; in {
		t.Error("a routed row cannot be rendered here and must not be an album member")
	}
	if _, in := ix.albumOf[virtual.Path]; in {
		t.Error("an SACD virtual track is never rendered on the bridge and must not be an album member")
	}

	b := librarycat.New()
	for _, r := range all {
		b.Add(librarycat.Row{
			Path: r.Path, Title: r.Title, Artist: r.Artist, AlbumArtist: r.AlbumArtist, Album: r.Album, Year: r.Year,
			Disc: r.Disc, DiscTagged: r.DiscTagged, Track: r.Track, TrackTagged: r.TrackTagged,
			Size: r.Size, Duration: r.Duration, SampleRate: r.SampleRate, BitsPerSample: r.BitsPerSample,
			IsDSD: r.IsDSD, Codec: r.Codec, RoutedUDN: r.RoutedUDN,
		})
	}
	cat := b.Build(h.now)
	for i := range refs {
		for k := range refs {
			a, bp := refs[i].Path, refs[k].Path
			ci, _ := cat.AlbumIDForPath(a)
			ck, _ := cat.AlbumIDForPath(bp)
			if (ix.albumOf[a] == ix.albumOf[bp]) != (ci == ck) {
				t.Errorf("%s / %s: index says same=%v, catalog says same=%v", a, bp, ix.albumOf[a] == ix.albumOf[bp], ci == ck)
			}
		}
	}
	// And the grouping is the intended one, not merely consistent.
	if ix.albumOf[refs[0].Path] != ix.albumOf[refs[1].Path] || ix.albumOf[refs[2].Path] != ix.albumOf[refs[3].Path] ||
		ix.albumOf[refs[4].Path] != ix.albumOf[refs[5].Path] || ix.albumOf[refs[6].Path] != ix.albumOf[refs[7].Path] ||
		ix.albumOf[refs[8].Path] == ix.albumOf[refs[9].Path] {
		t.Errorf("unexpected grouping: %v", ix.albumOf)
	}
}

// TestAlbumMatesStayOnTheJobsProfile: DSD64/128/256 of one album decode to
// the same 44.1 kHz and 176.4 kHz intermediates, so they share a boost; a
// 48k-family track of the same album renders to another rate and does not.
func TestAlbumMatesStayOnTheJobsProfile(t *testing.T) {
	h := newHarness(t,
		dsdRef("A/64.dsf", "Album", "Artist", 2000, dsd64),
		dsdRef("A/128.dsf", "Album", "Artist", 2000, dsd128),
		dsdRef("A/256.dsf", "Album", "Artist", 2000, dsd256),
		dsdRef("A/48k.dsf", "Album", "Artist", 2000, dsd64x48),
	)
	ctx := context.Background()
	names := func(ms []member) []string {
		out := pathsOf(ms)
		sort.Strings(out)
		return out
	}
	for _, tc := range []struct {
		spec transcode.JobSpec
		want []string
	}{
		{compactSpec("A/64.dsf", dsd64), []string{"A/128.dsf", "A/256.dsf"}},
		{faithfulSpec("A/64.dsf", dsd64), []string{"A/128.dsf", "A/256.dsf"}},
		{compactSpec("A/48k.dsf", dsd64x48), nil},
	} {
		mates, err := h.r.albumMates(ctx, tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(mates); len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) || (len(got) > 1 && got[1] != tc.want[1]) {
			t.Errorf("%s %s: mates %v, want %v", tc.spec.SourceLibraryRel, tc.spec.Kind, got, tc.want)
		}
	}
	_, ok, err := h.r.AlbumGainDB(ctx, compactSpec("A/48k.dsf", dsd64x48), fp(-3))
	if err != nil || ok {
		t.Errorf("a track with no album-mate on its profile keeps its own guard: ok=%v err=%v", ok, err)
	}
}

// ── the decision ───────────────────────────────────────────────────────────

func album(n int) []manifest.CatalogRef {
	refs := make([]manifest.CatalogRef, n)
	for i := range refs {
		refs[i] = dsdRef("Album/"+string(rune('a'+i))+".dsf", "Album", "Artist", 2000, dsd64)
	}
	return refs
}

// TestAlbumGainUsesStoredPeaksWithoutMeasuring: every album-mate has a fresh
// peak, so nothing is decoded and the boost is the album's hottest track's.
func TestAlbumGainUsesStoredPeaksWithoutMeasuring(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.peaks.put(refs[1].Path, j.DSDPeakProfile(), fp(-4.0))
	h.peaks.put(refs[2].Path, j.DSDPeakProfile(), fp(-8.0))
	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-6.0))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if g != 3.0 {
		t.Errorf("gain = %.1f, want 3.0 (the −4 dBTP track lands at −1)", g)
	}
	if n := h.meas.totalCalls(); n != 0 {
		t.Errorf("measured %d tracks with every peak on record", n)
	}
}

// TestAlbumGainMeasuresEachMissingMateOnceAndRecordsIt: missing peaks are
// measured by the render itself, once each, and recorded with the
// album-mate's own source facts on the job's profile.
func TestAlbumGainMeasuresEachMissingMateOnceAndRecordsIt(t *testing.T) {
	refs := album(4)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.peaks.put(refs[1].Path, j.DSDPeakProfile(), fp(-7.0))
	h.meas.peaks[refs[2].Path] = fp(-2.5)
	h.meas.peaks[refs[3].Path] = fp(-9.0)
	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-6.0))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if g != 1.5 {
		t.Errorf("gain = %.1f, want 1.5 (the −2.5 dBTP track constrains the album)", g)
	}
	for _, p := range []string{refs[2].Path, refs[3].Path} {
		if n := h.meas.callCount(p); n != 1 {
			t.Errorf("%s measured %d times, want 1", p, n)
		}
	}
	if n := h.meas.callCount(refs[1].Path); n != 0 {
		t.Errorf("the track with a stored peak was measured %d times", n)
	}
	if len(h.peaks.upserts) != 2 {
		t.Fatalf("recorded %d peaks, want the 2 measured", len(h.peaks.upserts))
	}
	for _, u := range h.peaks.upserts {
		if u.Profile != j.DSDPeakProfile() || u.SourceMTimeNS != 7 || u.SourceSize != 9 || u.MeasuredAt != h.now.UnixNano() {
			t.Errorf("recorded row %+v: want the job's profile, the album-mate's source facts, and now", u)
		}
	}
}

// TestAMateThatCannotBeMeasuredDoesNotConstrain: a file the decoder refuses
// will not render either, so it must not hold the whole album's boost down.
func TestAMateThatCannotBeMeasuredDoesNotConstrain(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.meas.errs[refs[1].Path] = errors.New("corrupt file")
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err != nil || !ok || g != 4.0 {
		t.Errorf("gain=%.1f ok=%v err=%v, want 4.0 from the measurable tracks", g, ok, err)
	}
	if n := h.meas.callCount(refs[1].Path); n != 1 {
		t.Errorf("a failing album-mate is tried once per decision, got %d", n)
	}
}

func TestSilentAlbumGetsTheNominalBoost(t *testing.T) {
	refs := album(2)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.peaks.put(refs[1].Path, j.DSDPeakProfile(), nil)
	g, ok, err := h.r.AlbumGainDB(context.Background(), j, nil)
	if err != nil || !ok || g != 6.0 {
		t.Errorf("gain=%.1f ok=%v err=%v, want +6 for an album with no measurable peak", g, ok, err)
	}
}

func TestTrackWithNoAlbumKeepsItsGuard(t *testing.T) {
	h := newHarness(t, dsdRef("Solo/01.dsf", "Solo", "Artist", 2000, dsd64))
	ctx := context.Background()
	for _, j := range []transcode.JobSpec{compactSpec("Solo/01.dsf", dsd64), compactSpec("Unknown/01.dsf", dsd64)} {
		if _, ok, err := h.r.AlbumGainDB(ctx, j, fp(-3)); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v, want the per-track guard", j.SourceLibraryRel, ok, err)
		}
	}
	if n := h.cat.streams.Load(); n != 1 {
		t.Errorf("index built %d times; a just-built index must not rebuild for an unknown path", n)
	}
	h.now = h.now.Add(unknownPathRebuildAfter)
	_, _, _ = h.r.AlbumGainDB(ctx, compactSpec("Unknown/01.dsf", dsd64), fp(-3))
	if n := h.cat.streams.Load(); n != 2 {
		t.Errorf("index built %d times; an unknown path in an older index must rebuild it once", n)
	}
}

func TestNonDSDJobHasNoAlbumGain(t *testing.T) {
	h := newHarness(t, album(2)...)
	j := compactSpec("Album/a.dsf", dsd64)
	j.SourceIsDSD = false
	if _, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-3)); ok || err != nil {
		t.Errorf("ok=%v err=%v", ok, err)
	}
	if b := h.r.SurveyBudget(context.Background(), j); b != 0 {
		t.Errorf("budget %v for a PCM job", b)
	}
}

// ── claims ─────────────────────────────────────────────────────────────────

// TestConcurrentRendersShareOneSurvey: three renders of one six-track album
// run at once. Each claims its own track before Stage A, so no survey
// decodes a track another render is already decoding, and the three
// unrendered tracks are measured exactly once between them. All three land
// on the same boost.
func TestConcurrentRendersShareOneSurvey(t *testing.T) {
	refs := album(6)
	h := newHarness(t, refs...)
	h.meas.delay = 20 * time.Millisecond
	own := []float64{-5.0, -7.0, -6.5}
	h.meas.peaks[refs[3].Path] = fp(-3.0)
	h.meas.peaks[refs[4].Path] = fp(-9.0)
	h.meas.peaks[refs[5].Path] = fp(-8.0)
	ctx := context.Background()

	// The pool starts all three before any finishes Stage A.
	var resolves []func(*float64, error)
	for i := range 3 {
		resolves = append(resolves, h.r.Claim(ctx, compactSpec(refs[i].Path, dsd64)))
	}
	gains := make([]float64, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			time.Sleep(time.Duration(i*5) * time.Millisecond) // Stage A, staggered
			resolves[i](fp(own[i]), nil)
			g, ok, err := h.r.AlbumGainDB(ctx, compactSpec(refs[i].Path, dsd64), fp(own[i]))
			if err != nil || !ok {
				t.Errorf("render %d: ok=%v err=%v", i, ok, err)
			}
			gains[i] = g
		})
	}
	wg.Wait()
	for i := range 3 {
		if n := h.meas.callCount(refs[i].Path); n != 0 {
			t.Errorf("rendered track %d was also measured %d times", i, n)
		}
	}
	for i := 3; i < 6; i++ {
		if n := h.meas.callCount(refs[i].Path); n != 1 {
			t.Errorf("unrendered track %d measured %d times, want exactly 1", i, n)
		}
	}
	for i := range 3 {
		if gains[i] != 2.0 {
			t.Errorf("render %d gain %.1f, want 2.0 (the −3 dBTP track constrains all six)", i, gains[i])
		}
	}
}

// TestWaitingOnAFailedRenderMeasuresTheTrackItself: a survey waiting on a
// render that fails before its Stage B must not give that track up — the
// failure may be transient — so it measures the track itself.
func TestWaitingOnAFailedRenderMeasuresTheTrackItself(t *testing.T) {
	refs := album(2)
	h := newHarness(t, refs...)
	h.meas.peaks[refs[1].Path] = fp(-2.0)
	ctx := context.Background()
	resolveB := h.r.Claim(ctx, compactSpec(refs[1].Path, dsd64))
	go func() {
		time.Sleep(20 * time.Millisecond)
		resolveB(nil, errors.New("render of B was cancelled"))
	}()
	g, ok, err := h.r.AlbumGainDB(ctx, compactSpec(refs[0].Path, dsd64), fp(-9.0))
	if err != nil || !ok || g != 1.0 {
		t.Errorf("gain=%.1f ok=%v err=%v, want 1.0 with B measured here", g, ok, err)
	}
	if n := h.meas.callCount(refs[1].Path); n != 1 {
		t.Errorf("B measured %d times, want 1", n)
	}
}

func TestCancelledWaitReturnsTheContextError(t *testing.T) {
	refs := album(2)
	h := newHarness(t, refs...)
	_ = h.r.Claim(context.Background(), compactSpec(refs[1].Path, dsd64)) // never resolved
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := h.r.AlbumGainDB(ctx, compactSpec(refs[0].Path, dsd64), fp(-9.0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's", err)
	}
}

// TestClaimRecordsTheRendersOwnPeak: the render's peak is stored the moment
// its Stage B finishes, so a waiting survey reads it; a render that fails
// records nothing, and a second resolve is ignored.
func TestClaimRecordsTheRendersOwnPeak(t *testing.T) {
	refs := album(2)
	h := newHarness(t, refs...)
	ctx := context.Background()
	j := compactSpec(refs[0].Path, dsd64)
	resolve := h.r.Claim(ctx, j)
	resolve(fp(-4.5), nil)
	resolve(fp(-1.0), nil)
	got, _ := h.peaks.FreshDSDPeaks(ctx, j.DSDPeakProfile(), []string{j.SourceLibraryRel})
	if p, ok := got[j.SourceLibraryRel]; !ok || p.TruePeakDBTP == nil || *p.TruePeakDBTP != -4.5 || p.SourceMTimeNS != 7 || p.SourceSize != 9 {
		t.Errorf("recorded %+v, want the first resolve's −4.5 with the job's source facts", p)
	}
	failed := compactSpec(refs[1].Path, dsd64)
	h.r.Claim(ctx, failed)(nil, errors.New("boom"))
	got, _ = h.peaks.FreshDSDPeaks(ctx, failed.DSDPeakProfile(), []string{failed.SourceLibraryRel})
	if _, ok := got[failed.SourceLibraryRel]; ok {
		t.Error("a render that failed before Stage B must record no peak")
	}
}

// ── the deadline ───────────────────────────────────────────────────────────

func TestSurveyBudgetCountsOnlyUnmeasuredMates(t *testing.T) {
	refs := album(4)
	refs[1].Duration, refs[2].Duration, refs[3].Duration = 100, 200, 0
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.peaks.put(refs[1].Path, j.DSDPeakProfile(), fp(-5))
	want := 400*time.Second + unknownDurationBudget
	if got := h.r.SurveyBudget(context.Background(), j); got != want {
		t.Errorf("budget %v, want %v (2 × 200 s, plus the unknown-duration default)", got, want)
	}
	h.peaks.put(refs[2].Path, j.DSDPeakProfile(), fp(-5))
	h.peaks.put(refs[3].Path, j.DSDPeakProfile(), fp(-5))
	if got := h.r.SurveyBudget(context.Background(), j); got != 0 {
		t.Errorf("budget %v with every peak on record, want 0", got)
	}
	if got := measureBudget(10); got != minMeasureBudget {
		t.Errorf("a short track's budget %v, want the %v floor", got, minMeasureBudget)
	}
	if got := measureBudget(math.NaN()); got != unknownDurationBudget {
		t.Errorf("NaN duration budget %v", got)
	}
}

func TestInvalidateAndTTLRebuildTheIndex(t *testing.T) {
	refs := album(2)
	h := newHarness(t, refs...)
	ctx := context.Background()
	j := compactSpec(refs[0].Path, dsd64)
	_, _ = h.r.albumMates(ctx, j)
	_, _ = h.r.albumMates(ctx, j)
	if n := h.cat.streams.Load(); n != 1 {
		t.Fatalf("built %d times, want 1 while fresh", n)
	}
	h.r.Invalidate()
	_, _ = h.r.albumMates(ctx, j)
	if n := h.cat.streams.Load(); n != 2 {
		t.Errorf("built %d times, want a rebuild after Invalidate", n)
	}
	h.now = h.now.Add(defaultIndexTTL)
	_, _ = h.r.albumMates(ctx, j)
	if n := h.cat.streams.Load(); n != 3 {
		t.Errorf("built %d times, want a rebuild once the TTL passed", n)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no catalog, peaks or spec builder must refuse")
	}
}
