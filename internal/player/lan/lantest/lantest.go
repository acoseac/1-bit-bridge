// Package lantest is a LAN for the tests of the player's listener
// (internal/player/openhome) and of serve's wiring of it: an address
// source the test sets, and listeners on 127.0.0.1 whose connections
// report the LAN addresses the test names, the address the listener
// stands for and the source a dial stands for. So the surface's admission,
// its limits and its URLs are tested on loopback, and no test binds the
// host's LAN addresses or reads its interfaces.
package lantest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// sourceWait bounds how long a listener's Accept waits for the source of
// a connection Dial made: Dial records it the moment its connect returns,
// which the kernel may complete before the listener's Accept runs, or
// after.
const sourceWait = 10 * time.Second

// Box is the test's LAN.
type Box struct {
	t testing.TB

	mu         sync.Mutex
	inv        lan.Inventory
	listErr    error
	listenErrs map[netip.Addr]error
	listens    []netip.AddrPort
	listeners  []*listener
	sources    map[int]netip.Addr
	sourced    chan struct{}
	sndBuf     int
	rcvBuf     int
	memory     bool
	pipes      []*pipeListener
}

// New is an empty LAN: no address, so a surface on it binds nothing until
// the test Sets one.
func New(t testing.TB) *Box {
	return &Box{t: t, listenErrs: map[netip.Addr]error{}, sources: map[int]netip.Addr{}, sourced: make(chan struct{})}
}

// Address is a served address: the address with its on-link prefix
// ("192.168.1.20/24"), and its interface's name and index.
func Address(prefix, iface string, index int) lan.Address {
	return lan.Address{Prefix: netip.MustParsePrefix(prefix), Interface: iface, Index: index}
}

// Set is what the box serves from now on, and its own addresses; each
// served address is one of them already.
func (b *Box) Set(served []lan.Address, own ...netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listErr = nil
	b.inv = lan.Inventory{Served: slices.Clone(served)}
	for _, a := range served {
		b.inv.Own = append(b.inv.Own, a.Prefix.Addr())
	}
	b.inv.Own = append(b.inv.Own, own...)
}

// FailListing makes every listing from now on fail with err, until Set.
func (b *Box) FailListing(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listErr = err
}

// FailListen makes a listen on the address at fail with err (nil puts it
// back).
func (b *Box) FailListen(at netip.Addr, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		delete(b.listenErrs, at)
		return
	}
	b.listenErrs[at] = err
}

// Buffers sets the send buffer of every connection the box's listeners
// accept from now on, and the receive buffer of every dial: a client that
// stops reading then holds a server's writes after little has been sent.
func (b *Box) Buffers(send, receive int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sndBuf, b.rcvBuf = send, receive
}

// Addresses is the surface's address source.
func (b *Box) Addresses() (lan.Inventory, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listErr != nil {
		return lan.Inventory{}, b.listErr
	}
	return lan.Inventory{Served: slices.Clone(b.inv.Served), Own: slices.Clone(b.inv.Own)}, nil
}

// Listen is the surface's ListenFunc: a listener on 127.0.0.1 and a port
// the kernel picks, which stands for at's address on that port, whatever
// port at asks for.
func (b *Box) Listen(ctx context.Context, at netip.AddrPort, _ lan.Address) (net.Listener, string, error) {
	b.mu.Lock()
	b.listens = append(b.listens, at)
	err := b.listenErrs[at.Addr()]
	b.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	if b.memory {
		return b.listenPipe(at)
	}
	var lc net.ListenConfig
	real, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	port := real.Addr().(*net.TCPAddr).AddrPort().Port()
	l := &listener{box: b, real: real, lan: netip.AddrPortFrom(at.Addr(), port)}
	b.mu.Lock()
	b.listeners = append(b.listeners, l)
	b.mu.Unlock()
	b.t.Cleanup(func() { _ = real.Close() })
	return l, "none: a loopback listener of the test's", nil
}

// Listens are the addresses every Listen was asked for, in order.
func (b *Box) Listens() []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.listens)
}

// Open are the LAN addresses of the listeners still open.
func (b *Box) Open() []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []netip.AddrPort
	for _, l := range b.pipes {
		if !l.isClosed() {
			out = append(out, l.at)
		}
	}
	for _, l := range b.listeners {
		if !l.isClosed() {
			out = append(out, l.lan)
		}
	}
	return out
}

// Dial connects to the box's listener that stands for to, a connection
// that arrives from the source from.
func (b *Box) Dial(to netip.AddrPort, from netip.Addr) (net.Conn, error) {
	if b.memory {
		return b.dialPipe(to, from)
	}
	b.mu.Lock()
	var l *listener
	for _, c := range b.listeners {
		if c.lan == to && !c.isClosed() {
			l = c
		}
	}
	rcv := b.rcvBuf
	b.mu.Unlock()
	if l == nil {
		return nil, errors.New("lantest: no listener stands for " + to.String())
	}
	c, err := net.Dial("tcp4", l.real.Addr().String())
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok && rcv > 0 {
		_ = tc.SetReadBuffer(rcv)
	}
	b.mu.Lock()
	b.sources[c.LocalAddr().(*net.TCPAddr).Port] = from
	close(b.sourced)
	b.sourced = make(chan struct{})
	b.mu.Unlock()
	return c, nil
}

// Client is an HTTP client whose connections arrive from the source from,
// to the listener a URL's host and port stand for. It keeps no connection
// past the test.
func (b *Box) Client(from netip.Addr) *http.Client {
	tr := &http.Transport{DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			return nil, err
		}
		return b.Dial(ap, from)
	}}
	b.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: time.Minute}
}

// sourceOf is the source the dial from port stands for, waiting for Dial
// to record it; the zero Addr, which no admission takes, for a connection
// no Dial made.
func (b *Box) sourceOf(port int) netip.Addr {
	deadline := time.NewTimer(sourceWait)
	defer deadline.Stop()
	for {
		b.mu.Lock()
		src, ok := b.sources[port]
		wait := b.sourced
		b.mu.Unlock()
		if ok {
			return src
		}
		select {
		case <-wait:
		case <-deadline.C:
			b.t.Errorf("lantest: a connection from port %d reached a listener of the box, and no Dial made it", port)
			return netip.Addr{}
		}
	}
}

// listener is a loopback listener that stands for a LAN address.
type listener struct {
	box  *Box
	real net.Listener
	lan  netip.AddrPort

	mu     sync.Mutex
	closed bool
}

// Accept takes the next connection, as if it arrived on l's LAN address
// from the source its Dial stands for.
func (l *listener) Accept() (net.Conn, error) {
	c, err := l.real.Accept()
	if err != nil {
		return nil, err
	}
	l.box.mu.Lock()
	snd := l.box.sndBuf
	l.box.mu.Unlock()
	if tc, ok := c.(*net.TCPConn); ok && snd > 0 {
		_ = tc.SetWriteBuffer(snd)
	}
	port := c.RemoteAddr().(*net.TCPAddr).Port
	src := l.box.sourceOf(port)
	return &conn{Conn: c, local: net.TCPAddrFromAddrPort(l.lan), remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, uint16(port)))}, nil
}

// Addr is the LAN address the listener stands for.
func (l *listener) Addr() net.Addr { return net.TCPAddrFromAddrPort(l.lan) }

// Close closes the listener.
func (l *listener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.real.Close()
}

func (l *listener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// conn is an accepted connection with the LAN addresses it stands for.
type conn struct {
	net.Conn
	local, remote net.Addr
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

// SetLinger and CloseWrite are the TCP connection's.
func (c *conn) SetLinger(sec int) error {
	if tc, ok := c.Conn.(*net.TCPConn); ok {
		return tc.SetLinger(sec)
	}
	return nil
}

func (c *conn) CloseWrite() error {
	if tc, ok := c.Conn.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	return nil
}
