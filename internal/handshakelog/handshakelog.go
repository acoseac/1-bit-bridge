// Package handshakelog is the error log of the bridge's HTTP servers. It
// keeps a local liveness probe out of it, and every peer's address out of
// what it keeps.
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
//
// The peer's address comes out of every line either logger keeps
// (RedactPeers). net/http prints it in a failed handshake, a recovered
// panic and the HTTP/2 connection errors, and the bridge's privacy page
// promises that client IPs are not logged for the phone-facing API: a
// phone with a stale pin, a cancelled endpoint probe and a scanner each
// used to leave one. The line keeps everything else, the reason included.
package handshakelog

import (
	"errors"
	"io"
	"log"
	"net"
	"regexp"
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
// operator, with the peer's address taken out (RedactPeers).
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

// ErrorLog is the logger for an http.Server whose listener Wrap cannot
// take: one that yields *tls.Conn, such as tsnet's ListenTLS. It drops no
// line and takes the peer's address out of each one.
func ErrorLog() *log.Logger {
	return log.New(redactingLog{}, "", 0)
}

// redactingLog forwards each line to the standard logger, as a nil
// ErrorLog would, without the peer's address.
type redactingLog struct{}

func (redactingLog) Write(p []byte) (int, error) {
	log.Print(RedactPeers(string(p)))
	return len(p), nil
}

// ClientPlaceholder stands where a peer's address was.
const ClientPlaceholder = "<client address>"

// peerAddr matches an address where net/http and its bundled HTTP/2 server
// print a peer's (Go 1.26): after "from " ("http: TLS handshake error from
// %s", "timeout waiting for SETTINGS frames from %v", "http2: server
// connection error from %v"), after "client " ("error reading preface from
// client %v") and after "serving " ("http: panic serving %v", "http2: panic
// serving %v"). The address is IPv4 or bracketed IPv6, with its port.
// Anchoring on those words keeps a LOCAL address in the line, such as the
// listen address in an accept error, which is the operator's own
// configuration and says nothing about who connected.
var peerAddr = regexp.MustCompile(`((?:^|\s)(?:from|client|serving) )(?:\d{1,3}(?:\.\d{1,3}){3}|\[[^\]\s]+\]):\d+`)

// RedactPeers returns line with each peer address in it replaced by
// ClientPlaceholder.
func RedactPeers(line string) string {
	return peerAddr.ReplaceAllString(line, "${1}"+ClientPlaceholder)
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

// Write forwards the line to the standard logger, as a nil ErrorLog would
// but without the peer's address, unless it is a silent probe's. The probe
// is recognised by its address, so the check reads the line as net/http
// wrote it and the redaction comes after.
func (w errorLog) Write(p []byte) (int, error) {
	line := string(p)
	if !w.l.isSilentProbe(line) {
		log.Print(RedactPeers(line))
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
