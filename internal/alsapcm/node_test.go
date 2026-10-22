package alsapcm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestNodeAsksTheKernelWhatTheOpenNodeIs: Node is INFO on the open
// descriptor, asked each time it is called, never the endpoint the device
// was opened by: what the player's check after open reads the card from.
func TestNodeAsksTheKernelWhatTheOpenNodeIs(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	got, err := d.Node()
	if err != nil {
		t.Fatal(err)
	}
	if got != (NodeInfo{Card: 5, Device: 0, Subdevice: 0, Playback: true, ID: "USB Audio", Name: "USB Audio"}) {
		t.Errorf("Node = %+v", got)
	}
	k.inspect(func(k *fakeKernel) { k.info.card, k.info.subdevice = 7, 2 })
	if got, err := d.Node(); err != nil || got.Card != 7 || got.Subdevice != 2 {
		t.Errorf("Node after the kernel's answer changed = %+v, %v; want it asked again", got, err)
	}
	k.inspect(func(k *fakeKernel) { k.fail[ioctlInfo] = errnoENODEV })
	_, err = d.Node()
	wantReason(t, err, ReasonDisconnected)
	_ = d.Close()
	_, err = d.Node()
	wantReason(t, err, ReasonClosed)
}

// TestTheSinkAdmitsEachNodeBeforeAFrame: SinkConfig.Admit is asked about
// the node at the open and at the reopen after a release, before anything
// is written to it; a node it refuses is closed and the open or the resume
// refused as the wrong device, and the DAC hears nothing from it.
func TestTheSinkAdmitsEachNodeBeforeAFrame(t *testing.T) {
	refuse := errors.New("card 5 is not the bound unit")
	var mu sync.Mutex
	var asked []NodeInfo
	want := 5
	cfg := sinkConfig()
	cfg.ReleaseAfter = time.Second
	cfg.Admit = func(n NodeInfo) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, n)
		if n.Card != want {
			return refuse
		}
		return nil
	}

	k := newModel(t)
	mu.Lock()
	want = 6
	mu.Unlock()
	_, err := openSink(context.Background(), cfg, sinkSeams{open: func(ep Endpoint, c Config) (*Device, error) { return open(k, k.now, ep, c) }, now: k.now})
	e := wantReason(t, err, ReasonWrongDevice)
	if !errors.Is(e, refuse) {
		t.Errorf("the refusal %v does not carry Admit's reason", e)
	}
	k.inspect(func(k *fakeKernel) {
		if k.isOpen || k.closes != 1 || len(k.played) != 0 || k.appl != 0 {
			t.Errorf("a refused node: open %v, closes %d, %d bytes written", k.isOpen, k.closes, len(k.played))
		}
	})

	mu.Lock()
	want, asked = 5, nil
	mu.Unlock()
	k = newModel(t)
	s := openTestSink(t, k, cfg)
	in := content(0, 0, 30000)
	submit(t, s, in)
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 100*time.Millisecond)
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	k.grant(2 * time.Second)
	released := waitState(t, s, SinkReleased)
	k.revoke()
	heardBefore := len(heard(k))

	// While the device was free another DAC took card 5's place.
	mu.Lock()
	want = 6
	mu.Unlock()
	err = s.Resume()
	e = wantReason(t, err, ReasonWrongDevice)
	if e.Op != "resume" || !errors.Is(e, refuse) {
		t.Errorf("the refused resume: %+v", e)
	}
	if p := s.Position(); p.State != SinkReleased || p.Played != released.Played {
		t.Errorf("after a refused node: %+v, want still released at %d", p, released.Played)
	}
	k.inspect(func(k *fakeKernel) {
		if k.isOpen || k.opens != 2 || k.closes != 2 || len(k.heard) != heardBefore {
			t.Errorf("the refused reopen: open %v, %d opens, %d closes, heard %d bytes (had %d)", k.isOpen, k.opens, k.closes, len(k.heard), heardBefore)
		}
	})

	mu.Lock()
	want = 5
	mu.Unlock()
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 3 || asked[0].Card != 5 || !asked[0].Playback {
		t.Errorf("Admit was asked %+v; want the open, the refused reopen and the reopen", asked)
	}
}

// TestGuardNodeOpensRefusesEveryOpen: while a guard is set, Open and
// OpenSink open nothing and name the node they would have opened, so a
// test binary that sets one can fail any test that would open the box's
// DAC.
func TestGuardNodeOpensRefusesEveryOpen(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	remove := GuardNodeOpens(func(path string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, path)
	})
	defer remove()
	if d, err := Open(Endpoint{Card: 5, Device: 0}, cfg48k); d != nil || err == nil {
		t.Fatalf("Open under the guard = %v, %v", d, err)
	}
	s, err := OpenSink(context.Background(), SinkConfig{Endpoint: Endpoint{Card: 6, Device: 1},
		Transport: Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 2, Format: format.S16LE, ValidBits: 16}})
	if s != nil || err == nil {
		t.Fatalf("OpenSink under the guard = %v, %v", s, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "/dev/snd/pcmC5D0p" || seen[1] != "/dev/snd/pcmC6D1p" {
		t.Errorf("the guard saw %q", seen)
	}
}
