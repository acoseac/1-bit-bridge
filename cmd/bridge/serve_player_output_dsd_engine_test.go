package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The write of the DAC's DSD choice against the engine's sessions (B728 B9
// PR 3): the adapter reserves the player's configuration with the engine's
// owner (engine.ReserveConfiguration) before the backend's write, refused
// while anything plays, starts, pauses or releases, and holds it until the
// backend's write has returned, whatever the request does.

// standInPolicy is what the backend backend publishes for the test DAC known, its
// choice native: the policy serve's stand-in answers with.
func standInPolicy(t *testing.T) playback.PlayerPolicy {
	t.Helper()
	return readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), withKnownDAC).Policy()
}

// closeOnce closes ch once, however often it is called.
func closeOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// answered waits for ch's error until the test's deadline.
func answered(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-serveGiveUp(t):
		t.Fatalf("waiting for %s", what)
	}
	return nil
}

// async runs f on a goroutine of its own, its error on the channel.
func async(f func(context.Context) error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f(context.Background()) }()
	return ch
}

// engineCmd is an engine command with no condition, as async and the tables
// take one.
func engineCmd(f func(context.Context, ...engine.Cond) error) func(context.Context) error {
	return func(ctx context.Context) error { return f(ctx) }
}

// wantRefusedAtTheEngine requires a to be the engine's refusal of the write's
// reservation: 409 playback_not_stopped in the owner's words, with the epoch
// the backend publishes now, the backend's write never called, and the
// reservation free once answered.
func wantRefusedAtTheEngine(t *testing.T, what string, r *choiceRig, s *playerStandIn, a choiceAnswer) {
	t.Helper()
	obj := wantAnswer(t, what, a, http.StatusConflict, "playback_not_stopped", 4)
	if string(obj["message"]) != `"Stop playback to change DSD"` {
		t.Errorf("%s: the message %s, want the owner's words", what, obj["message"])
	}
	s.mu.Lock()
	n := s.chooses
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%s: the backend's write was called %d times", what, n)
	}
	if r.eng.Snapshot().ConfigurationReserved() {
		t.Errorf("%s: the reservation is held once the write answered", what)
	}
}

// TestAPlayParkedBeforeItsBackendRegistrationRefusesTheWrite (B728 B9 PR 3;
// the plan's named test): a Play from stopped whose Start is parked before it
// registers anything with the backend has the engine's transport starting, so
// the engine's owner refuses the write's reservation, and the write answers
// playback_not_stopped with the epoch published now, the backend's write
// never called.
func TestAPlayParkedBeforeItsBackendRegistrationRefusesTheWrite(t *testing.T) {
	s := newPlayerStandIn()
	s.policy = standInPolicy(t)
	hold := make(chan struct{})
	s.holdStart = map[string]chan struct{}{"a": hold}
	r := newChoiceRig(t, s)
	letGo := closeOnce(hold)
	t.Cleanup(letGo)
	queue(t, r.eng, 0, someReference(t, "a"))
	played := async(engineCmd(r.eng.Play))
	s.await(t, "the Start parked before it registers", func(b *playerStandIn) bool { return b.parked == 1 })
	wantRefusedAtTheEngine(t, "the write", r, s, r.put(t, choiceBody(t, dacOf(t, s.policy), dsdstate.ModeConvert)))
	letGo()
	if err := answered(t, played, "the Play"); err != nil {
		t.Fatalf("the Play let go: %v", err)
	}
}

// TestEachTransitionRefusesTheWriteAtTheEngine (B728 B9 PR 3): every state
// between two of a transition's steps, held open, refuses the write's
// reservation at the engine, the backend's write never called: a seek's
// release, Next's, Previous's, PlayID's and PlayIndex's while playing; the
// release after a refused start, and the next entry's Start parked after it;
// a Play during a release, which makes it a release to play; paused with the
// device held, and with it let go after the pause's release; and a release
// left unconfirmed past its deadline. With the engine stopped, the backend's
// own refusal answers alike, its write called once, and the reservation is
// free once it has answered.
func TestEachTransitionRefusesTheWriteAtTheEngine(t *testing.T) {
	pol := standInPolicy(t)
	body := choiceBody(t, dacOf(t, pol), dsdstate.ModeConvert)
	type held struct {
		s   *playerStandIn
		r   *choiceRig
		ids []uint32
		let func()
	}
	// setup is a rig over a stand-in whose Releases wait until let, edit
	// applied to it first, refs queued in order.
	setup := func(t *testing.T, edit func(*playerStandIn), refs ...string) held {
		t.Helper()
		s := newPlayerStandIn()
		s.policy = pol
		s.details = &engine.Details{Frames: 48000 * 600, RateHz: 48000, Bits: 24, Channels: 2, Codec: "FLAC", Lossless: true}
		release := make(chan struct{})
		s.holdRelease = release
		if edit != nil {
			edit(s)
		}
		r := newChoiceRig(t, s)
		let := closeOnce(release)
		t.Cleanup(let)
		h := held{s: s, r: r, let: let}
		var after uint32
		for _, ref := range refs {
			after = queue(t, r.eng, after, someReference(t, ref))
			h.ids = append(h.ids, after)
		}
		return h
	}
	playing := func(t *testing.T, h held, start func(context.Context) error) {
		t.Helper()
		if err := start(context.Background()); err != nil {
			t.Fatalf("the start: %v", err)
		}
		awaitEngine(t, h.r.eng, "playing", func(sn *engine.Snapshot) bool { return sn.State() == engine.Playing && !sn.Starting() })
	}
	releasing := func(t *testing.T, h held, what string) {
		t.Helper()
		awaitEngine(t, h.r.eng, what, func(sn *engine.Snapshot) bool { return sn.State() == engine.Releasing })
	}
	// whilePlaying is a command made while playing whose release is parked.
	whilePlaying := func(name string, refs []string, start func(held) func(context.Context) error, cmd func(held) func(context.Context) error) {
		t.Run(name, func(t *testing.T) {
			h := setup(t, nil, refs...)
			playing(t, h, start(h))
			done := async(cmd(h))
			releasing(t, h, "the release "+name+" makes")
			wantRefusedAtTheEngine(t, "a write during "+name, h.r, h.s, h.r.put(t, body))
			h.let()
			if err := answered(t, done, name); err != nil {
				t.Fatalf("%s let go: %v", name, err)
			}
		})
	}
	play := func(h held) func(context.Context) error { return engineCmd(h.r.eng.Play) }
	whilePlaying("a seek while playing", []string{"a"}, play, func(h held) func(context.Context) error {
		return func(ctx context.Context) error { return h.r.eng.SeekAbsolute(ctx, 10) }
	})
	whilePlaying("Next while playing", []string{"a", "b"}, play, func(h held) func(context.Context) error { return engineCmd(h.r.eng.Next) })
	whilePlaying("Previous while playing", []string{"a", "b"},
		func(h held) func(context.Context) error {
			return func(ctx context.Context) error { return h.r.eng.PlayID(ctx, h.ids[1]) }
		},
		func(h held) func(context.Context) error { return engineCmd(h.r.eng.Previous) })
	whilePlaying("PlayID while playing", []string{"a", "b"}, play, func(h held) func(context.Context) error {
		return func(ctx context.Context) error { return h.r.eng.PlayID(ctx, h.ids[1]) }
	})
	whilePlaying("PlayIndex while playing", []string{"a", "b"}, play, func(h held) func(context.Context) error {
		return func(ctx context.Context) error { return h.r.eng.PlayIndex(ctx, 1) }
	})

	t.Run("a skip after a refusal", func(t *testing.T) {
		holdB := make(chan struct{})
		h := setup(t, func(s *playerStandIn) {
			s.refuse = map[string]error{"a": &engine.Unplayable{Reason: "missing_file", Detail: "the test's refusal"}}
			s.holdStart = map[string]chan struct{}{"b": holdB}
		}, "a", "b")
		letB := closeOnce(holdB)
		t.Cleanup(letB)
		played := async(engineCmd(h.r.eng.Play))
		releasing(t, h, "the release after a's refusal, to play b")
		wantRefusedAtTheEngine(t, "a write during the release after the refusal", h.r, h.s, h.r.put(t, body))
		h.let()
		h.s.await(t, "b's Start parked", func(b *playerStandIn) bool { return b.parked == 1 })
		wantRefusedAtTheEngine(t, "a write during b's parked Start", h.r, h.s, h.r.put(t, body))
		letB()
		if err := answered(t, played, "the Play"); err != nil {
			t.Fatalf("the Play let go: %v", err)
		}
		awaitEngine(t, h.r.eng, "b playing", func(sn *engine.Snapshot) bool { return sn.State() == engine.Playing && !sn.Starting() })
	})

	t.Run("a Play during a release", func(t *testing.T) {
		h := setup(t, nil, "a")
		playing(t, h, engineCmd(h.r.eng.Play))
		stopped := async(engineCmd(h.r.eng.Stop))
		releasing(t, h, "the Stop's release")
		replayed := async(engineCmd(h.r.eng.Play))
		awaitEngine(t, h.r.eng, "a release to play", func(sn *engine.Snapshot) bool {
			return sn.State() == engine.Releasing && sn.Pending() == engine.IntentPlay
		})
		wantRefusedAtTheEngine(t, "a write during a release to play", h.r, h.s, h.r.put(t, body))
		h.let()
		_ = answered(t, stopped, "the Stop")
		if err := answered(t, replayed, "the Play"); err != nil {
			t.Fatalf("the Play during the release, let go: %v", err)
		}
	})

	t.Run("paused, the device held and then let go", func(t *testing.T) {
		h := setup(t, nil, "a")
		playing(t, h, engineCmd(h.r.eng.Play))
		if err := h.r.eng.Pause(context.Background()); err != nil {
			t.Fatalf("pause: %v", err)
		}
		awaitEngine(t, h.r.eng, "paused", func(sn *engine.Snapshot) bool { return sn.State() == engine.Paused })
		wantRefusedAtTheEngine(t, "a write while paused, the device held", h.r, h.s, h.r.put(t, body))
		sid, ev, ok := h.s.session()
		if !ok {
			t.Fatal("no session to let the device go")
		}
		ev.DeviceReleased(sid)
		awaitEngine(t, h.r.eng, "paused, the device let go", func(sn *engine.Snapshot) bool { return sn.State() == engine.Paused && sn.DeviceReleased() })
		wantRefusedAtTheEngine(t, "a write while paused, the device let go", h.r, h.s, h.r.put(t, body))
	})

	t.Run("a release left unconfirmed", func(t *testing.T) {
		h := setup(t, nil, "a")
		playing(t, h, engineCmd(h.r.eng.Play))
		stopped := async(engineCmd(h.r.eng.Stop))
		awaitEngine(t, h.r.eng, "the release unconfirmed past its deadline", func(sn *engine.Snapshot) bool {
			_, unconfirmed := sn.ReleaseUnconfirmed()
			return unconfirmed
		})
		wantRefusedAtTheEngine(t, "a write while a release is unconfirmed", h.r, h.s, h.r.put(t, body))
		h.let()
		_ = answered(t, stopped, "the Stop")
	})

	t.Run("the backend's own refusal", func(t *testing.T) {
		h := setup(t, func(s *playerStandIn) {
			s.choose = func(context.Context, playback.ChoiceRequest) playback.ChoiceResult {
				return playback.ChoiceResult{Outcome: playback.ChoicePlaybackNotStopped, Policy: pol, Epoch: true}
			}
		})
		obj := wantAnswer(t, "the write", h.r.put(t, body), http.StatusConflict, "playback_not_stopped", 4)
		if string(obj["message"]) != `"Stop playback to change DSD"` {
			t.Errorf("the message %s", obj["message"])
		}
		h.s.mu.Lock()
		n := h.s.chooses
		h.s.mu.Unlock()
		if n != 1 {
			t.Errorf("the backend's write was called %d times, want once", n)
		}
		if h.r.eng.Snapshot().ConfigurationReserved() {
			t.Error("the reservation is held once the write answered")
		}
	})
}

// heldChoiceStore is the player's DSD state in memory whose next write, once
// armed, waits at its start, before the store's own lock, until the test
// lets it go: a write whose fsync does not return. in is closed as it comes.
type heldChoiceStore struct {
	*playbacktest.MemStore
	armed   atomic.Bool
	in      chan struct{}
	gate    chan struct{}
	letOnce sync.Once
}

func newHeldChoiceStore(st dsdstate.State) *heldChoiceStore {
	return &heldChoiceStore{MemStore: playbacktest.NewMemStore(st), in: make(chan struct{}), gate: make(chan struct{})}
}

func (h *heldChoiceStore) Update(fn func(*dsdstate.State) error) error {
	if h.armed.CompareAndSwap(true, false) {
		close(h.in)
		<-h.gate
	}
	return h.MemStore.Update(fn)
}

// let lets the held write go.
func (h *heldChoiceStore) let() { h.letOnce.Do(func() { close(h.gate) }) }

// await waits until the write is held, until the test's deadline.
func (h *heldChoiceStore) await(t *testing.T) {
	t.Helper()
	select {
	case <-h.in:
	case <-serveGiveUp(t):
		t.Fatal("no write of the state came")
	}
}

// choosingStandIn is serve's stand-in for the engine's sessions, and the backend
// backend for the write of the DAC's choice: a write the backend backend makes,
// its store held where a test holds it, under the engine's reservation.
type choosingStandIn struct {
	*playerStandIn
	writer *playback.Backend
	// asked, when set, takes the context each write is asked under, never
	// waiting; returned, when set, runs once the backend's write has returned,
	// before the adapter lets the engine's reservation go.
	asked    chan context.Context
	returned func()
}

func (c *choosingStandIn) ChooseDSD(ctx context.Context, req playback.ChoiceRequest) playback.ChoiceResult {
	if c.asked != nil {
		select {
		case c.asked <- ctx:
		default:
		}
	}
	res := c.writer.ChooseDSD(ctx, req)
	if c.returned != nil {
		c.returned()
	}
	return res
}

func (c *choosingStandIn) Policy() playback.PlayerPolicy { return c.writer.Policy() }

// choosingRig is a rig over a stand-in whose writes the backend backend makes
// over store, seams beside its token key, each opt applied to the stand-in
// before the rig serves; store's held write is let go before the rig stops.
func choosingRig(t *testing.T, store *heldChoiceStore, seams *playback.Seams, opts ...func(*choosingStandIn)) (*choiceRig, *playerStandIn, *playback.Backend) {
	t.Helper()
	backend := readSideBackendKeyed(t, goldenTokenKey, seams, playbacktest.NewBox(5), store, withKnownDAC)
	s := newPlayerStandIn()
	c := &choosingStandIn{playerStandIn: s, writer: backend}
	for _, o := range opts {
		o(c)
	}
	r := newChoiceRig(t, c)
	joinBackendOnCleanup(t, backend)
	t.Cleanup(store.let)
	return r, s, backend
}

// startsRefused requires every command that would start a session to be
// refused configuring, the selection and the transport unchanged, and no
// Start to reach the backend.
func startsRefused(t *testing.T, r *choiceRig, s *playerStandIn, ids []uint32) {
	t.Helper()
	before := r.eng.Snapshot()
	for _, c := range []struct {
		name string
		f    func(context.Context) error
	}{
		{"Play", engineCmd(r.eng.Play)},
		{"PlayID", func(ctx context.Context) error { return r.eng.PlayID(ctx, ids[len(ids)-1]) }},
		{"PlayIndex", func(ctx context.Context) error { return r.eng.PlayIndex(ctx, len(ids)-1) }},
		{"Next", engineCmd(r.eng.Next)},
		{"Previous", engineCmd(r.eng.Previous)},
	} {
		if err := c.f(context.Background()); engine.ReasonOf(err) != engine.ReasonConfiguring {
			t.Errorf("%s while the write runs answered %v, want configuring", c.name, err)
		}
		if now := r.eng.Snapshot(); now.Selected() != before.Selected() || now.State() != engine.Stopped {
			t.Errorf("%s while the write runs moved the selection to %d (from %d), the transport to %s", c.name, now.Selected(), before.Selected(), now.State())
		}
	}
	if _, _, starts := s.counts(); starts != 0 {
		t.Errorf("%d Starts reached the backend while the write runs", starts)
	}
}

// TestAWriteHoldsEveryStartUntilItReturns (B728 B9 PR 3): while the backend's
// write runs, held in its store, the engine's reservation refuses Play,
// PlayID, PlayIndex, Next and Previous configuring, the selection unchanged
// and no Start reaching the backend; once it returns, written, the
// reservation is gone and Play is admitted.
func TestAWriteHoldsEveryStartUntilItReturns(t *testing.T) {
	store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	r, s, backend := choosingRig(t, store, nil)
	a := queue(t, r.eng, 0, someReference(t, "a"))
	b := queue(t, r.eng, a, someReference(t, "b"))
	store.armed.Store(true)
	answer, client := r.putAsync(t, context.Background(), choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert))
	store.await(t)
	if !r.eng.Snapshot().ConfigurationReserved() {
		t.Fatal("the write runs with no reservation of the engine's")
	}
	startsRefused(t, r, s, []uint32{a, b})
	store.let()
	wantAnswer(t, "the write", r.wait(t, answer, "the write's answer"), http.StatusOK, "", 4)
	if err := answered(t, client, "the write's client"); err != nil {
		t.Fatal(err)
	}
	if r.eng.Snapshot().ConfigurationReserved() {
		t.Fatal("the reservation is held once the write answered")
	}
	if err := r.eng.Play(context.Background()); err != nil {
		t.Fatalf("Play once the write returned: %v", err)
	}
	awaitEngine(t, r.eng, "playing", func(sn *engine.Snapshot) bool { return sn.State() == engine.Playing })
}

// TestTheReservationIsHeldUntilTheWriteReturnsWhateverTheRequestDoes (B728
// B9 PR 3): the reservation is the adapter's until the backend's write has
// returned. A client that goes away while the store's write runs, the
// server's request ended with it, leaves Play refused configuring, and the
// write runs to its end, written, the reservation still held as it returns,
// its answer 200 whatever the client did, the reservation gone after it and
// Play admitted. Two writes of one revision, the first held, hold two
// reservations: the second waits its turn and is stale. A write that panics
// is 500 internal with no member, its value on no line, the reservation let
// go. And serve's stop while a write runs keeps the player's share of the
// data directory's lock until the write returns.
func TestTheReservationIsHeldUntilTheWriteReturnsWhateverTheRequestDoes(t *testing.T) {
	t.Run("the request cancelled while the write runs", func(t *testing.T) {
		store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		asked := make(chan context.Context, 1)
		returned, look := make(chan struct{}), make(chan struct{})
		var lookOnce sync.Once
		looked := func() { lookOnce.Do(func() { close(look) }) }
		r, s, backend := choosingRig(t, store, nil, func(c *choosingStandIn) {
			c.asked = asked
			c.returned = func() { close(returned); <-look }
		})
		t.Cleanup(looked)
		queue(t, r.eng, 0, someReference(t, "a"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store.armed.Store(true)
		answer, client := r.putAsync(t, ctx, choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert))
		store.await(t)
		var request context.Context
		select {
		case request = <-asked:
		case <-serveGiveUp(t):
			t.Fatal("the write was never asked")
		}
		cancel()
		if err := answered(t, client, "the client's end"); !errors.Is(err, context.Canceled) {
			t.Fatalf("the client's PUT answered %v, want its own cancellation", err)
		}
		// The server sees the client go: the request's context, the one the
		// write was asked under, ends while the write runs.
		select {
		case <-request.Done():
		case <-serveGiveUp(t):
			t.Fatal("the server never ended the request whose client went away")
		}
		if err := r.eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonConfiguring {
			t.Fatalf("Play with the request gone and the write running answered %v, want configuring", err)
		}
		store.let()
		select {
		case <-returned:
		case <-serveGiveUp(t):
			t.Fatal("the backend's write never returned")
		}
		if !r.eng.Snapshot().ConfigurationReserved() {
			t.Error("the reservation was let go before the backend's write returned")
		}
		looked()
		wantAnswer(t, "the write", r.wait(t, answer, "the write's answer"), http.StatusOK, "", 4)
		if mode, _, _ := store.State().ChoiceFor(playbacktest.DACKey(), playbacktest.DACStamp()); mode != dsdstate.ModeConvert {
			t.Errorf("the state holds %s, want the write's convert", mode)
		}
		if r.eng.Snapshot().ConfigurationReserved() {
			t.Fatal("the reservation is held once the write returned")
		}
		if err := r.eng.Play(context.Background()); err != nil {
			t.Fatalf("Play once the write returned: %v", err)
		}
		if _, _, starts := s.counts(); starts != 1 {
			t.Errorf("%d Starts, want the Play's one", starts)
		}
	})

	t.Run("two writes of one revision, the first held", func(t *testing.T) {
		store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		queued := make(chan struct{})
		r, _, backend := choosingRig(t, store, &playback.Seams{ChoiceQueued: closeOnce(queued)})
		queue(t, r.eng, 0, someReference(t, "a"))
		body := choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert)
		store.armed.Store(true)
		first, _ := r.putAsync(t, context.Background(), body)
		store.await(t)
		second, _ := r.putAsync(t, context.Background(), body)
		select {
		case <-queued:
		case <-serveGiveUp(t):
			t.Fatal("the second write never waited for its turn")
		}
		if err := r.eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonConfiguring {
			t.Fatalf("Play with two writes under way answered %v, want configuring", err)
		}
		store.let()
		wantAnswer(t, "the first", r.wait(t, first, "the first's answer"), http.StatusOK, "", 4)
		wantAnswer(t, "the second", r.wait(t, second, "the second's answer"), http.StatusConflict, "stale", 4)
		if r.eng.Snapshot().ConfigurationReserved() {
			t.Fatal("a reservation is held once both writes answered")
		}
		if err := r.eng.Play(context.Background()); err != nil {
			t.Fatalf("Play once both writes returned: %v", err)
		}
	})

	t.Run("a write that panics", func(t *testing.T) {
		rec := loggingtest.Record(t)
		const value = "the test's panic, which no line may carry"
		s := newPlayerStandIn()
		s.policy = standInPolicy(t)
		s.choose = func(context.Context, playback.ChoiceRequest) playback.ChoiceResult { panic(value) }
		r := newChoiceRig(t, s)
		queue(t, r.eng, 0, someReference(t, "a"))
		obj := wantAnswer(t, "the write", r.put(t, choiceBody(t, dacOf(t, s.policy), dsdstate.ModeConvert)), http.StatusInternalServerError, "internal", 0)
		if keys := slices.Sorted(maps.Keys(obj)); !slices.Equal(keys, []string{"error", "message"}) {
			t.Errorf("the answer's members %q, want the envelope alone", keys)
		}
		if lines := rec.Carrying(value); len(lines) != 0 {
			t.Errorf("the panic's value reached a line: %q", lines)
		}
		if lines := rec.Lines("player: a panic was recovered; the library is served as before"); len(lines) != 1 || !strings.Contains(lines[0], "the DSD choice's write") {
			t.Errorf("the panic was logged %q, want once as the write's", lines)
		}
		if r.eng.Snapshot().ConfigurationReserved() {
			t.Fatal("the reservation is held once the write that panicked answered")
		}
		if err := r.eng.Play(context.Background()); err != nil {
			t.Fatalf("Play after the write that panicked: %v", err)
		}
	})

	t.Run("serve's stop while a write runs", func(t *testing.T) {
		needsDirectorySync(t)
		persistentMedium(t)
		box := newDSDBox(t)
		goBarDSD(t, box.trees)
		write := holdChoiceWrite(t)
		probe := newHeldDACProbe(false)
		published := make(chan struct{}, 1)
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, seams: noControlNodes(), bounds: servePlayerBounds{backend: 200 * time.Millisecond},
			with: func(o *serveOpts) {
				box.with(o)
				o.playerBackend = func(cfg playback.Config) (playBackend, error) {
					cfg.Probe = probe.probe
					cfg.Seams = &playback.Seams{Published: func() {
						select {
						case published <- struct{}{}:
						default:
						}
					}}
					return newServePlayerBackend(cfg)
				}
			}})
		// Registered after the boot, so it runs before serve's drain.
		t.Cleanup(write.let)
		eng := ps.runningEngine(t)
		availableOutput(t, eng)
		be, ok := ps.player.be.(*playback.Backend)
		if !ok {
			t.Fatalf("serve's player runs %T, want the backend production builds", ps.player.be)
		}
		for p := be.Policy(); p.DSD == nil || p.DSD.DAC == nil; p = be.Policy() {
			select {
			case <-published:
			case <-serveGiveUp(t):
				t.Fatalf("serve's player never published the GO bar's DSD: %+v", p)
			}
		}
		view := newBridgeOutput(servePlayerPlan{lists: true, start: true}, ps.player, ps.holder.Load())
		d := dacOf(t, be.Policy())
		res := make(chan string, 1)
		go func() {
			r := view.ChooseDSD(context.Background(), api.BridgeDSDChoiceRequest{DACKey: d.DACKey, DACToken: d.DACToken, BaseRevision: d.Choice.Revision, Mode: dsdstate.ModeOff})
			res <- r.Outcome
		}()
		write.await(t)
		if !eng.Snapshot().ConfigurationReserved() {
			t.Error("the write runs with no reservation of the engine's")
		}
		stopped(t, ps)
		if closedNow(ps.player.settled) {
			t.Error("the player settled with the write of its DSD choice under way")
		}
		write.let()
		select {
		case <-ps.player.settled:
		case <-serveGiveUp(t):
			t.Fatal("the player kept its share of the data directory's lock once the write returned")
		}
		select {
		case outcome := <-res:
			if outcome != api.DSDChoiceWritten {
				t.Errorf("the write answered %s, want written", outcome)
			}
		case <-serveGiveUp(t):
			t.Fatal("the write never answered")
		}
		if st, _ := dsdStateIn(t, ps.dataDir); !slices.ContainsFunc(st.DACs, func(dac dsdstate.DAC) bool { return dac.Choice != nil && dac.Choice.Mode == dsdstate.ModeOff }) {
			t.Error("the state holds no choice of off once the write returned")
		}
	})
}

// appChoice is a choice the app set, as the player's DSD state's file writes
// it.
var appChoice = regexp.MustCompile(`"setBy":\s*"app"`)

// heldChoiceWrite holds the first write of the player's DSD state whose bytes
// carry a choice the app set, inside the store's own lock, at the fsync of the
// file the write stages, as a write whose fsync does not return, until the
// test lets it go. Every other fsync is the file's own.
type heldChoiceWrite struct {
	came, gate    chan struct{}
	once, letOnce sync.Once
}

// holdChoiceWrite puts the hold in place before serve boots: its restore,
// registered first, runs after serve's drain, once no writer reads the seam.
func holdChoiceWrite(t *testing.T) *heldChoiceWrite {
	t.Helper()
	h := &heldChoiceWrite{came: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(atomicwrite.SetSyncFileForTest(func(f *os.File) error {
		if strings.HasPrefix(filepath.Base(f.Name()), ".player-dsd-") {
			if b, err := os.ReadFile(f.Name()); err == nil && appChoice.Match(b) {
				first := false
				h.once.Do(func() {
					first = true
					close(h.came)
				})
				if first {
					<-h.gate
				}
			}
		}
		return f.Sync()
	}))
	return h
}

func (h *heldChoiceWrite) let() { h.letOnce.Do(func() { close(h.gate) }) }

// await waits until the write is held, until the test's deadline.
func (h *heldChoiceWrite) await(t *testing.T) {
	t.Helper()
	select {
	case <-h.came:
	case <-serveGiveUp(t):
		t.Fatal("the write of the DAC's choice never reached the player's DSD state")
	}
}

// TestTheWritesFailedPathsThroughTheRoute (B728 B9 PR 3): each way a write
// fails before it reaches the state's file is persist_failed, the state as it
// was: its turn held by another write past its request (the client gone,
// recorded 499); the writes closed, as serve's stop closes them; a probe
// holding the DAC past its request (the client gone); and the store refusing.
func TestTheWritesFailedPathsThroughTheRoute(t *testing.T) {
	t.Run("the turn held past the request", func(t *testing.T) {
		store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		queued := make(chan struct{})
		r, _, backend := choosingRig(t, store, &playback.Seams{ChoiceQueued: closeOnce(queued)})
		body := choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert)
		store.armed.Store(true)
		first, _ := r.putAsync(t, context.Background(), body)
		store.await(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		second, _ := r.putAsync(t, ctx, choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeOff))
		select {
		case <-queued:
		case <-serveGiveUp(t):
			t.Fatal("the second write never waited for its turn")
		}
		cancel()
		a := r.wait(t, second, "the second's answer")
		if a.status != 499 || !strings.Contains(string(a.body), `"persist_failed"`) {
			t.Fatalf("the second answered %d %s, want persist_failed recorded 499", a.status, a.body)
		}
		store.let()
		wantAnswer(t, "the first", r.wait(t, first, "the first's answer"), http.StatusOK, "", 4)
		if mode, _, _ := store.State().ChoiceFor(playbacktest.DACKey(), playbacktest.DACStamp()); mode != dsdstate.ModeConvert || store.Writes() != 1 {
			t.Errorf("the state holds %s after %d writes, want the first's convert alone", mode, store.Writes())
		}
	})

	t.Run("the writes closed", func(t *testing.T) {
		store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		r, _, backend := choosingRig(t, store, nil)
		backend.CloseDSDWrites()
		wantAnswer(t, "the write", r.put(t, choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert)), http.StatusInternalServerError, "persist_failed", 4)
		if store.Writes() != 0 {
			t.Errorf("the state was written %d times", store.Writes())
		}
	})

	t.Run("a probe holding the DAC past the request", func(t *testing.T) {
		store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		leased := make(chan struct{})
		locate := &parkedLocator{came: make(chan struct{}), gate: make(chan struct{})}
		p := playbacktest.Policy(5, playbacktest.NewBox(5), store)
		withKnownDAC(p)
		backend, err := playback.New(playback.Config{DSD: p, Resolver: dopContents{}, Locator: locate,
			Seams: &playback.Seams{DACTokenKey: goldenTokenKey, ChoiceLeased: closeOnce(leased)}})
		if err != nil {
			t.Fatal(err)
		}
		backend.RefreshDSD(true)
		r := newChoiceRig(t, &choosingStandIn{playerStandIn: newPlayerStandIn(), writer: backend})
		joinBackendOnCleanup(t, backend)
		t.Cleanup(locate.let)
		backend.RequestProbeDSD(context.Background(), playback.ProbeConditions{Available: true, Fresh: true})
		select {
		case <-locate.came:
		case <-serveGiveUp(t):
			t.Fatal("the probe never took the DAC")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		answer, _ := r.putAsync(t, ctx, choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert))
		select {
		case <-leased:
		case <-serveGiveUp(t):
			t.Fatal("the write never took its lease to wait for the probe")
		}
		cancel()
		if a := r.wait(t, answer, "the write's answer"); a.status != 499 || !strings.Contains(string(a.body), `"persist_failed"`) {
			t.Fatalf("the write answered %d %s, want persist_failed recorded 499", a.status, a.body)
		}
		if store.Writes() != 0 {
			t.Errorf("the state was written %d times", store.Writes())
		}
	})

	t.Run("the store refusing", func(t *testing.T) {
		store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		r, _, backend := choosingRig(t, store, nil)
		store.Fail(errors.New("the test's store refuses the write"))
		wantAnswer(t, "the write", r.put(t, choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert)), http.StatusInternalServerError, "persist_failed", 4)
		if mode, _, _ := store.State().ChoiceFor(playbacktest.DACKey(), playbacktest.DACStamp()); mode != dsdstate.ModeNative {
			t.Errorf("the state holds %s, want native as it was", mode)
		}
	})
}

// parkedLocator is a Locator whose Locate waits until the test lets it go,
// as a reading the kernel holds, and then answers that the unit is not there:
// a probe holding the DAC's slot. came is closed as the first comes.
type parkedLocator struct {
	came, gate    chan struct{}
	once, letOnce sync.Once
}

func (l *parkedLocator) Locate(context.Context) (playback.Unit, error) {
	l.once.Do(func() { close(l.came) })
	<-l.gate
	return playback.Unit{}, errors.New("the test's locator: no unit")
}

func (l *parkedLocator) let() { l.letOnce.Do(func() { close(l.gate) }) }

// parkedChoiceBox is the test DAC's box whose next reading of the unit's
// model, once armed, waits until the test lets it go, came closed as it comes:
// a refresh held in its reading of the DAC, holding the refreshes' lock.
type parkedChoiceBox struct {
	*playbacktest.Box
	armed      atomic.Bool
	came, gate chan struct{}
	letOnce    sync.Once
}

func (p *parkedChoiceBox) UnitModel(card int) (alsa.UnitModel, error) {
	if p.armed.CompareAndSwap(true, false) {
		close(p.came)
		<-p.gate
	}
	return p.Box.UnitModel(card)
}

func (p *parkedChoiceBox) let() { p.letOnce.Do(func() { close(p.gate) }) }

// seenContent is a resolver that closes came when it is first asked, and
// refuses: a Start come past the backend's configuration lease, refused before
// the device.
type seenContent struct {
	came chan struct{}
	once sync.Once
}

func (c *seenContent) Open(context.Context, engine.Reference) (playback.Content, error) {
	c.once.Do(func() { close(c.came) })
	return playback.Content{}, errors.New("the test's content refuses every open")
}

// TestAWriteWaitingForARefreshAnswersWithinItsTime (B728 B9 PR 3b): a PUT
// whose route's time runs out while a refresh, parked in its reading of the
// DAC, holds the refreshes' lock answers persist_failed within that time, the
// refresh still parked, having let go of the engine's reservation and the
// backend's lease: a Play is admitted, and its Start, through the real
// backend, comes past the lease to the content, the refresh still parked. Let
// go, the refresh ends, and the write wrote nothing.
func TestAWriteWaitingForARefreshAnswersWithinItsTime(t *testing.T) {
	box := &parkedChoiceBox{Box: playbacktest.NewBox(5), came: make(chan struct{}), gate: make(chan struct{})}
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	content := &seenContent{came: make(chan struct{})}
	p := playbacktest.Policy(5, box.Box, store)
	p.Box = box
	backend, err := playback.New(playback.Config{DSD: p, Resolver: content, Locator: refusedNativeLocator{new(int)},
		Seams: &playback.Seams{DACTokenKey: goldenTokenKey}})
	if err != nil {
		t.Fatal(err)
	}
	backend.RefreshDSD(true)
	r := newChoiceRig(t, backend)
	queue(t, r.eng, 0, someReference(t, "a"))
	body := choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert)
	box.armed.Store(true)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		backend.RefreshDSD(true)
	}()
	drainLoopOnCleanup(t, box.let, refreshed, "the parked refresh")
	select {
	case <-box.came:
	case <-serveGiveUp(t):
		t.Fatal("the refresh never parked in its reading of the DAC")
	}
	r.budget = 500 * time.Millisecond
	answer, _ := r.putAsync(t, context.Background(), body)
	wantAnswer(t, "the write", r.wait(t, answer, "the PUT's answer, the refresh still parked"), http.StatusInternalServerError, "persist_failed", 4)
	if r.eng.Snapshot().ConfigurationReserved() {
		t.Fatal("the engine's reservation is held once the PUT answered")
	}
	// Admitted, its one entry refused by the content it reached past the
	// backend's lease: all_unplayable, never configuring.
	if err := r.eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonAllUnplayable {
		t.Fatalf("Play once the PUT answered: %v, want all_unplayable from its Start", err)
	}
	select {
	case <-content.came:
	case <-serveGiveUp(t):
		t.Fatal("the Start never came past the backend's lease to the content")
	}
	box.let()
	select {
	case <-refreshed:
	case <-serveGiveUp(t):
		t.Fatal("the refresh never ended once let go")
	}
	if store.Writes() != 0 {
		t.Errorf("the write past its time wrote the state %d times", store.Writes())
	}
}

// absentUnitLocator answers that the bound unit is not there, holding nothing:
// a tone let go ends there, before any open.
type absentUnitLocator struct{}

func (absentUnitLocator) Locate(context.Context) (playback.Unit, error) {
	return playback.Unit{}, errors.New("the test's locator: no unit")
}

// TestAWriteBesideTheConsolesToneIsRefusedAndLetsTheReservationGo (B728 B9
// PR 3b beside B601): the console's test tone takes the backend's device slot
// while a refresh, parked in its reading of the DAC, holds the refreshes'
// lock, and is held there, having opened nothing (Seams.ToneAdmitted); the
// engine's transport is stopped. The tone is active, the backend's status
// saying it plays on both sides of a GET /v1/outputs, and that GET's element
// of kind bridge reads as it did before the tone: its state stopped and no
// playback observation, since the tone runs outside the engine and publishes
// none. The refresh let go publishes the token GET published before, and a
// PUT made from that dsdDAC is granted the engine's reservation and refused by
// the backend's lease: 409 playback_not_stopped with the four members, its
// dsdDAC GET's, nothing written, and the reservation let go as it answers.
// The tone let go gives the slot back, and a Play is admitted, its Start
// coming past the lease to the content.
func TestAWriteBesideTheConsolesToneIsRefusedAndLetsTheReservationGo(t *testing.T) {
	box := &parkedChoiceBox{Box: playbacktest.NewBox(5), came: make(chan struct{}), gate: make(chan struct{})}
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	content := &seenContent{came: make(chan struct{})}
	p := playbacktest.Policy(5, box.Box, store)
	p.Box = box
	admitted, hold := make(chan struct{}), make(chan struct{})
	backend, err := playback.New(playback.Config{DSD: p, Resolver: content, Locator: absentUnitLocator{},
		OpenTone: func(alsapcm.Endpoint, alsapcm.Config) (*alsapcm.Device, error) {
			return nil, errors.New("the test's tone opens no node")
		},
		Seams: &playback.Seams{DACTokenKey: goldenTokenKey, ToneAdmitted: func() { close(admitted); <-hold }}})
	if err != nil {
		t.Fatal(err)
	}
	backend.RefreshDSD(true)
	r := newChoiceRig(t, backend)
	queue(t, r.eng, 0, someReference(t, "a"))
	// GET /v1/outputs' element of kind bridge before the tone: its transport
	// stopped, no occurrence ever observed.
	member := func(obj map[string]json.RawMessage, key string) string {
		if raw, ok := obj[key]; ok {
			return string(raw)
		}
		return "absent"
	}
	idle := r.bridge(t)
	if st, pb := member(idle, "state"), member(idle, "playback"); st != `"stopped"` || pb != "absent" {
		t.Fatalf("GET /v1/outputs before the tone: state %s, playback %s; want stopped, absent", st, pb)
	}
	before := dsdDACOf(t, idle)
	box.armed.Store(true)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		backend.RefreshDSD(true)
	}()
	drainLoopOnCleanup(t, box.let, refreshed, "the parked refresh")
	select {
	case <-box.came:
	case <-serveGiveUp(t):
		t.Fatal("the refresh never parked in its reading of the DAC")
	}
	backend.ToneOutput(true, "")
	toned, toneDone := make(chan playback.ToneResult, 1), make(chan struct{})
	go func() {
		defer close(toneDone)
		toned <- backend.Tone(context.Background(), playback.ToneRequest{LevelDBFS: -40, Seconds: 1})
	}()
	let := closeOnce(hold)
	drainLoopOnCleanup(t, let, toneDone, "the console's tone held in the device slot")
	select {
	case <-admitted:
	case <-serveGiveUp(t):
		t.Fatal("the tone never took the device slot beside the parked refresh")
	}
	box.let()
	select {
	case <-refreshed:
	case <-serveGiveUp(t):
		t.Fatal("the refresh never ended once let go")
	}
	// The tone plays, as the backend's status says on both sides of the GET:
	// admitted to the slot and not ended.
	if st := backend.ToneStatus(); st.State != playback.ToneStatePlaying {
		t.Fatalf("the tone's status before GET /v1/outputs: %+v; want playing", st)
	}
	sounding := r.bridge(t)
	if st := backend.ToneStatus(); st.State != playback.ToneStatePlaying {
		t.Fatalf("the tone's status after GET /v1/outputs: %+v; want playing", st)
	}
	// The tone runs outside the engine and publishes no observation: the
	// element's transport and its playback observation read as before it.
	for _, key := range []string{"state", "playback"} {
		if got, was := member(sounding, key), member(idle, key); got != was {
			t.Errorf("GET /v1/outputs while the tone plays: %s %s; want %s, as before the tone", key, got, was)
		}
	}
	if d := dsdDACOf(t, sounding); d.DACKey != before.DACKey || d.DACToken != before.DACToken {
		t.Fatalf("the refresh beside the tone published %s %s, want the DAC GET published before, %s %s", d.DACKey, d.DACToken, before.DACKey, before.DACToken)
	}
	obj := wantAnswer(t, "the write beside the tone", r.put(t, choiceBody(t, before, dsdstate.ModeConvert)), http.StatusConflict, "playback_not_stopped", 4)
	if d := dsdDACOf(t, obj); d.DACToken != before.DACToken {
		t.Errorf("the refusal's dsdDAC token %s, want the one GET published, %s", d.DACToken, before.DACToken)
	}
	if r.eng.Snapshot().ConfigurationReserved() {
		t.Fatal("the engine's reservation is held once the PUT answered")
	}
	if n := store.Writes(); n != 0 {
		t.Errorf("the write beside the tone wrote the state %d times", n)
	}
	let()
	select {
	case res := <-toned:
		if res.Outcome != playback.ToneNotLocated {
			t.Errorf("the tone let go ended %q, want %q", res.Outcome, playback.ToneNotLocated)
		}
	case <-serveGiveUp(t):
		t.Fatal("the tone never ended once let go")
	}
	// Admitted, its one entry refused by the content it reached past the
	// backend's lease: all_unplayable, never configuring.
	if err := r.eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonAllUnplayable {
		t.Fatalf("Play once the tone gave the slot back: %v, want all_unplayable from its Start", err)
	}
	select {
	case <-content.came:
	case <-serveGiveUp(t):
		t.Fatal("the Start never came past the backend's lease to the content")
	}
}

// TestAGETAcquiredBeforeAWritesPublicationOrdersBeforeItsAnswer (B728 B9 PR 3;
// the bridge's half of the client's ordering): a GET made while a write is
// held in its store answers the publication standing, N; the write's answer,
// once it returns, carries a later one, its bootId, token and
// policyGeneration the same where the write changed nothing that plays.
func TestAGETAcquiredBeforeAWritesPublicationOrdersBeforeItsAnswer(t *testing.T) {
	store := newHeldChoiceStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	backend := readSideBackend(t, playbacktest.NewBox(5), store, withKnownDAC)
	r := newChoiceRig(t, backend)
	t.Cleanup(store.let)
	d := r.dac(t)
	store.armed.Store(true)
	answer, _ := r.putAsync(t, context.Background(), choiceBody(t, d, dsdstate.ModeNative))
	store.await(t)
	got := r.bridge(t)
	before := dsdPlaybackOf(t, got)
	store.let()
	obj := wantAnswer(t, "the write", r.wait(t, answer, "the write's answer"), http.StatusOK, "", 4)
	after := dsdPlaybackOf(t, obj)
	if after.Publication <= before.Publication || before.Publication == 0 {
		t.Errorf("the GET answered publication %d and the write %d: the write's is the later", before.Publication, after.Publication)
	}
	if after.BootID != before.BootID || after.PolicyGeneration != before.PolicyGeneration {
		t.Errorf("the write moved the boot %s→%s or the generation %d→%d, changing nothing that plays", before.BootID, after.BootID, before.PolicyGeneration, after.PolicyGeneration)
	}
	if now := dsdDACOf(t, obj); now.DACToken != dsdDACOf(t, got).DACToken || now.Choice.Revision != 2 {
		t.Errorf("the write's dsdDAC %+v, want the token the GET carried and revision 2", now)
	}
}

// TestTheChoiceAnswerShapes (B728 B9 PR 3): the answers the adapter makes
// without the backend's epoch carry none of the members: a player that is not
// ready, and one whose start failed, are 409 dac_unavailable; words the route
// does not take are 400 before the player is asked, ready or not; a write
// that panics is 500 internal; and a write whose publication failed answers
// its outcome. With the epoch the write carries, a refusal carries the four
// members, and three, no dsdDAC, where the epoch names no DAC.
func TestTheChoiceAnswerShapes(t *testing.T) {
	pol := standInPolicy(t)
	body := choiceBody(t, dacOf(t, pol), dsdstate.ModeConvert)
	t.Run("a player not ready, and one whose start failed", func(t *testing.T) {
		needsDirectorySync(t)
		persistentMedium(t)
		for _, ready := range []bool{false, true} {
			cfg := playerOutputConfig(t.TempDir(), nil)
			seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
			readyCh := make(chan struct{})
			if ready {
				close(readyCh)
			}
			r := serveChoiceRoute(t, nil, newBridgeOutput(servePlayerPlan{lists: true, start: true}, &bridgePlayer{ready: readyCh}, cfg))
			wantAnswer(t, "a write", r.put(t, body), http.StatusConflict, "dac_unavailable", 0)
			wantAnswer(t, "bad words", r.put(t, strings.Replace(body, `"convert"`, `"loud"`, 1)), http.StatusBadRequest, "bad_request", 0)
			wantAnswer(t, "a token of no token's form", r.put(t, strings.Replace(body, dacOf(t, pol).DACToken, "t1-short", 1)), http.StatusBadRequest, "bad_request", 0)
		}
	})
	for _, tc := range []struct {
		name    string
		result  playback.ChoiceResult
		status  int
		code    string
		members int
	}{
		{"a publication that failed", playback.ChoiceResult{Outcome: playback.ChoiceWritten}, http.StatusOK, "", 0},
		{"a refusal with its epoch", playback.ChoiceResult{Outcome: playback.ChoiceStale, Policy: pol, Epoch: true}, http.StatusConflict, "stale", 4},
		{"a refusal whose epoch names no DAC", playback.ChoiceResult{Outcome: playback.ChoiceDACUnavailable, Policy: noDACPolicy(t), Epoch: true}, http.StatusConflict, "dac_unavailable", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPlayerStandIn()
			s.policy = pol
			s.choose = func(context.Context, playback.ChoiceRequest) playback.ChoiceResult { return tc.result }
			r := newChoiceRig(t, s)
			wantAnswer(t, tc.name, r.put(t, body), tc.status, tc.code, tc.members)
		})
	}
}

// noDACPolicy is what the backend backend publishes with no DAC available.
func noDACPolicy(t *testing.T) playback.PlayerPolicy {
	t.Helper()
	return readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()), func(p *playback.DSDPolicy) {
		p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
	}).Policy()
}
