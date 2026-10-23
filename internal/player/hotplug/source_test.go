package hotplug

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSocket is the kernel's uevent socket for the source's own logic: a
// test queues what the kernel (or another sender) would send, and wait
// blocks until something is queued or its context ends.
type fakeSocket struct {
	mu     sync.Mutex
	queue  []fakeDatagram
	ready  chan struct{}
	closed int
}

// fakeDatagram is one recv's answer: a message from pid with cred, or err.
type fakeDatagram struct {
	data []byte
	pid  uint32
	cred *credentials
	err  error
}

func newFakeSocket() *fakeSocket { return &fakeSocket{ready: make(chan struct{}, 1)} }

func (s *fakeSocket) push(d ...fakeDatagram) {
	s.mu.Lock()
	s.queue = append(s.queue, d...)
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *fakeSocket) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ready:
		return nil
	}
}

func (s *fakeSocket) recv(buf []byte) (received, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return received{}, errEAGAIN
	}
	d := s.queue[0]
	s.queue = s.queue[1:]
	if d.err != nil {
		return received{}, d.err
	}
	copy(buf, d.data)
	return received{n: len(d.data), truncated: len(d.data) > len(buf), pid: d.pid, cred: d.cred}, nil
}

func (s *fakeSocket) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *fakeSocket) closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// root is the kernel's credentials: pid 0, uid 0.
var root = &credentials{}

// fromKernel is msg as the kernel sends it: port id 0, root's credentials.
func fromKernel(msg []byte) fakeDatagram { return fakeDatagram{data: msg, cred: root} }

// sentinel is a usb add a test sends last: the source reads in order, so
// once it arrives every datagram before it has been handled.
var sentinel = uevent("add", "/devices/sentinel/usb9/9-9", "usb", "999999")

// waitFor receives from events until an event whose DevPath is devpath,
// returning what came before it, or fails the test after a minute.
func waitFor(t *testing.T, events <-chan Uevent, devpath string) []Uevent {
	t.Helper()
	var before []Uevent
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	for {
		select {
		case u := <-events:
			if u.DevPath == devpath {
				return before
			}
			before = append(before, u)
		case <-deadline.C:
			t.Fatalf("no event at %s within a minute", devpath)
		}
	}
}

// TestTheSourceDropsAndCountsWhatTheKernelDidNotSend: a message from any
// sender but the kernel (another port id, a uid other than root, or no
// credentials at all), a malformed message and a truncated one are each
// dropped and counted, a message of another subsystem is counted and
// ignored, and only the kernel's sound and usb events reach the watcher.
// The source counts a wake after the watcher has the event, so the counts
// are read once the source has ended, not the moment the sentinel arrives.
func TestTheSourceDropsAndCountsWhatTheKernelDidNotSend(t *testing.T) {
	sock := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := newKernelSource(ctx, sock, true)
	good := uevent("add", "/devices/platform/x/usb1/1-1", "usb", "10")
	sock.push(
		fakeDatagram{data: good, pid: 4242, cred: root},
		fakeDatagram{data: good, cred: &credentials{uid: 1000}},
		fakeDatagram{data: good},
		fromKernel([]byte("add@/devices/x\x00ACTION=remove\x00")),
		fromKernel(append(uevent("add", "/devices/y", "usb", "11"), make([]byte, maxUeventBytes)...)),
		fromKernel(uevent("add", "/devices/virtual/block/loop7", "block", "12")),
		fromKernel(good),
		fromKernel(sentinel),
	)
	got := waitFor(t, src.Events(), "/devices/sentinel/usb9/9-9")
	if len(got) != 1 || got[0].DevPath != "/devices/platform/x/usb1/1-1" || got[0].Seqnum != 10 {
		t.Fatalf("the watcher was woken by %+v, want the kernel's usb add alone", got)
	}
	cancel()
	ended := time.NewTimer(time.Minute)
	defer ended.Stop()
	select {
	case <-src.Done():
	case <-ended.C:
		t.Fatal("the source did not end within a minute of its context")
	}
	c := src.Status().Counts
	want := Counts{Messages: 8, Woke: 2, Foreign: 3, Malformed: 1, Truncated: 1, Ignored: 1}
	if c != want {
		t.Errorf("counts %+v, want %+v", c, want)
	}
}

// TestTheSourceTrustsThePortIDAloneWithoutCredentials: where SO_PASSCRED
// could not be set no message carries credentials, and the kernel's port
// id, 0, is what tells it from any other sender.
func TestTheSourceTrustsThePortIDAloneWithoutCredentials(t *testing.T) {
	sock := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := newKernelSource(ctx, sock, false)
	sock.push(fakeDatagram{data: uevent("add", "/devices/a", "usb", "1"), pid: 77},
		fakeDatagram{data: uevent("add", "/devices/b", "usb", "2")}, fakeDatagram{data: sentinel})
	if got := waitFor(t, src.Events(), "/devices/sentinel/usb9/9-9"); len(got) != 1 || got[0].DevPath != "/devices/b" {
		t.Fatalf("woken by %+v", got)
	}
	if c := src.Status().Counts; c.Foreign != 1 {
		t.Errorf("counts %+v", c)
	}
}

// TestTheSourceSaysNoEventsYetUntilTheKernelSpeaks: a socket that opened
// reports "netlink (no events yet)" until the first message from the
// kernel, whatever it says, and "netlink" from then on; a foreign message
// does not count. The change is noted to the watcher.
func TestTheSourceSaysNoEventsYetUntilTheKernelSpeaks(t *testing.T) {
	sock := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := newKernelSource(ctx, sock, true)
	if m := src.Status().Mode; m != ModeNetlinkQuiet {
		t.Fatalf("before any message: %s", m)
	}
	sock.push(fakeDatagram{data: sentinel, pid: 9, cred: root}, fromKernel(uevent("add", "/devices/z", "usb", "1")))
	waitFor(t, src.Events(), "/devices/z")
	if m := src.Status().Mode; m != ModeNetlink {
		t.Errorf("after the kernel's first message: %s", m)
	}
	select {
	case <-src.Notes():
	default:
		t.Error("the change of mode was not noted")
	}
}

// TestTheSourceReportsWhatTheKernelDropped: ENOBUFS is the kernel saying
// it dropped events: it is counted and noted, TakeLost reports it once,
// and reading goes on.
func TestTheSourceReportsWhatTheKernelDropped(t *testing.T) {
	sock := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := newKernelSource(ctx, sock, true)
	sock.push(fakeDatagram{err: errENOBUFS}, fakeDatagram{err: errEINTR}, fromKernel(sentinel))
	waitFor(t, src.Events(), "/devices/sentinel/usb9/9-9")
	if !src.TakeLost() || src.TakeLost() {
		t.Error("TakeLost did not report the drop once")
	}
	if c := src.Status().Counts; c.Overflows != 1 {
		t.Errorf("counts %+v", c)
	}
	select {
	case <-src.Notes():
	default:
		t.Error("the drop was not noted")
	}
}

// TestTheSourceNeverWaitsForTheWatcher: with the watcher's queue full, the
// source drops the next event rather than wait, counts it, and reports it
// as lost, so the watcher reconciles everything.
func TestTheSourceNeverWaitsForTheWatcher(t *testing.T) {
	sock := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := newKernelSource(ctx, sock, true)
	var burst []fakeDatagram
	for i := 0; i < eventQueue+5; i++ {
		burst = append(burst, fromKernel(uevent("add", "/devices/b", "usb", "1")))
	}
	burst = append(burst, fromKernel(uevent("remove", "/devices/last", "sound", "2")))
	sock.push(burst...)
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	// Every message here wakes the watcher, so each is handed over or
	// dropped as behind; a message is counted read before either.
	for c := src.Status().Counts; c.Woke+c.Behind < uint64(len(burst)); c = src.Status().Counts {
		select {
		case <-src.Notes():
		case <-deadline.C:
			t.Fatalf("the source stopped reading at %+v", src.Status().Counts)
		}
	}
	if c := src.Status().Counts; c.Woke != eventQueue || c.Behind != 6 || !src.TakeLost() {
		t.Errorf("counts %+v", c)
	}
}

// TestTheSourceStopsAtOnceAndClosesItsSocket: a source waiting for the
// kernel stops when its context ends, and closes its socket once; one whose
// socket fails falls back to polling, says why, and closes it too.
func TestTheSourceStopsAtOnceAndClosesItsSocket(t *testing.T) {
	sock := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	src := newKernelSource(ctx, sock, true)
	cancel()
	select {
	case <-src.Done():
	case <-time.After(time.Minute):
		t.Fatal("the source did not stop")
	}
	if sock.closes() != 1 {
		t.Errorf("the socket was closed %d times", sock.closes())
	}

	sock = newFakeSocket()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	src = newKernelSource(ctx, sock, true)
	sock.push(fakeDatagram{err: errors.New("bad file descriptor")})
	select {
	case <-src.Done():
	case <-time.After(time.Minute):
		t.Fatal("a failed socket did not end the source")
	}
	if st := src.Status(); st.Mode != ModePolling || st.Reason == "" || sock.closes() != 1 {
		t.Errorf("after the socket failed: %+v, closed %d", st, sock.closes())
	}
}

// endlessSocket is a socket the kernel never stops filling: every recv
// answers msg (the kernel's) or err, never EAGAIN, until the test lets it
// run dry. reading is closed at the first recv: the source is draining.
type endlessSocket struct {
	msg     []byte
	err     error
	reading chan struct{}
	once    sync.Once
	dry     atomic.Bool
	closed  atomic.Int32
}

func (s *endlessSocket) wait(ctx context.Context) error { return ctx.Err() }

func (s *endlessSocket) recv(buf []byte) (received, error) {
	s.once.Do(func() { close(s.reading) })
	switch {
	case s.dry.Load():
		return received{}, errEAGAIN
	case s.err != nil:
		return received{}, s.err
	}
	return received{n: copy(buf, s.msg), cred: root}, nil
}

func (s *endlessSocket) close() error {
	s.closed.Add(1)
	return nil
}

// TestTheSourceStopsWhileTheKernelKeepsSending: a socket that is refilled
// as fast as it is read never answers EAGAIN, so a source that looked at
// its context only once a drain ran dry read on past its stop, keeping its
// goroutine and its socket past serve's stop deadline (round 2). It stops
// at once, whether the socket answers messages, EINTR or ENOBUFS without
// end, and closes the socket once.
func TestTheSourceStopsWhileTheKernelKeepsSending(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  []byte
		err  error
	}{
		{"messages", uevent("change", "/devices/platform/x/usb1/1-1", "usb", "1"), nil},
		{"EINTR", nil, errEINTR},
		{"ENOBUFS", nil, errENOBUFS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sock := &endlessSocket{msg: tc.msg, err: tc.err, reading: make(chan struct{})}
			// A source that did not stop sees its stop once the socket runs
			// dry, so a failed run leaves nothing spinning.
			defer sock.dry.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			src := newKernelSource(ctx, sock, true)
			select {
			case <-sock.reading:
			case <-time.After(watchdog):
				t.Fatal("the source never read")
			}
			cancel()
			select {
			case <-src.Done():
			case <-time.After(watchdog):
				t.Fatalf("the source read on past its stop while the kernel kept sending: %+v", src.Status().Counts)
			}
			if n := sock.closed.Load(); n != 1 {
				t.Errorf("the socket was closed %d times", n)
			}
		})
	}
}

// TestAPollingSourceSaysWhy: a source with no socket delivers nothing, is
// done from the start, and says what it is and why.
func TestAPollingSourceSaysWhy(t *testing.T) {
	src := newPollingSource(ModePolling, "the netlink socket could not be opened: operation not permitted")
	if src.Events() != nil {
		t.Error("a polling source has events")
	}
	select {
	case <-src.Done():
	default:
		t.Error("a polling source is not done")
	}
	if s := src.Status().String(); s != "polling only: the netlink socket could not be opened: operation not permitted" {
		t.Errorf("status %q", s)
	}
	if s := newPollingSource(ModeUnsupported, "").Status().String(); s != "unsupported on this platform" {
		t.Errorf("status %q", s)
	}
}
