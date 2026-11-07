package enrich

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
)

// enrich.enabled and enrich.deezerEnabled, as the enricher reads them
// (backlog B617). Each is a live predicate cmd/bridge hands the enricher
// (WithEnabled, WithDeezerEnabled), nil meaning on, the way acousticEnabled
// reads, since NewEnricher's other callers are tests; the enricher hands
// enrichgate its methods, on and deezerOn, never a nil.
//
// Off, the enricher asks MusicBrainz, the Cover Art Archive, Deezer and Atlas
// nothing: Run asks enabled before each batch and parks without touching the
// store, and each batch runs on enrich.enabled's context
// (enrichgate.CancelWhenOff). Every client holds the switches cmd/bridge built
// it with (enrichgate.Switches) and asks them whatever context a request
// carries, so a request that starts after the flip is refused before it
// leaves, an answer that arrives after it is not taken, and a request on the
// wire is cancelled within a second; each refusal cancels the batch's context
// at once, so the pass stops as at a shutdown, and stampEnriched, markSkipped
// and the fingerprint veto's write ask the switch again at the write. A row
// waits at enriched_at = 0, and turning the switch on resumes it.
// deezerEnabled stops the Deezer requests alone: each runs on a context of
// its own (enrichgate.CancelWhenDeezerOff), so a Deezer flip cancels a Deezer
// request and leaves the batch, and MusicBrainz on it, running.

// errDeezerOff is a Deezer request enrich.deezerEnabled refused or cut off:
// Deezer was not asked, or its answer was not taken. It is no miss, so
// neither caller of ensureArtistImageCached puts it in deezerNegCache, which
// would outlive the switch for the process's life, and neither logs it. The
// track is stamped with no portrait, which "Retry missing" queues again once
// Deezer is on.
var errDeezerOff = enrichgate.ErrDeezerOff

// WithEnabled attaches the live predicate for enrich.enabled. Nil (the
// default) is on.
func (e *Enricher) WithEnabled(f func() bool) *Enricher {
	e.enabled = f
	return e
}

// WithDeezerEnabled attaches the live predicate for enrich.deezerEnabled. Nil
// (the default) is on.
func (e *Enricher) WithDeezerEnabled(f func() bool) *Enricher {
	e.deezerEnabled = f
	return e
}

// on is enrich.enabled as the predicate answers it now.
func (e *Enricher) on() bool { return e.enabled == nil || e.enabled() }

// deezerOn is enrich.deezerEnabled as the predicate answers it now.
func (e *Enricher) deezerOn() bool { return e.deezerEnabled == nil || e.deezerEnabled() }

// deezerCall makes one Deezer request, call, after Deezer's pacing, on a
// context derived from the batch's that enrich.deezerEnabled cancels
// (enrichgate.CancelWhenDeezerOff), and answers the switch's refusal for a
// pacing or a request the switch ended: errDeezerOff for Deezer's, which
// leaves the batch running, or enrichgate.ErrOff for enrich.enabled's, which
// has cancelled the batch. A request the client refused or whose answer it
// did not take answers the same refusal through call's error.
func (e *Enricher) deezerCall(ctx context.Context, call func(context.Context) error) error {
	dctx, release := enrichgate.CancelWhenDeezerOff(ctx, e.deezerOn, e.switchEvery)
	defer release()
	if !sleepCtx(dctx, e.DeezerMinInterval) {
		if r := enrichgate.Refusal(dctx); r != nil {
			return r
		}
		return ctx.Err()
	}
	err := call(dctx)
	if err != nil {
		if r := enrichgate.Refusal(dctx); r != nil {
			return r
		}
	}
	return err
}

// stampAdmitted asks enrich.enabled again just before a write of what the
// pass learnt, a stamp or a fingerprint veto: off, the write is not made, as
// at a shutdown, and the batch's context is cancelled so the pass stops
// there (enrichgate.Admit). An answer taken while the switch was on can still
// meet it off here.
func (e *Enricher) stampAdmitted(ctx context.Context) bool {
	return enrichgate.Admit(ctx, e.on) == nil
}

// The Info lines the enricher logs about its switches: one at its start for
// each that is off, and one at each flip it sees after that.
const (
	msgEnrichmentOff         = "enrichment is off (enrich.enabled): nothing is sent to MusicBrainz, the Cover Art Archive or Deezer, and the tracks waiting for it wait"
	msgEnrichmentSwitchedOff = "enrichment switched off (enrich.enabled): nothing is sent to MusicBrainz, the Cover Art Archive or Deezer until it is on"
	msgEnrichmentSwitchedOn  = "enrichment switched on (enrich.enabled): the tracks waiting for it are enriched"
	msgDeezerOff             = "Deezer artist portraits are off (enrich.deezerEnabled): an artist enriched meanwhile gets no portrait"
	msgDeezerSwitchedOff     = "Deezer artist portraits switched off (enrich.deezerEnabled): an artist enriched meanwhile gets no portrait"
	msgDeezerSwitchedOn      = "Deezer artist portraits switched on (enrich.deezerEnabled): Retry missing fetches the portraits the artists enriched meanwhile lack"
)

// switchReport keeps what Run last saw of the two switches, to log the lines
// above once each.
type switchReport struct {
	seen       bool
	on, deezer bool
}

// note records the switches as Run sees them now, and logs what changed.
func (r *switchReport) note(on, deezer bool) {
	if !r.seen {
		r.seen, r.on, r.deezer = true, on, deezer
		if !on {
			logger.Info(msgEnrichmentOff)
		}
		if !deezer {
			logger.Info(msgDeezerOff)
		}
		return
	}
	if on != r.on {
		r.on = on
		if on {
			logger.Info(msgEnrichmentSwitchedOn)
		} else {
			logger.Info(msgEnrichmentSwitchedOff)
		}
	}
	if deezer != r.deezer {
		r.deezer = deezer
		if deezer {
			logger.Info(msgDeezerSwitchedOn)
		} else {
			logger.Info(msgDeezerSwitchedOff)
		}
	}
}
