// Package albumgain decides the album-level boost for DSD renditions: one
// clip-guarded boost for every track of an album, the one its hottest track
// allows, so a rendered album keeps the balance it was mastered with.
// ops/plan-2026-09-27-dsd-album-gain.md is the design; its Phase 0 is the
// measurement that justified it.
//
// Resolver implements transcode.AlbumGainer. It does three things:
//
//   - Membership. An album is the admin catalog's own album identity,
//     dupes.AlbumIDOf(dupes.Resolve(row)) — a mirror of the iOS app's key,
//     so the album whose balance this keeps is the one the listener sees.
//     An album is a SET of tracks, never a folder. Only local DSD tracks
//     that render to the job's profile count: a routed row or an SACD
//     virtual track is never rendered here, and a track in the other rate
//     family decodes to a different intermediate.
//   - The survey. Before a render's Stage C, every album-mate's peak must
//     be known: from dsd_peaks when a fresh one is on record, otherwise
//     measured by the render itself (transcode.MeasureDSDPeak).
//   - Claims. A track being measured, by its own render or by a survey, is
//     claimed, so a second render waits for that peak instead of decoding
//     the file again.
//
// It cannot deadlock. A claim is only ever held by work that is decoding —
// a render's Stages A and B, or a survey's measurement — and neither waits
// on anything while it holds one. A render resolves its own claim before it
// surveys, and releases each survey claim before it waits, so it waits only
// on claims held by work that is still making progress.
package albumgain

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

var logger = logging.Component("albumgain")

// CatalogStreamer streams the served DSD rows
// (manifest.Store.StreamDSDCatalogRefs).
type CatalogStreamer interface {
	StreamDSDCatalogRefs(ctx context.Context, fn func(manifest.CatalogRef) error) error
}

// PeakStore reads and records peaks (manifest.Store).
type PeakStore interface {
	FreshDSDPeaks(ctx context.Context, profile string, paths []string) (map[string]manifest.DSDPeak, error)
	UpsertDSDPeak(ctx context.Context, p manifest.DSDPeak) error
}

// SpecFor builds the spec that measures path on like's profile: the
// album-mate's own source facts under like's kind, target and quality.
type SpecFor func(ctx context.Context, path string, like transcode.JobSpec) (transcode.JobSpec, error)

// Measurer measures one spec's true peak at unity decode; nil = silent.
type Measurer func(ctx context.Context, j transcode.JobSpec) (*float64, error)

// Config wires a Resolver.
type Config struct {
	Catalog CatalogStreamer
	Peaks   PeakStore
	SpecFor SpecFor
	// Measure defaults to transcode.MeasureDSDPeak.
	Measure Measurer
	// IndexTTL bounds how stale the album index may get between Invalidate
	// calls. Defaults to defaultIndexTTL.
	IndexTTL time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

const (
	// defaultIndexTTL: an album's membership changes when a scan lands,
	// which Invalidate reports, so the TTL only bounds a missed hook.
	defaultIndexTTL = 2 * time.Minute
	// unknownPathRebuildAfter: a path missing from an index at least this
	// old is probably a track scanned since, so the index is rebuilt once
	// rather than rendering the track as if it had no album.
	unknownPathRebuildAfter = 10 * time.Second
	// budgetLookupTimeout bounds SurveyBudget's store reads: it runs as a
	// job starts, and a slow read must not hold the worker up.
	budgetLookupTimeout = 10 * time.Second
	// unknownDurationBudget is the deadline one measurement adds when the
	// album-mate's duration is unknown — the base a render starts from.
	unknownDurationBudget = 10 * time.Minute
	// minMeasureBudget floors a known duration's budget.
	minMeasureBudget = time.Minute
)

// Resolver is the album-level gain decider. Safe for concurrent use.
type Resolver struct {
	cfg Config

	rebuildMu sync.Mutex // serialises index rebuilds

	mu     sync.Mutex // guards ix and claims
	ix     *index
	claims map[claimKey]*claim
}

var _ transcode.AlbumGainer = (*Resolver)(nil)

// New returns a Resolver. Catalog, Peaks and SpecFor are required.
func New(cfg Config) (*Resolver, error) {
	if cfg.Catalog == nil || cfg.Peaks == nil || cfg.SpecFor == nil {
		return nil, fmt.Errorf("albumgain: Catalog, Peaks and SpecFor are required")
	}
	if cfg.Measure == nil {
		cfg.Measure = transcode.MeasureDSDPeak
	}
	if cfg.IndexTTL <= 0 {
		cfg.IndexTTL = defaultIndexTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Resolver{cfg: cfg, claims: map[claimKey]*claim{}}, nil
}

// Invalidate drops the album index; the next render rebuilds it. Called
// when a scan lands, since that is when membership changes.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	r.ix = nil
	r.mu.Unlock()
}

// SurveyBudget is transcode.AlbumGainer's deadline estimate: for each
// album-mate with no fresh peak, twice its duration (the ratio
// jobTimeoutFor gives a render), floored at a minute. 0 when nothing needs
// measuring, and 0 when the estimate itself fails — the render then runs on
// its own deadline, and a survey that outlives it fails the job, which the
// next attempt finishes from the peaks this one recorded.
func (r *Resolver) SurveyBudget(ctx context.Context, j transcode.JobSpec) time.Duration {
	profile := j.DSDPeakProfile()
	if profile == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, budgetLookupTimeout)
	defer cancel()
	mates, err := r.albumMates(ctx, j)
	if err != nil || len(mates) == 0 {
		return 0
	}
	fresh, err := r.cfg.Peaks.FreshDSDPeaks(ctx, profile, pathsOf(mates))
	if err != nil {
		return 0
	}
	var total time.Duration
	for _, m := range mates {
		if _, ok := fresh[m.path]; !ok {
			total += measureBudget(m.durationSec)
		}
	}
	return total
}

func measureBudget(sec float64) time.Duration {
	if sec <= 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
		return unknownDurationBudget
	}
	return max(time.Duration(2*sec*float64(time.Second)), minMeasureBudget)
}

// Claim is transcode.AlbumGainer's claim on the render's own track. When the
// render resolves it with a peak, the peak is recorded at once, so a survey
// waiting on this track reads it from the store.
func (r *Resolver) Claim(ctx context.Context, j transcode.JobSpec) func(*float64, error) {
	profile := j.DSDPeakProfile()
	if profile == "" {
		return func(*float64, error) {
			// Not a DSD render: there is no peak to record and no claim to
			// release.
		}
	}
	key := claimKey{path: j.SourceLibraryRel, profile: profile}
	// Another render's survey may already be measuring this track; this
	// render decodes it anyway (it needs the scratch for Stage C), records
	// its own peak, and leaves that claim to its holder.
	c, mine := r.tryClaim(key)
	var once sync.Once
	return func(tp *float64, err error) {
		once.Do(func() {
			if err == nil {
				r.record(ctx, j, profile, tp)
			}
			if mine {
				r.release(key, c)
			}
		})
	}
}

// AlbumGainDB is transcode.AlbumGainer's decision: the album-level boost for
// j, from j's own peak and every album-mate's. ok=false when j has no
// album-mate on its profile. An album-mate that cannot be measured does not
// constrain the album; it will not render either, for the same reason.
func (r *Resolver) AlbumGainDB(ctx context.Context, j transcode.JobSpec, own *float64) (float64, bool, error) {
	profile := j.DSDPeakProfile()
	if profile == "" {
		return 0, false, nil
	}
	mates, err := r.albumMates(ctx, j)
	if err != nil {
		return 0, false, err
	}
	if len(mates) == 0 {
		return 0, false, nil
	}
	s := &survey{r: r, j: j, profile: profile, mates: mates, paths: pathsOf(mates),
		measured: map[string]*float64{}, gaveUp: map[string]bool{}}
	for {
		waits, err := s.pass(ctx)
		if err != nil {
			return 0, false, err
		}
		if len(waits) == 0 {
			return transcode.AlbumClipGuardedGainDB(s.peaks(own)), true, nil
		}
		if err := waitForClaims(ctx, waits); err != nil {
			return 0, false, err
		}
		// Read again: a claim can resolve without leaving a peak (its
		// holder failed or gave up), and the next pass then measures that
		// mate here.
	}
}

// survey is one AlbumGainDB call: the album-mates, the peaks the last
// pass read from the store, and what this call measured or gave up on.
// measured and gaveUp are kept per call, whether or not the store accepted
// a row, so a mate is measured at most once here even if its peak cannot
// be recorded.
type survey struct {
	r        *Resolver
	j        transcode.JobSpec
	profile  string
	mates    []member
	paths    []string
	fresh    map[string]manifest.DSDPeak
	measured map[string]*float64
	gaveUp   map[string]bool
}

// pass reads the fresh peaks, measures every unknown mate nobody else is
// measuring, and returns the claims held by others, to wait on. Only a
// cancelled context or a failed store read is an error.
func (s *survey) pass(ctx context.Context) ([]*claim, error) {
	fresh, err := s.r.cfg.Peaks.FreshDSDPeaks(ctx, s.profile, s.paths)
	if err != nil {
		return nil, fmt.Errorf("album gain: read peaks: %w", err)
	}
	if fresh == nil {
		fresh = map[string]manifest.DSDPeak{} // measureClaimed writes into it
	}
	s.fresh = fresh
	var waits []*claim
	for _, m := range s.mates {
		if !s.unknown(m.path) {
			continue
		}
		key := claimKey{path: m.path, profile: s.profile}
		c, mine := s.r.tryClaim(key)
		if !mine {
			waits = append(waits, c)
			continue
		}
		if err := s.measureClaimed(ctx, m.path, key, c); err != nil {
			return nil, err
		}
	}
	return waits, nil
}

// unknown reports whether a mate's peak is still to be found.
func (s *survey) unknown(path string) bool {
	if _, ok := s.fresh[path]; ok {
		return false
	}
	if _, ok := s.measured[path]; ok {
		return false
	}
	return !s.gaveUp[path]
}

// measureClaimed measures a mate this call holds the claim on, and releases
// the claim before anything else. The store is read again first, under the
// claim: the pass's read can be minutes old by the time the survey reaches
// this mate, and another survey may have measured it, recorded the peak and
// released its claim since. A measurement records before it releases, so
// this read sees it and the mate is not decoded twice
// (TestASurveyRereadsThePeakUnderItsClaim). A failed measurement leaves the
// mate out of the album; only a cancelled context or a failed store read is
// returned.
//
// The deferred release is for a panic. The measurement is a decode, and
// transcode.Pool's processJob recovers a panic in its runner and keeps the
// worker, so the process outlives it. Released only by the calls below, the
// claim stayed registered with its done channel open, and every later render
// of the album waited on it until its own deadline, failed, and did the same
// on retry, until a restart (TestAMateWhoseMeasurementPanicsReleasesItsClaim).
// The render's own claim has always resolved on every exit (renderDSD
// defers it). release is idempotent, so the explicit calls keep their
// release-before-record order and the deferred one then does nothing. It is
// the direct call form, so its arguments are bound here, at entry.
func (s *survey) measureClaimed(ctx context.Context, path string, key claimKey, c *claim) error {
	defer s.r.release(key, c)
	recorded, err := s.r.cfg.Peaks.FreshDSDPeaks(ctx, s.profile, []string{path})
	if err != nil {
		s.r.release(key, c)
		return fmt.Errorf("album gain: read peaks: %w", err)
	}
	if p, ok := recorded[path]; ok {
		s.r.release(key, c)
		s.fresh[path] = p
		return nil
	}
	tp, err := s.r.measure(ctx, path, s.j, s.profile)
	s.r.release(key, c)
	if err == nil {
		s.measured[path] = tp
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.gaveUp[path] = true
	logger.Warn("album gain: an album-mate could not be measured, so it does not constrain the album",
		"path", s.j.SourceLibraryRel, "mate", path, "err", err)
	return nil
}

// peaks is own plus every mate's known peak, for AlbumClipGuardedGainDB.
func (s *survey) peaks(own *float64) []*float64 {
	peaks := make([]*float64, 0, len(s.mates)+1)
	peaks = append(peaks, own)
	for _, m := range s.mates {
		if p, ok := s.fresh[m.path]; ok {
			peaks = append(peaks, p.TruePeakDBTP)
		} else if tp, ok := s.measured[m.path]; ok {
			peaks = append(peaks, tp)
		}
	}
	return peaks
}

// waitForClaims waits until every claim resolves, or ctx ends.
func waitForClaims(ctx context.Context, waits []*claim) error {
	for _, c := range waits {
		select {
		case <-c.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *Resolver) measure(ctx context.Context, path string, like transcode.JobSpec, profile string) (*float64, error) {
	spec, err := r.cfg.SpecFor(ctx, path, like)
	if err != nil {
		return nil, err
	}
	if got := spec.DSDPeakProfile(); got != profile {
		return nil, fmt.Errorf("album gain: %s would be measured on %q, not %q", path, got, profile)
	}
	tp, err := r.cfg.Measure(ctx, spec)
	if err != nil {
		// A mate's decode names its absolute path and the scratch under the
		// tempDir, and measureClaimed logs this error: the redaction a
		// failed job's own message gets, by the mate's spec.
		return nil, spec.RedactError(err)
	}
	r.record(ctx, spec, profile, tp)
	return tp, nil
}

// record stores a measurement. A failure is logged and otherwise ignored:
// the caller keeps the number it measured, and the next render re-measures.
func (r *Resolver) record(ctx context.Context, j transcode.JobSpec, profile string, tp *float64) {
	err := r.cfg.Peaks.UpsertDSDPeak(ctx, manifest.DSDPeak{
		SourcePath: j.SourceLibraryRel, Profile: profile, TruePeakDBTP: tp,
		SourceMTimeNS: j.SourceMTimeNS, SourceSize: j.SourceSize,
		MeasuredAt: r.cfg.Now().UnixNano(),
	})
	if err != nil && ctx.Err() == nil {
		logger.Warn("album gain: could not record a peak", "path", j.SourceLibraryRel, "err", err)
	}
}

// ── claims ─────────────────────────────────────────────────────────────────

type claimKey struct{ path, profile string }

type claim struct {
	done chan struct{}
	once sync.Once
}

func (r *Resolver) tryClaim(k claimKey) (*claim, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.claims[k]; ok {
		return c, false
	}
	c := &claim{done: make(chan struct{})}
	r.claims[k] = c
	return c, true
}

func (r *Resolver) release(k claimKey, c *claim) {
	r.mu.Lock()
	if r.claims[k] == c {
		delete(r.claims, k)
	}
	r.mu.Unlock()
	c.once.Do(func() { close(c.done) })
}

// ── the album index ────────────────────────────────────────────────────────

type member struct {
	path        string
	sampleRate  int
	durationSec float64
}

type index struct {
	albumOf map[string]string   // path → album id
	members map[string][]member // album id → local DSD members
	builtAt time.Time
}

// albumMates returns j's album-mates on j's profile: the other local DSD
// tracks of j's album whose tier renders to j's target rate.
func (r *Resolver) albumMates(ctx context.Context, j transcode.JobSpec) ([]member, error) {
	ix, err := r.currentIndex(ctx, j.SourceLibraryRel)
	if err != nil {
		return nil, err
	}
	id, ok := ix.albumOf[j.SourceLibraryRel]
	if !ok {
		return nil, nil
	}
	var mates []member
	for _, m := range ix.members[id] {
		if m.path == j.SourceLibraryRel || targetRateFor(j.Kind, m.sampleRate) != j.TargetSampleRate {
			continue
		}
		mates = append(mates, m)
	}
	return mates, nil
}

// targetRateFor is the rate a DSD source renders to on a tier — the same
// function each tier's spec builder uses, so "same target" means "same
// intermediate".
func targetRateFor(kind transcode.JobKind, dsdRate int) int {
	if kind == transcode.JobKindPCMRender {
		return transcode.TargetRateForPCMRender(dsdRate)
	}
	return transcode.TargetRateForOptimize(dsdRate)
}

func (r *Resolver) currentIndex(ctx context.Context, path string) (*index, error) {
	usable := func(ix *index) bool {
		if ix == nil {
			return false
		}
		age := r.cfg.Now().Sub(ix.builtAt)
		if age >= r.cfg.IndexTTL {
			return false
		}
		_, known := ix.albumOf[path]
		return known || age < unknownPathRebuildAfter
	}
	r.mu.Lock()
	ix := r.ix
	r.mu.Unlock()
	if usable(ix) {
		return ix, nil
	}
	r.rebuildMu.Lock()
	defer r.rebuildMu.Unlock()
	r.mu.Lock()
	ix = r.ix
	r.mu.Unlock()
	if usable(ix) { // another render rebuilt it while this one waited
		return ix, nil
	}
	fresh, err := buildIndex(ctx, r.cfg.Catalog, r.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("album gain: build the album index: %w", err)
	}
	r.mu.Lock()
	r.ix = fresh
	r.mu.Unlock()
	return fresh, nil
}

func buildIndex(ctx context.Context, c CatalogStreamer, now time.Time) (*index, error) {
	ix := &index{albumOf: map[string]string{}, members: map[string][]member{}, builtAt: now}
	err := c.StreamDSDCatalogRefs(ctx, func(ref manifest.CatalogRef) error {
		if !ref.IsDSD || ref.RoutedUDN != "" || manifest.IsSACDVirtualPath(ref.Path) {
			return nil
		}
		id := dupes.AlbumIDOf(dupes.Resolve(dupeRow(ref)))
		ix.albumOf[ref.Path] = id
		ix.members[id] = append(ix.members[id], member{path: ref.Path, sampleRate: ref.SampleRate, durationSec: ref.Duration})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ix, nil
}

// dupeRow is librarycat's Row.dupeRow over a CatalogRef: the admin catalog's
// exact input to dupes.Resolve, so a DSD track lands in the album the catalog
// shows. TestIndexGroupsLikeTheAdminCatalog keeps the two in step.
func dupeRow(r manifest.CatalogRef) dupes.Row {
	return dupes.Row{
		Path: r.Path, Title: r.Title, Album: r.Album, AlbumArtist: r.AlbumArtist,
		Artist: r.Artist, Year: r.Year,
		Disc: r.Disc, DiscTagged: r.DiscTagged,
		Track: r.Track, TrackTagged: r.TrackTagged,
		Size: r.Size, Duration: r.Duration,
		SampleRate: r.SampleRate, BitsPerSample: r.BitsPerSample,
		IsDSD: r.IsDSD, Codec: r.Codec,
	}
}

func pathsOf(ms []member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.path
	}
	return out
}
