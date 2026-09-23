// Package handshakelog keeps a local liveness probe out of a TLS
// listener's error log, and nothing else.
//
// The image's HEALTHCHECK runs `bridge health`, which proves the API
// listener is up by connecting and closing — no TLS, so no certificate to
// trust or to skip trusting. net/http logs every TLS handshake that fails,
// and a peer that closes before its ClientHello fails one:
//
//	http: TLS handshake error from 127.0.0.1:41418: EOF
//
// That is one line every 30 seconds, 2,880 a day, for the life of every
// container — the M-SEARCH class of flood, where the cost is not the disk
// but every other line becoming unfindable.
//
// Two things were measured before settling on dropping the line (the full
// record is in ops/engineering-log.md):
//
//   - The TEXT cannot identify the probe. A client that sends a complete
//     ClientHello (1,483 bytes from Go's) and then vanishes without an
//     alert gets the byte-identical line — and that is a real client
//     giving up mid-handshake. The probe is the one shape whose peer sent
//     NOTHING, so the listener records, per connection, whether the peer's
//     first answer was EOF, and the logger drops a line only on that
//     evidence.
//   - Making the probe finish a real handshake instead removes the line
//     only while the cert is healthy. Verified against the cert on disk,
//     the probe reports a live bridge dead whenever the served cert is
//     expired, not yet valid (a clock that ran ahead at mint time) or
//     rotated on disk ahead of the restart that serves it — and in each of
//     those the server logs `remote error: tls: bad certificate` for every
//     probe, so the noise returns exactly when an operator is reading the
//     log.
//
// Only a peer on this host is ever dropped: a loopback source, or a
// source address equal to the destination, which is what `bridge health`
// produces against a listenAddress bound to one specific IP (it dials that
// IP as-is). A completed TCP handshake cannot fake either, since the
// SYN-ACK goes to the claimed source. A silent close from anywhere else is
// a scanner or a load balancer's TCP check, and it is still logged.
//
// HTTP/3 needs nothing: the probe is TCP, and quic-go's http3.Server logs a
// failed connection only at Debug, through a Logger the bridge never sets.
package handshakelog

import (
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// Wrap returns l with every connection from this host instrumented, and
// the logger to install as the ErrorLog of the http.Server that serves it.
// The halves only work together: the logger drops a line on evidence the
// listener collected, and without it drops nothing.
//
// Every line the logger keeps goes to the standard library's default
// logger, which is exactly where a nil ErrorLog sends it — so panics,
// accept errors, HTTP/2 errors and every other handshake failure reach the
// operator unchanged.
//
// Wrap the RAW listener: the one handed to ServeTLS, or the one BEFORE
// tls.NewListener. Never wrap a listener that already yields *tls.Conn
// (tsnet's ListenTLS does): http.Server finds the handshake, ALPN and
// r.TLS by asserting that the accepted conn IS a *tls.Conn, and a wrapper
// around one hides all three.
func Wrap(l net.Listener) (net.Listener, *log.Logger) {
	w := &listener{Listener: l}
	return w, log.New(errorLog{w}, "", 0)
}

type listener struct {
	net.Listener

	// local maps a remote address, spelled as net/http prints it, to the
	// open connection from this host that holds it. Nothing else is ever
	// registered, so connections from elsewhere — every real client of a
	// LAN or tailnet bridge — are returned untouched and cost nothing.
	local sync.Map // string → *conn
}

// Accept registers each connection from this host under its peer's
// address, wrapped so its first read is recorded. Every other connection
// is returned exactly as it came.
func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil || !fromThisHost(c) {
		return c, err
	}
	pc := &conn{Conn: c, owner: l, key: c.RemoteAddr().String()}
	l.local.Store(pc.key, pc)
	return pc, nil
}

// fromThisHost reports whether c's peer is on this machine: a loopback
// source, or a source equal to the destination.
func fromThisHost(c net.Conn) bool {
	remote, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	if remote.IP.IsLoopback() {
		return true
	}
	local, ok := c.LocalAddr().(*net.TCPAddr)
	return ok && remote.IP.Equal(local.IP)
}

// What a connection's peer did first. Decided once, by the first read that
// returns data or EOF, and never changed.
const (
	undecided int32 = iota
	spoke           // the peer sent at least one byte
	silent          // the peer closed without sending one
)

type conn struct {
	net.Conn
	owner *listener
	key   string
	first atomic.Int32
	once  sync.Once
}

// Read passes through, and the first read that returns data or EOF
// records which of the two it was.
func (c *conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.first.Load() == undecided {
		switch {
		case n > 0:
			c.first.Store(spoke)
		case errors.Is(err, io.EOF):
			c.first.Store(silent)
		}
	}
	return n, err
}

// Close deregisters the connection. CompareAndDelete, not Delete: once
// this connection is gone its peer's address can be handed to a new one,
// and a late Close must not take the newer registration with it.
func (c *conn) Close() error {
	c.once.Do(func() { c.owner.local.CompareAndDelete(c.key, c) })
	return c.Conn.Close()
}

// The line net/http writes for a failed handshake, split around the peer
// address; the EOF reason is the only one a silent peer can produce.
const (
	handshakeErrorPrefix = "http: TLS handshake error from "
	silentPeerReason     = ": EOF"
)

// errorLog is the writer behind the logger Wrap returns: net/http formats
// each line and hands it over whole.
type errorLog struct{ l *listener }

// Write forwards the line to the standard logger, exactly as a nil
// ErrorLog would, unless it is a silent probe's.
func (w errorLog) Write(p []byte) (int, error) {
	line := string(p)
	if !w.l.isSilentProbe(line) {
		log.Print(line)
	}
	return len(p), nil
}

// isSilentProbe reports whether line is the handshake failure of an open
// connection from this host whose peer closed before sending a byte. The
// line is written on the connection's own goroutine BEFORE net/http closes
// it, so the registration is still there to consult.
//
// A line in any other shape fails OPEN — it is logged. If a future Go
// rewords this message, the cost is the probe's noise coming back, never
// a real failure going missing; TestTheSilentLoopbackProbeIsNotLogged runs
// the real net/http server so that change turns it red.
func (l *listener) isSilentProbe(line string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), handshakeErrorPrefix)
	if !ok {
		return false
	}
	addr, ok := strings.CutSuffix(rest, silentPeerReason)
	if !ok {
		return false
	}
	v, ok := l.local.Load(addr)
	return ok && v.(*conn).first.Load() == silent
}
