package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"tailscale.com/ipn/ipnstate"
)

// tsnetNode is the embedded tailnet node as serve drives it. The
// production one is internal/tsnet's *Server; the boot tests pass a fake
// (serveOpts.tsnetNode) to hold a start or a listen open across a
// shutdown.
type tsnetNode interface {
	Start(context.Context) error
	Close() error
	Status(context.Context) (*ipnstate.Status, error)
	CertDomains() []string
	ListenTLS(addr string) (net.Listener, error)
	ListenPacket(network, addr string) (net.PacketConn, error)
	HTTP3TLSConfig() *tls.Config
	// The metrics collector's provider (metrics.RegisterTsnetProvider).
	MetricsState() int
	MetricsPeersOnline() int
	MetricsDERPLatencies() map[string]float64
}

// tsnetStartTimeout bounds how long the embedded node may take to come up,
// interactive auth included. The LAN listener serves regardless.
const tsnetStartTimeout = 5 * time.Minute

// tsnetStatusTimeout bounds the status query the tailnet HTTP/3 bind makes
// right after Start. Status can take a few hundred ms to settle.
const tsnetStatusTimeout = 5 * time.Second

// bringTsnetUp starts node, and reports whether it came up. A start that
// failed is reported on stderr, and the LAN listener carries on without the
// tailnet. One the shutdown stopped is not reported. ctx is serve's; the
// start's own timeout derives from it, so a start that ran out of time is
// still a failure.
func bringTsnetUp(ctx context.Context, node interface{ Start(context.Context) error }, stderr io.Writer) bool {
	startCtx, cancel := context.WithTimeout(ctx, tsnetStartTimeout)
	defer cancel()
	if err := node.Start(startCtx); err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			fmt.Fprintf(stderr, "tsnet: bring node up: %v (LAN listener still active)\n", failure)
		}
		return false
	}
	return true
}

// tsnetH3Status asks node for the status the tailnet HTTP/3 bind needs, and
// reports whether it answered. A query that failed is logged, and the caller
// runs HTTP/2 only on the tailnet; one the shutdown stopped is not logged,
// for bringTsnetUp's reasons.
func tsnetH3Status(ctx context.Context, node interface {
	Status(context.Context) (*ipnstate.Status, error)
}) (*ipnstate.Status, bool) {
	statusCtx, cancel := context.WithTimeout(ctx, tsnetStatusTimeout)
	defer cancel()
	status, err := node.Status(statusCtx)
	if err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Warn("Failed to query tsnet status for h3 bind, running HTTP/2 only on tailnet", "err", failure)
		}
		return nil, false
	}
	return status, true
}

// tsnetListen opens the tailnet HTTPS listener, and reports whether it did.
// A listen that failed is reported on stderr, and the LAN listener carries
// on without the tailnet.
//
// It opens none once the shutdown has begun: serve is stopping, so the
// listener would only be closed again, or fail against a node the shutdown
// has closed. A listen that fails after the shutdown began is not reported.
// That failure is the node closing under it, and it carries no
// cancellation for ctxerr to find, so the context is what tells it apart.
// Joining this goroutine before the node closes would remove the race; it
// is not joined yet (#997's class).
func tsnetListen(ctx context.Context, node interface {
	ListenTLS(addr string) (net.Listener, error)
}, addr string, stderr io.Writer) (net.Listener, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	lis, err := node.ListenTLS(addr)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(stderr, "tsnet: ListenTLS: %v\n", err)
		}
		return nil, false
	}
	if ctx.Err() != nil {
		// The shutdown began while the listen ran, and the wrapper takes
		// no context, so the listen could still succeed. The listener is
		// closed here rather than handed over to be served (CodeRabbit,
		// #1005).
		_ = lis.Close()
		return nil, false
	}
	return lis, true
}
