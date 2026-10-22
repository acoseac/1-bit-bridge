package hotplug

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// watchdog is how long a test waits for the watcher to reach a state it
// must reach: a hang becomes a failure, never a stuck test binary. A
// passing step takes milliseconds.
const watchdog = 30 * time.Second

// The Pi 5 tree's places (pi5-go-bar), as the fake tree names them and as
// the kernel's events name them.
const (
	usb1        = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1"
	goBar       = usb1 + "/1-1"
	goBarSerial = "GOBAR-7Q2X-91KZ-4471"
	goBarPort   = "platform-xhci-hcd.0/usb2-1:1.0"
)

// devpath is a fake tree's sys/... place as a uevent's DEVPATH names it.
func devpath(dir string) string { return strings.TrimPrefix(dir, "sys") }

// plug is the manifest of a unit at dir (a USB device directory under a
// root hub) at devnum, whose interface 0 is sound card n, playing on 0/0.
func plug(dir string, n int, vidpid, serial string, devnum int) string {
	vid, pid, _ := strings.Cut(vidpid, ":")
	s := ""
	if serial != "" {
		s = " serial=" + serial
	}
	base := filepath.Base(dir)
	return fmt.Sprintf("usbdev %s vendor=%s product=%s%s devnum=%d\nusbcard %d %s/%s:1.0\nproccard %d DAC%d \"Synthetic DAC\" usb=%s pcm=0.0\n",
		dir, vid, pid, s, devnum, n, dir, base, n, n, vidpid)
}

// plugEvents and unplugEvents are what the kernel sends when the unit at
// dir, whose interface 0 is card n, arrives and goes.
func plugEvents(dir string, n int) []Uevent {
	d := devpath(dir)
	iface := d + "/" + filepath.Base(dir) + ":1.0"
	card := fmt.Sprintf("%s/sound/card%d", iface, n)
	return []Uevent{
		{Action: "add", DevPath: d, Subsystem: "usb", DevType: "usb_device"},
		{Action: "add", DevPath: iface, Subsystem: "usb", DevType: "usb_interface"},
		{Action: "bind", DevPath: iface, Subsystem: "usb", DevType: "usb_interface", Driver: "snd-usb-audio"},
		{Action: "add", DevPath: card, Subsystem: "sound"},
		{Action: "add", DevPath: fmt.Sprintf("%s/pcmC%dD0p", card, n), Subsystem: "sound"},
	}
}

func unplugEvents(dir string, n int) []Uevent {
	d := devpath(dir)
	iface := d + "/" + filepath.Base(dir) + ":1.0"
	card := fmt.Sprintf("%s/sound/card%d", iface, n)
	return []Uevent{
		{Action: "remove", DevPath: fmt.Sprintf("%s/pcmC%dD0p", card, n), Subsystem: "sound"},
		{Action: "remove", DevPath: card, Subsystem: "sound"},
		{Action: "unbind", DevPath: iface, Subsystem: "usb", DevType: "usb_interface"},
		{Action: "remove", DevPath: iface, Subsystem: "usb", DevType: "usb_interface"},
		{Action: "remove", DevPath: d, Subsystem: "usb", DevType: "usb_device"},
	}
}

// bySerial, byModel and byPort are bindings of the GO bar.
func bySerial() config.PlayerDevice {
	return config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009", Serial: usbid.New(goBarSerial)}
}

func byModel(vid, pid string) config.PlayerDevice {
	return config.PlayerDevice{Transport: "usb", Binding: config.BindingUniqueModel, VendorID: vid, ProductID: pid}
}

func byPort(port string) config.PlayerDevice {
	return config.PlayerDevice{Transport: "usb", Binding: config.BindingPort, Port: port}
}

// fakeSource is an event source a test feeds: its events channel holds
// what the kernel would send, and its status and lost flag are the test's.
type fakeSource struct {
	events chan Uevent
	notes  chan struct{}
	done   chan struct{}
	lost   atomic.Bool
	mu     sync.Mutex
	status SourceStatus
}

func newFakeSource(ctx context.Context, mode Mode) *fakeSource {
	s := &fakeSource{events: make(chan Uevent, eventQueue), notes: make(chan struct{}, 1), done: make(chan struct{}),
		status: SourceStatus{Mode: mode}}
	go func() {
		<-ctx.Done()
		close(s.done)
	}()
	return s
}

func (s *fakeSource) Events() <-chan Uevent  { return s.events }
func (s *fakeSource) Notes() <-chan struct{} { return s.notes }
func (s *fakeSource) TakeLost() bool         { return s.lost.Swap(false) }
func (s *fakeSource) Done() <-chan struct{}  { return s.done }
func (s *fakeSource) Status() SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// overflow is the kernel's ENOBUFS.
func (s *fakeSource) overflow() {
	s.lost.Store(true)
	s.note()
}

func (s *fakeSource) setMode(m Mode) {
	s.mu.Lock()
	s.status.Mode = m
	s.mu.Unlock()
	s.note()
}

func (s *fakeSource) note() {
	select {
	case s.notes <- struct{}{}:
	default:
	}
}

// fakeSettings is the live config's player block: a test sets it, and a
// change is broadcast as config.RuntimeConfig's is.
type fakeSettings struct {
	mu      sync.Mutex
	s       Settings
	changed chan struct{}
}

func (f *fakeSettings) get() Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s
}

func (f *fakeSettings) next() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changed == nil {
		f.changed = make(chan struct{})
	}
	return f.changed
}

func (f *fakeSettings) set(s Settings) {
	f.mu.Lock()
	f.s = s
	if f.changed != nil {
		close(f.changed)
		f.changed = nil
	}
	f.mu.Unlock()
}

// syncBuffer is a log the watcher writes and a test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// harness runs a Service on a fake tree, a fake clock, a fake source and
// fake settings, and stops it, joined, when the test ends.
type harness struct {
	t        *testing.T
	root     string
	clock    *fakeClock
	settings *fakeSettings
	svc      *Service
	logs     *syncBuffer
	sub      *Subscription

	mu      sync.Mutex
	sources []*fakeSource
	mode    Mode
	reason  string
	// hook wraps the reading of the tree (setObserve), nil to read it.
	hook func(ctx context.Context, read func(context.Context) (binding.Inventory, error)) (binding.Inventory, error)

	cancel  context.CancelFunc
	stopped chan struct{}
	events  uint64
}

// setObserve wraps the readings from now on.
func (h *harness) setObserve(f func(ctx context.Context, read func(context.Context) (binding.Inventory, error)) (binding.Inventory, error)) {
	h.mu.Lock()
	h.hook = f
	h.mu.Unlock()
}

// options are what a test changes before the harness starts.
type options struct {
	device  config.PlayerDevice
	enabled bool
	// mode and reason are each source's, from its start.
	mode   Mode
	reason string
	// manifest is the tree to lay out; "" is pi5-go-bar.
	manifest string
	// observe wraps the reading of the tree, for a test that blocks or
	// fails it; nil reads it.
	observe func(ctx context.Context, read func(context.Context) (binding.Inventory, error)) (binding.Inventory, error)
	timing  *Timing
}

func pi5Manifest() string {
	return filepath.Join("..", "..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt")
}

// newHarness lays out the tree, starts the service with the player enabled
// and bound by serial unless o says otherwise, and waits for its first
// reconcile.
func newHarness(t *testing.T, opts ...func(*options)) *harness {
	t.Helper()
	o := options{device: bySerial(), enabled: true, mode: ModeNetlink}
	for _, f := range opts {
		f(&o)
	}
	manifest := o.manifest
	if manifest == "" {
		manifest = pi5Manifest()
	}
	h := &harness{t: t, root: sysfstest.Build(t, manifest), clock: newFakeClock(), settings: &fakeSettings{}, logs: &syncBuffer{},
		mode: o.mode, reason: o.reason, stopped: make(chan struct{})}
	h.settings.set(Settings{Enabled: o.enabled, Device: o.device})
	read := binding.Observer{
		Proc:  alsa.Reader{ProcRoot: filepath.Join(h.root, "proc")},
		Sysfs: alsa.SysfsReader{Root: filepath.Join(h.root, "sys")},
	}.ObserveContext
	h.hook = o.observe
	observe := func(ctx context.Context) (binding.Inventory, error) {
		h.mu.Lock()
		hook := h.hook
		h.mu.Unlock()
		if hook == nil {
			return read(ctx)
		}
		return hook(ctx, read)
	}
	timing := DefaultTiming()
	if o.timing != nil {
		timing = *o.timing
	}
	h.svc = NewService(Config{
		Settings: h.settings.get,
		Changed:  h.settings.next,
		Observe:  observe,
		OpenSource: func(ctx context.Context) Source {
			h.mu.Lock()
			defer h.mu.Unlock()
			s := newFakeSource(ctx, h.mode)
			s.status.Reason = h.reason
			h.sources = append(h.sources, s)
			return s
		},
		Clock:  h.clock,
		Timing: timing,
		Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	h.sub = h.svc.Feed().Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.stopped)
		h.svc.Run(ctx)
	}()
	t.Cleanup(h.stop)
	if o.enabled {
		h.waitStats("the first reconcile", func(s Stats) bool { return s.Reconciles >= 1 })
	}
	return h
}

// stop cancels the service and waits for Run to return. A stop that
// waits on the clock (a read the watcher must abandon) is the test's to
// drive; this one fails rather than hang.
func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.stopped:
	case <-time.After(watchdog):
		h.t.Errorf("the service did not stop within %v", watchdog)
	}
}

// source is the event source of the watch running now.
func (h *harness) source() *fakeSource {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sources) == 0 {
		h.t.Fatal("no watch has started")
	}
	return h.sources[len(h.sources)-1]
}

// waitStats waits until the service's stats satisfy cond.
func (h *harness) waitStats(what string, cond func(Stats) bool) Stats {
	h.t.Helper()
	return h.waitStatus(what, func(s Status) bool { return cond(s.Stats) }).Stats
}

// waitStatus waits until the service's status satisfies cond, on the
// service's own Updates: never a sleep or a poll.
func (h *harness) waitStatus(what string, cond func(Status) bool) Status {
	h.t.Helper()
	deadline := time.NewTimer(watchdog)
	defer deadline.Stop()
	for {
		ch := h.svc.Updates()
		st := h.svc.Status()
		if cond(st) {
			return st
		}
		select {
		case <-ch:
		case <-deadline.C:
			h.t.Fatalf("waiting for %s: the watcher is at %+v", what, st)
		}
	}
}

// send hands the watcher events and waits until it has taken each one.
func (h *harness) send(evs ...Uevent) {
	h.t.Helper()
	src := h.source()
	for _, e := range evs {
		src.events <- e
		h.events++
		want := h.events
		h.waitStats(fmt.Sprintf("event %d taken", want), func(s Stats) bool { return s.Events >= want })
	}
}

// apply changes the fake tree.
func (h *harness) apply(manifest string) {
	h.t.Helper()
	sysfstest.Apply(h.t, h.root, manifest)
}

// advance moves the fake clock.
func (h *harness) advance(d time.Duration) { h.clock.Advance(d) }

// reconciles waits until the watcher has reconciled n times in all and
// returns its stats.
func (h *harness) reconciles(n uint64) Stats {
	h.t.Helper()
	return h.waitStats(fmt.Sprintf("%d reconciles", n), func(s Stats) bool { return s.Reconciles >= n })
}

// latest is the feed's newest snapshot.
func (h *harness) latest() Snapshot {
	h.t.Helper()
	s, ok := h.svc.Feed().Latest()
	if !ok {
		h.t.Fatal("nothing has been published")
	}
	return s
}

// state checks the newest snapshot's binding state, card and generation.
func (h *harness) state(state binding.State, card int, gen uint64) Snapshot {
	h.t.Helper()
	s := h.latest()
	if r := s.Resolution(); r.State != state || r.Card != card || s.Generation() != gen {
		h.t.Fatalf("snapshot %d: %s on card %d (%s); want generation %d, %s on card %d",
			s.Generation(), r.State, r.Card, r.Reason, gen, state, card)
	}
	return s
}

// published waits until n snapshots have been published in all.
func (h *harness) published(n uint64) {
	h.t.Helper()
	h.waitStats(fmt.Sprintf("%d publishes", n), func(s Stats) bool { return s.Publishes >= n })
}
