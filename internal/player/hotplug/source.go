package hotplug

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
)

// Mode is how the watcher hears about the box's hardware.
type Mode string

const (
	// ModeNetlink is the kernel's uevent socket, which has delivered at
	// least one message.
	ModeNetlink Mode = "netlink"
	// ModeNetlinkQuiet is the kernel's uevent socket before its first
	// message: in some containers the socket opens and no event ever comes,
	// so until one does the watcher may be on its periodic readings alone.
	ModeNetlinkQuiet Mode = "netlink (no events yet)"
	// ModePolling is no event source, the periodic readings alone, and the
	// status's Reason says why.
	ModePolling Mode = "polling only"
	// ModeUnsupported is a platform with no player: not Linux.
	ModeUnsupported Mode = "unsupported on this platform"
)

// Counts are what an event source has read and done with it.
type Counts struct {
	// Messages are the datagrams read from the socket, from any sender.
	Messages uint64
	// Woke are the events handed to the watcher.
	Woke uint64
	// Ignored are the kernel's events of another subsystem or action.
	Ignored uint64
	// Foreign are messages a sender other than the kernel sent: dropped.
	Foreign uint64
	// Malformed are the kernel's messages that did not parse: dropped.
	Malformed uint64
	// Truncated are messages longer than the read buffer: dropped.
	Truncated uint64
	// Overflows are the kernel's ENOBUFS: it dropped events.
	Overflows uint64
	// Behind are events dropped because the watcher had not taken the
	// ones before them: each is reported as lost.
	Behind uint64
}

// SourceStatus is an event source's mode, why when it is polling, and its
// counts.
type SourceStatus struct {
	Mode   Mode
	Reason string
	Counts Counts
}

// String is the mode as an operator reads it: "netlink", "netlink (no
// events yet)", "polling only: why", or "unsupported on this platform".
func (s SourceStatus) String() string {
	if s.Mode == ModePolling && s.Reason != "" {
		return string(s.Mode) + ": " + s.Reason
	}
	return string(s.Mode)
}

// Source is where the watcher hears the kernel: the netlink socket on
// Linux, nothing at all (polling), or a channel a test feeds. It never
// waits for the watcher: an event the watcher has not room for is dropped
// and reported as lost, which makes the watcher read everything.
type Source interface {
	// Events are the kernel's events that wake the watcher, in the order
	// the kernel sent them; nil for a source with none.
	Events() <-chan Uevent
	// Notes signals, coalesced, that TakeLost or Status may have news.
	Notes() <-chan struct{}
	// TakeLost reports whether events were lost since its last call (the
	// kernel's ENOBUFS, or the watcher fell behind), and forgets it.
	TakeLost() bool
	// Status is the source's mode and counts now.
	Status() SourceStatus
	// Done is closed once the source has stopped reading and closed its
	// socket, after its context ended or its socket failed.
	Done() <-chan struct{}
}

// eventQueue is how many events wait for the watcher before the next one
// is dropped and reported as lost. A hub's worth of devices sends a few
// hundred.
const eventQueue = 256

// errno is a Linux error number, written out so that the source's logic
// runs, and is tested, on every system: only the socket is Linux's
// (source_linux.go maps unix.Errno onto these; TestErrnoValuesAreLinuxs).
type errno uintptr

const (
	errEINTR   errno = 4
	errEAGAIN  errno = 11
	errENOBUFS errno = 105
)

func (e errno) Error() string {
	switch e {
	case errEINTR:
		return "interrupted system call"
	case errEAGAIN:
		return "resource temporarily unavailable"
	case errENOBUFS:
		return "no buffer space available"
	}
	return "errno " + strconv.FormatUint(uint64(e), 10)
}

// credentials are a message's sender as SCM_CREDENTIALS gives them.
type credentials struct {
	pid      int32
	uid, gid uint32
}

// received is one datagram: n is its whole length (MSG_TRUNC), which may
// pass the buffer it was read into; pid is the sender's netlink port id,
// the kernel's being 0; cred is nil when no credentials came with it.
type received struct {
	n         int
	truncated bool
	pid       uint32
	cred      *credentials
}

// socket is the kernel's uevent socket, the source's one seam.
type socket interface {
	// wait blocks until a message can be read, or ctx is done (its error).
	wait(ctx context.Context) error
	// recv reads one message into buf without blocking: errEAGAIN when
	// there is none, errENOBUFS when the kernel dropped some.
	recv(buf []byte) (received, error)
	close() error
}

// kernelSource reads the kernel's uevents through a socket on a goroutine
// of its own, which ends with its context or a failed socket and closes
// the socket once. A source with no socket polls: it is done from the
// start and has no events.
type kernelSource struct {
	events chan Uevent
	notes  chan struct{}
	done   chan struct{}
	lost   atomic.Bool

	mu     sync.Mutex
	status SourceStatus

	// raw, when set, is handed each message the kernel sent, as it came:
	// TestCaptureKernelUevents writes them out.
	raw func([]byte)
}

// newKernelSource starts reading sock until ctx ends. passcred says
// whether the socket asked for its senders' credentials (SO_PASSCRED):
// when it did, a message must carry root's.
func newKernelSource(ctx context.Context, sock socket, passcred bool) *kernelSource {
	return startKernelSource(ctx, sock, passcred, nil)
}

func startKernelSource(ctx context.Context, sock socket, passcred bool, raw func([]byte)) *kernelSource {
	s := &kernelSource{events: make(chan Uevent, eventQueue), notes: make(chan struct{}, 1), done: make(chan struct{}),
		status: SourceStatus{Mode: ModeNetlinkQuiet}, raw: raw}
	go s.run(ctx, sock, passcred)
	return s
}

// newPollingSource is a source that hears nothing, in mode, for reason.
func newPollingSource(mode Mode, reason string) *kernelSource {
	s := &kernelSource{notes: make(chan struct{}, 1), done: make(chan struct{}), status: SourceStatus{Mode: mode, Reason: reason}}
	close(s.done)
	return s
}

func (s *kernelSource) Events() <-chan Uevent {
	if s.events == nil {
		return nil
	}
	return s.events
}

func (s *kernelSource) Notes() <-chan struct{} { return s.notes }

func (s *kernelSource) TakeLost() bool { return s.lost.Swap(false) }

func (s *kernelSource) Done() <-chan struct{} { return s.done }

func (s *kernelSource) Status() SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// note tells the watcher there is news, never waiting.
func (s *kernelSource) note() {
	select {
	case s.notes <- struct{}{}:
	default:
	}
}

// count adds one to a counter under the status's lock.
func (s *kernelSource) count(c func(*Counts)) {
	s.mu.Lock()
	c(&s.status.Counts)
	s.mu.Unlock()
}

// run reads until ctx ends or the socket fails: it waits for the socket,
// then reads every message there is, and again.
func (s *kernelSource) run(ctx context.Context, sock socket, passcred bool) {
	defer close(s.done)
	defer func() { _ = sock.close() }()
	buf := make([]byte, maxUeventBytes)
	for {
		if err := sock.wait(ctx); err != nil {
			if ctx.Err() == nil {
				s.fail(err)
			}
			return
		}
		for {
			m, err := sock.recv(buf)
			switch {
			case err == nil:
				s.handle(m, buf, passcred)
				continue
			case errors.Is(err, errEINTR):
				continue
			case errors.Is(err, errENOBUFS):
				s.count(func(c *Counts) { c.Overflows++ })
				s.lost.Store(true)
				s.note()
				continue
			case errors.Is(err, errEAGAIN):
			default:
				s.fail(err)
				return
			}
			break
		}
	}
}

// fail is the socket failing: the watcher polls from here on.
func (s *kernelSource) fail(err error) {
	s.mu.Lock()
	s.status.Mode, s.status.Reason = ModePolling, "the netlink socket failed: "+err.Error()
	s.mu.Unlock()
	s.note()
}

// handle judges one datagram: the kernel's alone, whole, well formed, and
// one that wakes the watcher, which it is handed without waiting.
func (s *kernelSource) handle(m received, buf []byte, passcred bool) {
	s.count(func(c *Counts) { c.Messages++ })
	if m.pid != 0 || (passcred && (m.cred == nil || m.cred.uid != 0)) {
		s.count(func(c *Counts) { c.Foreign++ })
		return
	}
	s.mu.Lock()
	first := s.status.Mode == ModeNetlinkQuiet
	if first {
		s.status.Mode = ModeNetlink
	}
	s.mu.Unlock()
	if first {
		s.note()
	}
	if m.truncated || m.n > len(buf) {
		s.count(func(c *Counts) { c.Truncated++ })
		return
	}
	msg := buf[:m.n]
	if s.raw != nil {
		s.raw(append([]byte(nil), msg...))
	}
	u, err := ParseUevent(msg)
	if err != nil {
		s.count(func(c *Counts) { c.Malformed++ })
		return
	}
	if !u.Wakes() {
		s.count(func(c *Counts) { c.Ignored++ })
		return
	}
	select {
	case s.events <- u:
		s.count(func(c *Counts) { c.Woke++ })
	default:
		s.count(func(c *Counts) { c.Behind++ })
		s.lost.Store(true)
		s.note()
	}
}
