package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// http3ForceCloseAllowance is how long a drain of HTTP/3 servers is waited
// for past its grace. At the deadline http3.Server.Shutdown calls Close,
// which closes every connection, writing each one's CONNECTION_CLOSE, and
// only then waits for every handler to return. The writes are done within
// milliseconds of the deadline (under 4 ms in every drain measured, -race
// on a loaded host included); the wait is unbounded, since a handler that
// ignores its context holds it. A socket closed the moment the grace ran
// out lost the client's CONNECTION_CLOSE every time, whether the
// connection was idle, streaming or held by such a handler, and the
// client learned of the close only from its idle timeout. Closed after
// this allowance, it lost none. A tailnet node's Close closes the conns
// its servers write to, so the same holds there.
const http3ForceCloseAllowance = time.Second

// drainedWithin waits until drained is closed, for as long as ctx lasts and
// http3ForceCloseAllowance past that, and reports whether it was closed.
// ctx is the drain's own: the allowance is for the force-close its
// Shutdown begins when ctx ends.
func drainedWithin(ctx context.Context, drained <-chan struct{}) bool {
	if closedWithin(ctx, drained) {
		return true
	}
	allowance, cancel := context.WithTimeout(context.Background(), http3ForceCloseAllowance)
	defer cancel()
	return closedWithin(allowance, drained)
}

// lanHTTP3 is serve's HTTP/3 (QUIC) server on the LAN UDP socket, beside
// the HTTPS listener on the same address, and its one drain.
type lanHTTP3 struct {
	srv    *http3.Server
	conn   net.PacketConn
	stderr io.Writer

	stopOnce sync.Once
}

// serve serves srv on conn until srv is shut down. Serve returns
// http.ErrServerClosed then, whether or not conn has been closed as well.
func (l *lanHTTP3) serve() {
	if err := l.srv.Serve(l.conn); err != nil &&
		!errors.Is(err, http.ErrServerClosed) &&
		!strings.Contains(err.Error(), "server closed") {
		logger.Error("h3 serve direct", "err", err)
	}
}

// stop drains the server, until ctx ends and http3ForceCloseAllowance past
// that, and then closes its socket. A drain still running then (a handler
// that ignores its context, such as a read from a hung mount) costs a
// line, never a hung exit: the socket is closed under it, and the handler
// is left to return when it can, as http.Server.Shutdown leaves an HTTPS
// handler past its deadline. The socket must be closed on every exit path,
// because runServe can return to the launcher menu, whose next start binds
// the same UDP port.
//
// Only the first call drains; a later one does nothing. The shutdown branch
// drains beside the HTTPS server, and the deferred teardown drains on every
// other exit, so on a shutdown both run. A second Shutdown of a server
// whose first is still in Close would wait, for as long as that Close
// does, on the server's mutex, which Close holds while it waits for the
// handlers.
//
// Shutdown's error is reported only when it has returned. A drain given up
// on reports nothing later, so nothing reaches stderr after runServe has
// returned.
func (l *lanHTTP3) stop(ctx context.Context) {
	l.stopOnce.Do(func() {
		var err error
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			err = l.srv.Shutdown(ctx)
		}()
		switch {
		case !drainedWithin(ctx, drained):
			fmt.Fprintln(l.stderr, "shutdown: the LAN HTTP/3 server did not drain within grace; closing its socket under it")
		case err != nil:
			fmt.Fprintf(l.stderr, "lan h3 shutdown: %v\n", err)
		}
		_ = l.conn.Close()
	})
}
