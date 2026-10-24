// Package ssdptest provides the SSDP fake network for cross-package lifecycle
// tests. Its sockets use the same deadline, parking and packet model as 5a1's
// package-local fakeNet; no host socket or interface is used.
package ssdptest

import (
	"context"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

// Network is a network of a test's own for an SSDP subsystem (B470 PR 5a1):
// listeners it delivers datagrams To, with whatever metadata the test says
// the kernel reported, senders and answer sockets that record what they
// write in one sequence, and switches that make any of them fail, block or
// park. No socket is opened, and no host interface is read.
type Network struct {
	mu      sync.Mutex
	changed chan struct{}
	// Seq numbers every write, so a test can order sends across sockets.
	Seq int
	// writes is every datagram written, in order.
	writes []Write

	// Per interface name ("" for the OS default): a listen that fails, a
	// listener's setup in place of full capabilities, a sender open or
	// pin that fails, every sender write that fails.
	ListenErr  map[string]error
	Setups     map[string]dlna.SSDPListenerCaps
	SenderErr  map[string]error
	PinErr     map[string]error
	WriteErr   map[string]error
	BlockSends map[string]bool // a sender's writes wait for its deadline or close
	// BlockReplies makes every answer socket dialled From now on wait, on
	// each write, for its deadline or its close.
	BlockReplies   bool
	ReplyRoute     dlna.SSDPEndpointSnapshot // default route, possibly through another NIC/VPN
	StrictReplyErr error                     // pin failure at reply creation
	StrictWriteErr error                     // per-packet interface selection failure

	// listeners is every listener opened, in order.
	listeners []*Listener
	Listens   int
	// senders is every sender opened, in order.
	senders []*Sender
	// replies is every answer socket dialled, in order.
	replies []*Reply

	// Parking: each, when set, runs where its name says and may block.
	OnListen      func(Iface string)
	OnDial        func(To netip.AddrPort)
	OnReplyWrite  func(r *Reply, n int) // before the reply's n-th write (0-based)
	OnSenderWrite func(s *Sender, Pkt string)
}

// Write is one datagram a fake socket wrote.
type Write struct {
	Seq   int
	Kind  string // "notify" (a sender's) or "reply"
	Iface string // the sender's interface, for a notify
	To    string
	Pkt   string
}

// New returns an empty network with full listener capabilities by default.
func New() *Network {
	return &Network{changed: make(chan struct{}),
		ListenErr:  map[string]error{},
		Setups:     map[string]dlna.SSDPListenerCaps{},
		SenderErr:  map[string]error{},
		PinErr:     map[string]error{},
		WriteErr:   map[string]error{},
		BlockSends: map[string]bool{},
	}
}

// fullSetup is a listener that reports destinations and arrivals and is
// confined.
var fullSetup = dlna.SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true, ReceiveConfined: true}

func ifaceNameOf(Iface *net.Interface) string {
	if Iface == nil {
		return ""
	}
	return Iface.Name
}

func (n *Network) listen(_ context.Context, Iface *net.Interface, _ *slog.Logger) (dlna.SSDPReader, dlna.SSDPListenerCaps, error) {
	name := ifaceNameOf(Iface)
	n.mu.Lock()
	n.Listens++
	hook := n.OnListen
	n.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.ListenErr[name]; err != nil {
		return nil, dlna.SSDPListenerCaps{}, err
	}
	setup, ok := n.Setups[name]
	if !ok {
		setup = fullSetup
	}
	l := &Listener{Iface: name, closed: make(chan struct{}), wake: make(chan struct{}, 1)}
	if Iface != nil {
		l.index = Iface.Index
	}
	n.listeners = append(n.listeners, l)
	return l, setup, nil
}

func (n *Network) openSender(Iface *net.Interface) (dlna.SSDPSender, error, error) {
	name := ifaceNameOf(Iface)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.SenderErr[name]; err != nil {
		return nil, nil, err
	}
	s := &Sender{n: n, Iface: name, closed: make(chan struct{})}
	n.senders = append(n.senders, s)
	var PinErr error
	if Iface != nil {
		PinErr = n.PinErr[name]
	}
	return s, PinErr, nil
}

func (n *Network) dialReply(To netip.AddrPort, From *dlna.SSDPEndpointSnapshot) (dlna.SSDPReply, error) {
	n.mu.Lock()
	hook := n.OnDial
	n.mu.Unlock()
	if hook != nil {
		hook(To)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if From != nil && n.StrictReplyErr != nil {
		return nil, n.StrictReplyErr
	}
	r := &Reply{n: n, To: To.String(), closed: make(chan struct{}), block: n.BlockReplies, From: n.ReplyRoute}
	if From != nil {
		r.From = *From
		r.WriteErr = n.StrictWriteErr
	}
	n.replies = append(n.replies, r)
	return r, nil
}

// record appends one write under n.mu.
func (n *Network) record(Kind, Iface, To, Pkt string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Seq++
	n.writes = append(n.writes, Write{Seq: n.Seq, Kind: Kind, Iface: Iface, To: To, Pkt: Pkt})
	close(n.changed)
	n.changed = make(chan struct{})
}

// Snapshot is the writes so far.
func (n *Network) Snapshot() []Write {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Write(nil), n.writes...)
}

// OpenSockets counts the fake sockets of each Kind not yet closed.
func (n *Network) OpenSockets() (listeners, senders, replies int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.listeners {
		if !l.isClosed() {
			listeners++
		}
	}
	for _, s := range n.senders {
		if !s.isClosed() {
			senders++
		}
	}
	for _, r := range n.replies {
		if !r.isClosed() {
			replies++
		}
	}
	return
}

// OpenListener is the open listener for Iface's name, the latest one.
func (n *Network) OpenListener(t testing.TB, Iface string) *Listener {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.listeners) - 1; i >= 0; i-- {
		if l := n.listeners[i]; l.Iface == Iface && !l.isClosed() {
			return l
		}
	}
	t.Fatalf("no open listener for %q", Iface)
	return nil
}

// datagram is one datagram a fake listener holds.
type datagram struct {
	Pkt  string
	src  netip.AddrPort
	meta dlna.SSDPDatagram
}

// Listener is a listener's socket the test feeds.
type Listener struct {
	Iface string
	index int

	mu      sync.Mutex
	queue   []datagram
	waiting bool // the reader is in a read with nothing queued
	closed  chan struct{}
	once    sync.Once
	wake    chan struct{}

	// OnClose, when set, runs inside Close before the socket counts as
	// closed, and may block: a listener parked while it closes.
	OnClose func()
}

func (l *Listener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// Deliver queues one datagram for the reader.
func (l *Listener) Deliver(Pkt string, src netip.AddrPort, meta dlna.SSDPDatagram) {
	l.mu.Lock()
	l.queue = append(l.queue, datagram{Pkt: Pkt, src: src, meta: meta})
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Listener) ReadDatagram(buf, _ []byte, deadline time.Time) (int, dlna.SSDPDatagram, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		l.mu.Lock()
		if len(l.queue) > 0 {
			d := l.queue[0]
			l.queue = l.queue[1:]
			l.waiting = false
			l.mu.Unlock()
			d.meta.Source = d.src
			return copy(buf, d.Pkt), d.meta, nil
		}
		l.waiting = true
		l.mu.Unlock()
		select {
		case <-l.closed:
			return 0, dlna.SSDPDatagram{}, net.ErrClosed
		case <-timer.C:
			return 0, dlna.SSDPDatagram{}, os.ErrDeadlineExceeded
		case <-l.wake:
		}
	}
}

// Idle reports whether the reader has taken every datagram and is back in a
// read, so every datagram delivered before has been dispatched.
func (l *Listener) Idle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiting && len(l.queue) == 0
}

// Close releases the listener and wakes its reader.
func (l *Listener) Close() error {
	if l.OnClose != nil {
		l.OnClose()
	}
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Sender is an endpoint's NOTIFY sender.
type Sender struct {
	n     *Network
	Iface string

	mu       sync.Mutex
	deadline time.Time
	closed   chan struct{}
	once     sync.Once
}

func (s *Sender) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// SetWriteDeadline bounds the sender's writes.
func (s *Sender) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = t
	return nil
}

// WriteToUDP records one NOTIFY datagram or the configured failure.
func (s *Sender) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	if s.isClosed() {
		return 0, net.ErrClosed
	}
	Pkt := string(b)
	s.n.mu.Lock()
	hook, werr, block := s.n.OnSenderWrite, s.n.WriteErr[s.Iface], s.n.BlockSends[s.Iface]
	s.n.mu.Unlock()
	if hook != nil {
		hook(s, Pkt)
	}
	s.mu.Lock()
	deadline := s.deadline
	s.mu.Unlock()
	if block {
		return 0, waitForDeadlineOrClose(deadline, s.closed)
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if s.isClosed() {
		return 0, net.ErrClosed
	}
	if werr != nil {
		return 0, werr
	}
	s.n.record("notify", s.Iface, addr.String(), Pkt)
	return len(b), nil
}

// Close interrupts pending sender writes and releases the socket.
func (s *Sender) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// waitForDeadlineOrClose blocks as a write the kernel cannot take blocks:
// until the deadline, a timeout, or the socket's close.
func waitForDeadlineOrClose(deadline time.Time, closed <-chan struct{}) error {
	if deadline.IsZero() {
		<-closed
		return net.ErrClosed
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-closed:
		return net.ErrClosed
	case <-timer.C:
		return os.ErrDeadlineExceeded
	}
}

// Reply is one answer burst's socket.
type Reply struct {
	n        *Network
	To       string
	From     dlna.SSDPEndpointSnapshot // zero means the OS route selected the source/interface
	WriteErr error

	mu       sync.Mutex
	deadline time.Time
	writes   int
	closed   chan struct{}
	once     sync.Once
	// block makes each write wait for the deadline or the close.
	block bool
}

func (r *Reply) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

// SetWriteDeadline bounds this response burst.
func (r *Reply) SetWriteDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline = t
	return nil
}

// Write records one response or the configured failure.
func (r *Reply) Write(b []byte) (int, error) {
	r.mu.Lock()
	n := r.writes
	r.writes++
	deadline, block := r.deadline, r.block
	r.mu.Unlock()
	r.n.mu.Lock()
	hook := r.n.OnReplyWrite
	r.n.mu.Unlock()
	if hook != nil {
		hook(r, n)
	}
	if r.isClosed() {
		return 0, net.ErrClosed
	}
	if r.WriteErr != nil {
		return 0, r.WriteErr
	}
	if block {
		return 0, waitForDeadlineOrClose(deadline, r.closed)
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	r.n.record("reply", "", r.To, string(b))
	return len(b), nil
}

// Close interrupts pending reply writes and releases the socket.
func (r *Reply) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// Configure changes fault injection under the network lock. Hooks themselves
// run outside it, so a barrier can park a send while the test reads packets.
func (n *Network) Configure(f func(*Network)) { n.mu.Lock(); defer n.mu.Unlock(); f(n) }

// Changed closes at the next packet write.
func (n *Network) Changed() <-chan struct{} { n.mu.Lock(); defer n.mu.Unlock(); return n.changed }

// Transport adapts the fake sockets to the production subsystem's instance seam.
func (n *Network) Transport() *dlna.SSDPNetwork {
	return &dlna.SSDPNetwork{Listen: n.listen, OpenSender: n.openSender, Reply: n.dialReply, Delay: func(time.Duration) time.Duration { return 0 }, IntervalFloor: time.Millisecond}
}

// ReplySources returns the selected endpoint of every reply socket.
func (n *Network) ReplySources() []dlna.SSDPEndpointSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []dlna.SSDPEndpointSnapshot
	for _, r := range n.replies {
		out = append(out, r.From)
	}
	return out
}
