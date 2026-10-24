package dlna

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// maxConcurrentMSearchResponders is the MediaServer's responder slots per
// advertise endpoint: how many M-SEARCH answers one endpoint may have in
// flight. Each may sleep up to `mxResponseCeilingSeconds` (the spec-mandated
// randomized reply delay) before sending, so without a cap a burst of
// `ssdp:all` searches — accidental or hostile — could spawn unbounded
// goroutines. When an endpoint's slots are full, its answer to another search
// is dropped (the control point re-searches; SSDP discovery is best-effort).
// Another registration sets its own (SSDPRegistration.ResponderSlots).
const maxConcurrentMSearchResponders = 16

// defaultMSearchJitter is the reply spread applied when an M-SEARCH omits
// MX (or sends MX:0). Still randomized so simultaneous searches don't
// reply in a synchronized burst.
const defaultMSearchJitter = 100 * time.Millisecond

// ssdpBurstDeadline bounds one burst of sends, all of its writes together: an
// alive or byebye burst from its first write, and an answer from its first
// response, after the MX delay, never from when the search arrived. A write
// that cannot go out by then fails, so no burst outlives it, and a
// withdrawal's join settles.
const ssdpBurstDeadline = time.Second

// ssdpWithdrawBudget is the most a withdrawal (SSDPHandle.Withdraw, an
// Update's removals, SSDPSubsystem.Close) takes, from its entry, across every
// endpoint and registration it withdraws: a caller's earlier deadline
// shortens it, and a caller's cancellation does not.
const ssdpWithdrawBudget = 5 * time.Second

// ssdpLogger is the SSDP-specific package logger — distinct subcomponent
// tag from `dlna` proper so operators grepping for the multicast /
// NOTIFY path can filter narrowly. Per CodeRabbit Major on PR #303.
var ssdpLogger = logging.Component("dlna-ssdp")

// defaultAdvertiseInterval is the canonical NOTIFY ssdp:alive cadence:
// slightly under SSDPMaxAge/2 to ensure re-announcement before any
// renderer would purge the prior entry.
const defaultAdvertiseInterval = 14 * time.Minute

// ssdpListenerReadDeadline bounds each blocking read of an SSDP listener so
// its loop wakes periodically to re-check the subsystem's context. Without
// it, a bare parent-ctx cancel that does not close the socket would park
// the goroutine inside the read indefinitely on a quiet network. Mirrors the
// sibling discovery client's runLoop (internal/dlna/discovery/client.go).
const ssdpListenerReadDeadline = 500 * time.Millisecond

// ssdpGroupAddr is the SSDP group's address, without its port.
var ssdpGroupAddr = netip.MustParseAddrPort(SSDPMulticastAddr).Addr()

// Errors the subsystem returns.
var (
	// ErrSSDPClosed is a Register or Update on a subsystem that is closed,
	// or whose parent context is done.
	ErrSSDPClosed = errors.New("dlna: the SSDP subsystem is closed")
	// ErrSSDPWithdrawn is an Update of a registration that was withdrawn.
	ErrSSDPWithdrawn = errors.New("dlna: the SSDP registration was withdrawn")
	// ErrSSDPNoEndpoint is a Register none of whose endpoints started.
	ErrSSDPNoEndpoint = errors.New("dlna: no SSDP endpoint started")
	// ErrSSDPStrictCapability is a strict endpoint refused because its
	// listener or its sender lacks what a strict registration needs.
	ErrSSDPStrictCapability = errors.New("dlna: a strict SSDP endpoint needs destination and arrival " +
		"reporting, receive confinement and a pinned sender")
)

// SSDPSubsystem is the process's one SSDP stack, which every SSDP device the
// bridge advertises registers with (B470 PR 5a1): the DLNA MediaServer
// today, and the player's OpenHome Source from PR 5a2. One device per
// listener set would steal unicast M-SEARCHes from another: every listener
// binds the wildcard on port 1900, and a unicast reaches one of them.
//
// It owns the listeners, one per interface identity (SSDPInterface) that a
// registered endpoint uses, shared by every registration on that interface
// and opened in endpoint order: the wildcard UDP4 bind on the group's port
// with one membership on the interface (listenSSDP), armed to report each
// datagram's destination and arrival interface. A listener is reference
// counted by the endpoints that use it, and closed, with its reader joined,
// when the last one goes; it is never closed while an endpoint holds it. Each
// endpoint has its own unconnected NOTIFY sender (openNotifySender), and each
// answer burst a transient socket of its own (dialReply). With no active
// endpoint it holds no socket.
//
// Each datagram is parsed once, then matched and admitted by each
// registration on its own (dispatch): see SSDPRegistration for what decides
// which endpoint answers. Construct it with NewSSDPSubsystem; Close
// withdraws every registration and closes it for good.
type SSDPSubsystem struct {
	log       *slog.Logger
	transport ssdpTransport

	// ctx is the subsystem's: derived from the parent NewSSDPSubsystem was
	// given, and cancelled by Close. The readers and every registration's
	// work run under it; a withdrawal's byebye does not.
	ctx    context.Context
	cancel context.CancelFunc

	// notifyTo is where every NOTIFY is written: the SSDP group. A test may
	// point it at a socket of its own before anything registers.
	notifyTo *net.UDPAddr

	// replyDelay draws an answer's delay in [0, window]; nil is uniform
	// random. A test sets its own before anything registers.
	replyDelay func(window time.Duration) time.Duration

	// intervalFloor is the shortest advertise interval a registration
	// gets: a shorter one is replaced by defaultAdvertiseInterval. A minute
	// in production; a test that watches repeats lowers it before anything
	// registers.
	intervalFloor time.Duration

	// slotTaken, when set, is told of every responder slot an answer
	// takes. nil in production; a test sets it before anything registers.
	slotTaken func(ep *ssdpEndpoint)

	// beforeByebyeDeadline parks a test between selecting and installing a
	// byebye deadline. nil in production; set before registering anything.
	beforeByebyeDeadline func()

	// mu guards everything below, and the published endpoint list of every
	// handle (SSDPHandle.active). Deadline installation holds it so Close's
	// clamp is atomic with every install; writes, sleeps and joins do not.
	mu            sync.Mutex
	closed        bool
	closeDeadline time.Time
	closeDone     chan struct{}
	senders       map[*ssdpOwnedSender]struct{} // includes staged and retiring endpoints
	listeners     map[ssdpListenerKey]*ssdpListener
	handles       []*SSDPHandle // includes withdrawals until their cleanup finishes

	// table is what the readers dispatch by, rebuilt under mu on every
	// change and read without it.
	table atomic.Pointer[ssdpTable]

	// inflight counts the Register and Update calls in progress, which
	// Close waits for. An Add happens only under mu while !closed.
	inflight sync.WaitGroup
	// readers counts the listeners' read loops.
	readers sync.WaitGroup
}

// SSDPSubsystemOptions configures NewSSDPSubsystem.
type SSDPSubsystemOptions struct {
	// Logger is the subsystem's logger, for what belongs to a listener (a
	// read error, a listener that cannot report destinations). A
	// registration logs its own on its own logger. nil is ssdpLogger.
	Logger *slog.Logger
	// Network replaces only this instance's transport; nil is the host's.
	Network *SSDPNetwork
}

// NewSSDPSubsystem returns a subsystem with no registration and no socket.
// ctx is its parent: its cancellation stops every listener's reader and every
// registration's announcements and answers, as a serve shutdown does; it does
// not withdraw them (Close does, with byebye, whether or not ctx is done).
func NewSSDPSubsystem(ctx context.Context, opts SSDPSubsystemOptions) *SSDPSubsystem {
	log := opts.Logger
	if log == nil {
		log = ssdpLogger
	}
	inner, cancel := context.WithCancel(ctx)
	s := &SSDPSubsystem{
		log:           log,
		transport:     hostSSDPTransport{},
		ctx:           inner,
		cancel:        cancel,
		notifyTo:      ssdpGroupUDPAddr,
		intervalFloor: time.Minute,
		listeners:     map[ssdpListenerKey]*ssdpListener{},
		senders:       map[*ssdpOwnedSender]struct{}{},
	}
	if opts.Network != nil {
		s.transport = injectedSSDPNetwork{*opts.Network}
		s.replyDelay = opts.Network.Delay
		if opts.Network.IntervalFloor > 0 {
			s.intervalFloor = opts.Network.IntervalFloor
		}
	}
	return s
}

// withdrawDeadline is the deadline of a withdrawal entered now: the earlier of
// the caller's deadline and ssdpWithdrawBudget from now.
func withdrawDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(ssdpWithdrawBudget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

// Close withdraws every registration and closes the subsystem for good: a
// Register or Update after it returns ErrSSDPClosed, and one in progress
// when it starts is waited for, and rolled back rather than committed. It
// cancels the subsystem's context first, so readers stop and no answer or
// alive is sent from then on, and then withdraws each registration with its
// byebye, under one budget: the earlier of ctx's deadline and five seconds
// from entry, shared by them all (ctx's cancellation does not cut it short,
// so a byebye goes out on a shutdown). On return no socket is open and every
// goroutine it started has returned. It returns the withdrawals' errors,
// joined; a second Close waits for the first under that first call's budget,
// then returns nil.
//
// Close takes ownership of every handle still listed when it begins, including
// a Withdraw in cleanup. That withdrawal's budget becomes the earlier of its
// own deadline and Close's, through its last release and removal from the list.
// A withdrawal already removed from the list is complete and is not owned.
func (s *SSDPSubsystem) Close(ctx context.Context) error {
	deadline := withdrawDeadline(ctx)
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.closed = true
	s.closeDeadline = deadline
	s.closeDone = make(chan struct{})
	for sender := range s.senders {
		_ = sender.setWriteDeadlineLocked(sender.deadline)
	}
	handles := append([]*SSDPHandle(nil), s.handles...)
	s.mu.Unlock()
	defer close(s.closeDone)
	s.cancel()

	// Also interrupt a Withdraw holding opMu, or an initial alive on an
	// unpublished endpoint. Neither is reachable through the dispatch table.
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-timer.C:
			s.interruptSenders()
		case <-stop:
		}
	}()
	defer func() { close(stop); <-stopped }()
	joined := make(chan struct{})
	go func() { s.inflight.Wait(); close(joined) }()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-joined:
	case <-timer.C:
		s.interruptSenders()
		<-joined // socket close wakes writes; join their rollback, too
	}

	var errs []error
	for _, h := range handles {
		if err := h.withdraw(deadline); err != nil {
			errs = append(errs, err)
		}
	}
	s.readers.Wait()
	if !time.Now().Before(deadline) {
		errs = append(errs, context.DeadlineExceeded)
	}
	return errors.Join(errs...)
}

// interruptSenders closes even unpublished and retiring endpoints' senders.
// No socket I/O runs under mu. A sender opened after the deadline is closed
// by ownSender before the operation can send on it.
func (s *SSDPSubsystem) interruptSenders() {
	s.mu.Lock()
	all := make([]*ssdpOwnedSender, 0, len(s.senders))
	for sender := range s.senders {
		all = append(all, sender)
	}
	s.mu.Unlock()
	for _, sender := range all {
		_ = sender.Close()
	}
}

// retirementDeadline shares Close's budget with a rollback or withdrawal.
func (s *SSDPSubsystem) retirementDeadline(deadline time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed && s.closeDeadline.Before(deadline) {
		return s.closeDeadline
	}
	return deadline
}

// enter admits one Register or Update call, which Close waits for; done ends
// it. It refuses once the subsystem is closed or its context is done.
func (s *SSDPSubsystem) enter() (done func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSSDPClosed
	}
	if err := s.ctx.Err(); err != nil {
		return nil, errors.Join(ErrSSDPClosed, err)
	}
	s.inflight.Add(1)
	return s.inflight.Done, nil
}

// delay draws an answer's delay in [0, window].
func (s *SSDPSubsystem) delay(window time.Duration) time.Duration {
	if s.replyDelay != nil {
		return s.replyDelay(window)
	}
	return time.Duration(rand.Int64N(int64(window) + 1))
}

// ssdpListenerKey is a listener's identity: its interface's index and name,
// and the incarnation token the caller gave (SSDPEndpoint.Incarnation). A
// new incarnation of an interface gets a listener and a membership of its
// own, beside a listener of the old one that a registration still holds.
type ssdpListenerKey struct {
	index       int
	name        string
	incarnation uint64
}

// public is the key as SSDPInterface.
func (k ssdpListenerKey) public() SSDPInterface {
	return SSDPInterface{Index: k.index, Name: k.name, Incarnation: k.incarnation}
}

// ssdpListenerState is where a listener is in its life.
type ssdpListenerState int

const (
	// listenerOpening is a listener one Register or Update is opening;
	// another that needs it waits for ready.
	listenerOpening ssdpListenerState = iota
	// listenerLive is open, with its reader running.
	listenerLive
	// listenerClosing is closing: its socket is being closed and its
	// reader joined. Another that needs it waits for gone, then opens a
	// fresh one.
	listenerClosing
)

// ssdpListener is one shared listener.
type ssdpListener struct {
	key   ssdpListenerKey
	iface *net.Interface // a copy; nil for the OS default

	// state and refs are guarded by the subsystem's mu.
	state ssdpListenerState
	refs  int

	ready chan struct{} // closed when opening ends, either way
	gone  chan struct{} // closed when closing ends

	// Set before ready closes on a successful open, read-only after.
	conn       ssdpReadConn
	setup      ssdpListenerSetup
	readerDone chan struct{}

	// reading is set, under the subsystem's mu, when the dispatch table
	// first lists an endpoint on the listener and its reader starts: until
	// then the socket queues what arrives, as an advertiser's did until its
	// first alive had gone out.
	reading bool
}

// acquireListener returns the listener for key, taking a reference: the
// live one, or one it opens. One being opened is waited for; one closing is
// waited for, and then opened afresh, so two listeners for one identity are
// never open at once.
func (s *SSDPSubsystem) acquireListener(key ssdpListenerKey, iface *net.Interface) (*ssdpListener, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrSSDPClosed
		}
		l := s.listeners[key]
		if l == nil {
			l = &ssdpListener{key: key, iface: iface, state: listenerOpening, refs: 1,
				ready: make(chan struct{}), gone: make(chan struct{})}
			s.listeners[key] = l
			s.mu.Unlock()
			return s.openListener(l)
		}
		switch l.state {
		case listenerLive:
			l.refs++
			s.mu.Unlock()
			return l, nil
		case listenerOpening:
			ready := l.ready
			s.mu.Unlock()
			<-ready
		default: // listenerClosing
			gone := l.gone
			s.mu.Unlock()
			<-gone
		}
	}
}

// openListener opens l, which acquireListener entered as opening. Its reader
// starts when the dispatch table first lists an endpoint on it
// (startReadersLocked). A listener that fails to open, or opens once the
// subsystem has closed, is removed again and its waiters look afresh.
func (s *SSDPSubsystem) openListener(l *ssdpListener) (*ssdpListener, error) {
	conn, setup, err := s.transport.listen(s.ctx, l.iface, s.log)
	s.mu.Lock()
	if err == nil && s.closed {
		_ = conn.Close()
		err = ErrSSDPClosed
	}
	if err != nil {
		if s.listeners[l.key] == l {
			delete(s.listeners, l.key)
		}
		close(l.ready)
		close(l.gone)
		s.mu.Unlock()
		return nil, err
	}
	l.conn, l.setup = conn, setup
	l.readerDone = make(chan struct{})
	l.state = listenerLive
	close(l.ready)
	s.mu.Unlock()
	return l, nil
}

// startReadersLocked starts the reader of each listener the table t lists
// an endpoint on and that has none yet. The subsystem's mu is held, and it
// is not closed (Close sets closed before it waits for the readers).
func (s *SSDPSubsystem) startReadersLocked(t *ssdpTable) {
	for _, e := range t.entries {
		for _, ep := range e.eps {
			if l := ep.listener; !l.reading {
				l.reading = true
				s.readers.Add(1)
				go s.readLoop(l)
			}
		}
	}
}

// releaseListener drops one reference to l, and closes l when it was the
// last: the socket first, which wakes the reader, then the reader's join,
// with l closing meanwhile so that one who needs it waits.
func (s *SSDPSubsystem) releaseListener(l *ssdpListener) {
	s.mu.Lock()
	l.refs--
	if l.refs > 0 {
		s.mu.Unlock()
		return
	}
	l.state = listenerClosing
	reading := l.reading
	s.mu.Unlock()
	_ = l.conn.Close()
	if reading {
		<-l.readerDone
	}
	s.mu.Lock()
	if s.listeners[l.key] == l {
		delete(s.listeners, l.key)
	}
	close(l.gone)
	s.mu.Unlock()
}

// readLoop reads l until its socket closes or the subsystem's context ends,
// and dispatches each datagram. The shared SSDP read-error policy
// (discovery.HandleReadErr, PR #469): a read-deadline timeout resets the
// streak and continues (the idle tick that keeps a context cancel
// responsive); a cancelled context or a closed socket exits; any other
// persistent error logs, with a context-aware backoff so it cannot hot-spin
// a core, and escalates to one Error once sustained.
func (s *SSDPSubsystem) readLoop(l *ssdpListener) {
	defer s.readers.Done()
	defer close(l.readerDone)
	buf := make([]byte, 2048) // SSDP packets are small; 2KB is plenty
	oob := make([]byte, datagramMetaSpace)
	streak := 0
	for {
		if s.ctx.Err() != nil {
			return
		}
		n, src, m, err := l.conn.readDatagram(buf, oob, time.Now().Add(ssdpListenerReadDeadline))
		if err != nil {
			if discovery.HandleReadErr(s.ctx, err, &streak, s.log) {
				return
			}
			continue
		}
		streak = 0
		s.dispatch(l, buf[:n], src, m)
	}
}

// interfaceName returns the interface's Name field, or "(any)" if nil
// (the OS-pick case). Slog helper — keeps the Start log message tidy.
func interfaceName(iface *net.Interface) string {
	if iface == nil {
		return "(any)"
	}
	return iface.Name
}

// pinMulticastInterface pins conn's outgoing multicast to iface. Every
// endpoint's sender is pinned so; the MediaServer only warns when it fails,
// so that endpoint's NOTIFYs take the OS default interface, and a strict
// registration refuses the endpoint. The tests that start an advertiser on
// the loopback interface make the same call on a socket of their own first,
// and skip where it fails: on that fallback their NOTIFYs would reach the
// LAN (loopbackInterface, backlog B38).
func pinMulticastInterface(conn *net.UDPConn, iface *net.Interface) error {
	return ipv4.NewPacketConn(conn).SetMulticastInterface(iface)
}

// openNotifySender opens the socket an endpoint writes its NOTIFYs from:
// UNCONNECTED, on the wildcard address and a port of the kernel's choosing.
// It is pinned to the endpoint's interface before anything is sent, and
// every NOTIFY is written to the group with WriteToUDP, so the kernel picks
// the datagram's route and source address from the pinned interface on each
// send, as the discovery clients' M-SEARCH sockets have always had it.
//
// It was net.DialUDP to the group, pinned afterwards, and a connect fixes
// the socket's route and source address at that moment, along the group's
// route (backlog B71, measured 2026-09-29). On Linux and macOS the pin still
// steered each datagram out of its own interface, with the source address
// the connect had fixed: in three network namespaces the NOTIFYs pinned to the
// second interface came from the FIRST interface's address, and a renderer
// with no route back to that subnet dropped every one; on macOS an
// advertiser pinned to lo0 announced from en0's address. On Windows the pin
// did not even steer: a socket connected that way and pinned to the loopback
// interface sent its datagram out of Ethernet. And on a host with no route
// to the group (no default route), the connect failed with "network is
// unreachable", no advertiser could start, and DLNA did not start at all;
// an unconnected socket needs no route to open, and a pinned send needs
// none either. On a host whose only interface carries the group's route
// nothing moves: the source is that interface's address either way.
func openNotifySender() (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
}
