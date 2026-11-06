package enrich

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAnEnricherSwitchedOffAsksNoUpstream: with enrich.enabled off, the
// enricher asks MusicBrainz, the Cover Art Archive, Deezer and Atlas nothing,
// reads nothing of the store and stamps nothing, however long it runs
// (backlog B617). On main nothing turned it off: an enricher handed this
// gate went on asking every service about the two seeded tracks.
func TestAnEnricherSwitchedOffAsksNoUpstream(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	seedSwitchTracks(t, store)

	var asked atomic.Int64
	e.WithEnabled(func() bool {
		asked.Add(1)
		return false
	})
	stop := runEnricher(t, e)
	// Five turns of the loop, or the first request: either is the enricher's
	// chance to ask.
	waitFor(t, func() bool { return asked.Load() >= 5 || ups.total() > 0 },
		"the enricher to ask its gate five times")
	stop()

	if got := ups.all(); len(got) != 0 {
		t.Errorf("an enricher switched off sent %d request(s):\n%s", len(got), strings.Join(got, "\n"))
	}
	waiting := unenrichedPaths(t, store)
	for _, tr := range switchTracks {
		if !waiting[tr.Path] {
			t.Errorf("%s was stamped by an enricher switched off", tr.Path)
		}
	}
	if done, skipped := e.Done(), e.skipped.Load(); done != 0 || skipped != 0 {
		t.Errorf("an enricher switched off counted %d done and %d skipped", done, skipped)
	}

	t.Run("it reads nothing of the store", func(t *testing.T) {
		e, store, _ := switchEnricher(t, newSwitchUpstreams(t))
		_ = store.Close() // any read of the store now fails, and Run says so
		var asked atomic.Int64
		e.WithEnabled(func() bool {
			asked.Add(1)
			return false
		})
		rec := loggingtest.Record(t)
		stop := runEnricher(t, e)
		waitFor(t, func() bool { return asked.Load() >= 5 || len(rec.Failures(msgListUnenriched)) > 0 },
			"the enricher to ask its gate five times")
		stop()
		if got := rec.Failures(msgListUnenriched); len(got) != 0 {
			t.Errorf("an enricher switched off listed the store's unenriched tracks:\n%s", strings.Join(got, "\n"))
		}
	})
}
