package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"tailscale.com/ipn/ipnstate"
)

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
func tsnetListen(ctx context.Context, node interface {
	ListenTLS(addr string) (net.Listener, error)
}, addr string, stderr io.Writer) (net.Listener, bool) {
	lis, err := node.ListenTLS(addr)
	if err != nil {
		fmt.Fprintf(stderr, "tsnet: ListenTLS: %v\n", err)
		return nil, false
	}
	return lis, true
}
