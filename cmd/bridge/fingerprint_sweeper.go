package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// fingerprintSweeper identifies tracks the text enricher gave up on, by their
// audio.
//
// # Why a sweeper and not a step inside the enricher
//
// Two reasons, both structural.
//
// The enricher's whole design premise is rate-limited, not CPU-bound: one
// goroutine that sleeps on politeness pacers. fpcalc saturates a core for
// roughly a second per track and there is no throttle for it, because the
// throttle IS the pacer and the pacer only fires on network calls.
//
// More seriously, the enricher has no filesystem dependency and must keep it
// that way. os.Stat takes no context, so a hung — as opposed to dropped —
// network mount would block the single goroutine that drives all enrichment,
// and shutdown would then wait out the whole bgWriters grace window. Here the
// same hang costs one worker.
//
// # What it is allowed to write
//
// Outcomes go into an in-memory cache that the enricher reads. The only
// database write is ResetEnrichedByPaths over the paths it actually resolved —
// deliberately NOT the library-wide ResetEnrichedMisses, which selects roughly
// half the library and would push a ~9,000-track delta to every paired device
// on every sweep (the PR #369 wipe-loop class, on a timer).
type fingerprintSweeper struct {
	store    *manifest.Store
	resolver *bridgefs.Resolver
	client   *acoustid.Client
	cache    *acoustid.Cache

	workers   int
	maxPerRun int
	length    time.Duration

	// pacer serialises AcoustID requests across workers. The politeness
	// contract is per-key, not per-worker, so a pool that paced individually
	// would burst by exactly its worker count.
	pacer sync.Mutex
	last  time.Time
}

// candidate is one track worth fingerprinting.
type candidate struct {
	path      string
	absPath   string
	durationS float64
	isDSD     bool
	artist    string
	size      int64
	mtimeNS   int64
}

// runFingerprintSweeper is the loop. Modelled on runAnalysisSweeper, including
// the settle delay so the first sweep does not compete with startup work.
func runFingerprintSweeper(ctx context.Context, s *fingerprintSweeper, interval time.Duration) {
	const settleDelay = 90 * time.Second
	select {
	case <-ctx.Done():
		return
	case <-time.After(settleDelay):
	}
	s.sweep(ctx)
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep runs one pass: collect candidates, fingerprint them, then re-queue
// whatever resolved.
func (s *fingerprintSweeper) sweep(ctx context.Context) {
	cands, err := s.collectCandidates(ctx)
	if err != nil {
		logger.Warn("fingerprint sweep: list candidates", "err", err)
		return
	}
	if len(cands) == 0 {
		return
	}
	logger.Info("fingerprint sweep starting", "candidates", len(cands), "workers", s.workers)

	resolved := s.fingerprintAll(ctx, cands)
	if ctx.Err() != nil {
		return
	}

	// Cache writes complete before the re-queue: the enricher must be able to
	// find an answer for every path it is handed. Reversed, a row could be
	// picked up on enriched_at=0 before its verdict existed, miss the cache,
	// and be re-skipped — burning the sweep for that track.
	if len(resolved) > 0 {
		n, err := s.store.ResetEnrichedByPaths(ctx, resolved)
		if err != nil {
			// A cancelled context here is a normal shutdown, not a fault —
			// logging it at Error would put a misleading line in the journal
			// every time the bridge stops mid-sweep.
			if ctx.Err() == nil {
				logger.Error("fingerprint sweep: re-queue", "err", err)
			}
			return
		}
		logger.Info("fingerprint sweep re-queued resolved tracks", "resolved", len(resolved), "requeued", n)
	}
}

// collectCandidates finds tracks the enricher gave up on that are worth the
// decode, cheapest checks first.
func (s *fingerprintSweeper) collectCandidates(ctx context.Context) ([]candidate, error) {
	var out []candidate
	// StreamTracks reuses ONE Track allocation across iterations, so the
	// callback must not retain the pointer. Everything below is copied by
	// value into a candidate before the next row overwrites it.
	err := s.store.StreamTracks(ctx, nil, func(t *manifest.Track) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(out) >= s.maxPerRun {
			return errSweepFull
		}
		// Only tracks still missing what fingerprinting can supply.
		if t.ArtistMBID != "" && t.MusicBrainzAlbumID != "" {
			return nil
		}
		durationS, isDSD := 0.0, false
		if t.Duration != nil {
			durationS = *t.Duration
		}
		if t.IsDSD != nil {
			isDSD = *t.IsDSD
		}
		// The pre-decode screen, before spending anything — on a
		// network-backed library this is also before spending egress.
		if acoustid.CheckEligible(durationS, isDSD) != acoustid.ReasonNone {
			return nil
		}
		// Already answered this exact file version in this process.
		abs, info, rerr := s.resolver.ResolveChecked(t.Path)
		if rerr != nil {
			// EVERY resolve error is treated identically — ENOENT and a
			// disconnected FUSE mount alike. Distinguishing them is the seed
			// of a "mark permanently unfingerprintable" bug during a mount
			// outage. Nothing is persisted, so an unreadable file costs
			// exactly today's behaviour: the enricher skips it as before.
			return nil
		}
		key := acoustid.Key{Path: t.Path, Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
		if _, hit := s.cache.Get(key); hit {
			return nil
		}
		out = append(out, candidate{
			path: t.Path, absPath: abs, durationS: durationS, isDSD: isDSD,
			artist: t.Artist, size: info.Size(), mtimeNS: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, errSweepFull) && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return out, nil
}

// errSweepFull stops the candidate stream once the per-run cap is reached.
var errSweepFull = errors.New("fingerprint: per-run cap reached")

// fingerprintAll runs the workers and returns the paths that resolved.
func (s *fingerprintSweeper) fingerprintAll(ctx context.Context, cands []candidate) []string {
	jobs := make(chan candidate)
	var (
		mu       sync.Mutex
		resolved []string
		wg       sync.WaitGroup
	)

	for range s.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				if ctx.Err() != nil {
					return
				}
				if ok := s.fingerprintOne(ctx, c); ok {
					mu.Lock()
					resolved = append(resolved, c.path)
					mu.Unlock()
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, c := range cands {
			select {
			case jobs <- c:
			case <-ctx.Done():
				return
			}
		}
	}()

	// A healthy sweep runs to COMPLETION however long it takes — 500
	// candidates on one worker is minutes, by design.
	//
	// The bounded grace applies only AFTER cancellation, which is the case it
	// exists for: exec.CommandContext sends SIGKILL on ctx expiry, but a
	// process blocked in a FUSE syscall sits in uninterruptible sleep and will
	// not take the signal until the driver unblocks, so a worker can outlive
	// cancellation and an unbounded wait would hold the bgWriters join open.
	//
	// An earlier version applied the grace unconditionally, which capped every
	// sweep at 60s: the workers kept running past it while the sweep reported
	// a truncated result and the next tick started on top of them. The two
	// situations look alike in the code and are not alike at all — one is
	// normal work, the other is a wedged filesystem.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitForWorkers(ctx, sweeperDrainGrace, done)

	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), resolved...)
}

// waitForWorkers blocks until the workers finish, or — if the context is
// already cancelled or becomes so — until the shutdown grace expires.
//
// Split out so the distinction it encodes is directly testable: a healthy
// sweep must never be cut short, and a cancelled one must never hang.
//
// grace is a PARAMETER rather than a package-level knob so the tests never
// have to mutate shared state to drive it — which would be a data race the
// moment anything ran in parallel, and lets sweeperDrainGrace stay a const.
func waitForWorkers(ctx context.Context, grace time.Duration, done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	select {
	case <-done:
	case <-time.After(grace):
		logger.Warn("fingerprint sweep: workers did not drain within the shutdown grace window; " +
			"continuing (a wedged filesystem can hold fpcalc in uninterruptible sleep)")
	}
}

// sweeperDrainGrace bounds how long a CANCELLED sweep waits for its workers
// before giving up on them. It is a shutdown bound, not a sweep budget — a
// healthy sweep is never subject to it.
const sweeperDrainGrace = 30 * time.Second

// fingerprintOne decodes, looks up, and records one track. Returns true when
// the gate accepted a match.
func (s *fingerprintSweeper) fingerprintOne(ctx context.Context, c candidate) bool {
	key := acoustid.Key{Path: c.path, Size: c.size, MTimeNS: c.mtimeNS}

	fp, err := acoustid.Compute(ctx, c.absPath, s.length)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, acoustid.ErrUnreadable) {
			logger.Warn("fingerprint: decode", "path", c.path, "err", err)
		}
		// A file that cannot be decoded is a property of the FILE, so record
		// the miss and stop re-decoding it every sweep.
		if errors.Is(err, acoustid.ErrUnreadable) {
			s.cache.Set(key, acoustid.Outcome{})
		}
		return false
	}

	in := acoustid.Input{
		DurationSec: c.durationS,
		IsDSD:       c.isDSD,
		Fingerprint: fp,
		// The junk classification lives in internal/enrich because it uses the
		// match-folding vocabulary; the gate needs the verdict here because a
		// track with no witness has to clear a higher submission bar.
		HasLocalArtistWitness: enrich.HasUsableArtistWitness(c.artist),
	}
	if reason := acoustid.CheckFingerprint(in); reason != acoustid.ReasonNone {
		s.cache.Set(key, acoustid.Outcome{})
		return false
	}

	s.wait(ctx)
	if ctx.Err() != nil {
		return false
	}
	results, err := s.client.Lookup(ctx, fp)
	if err != nil {
		if errors.Is(err, acoustid.ErrNoMatch) {
			// AcoustID answered cleanly and knows nothing. A fact about the
			// audio, so cache it.
			s.cache.Set(key, acoustid.Outcome{})
			return false
		}
		// TRANSIENT failures are not cached: the next sweep must retry them,
		// or a brief outage would permanently mark a whole batch unmatched.
		if ctx.Err() == nil && !acoustid.IsTransient(err) {
			logger.Warn("fingerprint: lookup", "path", c.path, "err", err)
			s.cache.Set(key, acoustid.Outcome{})
		}
		return false
	}

	in.Results = results
	decision, reason := acoustid.Accept(in)
	if reason != acoustid.ReasonNone {
		s.cache.Set(key, acoustid.Outcome{})
		return false
	}

	s.cache.Set(key, acoustid.Outcome{Matched: true, Decision: decision})
	// Provenance, so the write is attributable and reversible. Best-effort:
	// failing to record HOW a row was resolved must not stop it being
	// resolved.
	if err := s.store.SetAcoustIDMatch(ctx, c.path, decision.AcoustID); err != nil && ctx.Err() == nil {
		logger.Warn("fingerprint: record provenance", "path", c.path, "err", err)
	}
	return true
}

// wait applies the AcoustID pacing interval across ALL workers.
//
// Per-worker pacing would burst by exactly the worker count, because the
// politeness contract belongs to the API key rather than to any one goroutine.
//
// THE LOCK IS HELD ACROSS THE SLEEP, deliberately. An earlier version released
// it before sleeping to avoid blocking siblings, which defeated the whole
// mechanism: every worker then acquired the lock in turn, read the SAME stale
// s.last, computed the same delay, and they all woke and fired together —
// precisely the burst the pacer exists to prevent.
//
// The resulting convoy is not a smell here, it IS the rate limit: one request
// per interval is the intended throughput, and a worker waiting its turn has
// nothing else to do anyway — the lookup is its next step. Cancellation still
// returns immediately, releasing the lock via the defer.
func (s *fingerprintSweeper) wait(ctx context.Context) {
	s.pacer.Lock()
	defer s.pacer.Unlock()
	if wait := s.client.MinInterval() - time.Since(s.last); wait > 0 && !s.last.IsZero() {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	s.last = time.Now()
}

// acousticLookupAdapter bridges the cache to the enricher's interface. The
// enricher defines the interface it consumes (like PremiumCoverFetcher), so
// the translation lives here rather than in either package.
type acousticLookupAdapter struct{ cache *acoustid.Cache }

func (a acousticLookupAdapter) LookupPath(clientPath string) (enrich.AcousticMatch, bool) {
	d, ok := a.cache.LookupPath(clientPath)
	if !ok {
		return enrich.AcousticMatch{}, false
	}
	return enrich.AcousticMatch{
		ArtistMBID:    d.ArtistMBID,
		ArtistName:    d.ArtistName,
		RecordingMBID: d.RecordingMBID,
		AlbumHint:     d.AlbumHint,
		AcoustID:      d.AcoustID,
	}, true
}

// fingerprintCacheCap bounds the outcome cache.
//
// Sized for a large library rather than for the per-run cap, because the cache
// is what stops a repeat sweep re-decoding files it has already answered — the
// expensive half. Storing decisions rather than raw fingerprints keeps entries
// at roughly 200 bytes, so this is a few megabytes against the ~40 MB the four
// existing enricher caches already budget.
const fingerprintCacheCap = 20000
