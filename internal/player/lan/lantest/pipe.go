package lantest

import (
	"net"
	"net/netip"
	"sync"
	"testing"
)

// NewMemory is a box whose connections are net.Pipe pairs. It opens no host
// socket, while running net/http's real Serve, admission and handlers.
// Socket-buffer and TCP half-close tests must still use New's loopback box.
func NewMemory(t testing.TB) *Box { b := New(t); b.memory = true; return b }

type pipeListener struct {
	at       netip.AddrPort
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (b *Box) listenPipe(at netip.AddrPort) (net.Listener, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if at.Port() == 0 {
		at = netip.AddrPortFrom(at.Addr(), uint16(20000+len(b.pipes)))
	}
	l := &pipeListener{at: at, incoming: make(chan net.Conn), closed: make(chan struct{})}
	b.pipes = append(b.pipes, l)
	b.t.Cleanup(func() { _ = l.Close() })
	return l, "none: an in-memory listener of the test's", nil
}

func (b *Box) dialPipe(to netip.AddrPort, from netip.Addr) (net.Conn, error) {
	b.mu.Lock()
	var selected *pipeListener
	for _, l := range b.pipes {
		if l.at == to && !l.isClosed() {
			selected = l
		}
	}
	b.mu.Unlock()
	if selected == nil {
		return nil, net.ErrClosed
	}
	client, server := net.Pipe()
	remote := net.TCPAddrFromAddrPort(netip.AddrPortFrom(from, 50000))
	local := net.TCPAddrFromAddrPort(to)
	select {
	case selected.incoming <- &conn{Conn: server, local: local, remote: remote}:
		return &conn{Conn: client, local: remote, remote: local}, nil
	case <-selected.closed:
		_ = client.Close()
		_ = server.Close()
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Addr() net.Addr { return net.TCPAddrFromAddrPort(l.at) }
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}
