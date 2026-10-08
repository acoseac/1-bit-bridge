package main

import (
	"context"
	"net"

	"github.com/quic-go/quic-go"
)

// apiBaseContext parents every request on an HTTP/1 or HTTP/2 server on
// serveCtx. http.Server.Shutdown does not cancel a handler, and an event
// stream never goes idle, so a server whose requests are not children of
// the context shutdown already cancelled waits out the grace. A download
// keeps copying: ServeContent does not read the request context, and the
// grace still closes that connection.
func apiBaseContext(serveCtx context.Context) func(net.Listener) context.Context {
	return func(net.Listener) context.Context { return serveCtx }
}

// apiConnContext is apiBaseContext for HTTP/3. quic-go's server has no
// BaseContext; a request's context is a child of the QUIC connection's,
// and Shutdown does not cancel a stream that is already running. The
// connection context stays the parent, so a connection that closes still
// ends the request when the serve context does not.
func apiConnContext(serveCtx context.Context) func(context.Context, *quic.Conn) context.Context {
	return func(connCtx context.Context, _ *quic.Conn) context.Context {
		return cancelWith(connCtx, serveCtx)
	}
}

// cancelWith returns a child of parent that ends when parent ends or when
// also ends.
func cancelWith(parent, also context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(also, cancel)
	context.AfterFunc(ctx, func() { stop() })
	return ctx
}
