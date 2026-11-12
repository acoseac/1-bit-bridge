package enrich

import (
	"context"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The artist portraits enrich.deezerEnabled refused, fetched once it is on
// (backlog B761).
//
// A pass whose portrait lookup the Deezer switch refuses stamps its track
// like any other, so nothing at enriched_at says the portrait is missing: the
// stamp keeps, in the row, the name the lookup would have asked Deezer by
// (manifest.Track.PortraitRefused, tracks.deezer_pending_query). While
// enrichment and Deezer are both on, each turn of Run takes one page of those
// rows (runPortraits) and asks Deezer for each artist's portrait once, by
// that name and the row's artist MBID: no MusicBrainz or Cover Art Archive
// request, and no stamp reset. A row is cleared only by a portrait held (in
// the cache already, or fetched) or by Deezer's answer that it has none, and
// only at the version the page read it at; a refusal, a transient failure or
// a cancellation keeps it, and a failure puts its artist's next attempt off
// (portraitRetryState). Every path that asks for a portrait answers in these
// outcomes (artistPortrait), so the ordinary pass, the fingerprint path and
// the batch keep or clear a row by one rule.

// portraitOutcome is what one attempt at an artist's portrait came to.
type portraitOutcome int

const (
	// portraitHeld: the portrait is in the artwork cache, found there or
	// fetched. Settles the rows that wait for it.
	portraitHeld portraitOutcome = iota
	// portraitNone: Deezer answered that it has none: no artist matched the
	// name, or its search or its picture answered 404 or nothing. Terminal:
	// it settles the rows too, and goes in deezerNegCache.
	portraitNone
	// portraitRefused: a switch refused the lookup or cut it off, so Deezer
	// was not asked or its answer was not taken (errDeezerOff, or
	// enrich.enabled's ErrOff). No answer: never a miss.
	portraitRefused
	// portraitFailed: no answer that settles anything: a transient failure,
	// a cancellation, or any other error (an answer that is no image, a host
	// the client refuses, a write that failed). Asked again later.
	portraitFailed
)

// classifyPortrait is the outcome of an attempt that answered (found, err).
func classifyPortrait(found bool, err error) portraitOutcome {
	switch {
	case err == nil && found:
		return portraitHeld
	case err == nil:
		return portraitNone
	case enrichgate.Refused(err) != nil:
		return portraitRefused
	case IsNotFound(err):
		return portraitNone
	default:
		return portraitFailed
	}
}

// artistPortrait asks for mbid's portrait by name (ensureArtistImageCached)
// and answers what the attempt came to, with its error.
func (e *Enricher) artistPortrait(ctx context.Context, mbid, name string) (portraitOutcome, error) {
	found, err := e.ensureArtistImageCached(ctx, mbid, name)
	return classifyPortrait(found, err), err
}

const (
	// portraitRetryFirst is how long an artist whose portrait lookup failed
	// waits before it is asked again; each failure in a row doubles it, up
	// to portraitRetryMost.
	portraitRetryFirst = time.Minute
	portraitRetryMost  = 6 * time.Hour
	// portraitRestMost bounds how long the batch rests after a pass that
	// found no artist due (portraitPass.restUntil). The rest ends at the
	// earliest retry the pass saw, and at once when a pass keeps a refused
	// lookup; this bound is for a row a re-enrichment hands back with its
	// query kept and its artist's retry due before the rest ends ("Retry
	// missing", then a failure), which waits this long at most past it.
	portraitRestMost = 10 * time.Minute
	// portraitRetryCap bounds portraitRetry, as the other memos are bounded.
	portraitRetryCap = 100_000
	// defaultPortraitBatchLimit is PortraitBatchLimit's zero value.
	defaultPortraitBatchLimit = 50
)

// portraitRetryState is an artist whose portrait lookups failed: how many in
// a row, and when it may be asked again. The zero value has none.
type portraitRetryState struct {
	failures  int
	notBefore time.Time
}

// portraitPass is the portrait batch's pass over the rows that keep a
// refused lookup, page by page in path order. Run's goroutine alone reads
// and writes it.
type portraitPass struct {
	// cursor is the path the pass's last page ended on, "" between passes.
	cursor string
	// progressed says the pass asked for a portrait, or settled a row.
	progressed bool
	// due is the earliest retry among the artists the pass waited for or
	// saw fail, zero for none.
	due time.Time
	// restUntil is when the next pass may start, after one that found no
	// artist due: until then the batch reads nothing.
	restUntil time.Time
	// refusals is portraitRefusals as the pass began: a refused lookup kept
	// since makes a row due that the pass may not have read, so the pass
	// then does not rest, and a rest under way ends.
	refusals int64
}

// now is the time the portrait retries are paced by.
func (e *Enricher) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// portraitRetryAt is when mbid's portrait may be asked for again, and
// whether, at now, it has to wait for that: a failure is on record and its
// retry is not due.
func (e *Enricher) portraitRetryAt(mbid string, now time.Time) (time.Time, bool) {
	if e.portraitRetry == nil {
		return time.Time{}, false
	}
	st, ok := e.portraitRetry.Get(mbid)
	if !ok || st.failures == 0 || !now.Before(st.notBefore) {
		return time.Time{}, false
	}
	return st.notBefore, true
}

// notePortraitFailed puts mbid's next attempt off: portraitRetryFirst after
// its first failure, twice as long after each failure after it, and
// portraitRetryMost at most. It returns when the attempt is due.
func (e *Enricher) notePortraitFailed(mbid string) time.Time {
	if e.portraitRetry == nil {
		return time.Time{}
	}
	st, _ := e.portraitRetry.Get(mbid)
	st.failures++
	wait := portraitRetryFirst
	for i := 1; i < st.failures && wait < portraitRetryMost; i++ {
		wait *= 2
	}
	st.notBefore = e.now().Add(min(wait, portraitRetryMost))
	e.portraitRetry.Set(mbid, st)
	return st.notBefore
}

// notePortraitRefused counts a refused lookup a pass keeps: the row it
// stamps is due once both switches are on, so it ends the batch's rest.
func (e *Enricher) notePortraitRefused() { e.portraitRefusals.Add(1) }

// earliest is the earlier of two instants, the zero time being none.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	default:
		return b
	}
}

// notePortraitSettled forgets mbid's failures: its portrait is held, or
// Deezer has none.
func (e *Enricher) notePortraitSettled(mbid string) {
	if e.portraitRetry != nil && e.portraitRetry.Has(mbid) {
		e.portraitRetry.Set(mbid, portraitRetryState{})
	}
}

// portraitBatchLimit resolves PortraitBatchLimit's zero value.
func (e *Enricher) portraitBatchLimit() int {
	if e.PortraitBatchLimit <= 0 {
		return defaultPortraitBatchLimit
	}
	return e.PortraitBatchLimit
}

// runPortraits takes one page of the rows that keep a portrait lookup the
// Deezer switch refused, past where the pass's last page ended, and returns
// how long Run waits before its next turn: interBatchPause while the page was
// full, so the next page follows at once, and PollInterval once the pass has
// reached the end, a switch stopped it, or the batch rests. Run calls it only
// while enrichment and Deezer are both on, and it runs on enrich.enabled's
// context as a batch does; each Deezer request runs on Deezer's own
// (deezerCall).
//
// Rotation: each page reads past the last page's end, and a pass that reaches
// the end starts the next at the beginning, so rows whose artist keeps
// failing at the head of the order hold back no row behind them; and a
// failing artist is asked again only once its retry is due
// (portraitRetryAt), so it costs no request in between. One page a turn,
// beside the ordinary batch, keeps both moving. A pass that found no artist
// due, every one waiting for its retry, is not followed by another until the
// earliest of those retries (portraitRestMost at most), unless a pass keeps a
// refused lookup meanwhile: the batch reads nothing while it rests.
func (e *Enricher) runPortraits(ctx context.Context) time.Duration {
	if e.deezer == nil || e.store == nil {
		return e.PollInterval
	}
	p := &e.portraits
	now := e.now()
	if p.cursor == "" {
		refusals := e.portraitRefusals.Load()
		if now.Before(p.restUntil) && refusals == p.refusals {
			return e.PollInterval
		}
		// A pass begins.
		p.restUntil, p.refusals = time.Time{}, refusals
	}
	ctx, release := enrichgate.CancelWhenOff(ctx, e.on, e.switchEvery)
	defer release()
	limit := e.portraitBatchLimit()
	rows, err := e.store.PendingPortraits(ctx, p.cursor, limit)
	if err != nil {
		// A listing the shutdown or the switch stopped is not a failed one.
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("list pending portraits", "err", failure)
		}
		return e.PollInterval
	}
	if !e.portraitsOf(ctx, rows, now) {
		// A switch refused a lookup, or the pass was stopped: the rows wait
		// as they were, and the next pass starts over.
		p.cursor, p.progressed, p.due = "", false, time.Time{}
		return e.PollInterval
	}
	if len(rows) == limit {
		p.cursor = rows[len(rows)-1].Path
		return interBatchPause
	}
	// The pass has reached the end. One that found no artist due rests
	// until the earliest retry it saw, unless a refused lookup was kept
	// since it began, whose row it may not have read.
	rest := time.Time{}
	if !p.progressed && !p.due.IsZero() && e.portraitRefusals.Load() == p.refusals {
		rest = earliest(p.due, now.Add(portraitRestMost))
	}
	p.cursor, p.progressed, p.due, p.restUntil = "", false, time.Time{}, rest
	return e.PollInterval
}

// portraitGroup is the rows of a page that name one artist, and the name the
// first of them keeps.
type portraitGroup struct {
	mbid, query string
	rows        []manifest.PendingPortrait
}

// groupPortraits groups rows by artist MBID, in the order each artist first
// comes, so one lookup serves every row of an artist the page holds.
func groupPortraits(rows []manifest.PendingPortrait) []portraitGroup {
	at := make(map[string]int, len(rows))
	var out []portraitGroup
	for _, r := range rows {
		if i, ok := at[r.ArtistMBID]; ok {
			out[i].rows = append(out[i].rows, r)
			continue
		}
		at[r.ArtistMBID] = len(out)
		out = append(out, portraitGroup{mbid: r.ArtistMBID, query: r.Query, rows: []manifest.PendingPortrait{r}})
	}
	return out
}

// portraitsOf asks for each artist's portrait the page's rows wait for, once
// an artist, notes in the pass what it found, and clears the rows it settled.
// It answers false when it stopped before the page's end: a switch refused a
// lookup, or the pass's context ended. The rows it settled before then are
// cleared all the same.
func (e *Enricher) portraitsOf(ctx context.Context, rows []manifest.PendingPortrait, now time.Time) bool {
	p := &e.portraits
	var settled []manifest.PendingPortrait
	defer func() { e.settlePortraits(ctx, settled) }()
	for _, g := range groupPortraits(rows) {
		if ctx.Err() != nil {
			return false
		}
		if !isValidMBID(g.mbid) {
			// No portrait can be filed under a value that is no MBID, so
			// what such a row keeps is no lookup at all. The writers never
			// keep one (manifest.Track.PortraitRefused); this is the layer
			// under them, and the value is bounded before it is logged.
			logger.Warn("dropping a pending portrait whose artist is no MBID",
				"path", g.rows[0].Path, "value", truncateForLog(g.mbid))
			settled = append(settled, g.rows...)
			p.progressed = true
			continue
		}
		if at, wait := e.portraitRetryAt(g.mbid, now); wait {
			p.due = earliest(p.due, at)
			continue
		}
		p.progressed = true
		outcome := portraitNone
		var err error
		if !e.deezerNegCache.Has(g.mbid) {
			outcome, err = e.artistPortrait(ctx, g.mbid, g.query)
		}
		switch outcome {
		case portraitHeld:
			e.notePortraitSettled(g.mbid)
			settled = append(settled, g.rows...)
		case portraitNone:
			e.deezerNegCache.Set(g.mbid, struct{}{})
			e.notePortraitSettled(g.mbid)
			settled = append(settled, g.rows...)
			if err != nil && ctx.Err() == nil {
				e.logUpstream(upstreamDeezer, "artist image", err, "mbid", g.mbid)
			}
		case portraitRefused:
			return false
		case portraitFailed:
			if ctx.Err() != nil {
				return false
			}
			e.logUpstream(upstreamDeezer, "artist image", err, "mbid", g.mbid)
			p.due = earliest(p.due, e.notePortraitFailed(g.mbid))
		}
	}
	return true
}

// settlePortraits clears the pending query of the rows settled, each only at
// the version the page read it at, asking enrich.enabled again at the write
// as a stamp does (stampAdmitted).
func (e *Enricher) settlePortraits(ctx context.Context, rows []manifest.PendingPortrait) {
	if len(rows) == 0 {
		return
	}
	if e.beforePortraitClear != nil {
		e.beforePortraitClear()
	}
	if !e.stampAdmitted(ctx) {
		return
	}
	n, err := e.store.ClearPendingPortraits(ctx, rows)
	if err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("clear pending portraits", "err", failure)
		}
		return
	}
	if n > 0 {
		logger.Info("settled portraits Deezer was not asked for while it was off", "rows", n)
	}
}

// idle waits pause before Run's next turn, and less when a switch turns on
// that lets the enricher take up what it could not as its turn ended:
// enrich.enabled, for the rows that wait for it, or Deezer beside it, for the
// portraits that switch refused (backlog B761). on and portraits are what the
// turn found. It asks the switches every switchEvery (enrichgate.Interval, a
// second), and only while the pause is longer than that. It answers false
// once ctx ends.
func (e *Enricher) idle(ctx context.Context, pause time.Duration, on, portraits bool) bool {
	every := e.switchEvery
	if every <= 0 {
		every = enrichgate.Interval
	}
	if pause <= every {
		return sleepCtx(ctx, pause)
	}
	wait := time.NewTimer(pause)
	defer wait.Stop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-wait.C:
			return true
		case <-tick.C:
			nowOn := e.on()
			nowPortraits := nowOn && e.deezerOn()
			if (nowOn && !on) || (nowPortraits && !portraits) {
				return true
			}
			on, portraits = nowOn, nowPortraits
		}
	}
}
