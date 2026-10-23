package alsapcm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// locator is a SinkConfig.Locate a test moves: the endpoint it names, or
// the error it answers, and the calls it had.
type locator struct {
	mu    sync.Mutex
	at    Endpoint
	err   error
	calls int
}

func (l *locator) locate(context.Context) (Endpoint, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.at, l.err
}

func (l *locator) set(at Endpoint, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at, l.err = at, err
}

func (l *locator) called() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// releasedSink opens a sink through k with loc as its Locate and admit as
// its Admit, plays 100 ms of in, pauses, and waits for the release a second
// later.
func releasedSink(t *testing.T, k *fakeKernel, loc *locator, admit func(NodeInfo) error, in []byte) (*Sink, Position) {
	t.Helper()
	cfg := sinkConfig()
	cfg.ReleaseAfter = time.Second
	cfg.Locate, cfg.Admit = loc.locate, admit
	s := openTestSink(t, k, cfg)
	submit(t, s, in)
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 100*time.Millisecond)
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	k.grant(2 * time.Second)
	released := waitState(t, s, SinkReleased)
	k.revoke()
	return s, released
}

// TestSinkReopenOpensTheEndpointLocateNamesNow pins the reopen after a
// release (B470 PR 4b2): the sink asks Locate where the bound unit is now,
// opens that node, and Admit checks the node opened, so a card renumbered
// while the device was free (unplugged and plugged in again, card 5 now card
// 6) is found and played where it is, from the frame the DAC had not
// played; the open itself asks no Locate; and the sink names the endpoint it
// reopened from then on. Without Locate the reopen opened card 5's node
// again, which the kernel says is card 6, and refused it.
func TestSinkReopenOpensTheEndpointLocateNamesNow(t *testing.T) {
	k := newModel(t)
	loc := &locator{at: testEndpoint}
	var mu sync.Mutex
	var admitted []NodeInfo
	admit := func(n NodeInfo) error {
		mu.Lock()
		defer mu.Unlock()
		admitted = append(admitted, n)
		return nil
	}
	in := content(0, 0, 30000)
	s, _ := releasedSink(t, k, loc, admit, in)
	if n := loc.called(); n != 0 {
		t.Fatalf("Locate was asked %d times before any reopen; the open takes the endpoint it was given", n)
	}
	renumbered := Endpoint{Card: 6, Device: 0}
	k.inspect(func(k *fakeKernel) { k.info.card = 6 })
	loc.set(renumbered, nil)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume on the renumbered card: %v", err)
	}
	k.inspect(func(k *fakeKernel) {
		if len(k.opened) != 2 || k.opened[1] != renumbered.Path() {
			t.Errorf("opened %q; want the endpoint's node, then card 6's", k.opened)
		}
	})
	mu.Lock()
	if len(admitted) != 2 || admitted[1].Card != 6 {
		t.Errorf("Admit was asked %+v; want the open's card 5, then the reopen's card 6", admitted)
	}
	mu.Unlock()
	if got := s.Endpoint(); got != renumbered {
		t.Errorf("Endpoint() = %v after the reopen, want %v", got, renumbered)
	}
	if err := s.Recover(); err == nil || !strings.Contains(err.Error(), renumbered.String()) {
		t.Errorf("an error after the reopen, %v, does not name %v", err, renumbered)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard across both opens", heard(k), in)
	k.checkClosedEach(t)
}

// TestSinkReopenWhoseLocateFailsStaysReleased pins a reopen the binding
// cannot place: Locate's error refuses the resume, typed, nothing is
// opened, the sink stays released with its position and the frames it kept,
// and the next resume, once Locate names the unit again, plays them.
func TestSinkReopenWhoseLocateFailsStaysReleased(t *testing.T) {
	k := newModel(t)
	loc := &locator{at: testEndpoint}
	in := content(0, 0, 30000)
	s, released := releasedSink(t, k, loc, nil, in)
	gone := errors.New("device_missing: no card is the bound unit")
	loc.set(Endpoint{}, gone)
	err := s.Resume()
	e := wantReason(t, err, ReasonNotLocated)
	if e.Op != "resume" || !errors.Is(err, gone) {
		t.Errorf("the refused resume %+v does not carry Locate's error", e)
	}
	if p := s.Position(); p.State != SinkReleased || p.Played != released.Played || p.Written != released.Written {
		t.Errorf("after the refusal: %+v; want still released at %d", p, released.Played)
	}
	k.inspect(func(k *fakeKernel) {
		if len(k.opened) != 1 || k.isOpen {
			t.Errorf("a resume Locate refused opened %q (open now: %v)", k.opened, k.isOpen)
		}
	})
	loc.set(testEndpoint, nil)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume once the unit is found: %v", err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
	k.checkClosedEach(t)
}
