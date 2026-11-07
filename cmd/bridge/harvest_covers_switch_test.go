package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
)

// The harvest's premium cover fetch and enrich.enabled (backlog B617): off
// stops every premium cover request, and a request it refuses, an answer it
// does not take or a fetch it cuts off is ErrCoversPaused, which the sweep
// keeps the cover pending through, never a miss it counts toward the cap.

const switchCoverRelease = "77777777-7777-4777-8777-777777777777"

// switchCoverCred is the harvest credential the premium fetcher reads before
// each request; at, when set, runs as it is read: after the adapter asked the
// switch and before the request is sent.
type switchCoverCred struct {
	base string
	at   func()
}

func (c *switchCoverCred) AtlasCredential() (string, string, bool) {
	if c.at != nil {
		c.at()
	}
	return "bh-token", c.base, true
}

// switchCoverFixture is the serve adapter over the real premium fetcher, the
// switch it asks, and a TLS Atlas that answers each cover with a premium one
// unless source says otherwise.
type switchCoverFixture struct {
	atlas      *switchCounter
	on         atomic.Bool
	source     atomic.Value // the X-Atlas-Asset-Source Atlas answers with
	cred       *switchCoverCred
	adapter    atlasCoverRefetcher
	artworkDir string
}

func newSwitchCoverFixture(t *testing.T, every time.Duration) *switchCoverFixture {
	t.Helper()
	f := &switchCoverFixture{artworkDir: t.TempDir()}
	f.on.Store(true)
	f.source.Store("tidal")
	f.atlas = &switchCounter{}
	f.atlas.srv = httptest.NewTLSServer(f.atlas.handler(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/release/"+switchCoverRelease+"/front-") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Atlas-Asset-Source", f.source.Load().(string))
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10})
	}))
	t.Cleanup(f.atlas.srv.Close)
	f.cred = &switchCoverCred{base: f.atlas.srv.URL}
	f.adapter = atlasCoverRefetcher{
		premium:      enrich.NewAtlasPremiumFetcher(f.cred, "ua", f.atlas.srv.Client(), f.artworkDir, onSwitches),
		artworkDir:   f.artworkDir,
		coverSize:    enrich.DefaultCoverSize,
		enrichmentOn: f.on.Load,
		switchEvery:  every,
	}
	return f
}

// written says whether the release's cover is in the cache.
func (f *switchCoverFixture) written() bool {
	_, err := os.Stat(enrich.ArtworkCachePath(f.artworkDir, switchCoverRelease, enrich.DefaultCoverSize))
	return err == nil
}

// TestTheCoverAdapterAnswersAsItDidWithTheSwitchOn: on, a premium answer is
// a cover written and a CAA one a miss, as before the switch.
func TestTheCoverAdapterAnswersAsItDidWithTheSwitchOn(t *testing.T) {
	f := newSwitchCoverFixture(t, time.Hour)
	f.source.Store("caa")
	if got, err := f.adapter.RefetchPremium(context.Background(), switchCoverRelease); got || err != nil {
		t.Errorf("a CAA answer with the switch on = %v, %v; want a miss", got, err)
	}
	f.source.Store("tidal")
	if got, err := f.adapter.RefetchPremium(context.Background(), switchCoverRelease); !got || err != nil || !f.written() {
		t.Errorf("a premium answer with the switch on = %v, %v, written %v; want the cover written", got, err, f.written())
	}
}

// TestTheCoverAdapterAsksAtlasNothingWhileEnrichmentIsOff: off, the adapter
// sends nothing and answers the pause.
func TestTheCoverAdapterAsksAtlasNothingWhileEnrichmentIsOff(t *testing.T) {
	f := newSwitchCoverFixture(t, time.Hour)
	f.on.Store(false)
	got, err := f.adapter.RefetchPremium(context.Background(), switchCoverRelease)
	if got || !errors.Is(err, atlasharvest.ErrCoversPaused) {
		t.Errorf("RefetchPremium with enrichment off = %v, %v; want ErrCoversPaused", got, err)
	}
	if n := f.atlas.count(); n != 0 || f.written() {
		t.Errorf("Atlas was asked %d time(s) with enrichment off; written %v", n, f.written())
	}
}

// TestACoverFetchStartedJustAfterTheFlipIsRefused: the switch goes off after
// the adapter asked it and before the request leaves (as the credential is
// read), and the fetch's context asks it only once an hour here: the premium
// fetcher's own client refuses the request at its send.
func TestACoverFetchStartedJustAfterTheFlipIsRefused(t *testing.T) {
	f := newSwitchCoverFixture(t, time.Hour)
	f.cred.at = func() { f.on.Store(false) }
	got, err := f.adapter.RefetchPremium(context.Background(), switchCoverRelease)
	if got || !errors.Is(err, atlasharvest.ErrCoversPaused) {
		t.Errorf("a fetch started after the flip = %v, %v; want ErrCoversPaused", got, err)
	}
	if n := f.atlas.count(); n != 0 || f.written() {
		t.Errorf("Atlas was asked %d time(s) after the flip; written %v", n, f.written())
	}
}

// TestACoverAnswerAfterTheFlipIsNoMiss: a CAA answer, which with the switch
// on is a miss that counts toward the cap, is the pause when the switch went
// off before it arrived (the client does not take it) or after it was read
// (the adapter asks the switch again), so the cover keeps its attempts.
func TestACoverAnswerAfterTheFlipIsNoMiss(t *testing.T) {
	for _, when := range []string{"before it arrives", "once it is read"} {
		t.Run(when, func(t *testing.T) {
			f := newSwitchCoverFixture(t, time.Hour)
			f.source.Store("caa")
			if when == "before it arrives" {
				f.atlas.setHold(func(*http.Request) { f.on.Store(false) })
			} else {
				f.atlas.setHold(nil)
				f.adapter.premium = enrich.NewAtlasPremiumFetcher(f.cred, "ua",
					&http.Client{Transport: &flipAtBodyEOF{base: f.atlas.srv.Client().Transport, at: func() { f.on.Store(false) }}},
					f.artworkDir, onSwitches)
			}
			got, err := f.adapter.RefetchPremium(context.Background(), switchCoverRelease)
			if got || !errors.Is(err, atlasharvest.ErrCoversPaused) {
				t.Errorf("a CAA answer meeting the switch off = %v, %v; want ErrCoversPaused, never a miss", got, err)
			}
			if n := f.atlas.count(); n != 1 {
				t.Errorf("Atlas was asked %d time(s), want the one request", n)
			}
		})
	}
}

// TestACoverFetchInFlightAtTheFlipIsCancelledAndKept: a request Atlas holds
// when the switch goes off is cancelled at the fetch context's next ask of
// the switch, and answered as the pause, which keeps the cover pending with
// its attempts; nothing is written.
func TestACoverFetchInFlightAtTheFlipIsCancelledAndKept(t *testing.T) {
	f := newSwitchCoverFixture(t, time.Millisecond)
	holding, cancelled := make(chan struct{}), make(chan struct{})
	f.atlas.setHold(func(r *http.Request) {
		close(holding)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	})
	type answer struct {
		got bool
		err error
	}
	answered := make(chan answer, 1)
	go func() {
		got, err := f.adapter.RefetchPremium(context.Background(), switchCoverRelease)
		answered <- answer{got, err}
	}()
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the cover request never reached Atlas")
	}
	f.on.Store(false)
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the cover request held at the switch-off was not cancelled")
	}
	a := <-answered
	if a.got || !errors.Is(a.err, atlasharvest.ErrCoversPaused) {
		t.Errorf("a fetch cut off by the switch = %v, %v; want ErrCoversPaused", a.got, a.err)
	}
	if f.written() {
		t.Error("a cover was written by a fetch the switch cut off")
	}
}
