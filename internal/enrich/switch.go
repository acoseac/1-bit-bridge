package enrich

import (
	"context"
	"errors"
	"sync"
	"time"
)

// enrich.enabled and enrich.deezerEnabled, as the enricher reads them
// (backlog B617). Each is a live predicate cmd/bridge hands the enricher
// (WithEnabled, WithDeezerEnabled), nil meaning on, the way acousticEnabled
// reads, since NewEnricher's other callers are tests.
//
// Off, the enricher asks MusicBrainz, the Cover Art Archive, Deezer and Atlas
// nothing: Run asks enabled before each batch and parks without touching the
// store, and each batch runs on a context CancelWhenOff cancels within a
// second of a switch-off, so a request held at an upstream is cancelled and
// the pass reaches stampEnriched / markSkipped on a cancelled context, which
// record nothing, as at a shutdown. A row waits at enriched_at = 0, and
// turning the switch on resumes it. deezerEnabled stops the Deezer requests
// alone (errDeezerOff).

// cancelWhenOffEvery is how far apart CancelWhenOff asks its predicate: a
// switch-off stops what runs on its context within a second.
const cancelWhenOffEvery = time.Second

// CancelWhenOff returns a context derived from parent that is cancelled once
// on answers false, asked at most a second apart, and the function that
// releases it, which the caller defers. A context whose on answers false at
// the start is cancelled already. A nil on is never off: only parent ends the
// context then.
//
// The enricher's batch and the fingerprint sweep run on it, so the switch
// stops a pass the way a shutdown stops one: a request in flight is
// cancelled, a pacing sleep ends, a decode is killed, and a write the pass
// reaches on the cancelled context records nothing.
func CancelWhenOff(parent context.Context, on func() bool) (context.Context, context.CancelFunc) {
	return cancelWhenOff(parent, on, cancelWhenOffEvery)
}

// cancelWhenOff is CancelWhenOff asking on every `every`.
func cancelWhenOff(parent context.Context, on func() bool, every time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if on == nil {
		return ctx, cancel
	}
	if !on() {
		cancel()
		return ctx, cancel
	}
	released := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-released:
				return
			case <-tick.C:
				if !on() {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(released)
			<-exited
			cancel()
		})
	}
}

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

// errDeezerOff is ensureArtistImageCached's answer while enrich.deezerEnabled
// is off: Deezer was not asked. It is no miss, so neither caller puts it in
// deezerNegCache, which would outlive the switch for the process's life, and
// neither logs it. The track is stamped with no portrait, which "Retry
// missing" queues again once Deezer is on.
var errDeezerOff = errors.New("enrich: Deezer artist portraits are switched off")

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
