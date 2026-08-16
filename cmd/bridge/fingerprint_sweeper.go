package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
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

// pathResolver is the one method the sweeper needs from bridgefs.Resolver.
//
// Narrowed to an interface so a test can count the filesystem calls and see
// WHEN they happen — the property that matters here is not just how many stats
// the pass does but that none of them happens with a SQLite cursor open. A
// concrete type makes that unobservable, and "the code plainly does it in the
// right order" is what was true before the order was wrong.
//
// Production passes *bridgefs.Resolver, which keeps the hot-reload behaviour
// that made it borrow apiSrv.Resolver() rather than snapshot the roots.
type pathResolver interface {
	ResolveChecked(clientPath string) (string, os.FileInfo, error)
}

type fingerprintSweeper struct {
	store    *manifest.Store
	resolver pathResolver
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
	// notBefore is a pool-wide floor on the next request, set from an
	// upstream Retry-After. Guarded by pacer. See noteLookupErr.
	notBefore time.Time

	// compute is the fpcalc seam — nil means acoustid.Compute. Production
	// never sets it; tests inject canned fingerprints so the lookup path is
	// exercisable without an fpcalc spawn, which is what lets the rate-limit
	// wiring below be tested where it actually lives rather than only as an
	// isolated helper. Mirrors transcode.Pool.runner and manifest.Store.now.
	compute func(ctx context.Context, absPath string, length time.Duration) (acoustid.Fingerprint, error)
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

// fingerprintSweeperSettleDelay is the startup settle window. A var
// (not const) purely as the test seam — production never mutates.
var fingerprintSweeperSettleDelay = 90 * time.Second

// fingerprintNoMatchTTL is how long a persisted no-match suppresses
// re-fingerprinting a file whose bytes have not changed.
//
// It exists because AcoustID's database GROWS: a track nobody had submitted
// when we asked may be there months later, so a permanent negative would
// quietly freeze the library's ceiling at whatever AcoustID knew on the day
// each file was first swept. That objection is the reason the outcome cache
// was deliberately in-memory only (see acoustid.Cache); the TTL is what makes
// persisting it defensible rather than a regression.
//
// Thirty days trades a ~30x reduction in repeat decodes for re-asking about
// each unanswerable file once a month. A file-version change re-opens a row
// immediately regardless, and "Retry missing" clears these outright.
const fingerprintNoMatchTTL = 30 * 24 * time.Hour

// fingerprintTagVetoTTL is how long a persisted apply-time tag veto suppresses
// re-fingerprinting a file whose bytes AND artist tag have not changed.
//
// Deliberately the same value as the no-match TTL, and deliberately a separate
// constant rather than a reuse of it: the number agrees, the REASON does not. A
// no-match expires because AcoustID's database grows; a veto expires because
// the cluster this file resolves to can gain or lose recordings, so the answer
// the tag contradicted today may not be the answer given next month. Naming
// them apart is what lets one move without silently moving the other.
//
// Spelled as its own literal for that reason. It was written
// `= fingerprintNoMatchTTL` until 2026-08-16, which is the reuse the paragraph
// above rules out: retuning the no-match TTL would have dragged this one along
// silently, so the separation the name promises existed only on paper.
// TestFingerprintSuppressionTTLsAreIndependent pins it.
const fingerprintTagVetoTTL = 30 * 24 * time.Hour

// runFingerprintSweeper is the loop. Modelled on runAnalysisSweeper —
// settle delay, then ticker OR nudge (the admin "Sweep now" button
// non-blocking-sends on the buffered-1 channel; a pending nudge
// coalesces). The only nudge drain sits post-settle before the initial
// sweep; a nudge arriving mid-sweep stays buffered so the select fires
// an immediate follow-up. status (nil-safe) records the lifecycle for
// the admin Jobs surface.
func runFingerprintSweeper(ctx context.Context, s *fingerprintSweeper, interval time.Duration, nudge <-chan struct{}, status *sweepStatus[admin.FingerprintSweepCounts]) {
	run := func() {
		status.sweepStarted()
		status.sweepFinished(s.sweep(ctx))
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(fingerprintSweeperSettleDelay):
	}
	select {
	case <-nudge:
	default:
	}
	if interval > 0 {
		status.scheduleNext(time.Now().Add(interval))
	}
	run()
	if interval <= 0 && nudge == nil {
		return
	}
	var tickC <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tickC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tickC:
			status.scheduleNext(time.Now().Add(interval))
			run()
		case <-nudge:
			run()
		}
	}
}

// sweep runs one pass: collect candidates, fingerprint them, then re-queue
// whatever resolved. Returns the per-run counts for the admin recorder —
// nil on failure/cancel so the recorder keeps the last successful
// breakdown.
func (s *fingerprintSweeper) sweep(ctx context.Context) *admin.FingerprintSweepCounts {
	cands, err := s.collectCandidates(ctx)
	if err != nil {
		logger.Warn("fingerprint sweep: list candidates", "err", err)
		return nil
	}
	if len(cands) == 0 {
		return &admin.FingerprintSweepCounts{}
	}
	logger.Info("fingerprint sweep starting", "candidates", len(cands), "workers", s.workers)

	resolved := s.fingerprintAll(ctx, cands)
	if ctx.Err() != nil {
		return nil
	}

	counts := &admin.FingerprintSweepCounts{Candidates: len(cands), Resolved: len(resolved)}
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
			return nil
		}
		counts.Requeued = int(n)
		logger.Info("fingerprint sweep re-queued resolved tracks", "resolved", len(resolved), "requeued", n)
	}
	return counts
}

// collectCandidates finds tracks the enricher gave up on that are worth the
// decode, cheapest checks first.
func (s *fingerprintSweeper) collectCandidates(ctx context.Context) ([]candidate, error) {
	// Paths whose fingerprint verdict was ACCEPTED, fetched once per sweep:
	// acoustid_match is column-only (migration v28), so the streamed Track
	// below never carries it. An error fails the sweep rather than degrading
	// to an empty set — empty here would mean re-fingerprinting the whole
	// matched head, which on a network-backed library is hundreds of
	// whole-object reads, the exact cost the set exists to avoid. A skipped
	// background pass costs nothing and retries on the next tick, and a store
	// this query fails against would fail the stream right below anyway.
	matched, err := s.store.AcoustIDMatchedPaths(ctx)
	if err != nil {
		return nil, err
	}
	// Persisted no-match verdicts still inside the TTL. Same fail-the-sweep
	// posture as the matched set, for the same reason: degrading to empty
	// would re-decode every unanswerable row, which is the cost this exists
	// to avoid.
	noMatch, err := s.store.FreshAcoustIDNoMatches(ctx, time.Now().Add(-fingerprintNoMatchTTL).UnixNano())
	if err != nil {
		return nil, err
	}
	// Persisted apply-time tag vetoes still inside the TTL. Same posture again —
	// this one covers a refusal that happens AFTER a successful lookup, so
	// degrading to empty would re-buy the decode and the lookup both.
	vetoed, err := s.store.FreshAcoustIDTagVetoes(ctx, time.Now().Add(-fingerprintTagVetoTTL).UnixNano())
	if err != nil {
		return nil, err
	}
	var out []candidate
	// StreamTracks reuses ONE Track allocation across iterations, so the
	// callback must not retain the pointer. Everything below is copied by
	// value into a candidate before the next row overwrites it.
	err = s.store.StreamTracks(ctx, nil, func(t *manifest.Track) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(out) >= s.scanCap() {
			return errSweepFull
		}
		// Only tracks still missing what fingerprinting can supply.
		if t.ArtistMBID != "" && t.MusicBrainzAlbumID != "" {
			return nil
		}
		// ...and not tracks a fingerprint has already had its say on. Matched
		// AND holding an artist MBID means there is nothing left for another
		// decode to add: what is still missing — the release MBID, on ~1,300
		// of the production bridge's 1,456 matched rows — is the text ladder's
		// to find, and the write-target discipline means a fingerprint can
		// never supply it. Because the dedup cache is in-memory, before this
		// check every restart re-decoded these rows (whole-object reads on a
		// network-backed library), re-ran the lookups, and re-stamped
		// indexed_at into a no-op delta for every paired device.
		//
		// Deliberately NOT keyed on membership alone: matched-but-artistless
		// rows are verdicts vetoed at apply time, or lost to a restart between
		// re-queue and enrichment — provenance records acceptance, not
		// application (see SetAcoustIDMatch) — and those are exactly the rows
		// a sweep can still advance.
		//
		// The converse is deliberately approximate, and the approximation is
		// the safe direction. An artist MBID here may be TEXT-derived on a row
		// whose fingerprint verdict was then vetoed, which this reads as
		// settled. Re-sweeping it is deterministic — same file, same
		// fingerprint, same decision, same veto against the same tags — so the
		// skip costs nothing until the FILE itself changes, and the column
		// deliberately does not record which MBID came from audio, so no exact
		// test exists. Clearing acoustid_match (the undo path the column exists
		// for) is what re-opens such a row.
		if t.ArtistMBID != "" {
			if _, ok := matched[t.Path]; ok {
				return nil
			}
		}
		// ...and not a file AcoustID has already said it does not know.
		//
		// The in-memory cache covers a repeat within one process; this covers
		// a restart, which is where the cost actually landed — the decode is
		// a whole-object read on a network-backed library, and an unanswerable
		// row is unanswerable every time.
		//
		// Gated on the RECORDED file version, not merely on the path: a
		// re-encode or tag edit makes the scanner rewrite size+mtime, the pair
		// stops matching, and the row re-enters the pool. The map is already
		// TTL-bounded by the query, so membership here means "recent AND still
		// the same bytes".
		if rec, ok := noMatch[t.Path]; ok && rec.Size == t.Size && rec.MTimeNS == t.ModTime.UnixNano() {
			return nil
		}
		// ...and not a file whose verdict the enricher already REFUSED because
		// this row's own artist tag contradicted it.
		//
		// This is the other half of the matched-but-artistless population the
		// skip above deliberately keeps. Without a marker a veto looks exactly
		// like a verdict lost to a restart, so both had to stay retryable — and
		// the vetoed ones were re-decoded, re-looked-up and re-refused on every
		// restart, for a decision that is a pure function of inputs neither of
		// which changed. A lost verdict has provenance and NO marker, so it
		// still reaches the pool.
		//
		// Gated on the artist tag AS WELL as the file version, because the veto
		// is a function of both: a tag rewrite must re-open the row even when
		// the bytes are untouched, which is exactly what reExtractUnchanged
		// does on an ExtractorVersion bump.
		if rec, ok := vetoed[t.Path]; ok &&
			rec.Size == t.Size && rec.MTimeNS == t.ModTime.UnixNano() && rec.Artist == t.Artist {
			return nil
		}
		// ...and only after the text ladder has actually had its turn.
		// Enriched is spliced from enriched_at at read time, so false means
		// "queued, not yet attempted" — the enricher stamps on every exit
		// including markSkipped, so a genuine give-up always reads true.
		// Fingerprinting those would race ahead of the cheap path and spend a
		// decode (on a network-backed library, whole-object egress) to answer
		// a question text is about to answer for free. It also makes the
		// re-queue meaningful: a row already at enriched_at=0 is one
		// ResetEnrichedByPaths cannot advance.
		//
		// Fails OPEN on a nil pointer: that degrades to sweeping the wider
		// set, which is merely wasteful, where failing closed would disable
		// the feature outright and silently.
		if t.Enriched != nil && !*t.Enriched {
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
		//
		// Keyed from the ROW, which costs nothing: the scanner writes Size and
		// ModTime into tags_json, so StreamTracks has already unmarshalled
		// both by the time this callback runs, and time.Time's RFC3339Nano
		// round-trip is nanosecond-lossless. Statting first to build the key
		// meant paying a filesystem round-trip for every row only to discard
		// most of them — on a network-backed library the whole cost of the
		// pass, repeated on the rows already answered, every sweep forever.
		key := acoustid.Key{Path: t.Path, Size: t.Size, MTimeNS: t.ModTime.UnixNano()}
		if _, hit := s.cache.Get(key); hit {
			return nil
		}
		// absPath is filled in phase two. See below for why not here.
		out = append(out, candidate{
			path: t.Path, durationS: durationS, isDSD: isDSD,
			artist: t.Artist, size: t.Size, mtimeNS: t.ModTime.UnixNano(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, errSweepFull) && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return s.resolveCandidates(out), nil
}

// scanCap bounds how many rows the stream may COLLECT, as opposed to how many
// the sweep may WORK on.
//
// Two bounds because the cap moved sides. It used to be applied after
// resolution, so an unresolvable row cost nothing and the stream simply kept
// going until it had maxPerRun real candidates. Now that resolution happens
// after the stream, a single cap would let unresolvable rows consume the
// budget: a chunk of rows pointing at missing files — a folder removed but not
// yet reaped, a root half-mounted — would fill all 500 slots, phase two would
// drop every one of them, and the sweep would do nothing. StreamTracks order
// is stable, so the same rows would win the race on every sweep and the tracks
// behind them would never be reached.
//
// The headroom is what keeps that from happening, and it is a memory bound
// rather than a work bound: a candidate is roughly 150 bytes, so even at the
// factor below the slice stays well under a megabyte, which matters on the
// low-memory hosts the rest of this codebase streams for.
func (s *fingerprintSweeper) scanCap() int {
	return s.maxPerRun * candidateScanFactor
}

// candidateScanFactor is how much unresolvable material a sweep can absorb
// before it starts losing candidates to it: at 4, three quarters of the rows
// scanned can fail to resolve and the sweep still fills its budget.
const candidateScanFactor = 4

// resolveCandidates turns client paths into absolute ones, dropping any that
// will not resolve, and stops once it has maxPerRun of them.
//
// Runs AFTER the stream above, with no cursor open. That ordering is the
// point: SQLite in WAL mode cannot reset the log while a reader holds a
// snapshot, so a read transaction spanning thousands of filesystem calls pins
// the WAL at its start mark for the whole sweep while concurrent enrichment
// writes append behind it. Doing the I/O here leaves the stream doing nothing
// but SQLite work — which is what the sweeper's own docblock argues for one
// layer up, applied to the query path rather than only to the enricher.
//
// The early exit means the extra rows scanCap allows are only PAID for when
// they are needed: a healthy library resolves the first maxPerRun and never
// stats the rest.
func (s *fingerprintSweeper) resolveCandidates(in []candidate) []candidate {
	// Filter in place; the backing array is already sized. Indexed rather
	// than ranged so the ~80-byte candidate is not copied per iteration —
	// safe because append writes at len(out), which never runs ahead of i.
	out := in[:0]
	for i := range in {
		if len(out) >= s.maxPerRun {
			break
		}
		abs, _, err := s.resolver.ResolveChecked(in[i].path)
		if err != nil {
			// EVERY resolve error is treated identically — ENOENT and a
			// disconnected FUSE mount alike. Distinguishing them is the seed
			// of a "mark permanently unfingerprintable" bug during a mount
			// outage. Nothing is persisted, so an unreadable file costs
			// exactly today's behaviour: the enricher skips it as before.
			continue
		}
		in[i].absPath = abs
		out = append(out, in[i])
	}
	return out
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

	// The rate-limit pause gates the DECODE, not just the lookup. fpcalc costs
	// roughly a CPU second per track and, on a network-backed library, a
	// whole-object read — so a pool that kept fingerprinting through a pause
	// would burn exactly what RateLimitError's docblock says the pause exists
	// to save, and arrive at the wall anyway.
	s.awaitPause(ctx)
	if ctx.Err() != nil {
		return false
	}

	fp, err := s.fingerprintFile(ctx, c.absPath)
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
			// audio, so cache it — and persist it, so the next restart does
			// not buy the same answer with another whole-object read.
			//
			// ONLY this branch persists. The neighbours look similar and are
			// not: a lookup ERROR is a fact about the upstream, not the file,
			// and the gate rejections below additionally depend on the row's
			// own artist tag (HasLocalArtistWitness), so a tag fix — exactly
			// what an operator would do to resolve one — must re-open them.
			// Persisting either would sideline rows for a reason that has
			// nothing to do with their audio.
			s.cache.Set(key, acoustid.Outcome{})
			if err := s.store.SetAcoustIDNoMatch(ctx, c.path, c.size, c.mtimeNS); err != nil && ctx.Err() == nil {
				// Best-effort, exactly like the provenance write: failing to
				// remember a negative costs one future decode, and must not
				// turn a clean answer into a retryable failure.
				logger.Warn("fingerprint: record no-match", "path", c.path, "err", err)
			}
			return false
		}
		// A 429 tells the whole pool to stand down, not just this worker.
		// Must run BEFORE the classification below returns: a rate limit IS
		// transient, so that branch is a bare `return false` and the worker
		// would otherwise go straight back to decoding the next candidate.
		s.noteLookupErr(err)
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

// fingerprintFile decodes one file. The indirection is the fpcalc test seam;
// production leaves s.compute nil.
func (s *fingerprintSweeper) fingerprintFile(ctx context.Context, absPath string) (acoustid.Fingerprint, error) {
	if s.compute != nil {
		return s.compute(ctx, absPath, s.length)
	}
	return acoustid.Compute(ctx, absPath, s.length)
}

// noteLookupErr honours an upstream Retry-After by stalling the WHOLE pool.
//
// acoustid.RateLimitError existed with no consumer: its own docblock says it
// is there "so the sweeper can pause its WHOLE pool", and nothing read it. The
// consequence is worse here than for a plain metadata client because the
// expensive half happens BEFORE the request — a sweep that ignores a 429 with
// Retry-After: 60 burns up to maxPerRun decodes, and on a network-backed
// library the egress for them, against a service that asked us to stop.
//
// The deadline is anchored to now BEFORE the lock is taken: this may block
// behind a worker already sleeping out an earlier pause, and computing it
// afterwards would push it further out than the upstream actually asked for.
func (s *fingerprintSweeper) noteLookupErr(err error) {
	// Typed-nil guard: errors.As can report true with a nil pointer, and
	// reading RetryAfter off it would panic. Mirrors the *net.DNSError arm in
	// acoustid.IsTransient. Not reachable from this repo today — get() only
	// ever builds &RateLimitError{...} — but it costs nothing and the deref is
	// one custom wrapper away.
	var rle *acoustid.RateLimitError
	if !errors.As(err, &rle) || rle == nil {
		return
	}
	d := rle.RetryAfter
	if d <= 0 {
		return
	}
	// Clamped to the same bound the client applies when it parses the header,
	// rather than to a second one that could drift from it. Worth doing even
	// though the parse already caps: RetryAfter is an exported field on a type
	// constructible from outside that package, so the parsed cap is not a
	// guarantee about what arrives here.
	if d > acoustid.MaxRetryAfter {
		d = acoustid.MaxRetryAfter
	}
	deadline := time.Now().Add(d)

	s.pacer.Lock()
	// Only ever EXTEND. Several workers can be in flight when the limit is
	// hit, and a later, shorter piece of advice must not cut an earlier pause
	// short.
	extended := deadline.After(s.notBefore)
	if extended {
		s.notBefore = deadline
	}
	s.pacer.Unlock()

	if extended {
		logger.Warn("fingerprint: AcoustID rate limited; pausing the sweep pool", "retryAfter", d)
	}
}

// awaitPause blocks while a pool-wide rate-limit pause is in effect, and is
// what makes the pause cover the decode rather than only the request.
//
// Holds the pacer across the sleep for the same reason wait does, and here that
// is the mechanism itself: the siblings queued behind this lock are the "whole
// pool" part of pausing the whole pool.
//
// Sharing wait's mutex rather than taking a second one does mean a worker can
// block here while a sibling sleeps out the pacing interval inside wait — but
// only when the pool is already REQUEST-bound, since that sleep is
// `MinInterval - since(last)` and is zero whenever decoding is the slower half.
// So it costs decode parallelism exactly in the regime where another decode
// would have had nowhere to go anyway. Two mutexes would buy nothing and would
// have to be ordered against each other.
func (s *fingerprintSweeper) awaitPause(ctx context.Context) {
	s.pacer.Lock()
	defer s.pacer.Unlock()
	sleepOrDone(ctx, time.Until(s.notBefore))
}

// wait applies the AcoustID pacing interval across ALL workers, and honours any
// pool-wide pause a 429 has imposed since this worker started decoding.
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
	// The longer of the two floors: ordinary politeness pacing, and whatever
	// remains of a pause imposed while this worker was busy elsewhere.
	//
	// No IsZero guard on either: time.Since / time.Until saturate at ~292 years
	// on the zero Time, so on the first call both terms are hugely negative and
	// the sleep is already skipped. The conjunct that used to be here could
	// never change the outcome.
	d := s.client.MinInterval() - time.Since(s.last)
	if pause := time.Until(s.notBefore); pause > d {
		d = pause
	}
	sleepOrDone(ctx, d)
	s.last = time.Now()
}

// sleepOrDone sleeps for d, returning early if ctx is cancelled. A non-positive
// d returns immediately.
//
// time.NewTimer with a Stop rather than time.After: d can be as long as
// acoustid.MaxRetryAfter, and an abandoned hour-long timer per cancelled worker
// is exactly the accumulation the PR #290 convention exists to avoid.
func sleepOrDone(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
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
