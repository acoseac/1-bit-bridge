package dlna

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNet is a network of a test's own for an SSDP subsystem (B470 PR 5a1):
// listeners it delivers datagrams to, with whatever metadata the test says
// the kernel reported, senders and answer sockets that record what they
// write in one sequence, and switches that make any of them fail, block or
// park. No socket is opened, and no host interface is read.
type fakeNet struct {
	mu sync.Mutex
	// seq numbers every write, so a test can order sends across sockets.
	seq int
	// writes is every datagram written, in order.
	writes []fakeWrite

	// Per interface name ("" for the OS default): a listen that fails, a
	// listener's setup in place of full capabilities, a sender open or
	// pin that fails, every sender write that fails.
	listenErr  map[string]error
	setups     map[string]ssdpListenerSetup
	senderErr  map[string]error
	pinErr     map[string]error
	writeErr   map[string]error
	blockSends map[string]bool // a sender's writes wait for its deadline or close
	// blockReplies makes every answer socket dialled from now on wait, on
	// each write, for its deadline or its close.
	blockReplies   bool
	replyRoute     SSDPEndpointSnapshot // default route, possibly through another NIC/VPN
	strictReplyErr error                // pin failure at reply creation
	strictWriteErr error                // per-packet interface selection failure

	// listeners is every listener opened, in order.
	listeners []*fakeListener
	listens   int
	// senders is every sender opened, in order.
	senders []*fakeSender
	// replies is every answer socket dialled, in order.
	replies []*fakeReply

	// Parking: each, when set, runs where its name says and may block.
	onListen      func(iface string)
	onDial        func(to netip.AddrPort)
	onReplyWrite  func(r *fakeReply, n int) // before the reply's n-th write (0-based)
	onSenderWrite func(s *fakeSender, pkt string)
}

// fakeWrite is one datagram a fake socket wrote.
type fakeWrite struct {
	seq   int
	kind  string // "notify" (a sender's) or "reply"
	iface string // the sender's interface, for a notify
	to    string
	pkt   string
}

func newFakeNet() *fakeNet {
	return &fakeNet{
		listenErr:  map[string]error{},
		setups:     map[string]ssdpListenerSetup{},
		senderErr:  map[string]error{},
		pinErr:     map[string]error{},
		writeErr:   map[string]error{},
		blockSends: map[string]bool{},
	}
}

// fullSetup is a listener that reports destinations and arrivals and is
// confined.
var fullSetup = ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true, ReceiveConfined: true}}

func ifaceNameOf(iface *net.Interface) string {
	if iface == nil {
		return ""
	}
	return iface.Name
}

func (n *fakeNet) listen(_ context.Context, iface *net.Interface, _ *slog.Logger) (ssdpReadConn, ssdpListenerSetup, error) {
	name := ifaceNameOf(iface)
	n.mu.Lock()
	n.listens++
	hook := n.onListen
	n.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.listenErr[name]; err != nil {
		return nil, ssdpListenerSetup{}, err
	}
	setup, ok := n.setups[name]
	if !ok {
		setup = fullSetup
	}
	l := &fakeListener{iface: name, closed: make(chan struct{}), wake: make(chan struct{}, 1)}
	if iface != nil {
		l.index = iface.Index
	}
	n.listeners = append(n.listeners, l)
	return l, setup, nil
}

func (n *fakeNet) openSender(iface *net.Interface) (ssdpSendConn, error, error) {
	name := ifaceNameOf(iface)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.senderErr[name]; err != nil {
		return nil, nil, err
	}
	s := &fakeSender{n: n, iface: name, closed: make(chan struct{})}
	n.senders = append(n.senders, s)
	var pinErr error
	if iface != nil {
		pinErr = n.pinErr[name]
	}
	return s, pinErr, nil
}

func (n *fakeNet) dialReply(to netip.AddrPort, from *SSDPEndpointSnapshot) (ssdpReplyConn, error) {
	n.mu.Lock()
	hook := n.onDial
	n.mu.Unlock()
	if hook != nil {
		hook(to)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if from != nil && n.strictReplyErr != nil {
		return nil, n.strictReplyErr
	}
	r := &fakeReply{n: n, to: to.String(), closed: make(chan struct{}), block: n.blockReplies, from: n.replyRoute}
	if from != nil {
		r.from = *from
		r.writeErr = n.strictWriteErr
	}
	n.replies = append(n.replies, r)
	return r, nil
}

// record appends one write under n.mu.
func (n *fakeNet) record(kind, iface, to, pkt string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seq++
	n.writes = append(n.writes, fakeWrite{seq: n.seq, kind: kind, iface: iface, to: to, pkt: pkt})
}

// snapshot is the writes so far.
func (n *fakeNet) snapshot() []fakeWrite {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]fakeWrite(nil), n.writes...)
}

// openSockets counts the fake sockets of each kind not yet closed.
func (n *fakeNet) openSockets() (listeners, senders, replies int) {
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

// openListener is the open listener for iface's name, the latest one.
func (n *fakeNet) openListener(t *testing.T, iface string) *fakeListener {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.listeners) - 1; i >= 0; i-- {
		if l := n.listeners[i]; l.iface == iface && !l.isClosed() {
			return l
		}
	}
	t.Fatalf("no open listener for %q", iface)
	return nil
}

// fakeDatagram is one datagram a fake listener holds.
type fakeDatagram struct {
	pkt  string
	src  netip.AddrPort
	meta datagramMeta
}

// fakeListener is a listener's socket the test feeds.
type fakeListener struct {
	iface string
	index int

	mu      sync.Mutex
	queue   []fakeDatagram
	waiting bool // the reader is in a read with nothing queued
	closed  chan struct{}
	once    sync.Once
	wake    chan struct{}

	// onClose, when set, runs inside Close before the socket counts as
	// closed, and may block: a listener parked while it closes.
	onClose func()
}

func (l *fakeListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// deliver queues one datagram for the reader.
func (l *fakeListener) deliver(pkt string, src netip.AddrPort, meta datagramMeta) {
	l.mu.Lock()
	l.queue = append(l.queue, fakeDatagram{pkt: pkt, src: src, meta: meta})
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *fakeListener) readDatagram(buf, _ []byte, deadline time.Time) (int, netip.AddrPort, datagramMeta, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		l.mu.Lock()
		if len(l.queue) > 0 {
			d := l.queue[0]
			l.queue = l.queue[1:]
			l.waiting = false
			l.mu.Unlock()
			return copy(buf, d.pkt), d.src, d.meta, nil
		}
		l.waiting = true
		l.mu.Unlock()
		select {
		case <-l.closed:
			return 0, netip.AddrPort{}, datagramMeta{}, net.ErrClosed
		case <-timer.C:
			return 0, netip.AddrPort{}, datagramMeta{}, os.ErrDeadlineExceeded
		case <-l.wake:
		}
	}
}

// idle reports whether the reader has taken every datagram and is back in a
// read, so every datagram delivered before has been dispatched.
func (l *fakeListener) idle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiting && len(l.queue) == 0
}

func (l *fakeListener) Close() error {
	if l.onClose != nil {
		l.onClose()
	}
	l.once.Do(func() { close(l.closed) })
	return nil
}

// fakeSender is an endpoint's NOTIFY sender.
type fakeSender struct {
	n     *fakeNet
	iface string

	mu       sync.Mutex
	deadline time.Time
	closed   chan struct{}
	once     sync.Once
}

func (s *fakeSender) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *fakeSender) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = t
	return nil
}

func (s *fakeSender) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	if s.isClosed() {
		return 0, net.ErrClosed
	}
	pkt := string(b)
	s.n.mu.Lock()
	hook, werr, block := s.n.onSenderWrite, s.n.writeErr[s.iface], s.n.blockSends[s.iface]
	s.n.mu.Unlock()
	if hook != nil {
		hook(s, pkt)
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
	s.n.record("notify", s.iface, addr.String(), pkt)
	return len(b), nil
}

func (s *fakeSender) Close() error {
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

// fakeReply is one answer burst's socket.
type fakeReply struct {
	n        *fakeNet
	to       string
	from     SSDPEndpointSnapshot // zero means the OS route selected the source/interface
	writeErr error

	mu       sync.Mutex
	deadline time.Time
	writes   int
	closed   chan struct{}
	once     sync.Once
	// block makes each write wait for the deadline or the close.
	block bool
}

func (r *fakeReply) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

func (r *fakeReply) SetWriteDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline = t
	return nil
}

func (r *fakeReply) Write(b []byte) (int, error) {
	r.mu.Lock()
	n := r.writes
	r.writes++
	deadline, block := r.deadline, r.block
	r.mu.Unlock()
	r.n.mu.Lock()
	hook := r.n.onReplyWrite
	r.n.mu.Unlock()
	if hook != nil {
		hook(r, n)
	}
	if r.isClosed() {
		return 0, net.ErrClosed
	}
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	if block {
		return 0, waitForDeadlineOrClose(deadline, r.closed)
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	r.n.record("reply", "", r.to, string(b))
	return len(b), nil
}

func (r *fakeReply) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// synthIface is a synthetic interface: an index and a name, nothing read
// from the host.
func synthIface(index int, name string) *net.Interface {
	return &net.Interface{Index: index, Name: name, Flags: net.FlagUp | net.FlagMulticast}
}

// fakeSubsystem is a subsystem over n, with answers sent at once unless a
// test sets replyDelay, and every NOTIFY written to the group's address
// (which the fake senders only record).
func fakeSubsystem(t *testing.T, n *fakeNet) *SSDPSubsystem {
	t.Helper()
	sub := NewSSDPSubsystem(context.Background(), SSDPSubsystemOptions{Logger: discardLogger()})
	sub.transport = n
	sub.replyDelay = func(time.Duration) time.Duration { return 0 }
	t.Cleanup(func() { _ = sub.Close(context.Background()) })
	return sub
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// legacyReg is a MediaServer-style registration: NotifyTargetsFor(udn),
// admission by destination over served.
func legacyReg(udn string, served *servedAddrs, eps ...SSDPEndpoint) SSDPRegistration {
	return SSDPRegistration{
		Name:           "DLNA",
		UDN:            udn,
		Targets:        NotifyTargetsFor(udn),
		ServerToken:    "MediaServer/1",
		Endpoints:      eps,
		Admission:      ssdpServedAdmission{served: served},
		ResponderSlots: maxConcurrentMSearchResponders,
		Logger:         discardLogger(),
	}
}

// sourceTargets is a test device of another type, as 5a2's Source will be:
// its rootdevice, its UUID, a device type of its own, and a service type it
// shares with the MediaServer.
func sourceTargets(udn string) []NotifyTarget {
	return []NotifyTarget{
		{NT: "upnp:rootdevice", USN: udn + "::upnp:rootdevice"},
		{NT: udn, USN: udn},
		{NT: "urn:av-openhome-org:device:Source:1", USN: udn + "::urn:av-openhome-org:device:Source:1"},
		{NT: "urn:schemas-upnp-org:service:ConnectionManager:1", USN: udn + "::urn:schemas-upnp-org:service:ConnectionManager:1"},
	}
}

// onLink is the source rule 5a2 will supply: the searcher is on the
// endpoint's own subnet.
func onLink(s SSDPSearch) bool {
	return s.Endpoint.Local.IsValid() && s.Endpoint.Local.Contains(s.Datagram.Source.Addr())
}

// strictReg is a strict test device with an on-link source rule.
func strictReg(udn string, slots int, eps ...SSDPEndpoint) SSDPRegistration {
	return SSDPRegistration{
		Name:           "Source",
		UDN:            udn,
		Targets:        sourceTargets(udn),
		ServerToken:    "Source/1",
		Endpoints:      eps,
		Admission:      SSDPStrictAdmission{Source: onLink},
		ResponderSlots: slots,
		Logger:         discardLogger(),
	}
}

// search is an M-SEARCH for st, with no MX.
func search(st string) string {
	return "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nST: " + st + "\r\n\r\n"
}

// groupMeta is the metadata of a group datagram that arrived on the
// interface of index idx.
func groupMeta(idx int) datagramMeta {
	return datagramMeta{dst: ssdpGroupAddr, dstOK: true, ifIndex: idx, ifOK: idx > 0}
}

// unicastMeta is the metadata of a unicast to dst that arrived on the
// interface of index idx.
func unicastMeta(dst string, idx int) datagramMeta {
	return datagramMeta{dst: netip.MustParseAddr(dst), dstOK: true, ifIndex: idx, ifOK: idx > 0}
}

// waitFor polls cond until it holds, failing the test after five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settle waits until every open fake listener has dispatched everything
// delivered to it and every answer has finished writing (its responder slot
// is free).
func settle(t *testing.T, sub *SSDPSubsystem, n *fakeNet) {
	t.Helper()
	waitFor(t, "the listeners to dispatch", func() bool {
		n.mu.Lock()
		ls := append([]*fakeListener(nil), n.listeners...)
		n.mu.Unlock()
		for _, l := range ls {
			if !l.isClosed() && !l.idle() {
				return false
			}
		}
		return true
	})
	waitFor(t, "the answers to finish", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		for _, h := range sub.handles {
			for _, ep := range h.active {
				if len(ep.pool) > 0 {
					return false
				}
			}
		}
		return true
	})
}

// answer is one reply datagram as a test reads it.
type answer struct {
	to, location, st, usn string
}

// replies are the reply datagrams written since writes[from:], read as
// answers.
func replies(n *fakeNet, from int) []answer {
	var out []answer
	for _, w := range n.snapshot()[from:] {
		if w.kind != "reply" {
			continue
		}
		out = append(out, answer{to: w.to, location: header(w.pkt, "LOCATION"), st: header(w.pkt, "ST"), usn: header(w.pkt, "USN")})
	}
	return out
}

// header is a datagram's header value, or "".
func header(pkt, name string) string {
	for _, line := range strings.Split(pkt, "\r\n") {
		if k, v, ok := strings.Cut(line, ": "); ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// notifies are the NOTIFY datagrams of kind nts (ssdp:alive, ssdp:byebye)
// written since writes[from:], as "LOCATION USN".
func notifies(n *fakeNet, from int, nts string) []string {
	var out []string
	for _, w := range n.snapshot()[from:] {
		if w.kind == "notify" && header(w.pkt, "NTS") == nts {
			out = append(out, header(w.pkt, "LOCATION")+" "+header(w.pkt, "USN"))
		}
	}
	return out
}

// mustRegister registers reg on sub and fails the test on an error.
func mustRegister(t *testing.T, sub *SSDPSubsystem, reg SSDPRegistration) (*SSDPHandle, SSDPResult) {
	t.Helper()
	h, res, err := sub.Register(context.Background(), reg)
	if err != nil {
		t.Fatalf("Register %s: %v (%+v)", reg.UDN, err, res)
	}
	return h, res
}

// fakeServed is a MediaServer's served addresses on the synthetic host the
// tests share: en0 192.168.1.20/24 (and a link-local), en1 10.0.0.5/8, and
// Tailscale's utun7, which is no member.
func fakeServed() *servedAddrs {
	return newTestHost(
		lan("en0", "192.168.1.20", "fe80::e0"),
		lan("en1", "10.0.0.5"),
		tunnel("utun7", "100.64.0.7", "fd7a:115c:a1e0::7"),
	).served(nil)
}

// errInjected is a failure a test injects.
var errInjected = errors.New("injected")

// mustErr fails unless err is or wraps want.
func mustErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: %v, want %v", what, err, want)
	}
}

// describe is a list of answers as text, for a message.
func describe(as []answer) string {
	var b strings.Builder
	for _, a := range as {
		fmt.Fprintf(&b, "\n  to %s from %s: %s", a.to, a.location, a.usn)
	}
	return b.String()
}
