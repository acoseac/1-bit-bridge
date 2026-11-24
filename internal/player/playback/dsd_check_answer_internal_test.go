package playback

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// The listener's answer to the DSD check and the check's member of the read
// side, inside the backend (B728 B12 PR 4a): a check of another boot of the
// backend is not answerable, an answer whose publication panics keeps its
// write and consumes its check, and a transition of the check that meets a
// choice's write holding its reading stores no pair and is owed. A complete
// check's record is made here as its end leaves one (completeCheck): the
// real check's run is internal/alsapcm's.

// bootOver is a backend over store, its effective DSD set the fixture DAC's,
// read: publishedBackend's, on a store of the test's.
func bootOver(t *testing.T, store DSDStore, log *slog.Logger) *Backend {
	t.Helper()
	pol := &DSDPolicy{Store: store, Kernel: fixtureKernel, Box: fixtureBox{}, Probes: alsapcm.NewProbeCache(),
		Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: fixtureUSB(), Enumeration: "test/2"}, true },
		Now:   func() time.Time { return fixtureT0 }}
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, DSD: pol, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b
}

// completeCheck puts a complete native DSD64 check of the DAC b publishes in
// its record, as the check's end leaves one, published: its ID.
func completeCheck(t *testing.T, b *Backend) string {
	t.Helper()
	pub := b.dsdPub.Load()
	if pub.view.DAC == nil {
		t.Fatal("the backend publishes no DAC")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&checkCause{phase: CheckComplete})
	c := &checkHold{id: "0123456789abcdef0123456789abcdef", transport: dsdstate.TransportNative, bitRate: dsdstate.DSD64, token: pub.view.DAC.Token,
		identity: pub.set.set.Identity(), ctx: ctx, cancel: cancel, done: make(chan struct{}), confirmed: true, phase: CheckComplete, completed: b.dsdNow()}
	close(c.done)
	b.mu.Lock()
	b.checkRec = c
	b.publishCheckLocked()
	b.mu.Unlock()
	return c.id
}

// answerWith is an answer of heard to id, for the DAC under key and token.
func answerWith(b *Backend, key string, token DACToken, id, heard string) ChoiceResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return b.AnswerCheck(ctx, AnswerRequest{DACKey: key, DACToken: token, CheckID: id, Heard: heard})
}

// TestACheckFromAnotherBootIsNotAnswerable: a check's record and its token
// are one boot's: a new backend over the same state takes the old token for
// another DAC's (dac_changed), and its own token with the old check's ID for
// no check (check_not_answerable), nothing written; the boot that ran it
// takes the answer.
func TestACheckFromAnotherBootIsNotAnswerable(t *testing.T) {
	store := &fixtureStore{st: nativeFixture(t)}
	first := bootOver(t, store, nil)
	id := completeCheck(t, first)
	key, old := boundKey(t, first), boundToken(t, first)
	second := bootOver(t, store, nil)
	before := store.State()
	if res := answerWith(second, key, old, id, dsdstate.AnswerClean); res.Outcome != ChoiceDACChanged {
		t.Errorf("another boot's token: %+v; want dac_changed", res)
	}
	if res := answerWith(second, key, boundToken(t, second), id, dsdstate.AnswerClean); res.Outcome != ChoiceCheckNotAnswerable {
		t.Errorf("another boot's check: %+v; want check_not_answerable", res)
	}
	if after := store.State(); len(after.DACs[0].Listener) != len(before.DACs[0].Listener) {
		t.Errorf("answers another boot did not take wrote %+v", after.DACs[0].Listener)
	}
	if res := answerWith(first, key, old, id, dsdstate.AnswerClean); res.Outcome != ChoiceWritten {
		t.Errorf("the boot that ran the check: %+v; want written", res)
	}
}

// TestAnAnswerWhosePublicationPanicsKeepsItsWrite: an answer whose write is
// durable and whose publication panics before it stores its pair keeps its
// outcome, written, with no epoch, as a choice's write does: the panic is
// contained and a refresh asked, and the check is consumed, answered, the
// refresh publishing it so.
func TestAnAnswerWhosePublicationPanicsKeepsItsWrite(t *testing.T) {
	rec := loggingtest.NewRecorder()
	var b *Backend
	var saved *dsdSet
	log := slog.New(hookedLog{Handler: rec.Logger().Handler(), hook: func() {
		if saved != nil {
			b.dsd.Store(saved)
		}
	}})
	store := &fixtureStore{st: nativeFixture(t)}
	b = bootOver(t, store, log)
	id := completeCheck(t, b)
	key, token := boundKey(t, b), boundToken(t, b)
	refreshed := make(chan struct{})
	var once sync.Once
	b.cfg.Seams = &Seams{ChoiceWritten: func(error) {
		saved = b.dsd.Load()
		b.dsd.Store(nil)
	}, ChoiceRefreshed: func() { once.Do(func() { close(refreshed) }) }}
	res := answerWith(b, key, token, id, dsdstate.AnswerClean)
	select {
	case <-refreshed:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh the containment asked never ran")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	b.cfg.Seams = nil
	if res.Outcome != ChoiceWritten || res.Epoch {
		t.Errorf("the answer whose publication panicked: %+v; want written, with no epoch", res)
	}
	if st := store.State(); len(st.DACs[0].Listener) != 1 || st.DACs[0].Listener[0].Answer != dsdstate.AnswerClean {
		t.Errorf("the state holds %+v; want the clean answer", st.DACs[0].Listener)
	}
	if st, _ := b.CheckStatus(); st.Phase != CheckAnswered {
		t.Errorf("the check is %s; want answered, consumed by its durable write", st.Phase)
	}
	if m := b.dsdPub.Load().view.DAC.Check; m == nil || m.Phase != CheckAnswered {
		t.Errorf("the refresh published the check %+v; want answered", m)
	}
	if lines := rec.Lines(panicMessage); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "the publication of a choice's write") {
		t.Errorf("the panic was not logged as the publication's: %q", rec.All())
	}
	if res := answerWith(b, key, token, id, dsdstate.AnswerNoise); res.Outcome != ChoiceCheckNotAnswerable {
		t.Errorf("a second answer: %+v; want check_not_answerable", res)
	}
}

// TestACheckTransitionBesideAHeldChoiceReadingIsOwed: a transition of the
// check while a choice's write holds its reading stores no pair, so none is
// stored between that write's reading and its publication, and is owed: the
// write's end asks a refresh, which publishes the transition.
func TestACheckTransitionBesideAHeldChoiceReadingIsOwed(t *testing.T) {
	b := bootOver(t, &fixtureStore{st: nativeFixture(t)}, nil)
	refreshed := make(chan struct{})
	var once sync.Once
	b.cfg.Seams = &Seams{ChoiceRefreshed: func() { once.Do(func() { close(refreshed) }) }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := b.takeChoiceReading(ctx); err != nil {
		t.Fatal(err)
	}
	standing := b.dsdPub.Load()
	completeCheck(t, b)
	b.mu.Lock()
	owed := b.checkOwed
	b.mu.Unlock()
	if b.dsdPub.Load() != standing || !owed {
		t.Errorf("a transition beside the held reading stored a pair %v, owed %v; want none stored, owed", b.dsdPub.Load() != standing, owed)
	}
	b.endChoiceReading(ctx)
	select {
	case <-refreshed:
	case <-time.After(10 * time.Second):
		t.Fatal("no refresh was asked for the owed transition")
	}
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	b.cfg.Seams = nil
	if m := b.dsdPub.Load().view.DAC.Check; m == nil || m.Phase != CheckComplete {
		t.Errorf("the owed refresh published the check %+v; want complete", m)
	}
}
