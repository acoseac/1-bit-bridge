package enrich

import (
	"context"
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The artist portraits that wait for the portrait batch, fetched once
// enrichment and Deezer are both on and their retry is due (backlog B761).
//
// A pass whose portrait lookup waits stamps its track like any other, so
// nothing at enriched_at says the portrait is missing: the stamp keeps, in the
// row, the name the lookup asks Deezer by and the artist it was taken for
// (manifest.Track.PortraitPending). A lookup waits when the Deezer switch
// refused it, when it failed, and when the artist's last attempt by that name
// failed and its retry is not due: one backoff, the store's
// (manifest.PortraitAttempts, which a restart keeps), holds every path that
// asks Deezer for a portrait (the ordinary pass, the fingerprint path and the
// batch), and a lookup it holds back is recorded for the batch, so none is
// left with nothing to come back to it. While enrichment and Deezer are both
// on, each turn of Run takes one page of those rows (runPortraits) and asks
// Deezer once for each artist and name the page holds, by that name and the
// row's artist MBID: no MusicBrainz or Cover Art Archive request, and no stamp
// reset.
//
// What an attempt comes to (portraitOutcome) settles rows by one rule on
// every path: a portrait held settles every row of the artist; an answer that
// ends the name (Deezer has no such artist or no picture, or names a picture
// no wait changes for that name: on a host the client refuses, no image, too
// large) settles the rows that wait by that name alone, and goes in
// deezerNegCache under the artist and the name, so another name of the artist
// is still asked; a refusal keeps the rows waiting; and a failure (any other
// error) keeps them waiting and adds to the store's failed attempts, which put
// the next attempt off. Each row is cleared only at the version the page read
// it at.

// portraitOutcome is what one attempt at an artist's portrait came to.
type portraitOutcome int

const (
	// portraitHeld: the portrait is in the artwork cache, found there or
	// fetched. Settles every row of the artist.
	portraitHeld portraitOutcome = iota
	// portraitNone: Deezer answered that it has none by this name: no
	// artist matched it, or its search or its picture answered 404 or
	// nothing. Ends the name: settles its rows, and goes in deezerNegCache.
	portraitNone
	// portraitRejected: Deezer's answer named a picture no wait changes for
	// this name: on a host the client refuses, at a URL that does not
	// parse, past the size cap, or not an image (portraitRejectedAnswer).
	// Ends the name, as portraitNone does, and the artist's other names are
	// still asked.
	portraitRejected
	// portraitRefused: a switch refused the lookup or cut it off, so Deezer
	// was not asked or its answer was not taken (errDeezerOff, or
	// enrich.enabled's ErrOff). No answer: never a miss.
	portraitRefused
	// portraitFailed: no answer that settles anything: a transient failure,
	// a cancellation, or any other error (a decode error, a write that
	// failed, no artwork cache). Asked again once its retry is due.
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
	case portraitRejectedAnswer(err):
		return portraitRejected
	default:
		return portraitFailed
	}
}

// portraitRejectedAnswer reports whether err is Deezer's answer naming a
// picture no wait changes for the name it was asked by.
func portraitRejectedAnswer(err error) bool {
	var host *refusedHostError
	return errors.As(err, &host) || errors.Is(err, errNotAnImage) ||
		errors.Is(err, errImageTooLarge) || errors.Is(err, errBadImageURL)
}

// artistPortrait asks for mbid's portrait by name (ensureArtistImageCached)
// and answers what the attempt came to, with its error.
func (e *Enricher) artistPortrait(ctx context.Context, mbid, name string) (portraitOutcome, error) {
	found, err := e.ensureArtistImageCached(ctx, mbid, name)
	return classifyPortrait(found, err), err
}

// portraitKey is deezerNegCache's key for mbid's portrait by name. An MBID
// holds no NUL and is of one length, so the key reads one way only.
func portraitKey(mbid, name string) string { return mbid + "\x00" + name }

const (
	// portraitRetryFirst is how long an artist's portrait by a name waits
	// after its first failed attempt before it is asked again; each failure
	// in a row doubles it, up to portraitRetryMost.
	portraitRetryFirst = time.Minute
	portraitRetryMost  = 6 * time.Hour
	// portraitRestMost bounds how long the batch rests after a pass that
	// found no artist due (portraitPass.restUntil). The rest ends at the
	// earliest retry the pass saw, and at once when a pass keeps a refused
	// lookup; this bound is for a row a re-enrichment hands back waiting by a
	// name whose retry is due before the rest ends, which waits this long at
	// most past it.
	portraitRestMost = 10 * time.Minute
	// defaultPortraitBatchLimit is PortraitBatchLimit's zero value.
	defaultPortraitBatchLimit = 50
)

// portraitPass is the portrait batch's pass over the rows whose lookup
// waits, page by page in path order. Run's goroutine alone reads and writes
// it.
type portraitPass struct {
	// cursor is the path the pass's last page ended on, "" between passes.
	cursor string
	// progressed says the pass asked for a portrait, or settled a row.
	progressed bool
	// due is the earliest retry among the names the pass waited for or saw
	// fail, zero for none.
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

// portraitRetryAt is when the attempt after a may be made: at once where none
// failed, else portraitRetryFirst after the last failure, twice as long for
// each failure in a row after the first, and portraitRetryMost at most.
func portraitRetryAt(a manifest.PortraitAttempts) time.Time {
	if a.Failures == 0 {
		return time.Time{}
	}
	wait := portraitRetryFirst
	for i := 1; i < a.Failures && wait < portraitRetryMost; i++ {
		wait *= 2
	}
	return a.LastFailedAt.Add(min(wait, portraitRetryMost))
}

// portraitWaits reports whether mbid's portrait by name has to wait at now,
// and until when: its last attempt failed and the retry is not due. A store
// it cannot read is a wait of portraitRetryFirst, so no path asks Deezer
// blind.
func (e *Enricher) portraitWaits(ctx context.Context, mbid, name string, now time.Time) (time.Time, bool) {
	if e.store == nil {
		return time.Time{}, false
	}
	a, err := e.store.PortraitAttemptsOf(ctx, mbid, name)
	if err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("read portrait attempts", "mbid", mbid, "err", failure)
		}
		return now.Add(portraitRetryFirst), true
	}
	at := portraitRetryAt(a)
	return at, now.Before(at)
}

// notePortraitFailed adds a failed attempt at mbid's portrait by name, at
// now, to the store's, and returns when the next attempt is due.
func (e *Enricher) notePortraitFailed(ctx context.Context, mbid, name string, now time.Time) time.Time {
	if e.store == nil {
		return now.Add(portraitRetryFirst)
	}
	a, err := e.store.NotePortraitFailure(ctx, mbid, name, now)
	if err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("record a failed portrait attempt", "mbid", mbid, "err", failure)
		}
		return now.Add(portraitRetryFirst)
	}
	return portraitRetryAt(a)
}

// notePortraitFetched forgets mbid's failed attempts by every name: its
// portrait is in the cache.
func (e *Enricher) notePortraitFetched(ctx context.Context, mbid string) {
	if e.store == nil {
		return
	}
	if err := e.store.ForgetArtistPortraitAttempts(ctx, mbid); err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("forget failed portrait attempts", "mbid", mbid, "err", failure)
		}
	}
}

// notePortraitNameEnded remembers that an answer ended mbid's portrait by
// name, in deezerNegCache and by forgetting the name's failed attempts.
func (e *Enricher) notePortraitNameEnded(ctx context.Context, mbid, name string) {
	e.deezerNegCache.Set(portraitKey(mbid, name), struct{}{})
	if e.store == nil {
		return
	}
	if err := e.store.ForgetPortraitAttempts(ctx, mbid, name); err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("forget failed portrait attempts", "mbid", mbid, "err", failure)
		}
	}
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

// portraitBatchLimit resolves PortraitBatchLimit's zero value.
func (e *Enricher) portraitBatchLimit() int {
	if e.PortraitBatchLimit <= 0 {
		return defaultPortraitBatchLimit
	}
	return e.PortraitBatchLimit
}

// askPortrait is one pass's attempt at mbid's portrait by name, the ordinary
// pass's and the fingerprint path's alike, and records on t what the stamp
// writes of it (manifest.Track.PortraitPending, PortraitSettled). It answers
// enrich.enabled's refusal, which stops the pass and its stamp, and nil
// otherwise: a portrait is tier 2, so no failure of it blocks the stamp. A
// name an answer ended settles the row with no request; a portrait in the
// cache settles it; a name whose retry is not due leaves the row waiting with
// no request; and an attempt's outcome does as portraitOutcome says.
// logArgs are the log lines' attributes before the MBID.
func (e *Enricher) askPortrait(ctx context.Context, t *manifest.Track, mbid, name string, logArgs ...any) error {
	logArgs = append(logArgs, "mbid", mbid)
	if e.deezerNegCache.Has(portraitKey(mbid, name)) {
		// The memo is of an answer, so it settles the row, and sibling
		// tracks of the artist ask nothing again by that name.
		t.PortraitSettled()
		return nil
	}
	if cached, err := e.portraitCached(mbid, name); err == nil && cached {
		t.PortraitSettled()
		return nil
	}
	now := e.now()
	if _, wait := e.portraitWaits(ctx, mbid, name, now); wait {
		// The name's last attempt failed and its retry is not due: no
		// request, and the row waits for the batch, which asks once it is.
		t.PortraitPending(mbid, name)
		return nil
	}
	outcome, err := e.artistPortrait(ctx, mbid, name)
	switch outcome {
	case portraitHeld:
		e.notePortraitFetched(ctx, mbid)
		t.PortraitSettled()
	case portraitNone, portraitRejected:
		e.notePortraitNameEnded(ctx, mbid, name)
		t.PortraitSettled()
		if err != nil && ctx.Err() == nil {
			// A 404 or an empty picture is Debug through the streak; a
			// picture no wait changes is said as the failure it is.
			e.logUpstream(upstreamDeezer, "artist image", err, logArgs...)
		}
	case portraitRefused:
		if r := enrichgate.Refused(err); errors.Is(r, enrichgate.ErrOff) {
			// enrich.enabled's refusal: no miss to cache, nothing to log,
			// and no stamp, as at the other requests.
			return r
		}
		// Deezer was not asked: no miss to cache, nothing to log, and the
		// row waits by the name it would have been asked by.
		t.PortraitPending(mbid, name)
		e.notePortraitRefused()
	case portraitFailed:
		// A shutdown's cancellation is no failure, and no stamp follows it
		// (ensureArtistImageCached returns ctx.Err() from its pacing
		// sleepCtx). Mirrors the album/artist tier-1 ctx guard.
		if ctx.Err() != nil {
			return nil
		}
		e.logUpstream(upstreamDeezer, "artist image", err, logArgs...)
		e.notePortraitFailed(ctx, mbid, name, now)
		t.PortraitPending(mbid, name)
	}
	return nil
}

// runPortraits takes one page of the rows whose portrait lookup waits, past
// where the pass's last page ended, and returns how long Run waits before its
// next turn, and the switch's refusal when one stopped it (enrich.enabled's
// ErrOff, or Deezer's errDeezerOff), which Run's wait reads as that switch
// seen off. The wait is interBatchPause while the page was full, so the next
// page follows at once, and PollInterval once the pass has reached the end, a
// switch stopped it, or the batch rests. Run calls it only while enrichment
// and Deezer are both on, and it runs on enrich.enabled's context as a batch
// does; each Deezer request runs on Deezer's own (deezerCall).
//
// Rotation: each page reads past the last page's end, and a pass that reaches
// the end starts the next at the beginning, so rows whose artist keeps
// failing at the head of the order hold back no row behind them; and a name
// that failed is asked again only once its retry is due (portraitWaits), so it
// costs no request in between. One page a turn, beside the ordinary batch,
// keeps both moving. A pass that found no name due, every one waiting for its
// retry, is not followed by another until the earliest of those retries
// (portraitRestMost at most), unless a pass keeps a refused lookup meanwhile:
// the batch reads nothing while it rests.
func (e *Enricher) runPortraits(ctx context.Context) (time.Duration, error) {
	if e.deezer == nil || e.store == nil {
		return e.PollInterval, nil
	}
	p := &e.portraits
	now := e.now()
	if p.cursor == "" {
		refusals := e.portraitRefusals.Load()
		if now.Before(p.restUntil) && refusals == p.refusals {
			return e.PollInterval, nil
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
		return e.PollInterval, enrichgate.Refusal(ctx)
	}
	if ok, refused := e.portraitsOf(ctx, rows, now); !ok {
		// A switch refused a lookup, or the pass was stopped: the rows wait
		// as they were, and the next pass starts over.
		p.cursor, p.progressed, p.due = "", false, time.Time{}
		if refused == nil {
			refused = enrichgate.Refusal(ctx)
		}
		return e.PollInterval, refused
	}
	if len(rows) == limit {
		p.cursor = rows[len(rows)-1].Path
		return interBatchPause, nil
	}
	// The pass has reached the end. One that found no name due rests until
	// the earliest retry it saw, unless a refused lookup was kept since it
	// began, whose row it may not have read.
	rest := time.Time{}
	if !p.progressed && !p.due.IsZero() && e.portraitRefusals.Load() == p.refusals {
		rest = earliest(p.due, now.Add(portraitRestMost))
	}
	p.cursor, p.progressed, p.due, p.restUntil = "", false, time.Time{}, rest
	return e.PollInterval, nil
}

// portraitGroup is the rows of a page that wait for one artist's portrait by
// one name.
type portraitGroup struct {
	mbid, query string
	rows        []manifest.PendingPortrait
}

// groupPortraits groups rows by artist MBID and name, in the order each
// first comes, so one lookup serves every row that waits for it, and each
// name of an artist is a lookup of its own.
func groupPortraits(rows []manifest.PendingPortrait) []portraitGroup {
	at := make(map[string]int, len(rows))
	var out []portraitGroup
	for _, r := range rows {
		key := portraitKey(r.ArtistMBID, r.Query)
		if i, ok := at[key]; ok {
			out[i].rows = append(out[i].rows, r)
			continue
		}
		at[key] = len(out)
		out = append(out, portraitGroup{mbid: r.ArtistMBID, query: r.Query, rows: []manifest.PendingPortrait{r}})
	}
	return out
}

// portraitsOf asks for each portrait the page's rows wait for, once an
// artist and name, notes in the pass what it found, and clears the rows it
// settled. It answers false when it stopped before the page's end: a switch
// refused a lookup, whose refusal it answers too, or the pass's context
// ended. The rows it settled before then are cleared all the same.
func (e *Enricher) portraitsOf(ctx context.Context, rows []manifest.PendingPortrait, now time.Time) (bool, error) {
	p := &e.portraits
	var settled []manifest.PendingPortrait
	defer func() { e.settlePortraits(ctx, settled) }()
	for _, g := range groupPortraits(rows) {
		if ctx.Err() != nil {
			return false, nil
		}
		if !isValidMBID(g.mbid) {
			// No portrait can be filed under a value that is no MBID, so
			// what such a row keeps is no lookup at all. The writers never
			// keep one (manifest.Track.PortraitPending); this is the layer
			// under them, and the value is bounded before it is logged.
			logger.Warn("dropping a pending portrait whose artist is no MBID",
				"path", g.rows[0].Path, "value", truncateForLog(g.mbid))
			settled = append(settled, g.rows...)
			p.progressed = true
			continue
		}
		if e.deezerNegCache.Has(portraitKey(g.mbid, g.query)) {
			settled = append(settled, g.rows...)
			p.progressed = true
			continue
		}
		if cached, err := e.portraitCached(g.mbid, g.query); err == nil && cached {
			settled = append(settled, g.rows...)
			p.progressed = true
			continue
		}
		if at, wait := e.portraitWaits(ctx, g.mbid, g.query, now); wait {
			p.due = earliest(p.due, at)
			continue
		}
		p.progressed = true
		outcome, err := e.artistPortrait(ctx, g.mbid, g.query)
		switch outcome {
		case portraitHeld:
			e.notePortraitFetched(ctx, g.mbid)
			settled = append(settled, g.rows...)
		case portraitNone, portraitRejected:
			e.notePortraitNameEnded(ctx, g.mbid, g.query)
			settled = append(settled, g.rows...)
			if err != nil && ctx.Err() == nil {
				e.logUpstream(upstreamDeezer, "artist image", err, "mbid", g.mbid)
			}
		case portraitRefused:
			return false, enrichgate.Refused(err)
		case portraitFailed:
			if ctx.Err() != nil {
				return false, nil
			}
			e.logUpstream(upstreamDeezer, "artist image", err, "mbid", g.mbid)
			p.due = earliest(p.due, e.notePortraitFailed(ctx, g.mbid, g.query, now))
		}
	}
	return true, nil
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
		logger.Info("settled portraits that waited for Deezer", "rows", n)
	}
}

// idle waits pause before Run's next turn, and less when a switch turns on
// that lets the enricher take up what it could not as its turn ended:
// enrich.enabled, for the rows that wait for it, or Deezer beside it, for the
// portraits that wait (backlog B761). on and portraits are what the turn found
// them, off where a batch met a refusal. It asks the switches every
// switchEvery (enrichgate.Interval, a second), and only while the pause is
// longer than that. It answers false once ctx ends.
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
