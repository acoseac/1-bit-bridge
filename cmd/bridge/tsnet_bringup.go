package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
	"github.com/quic-go/quic-go/http3"
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
// listener would only be closed again. The listen itself takes no context.
// stop cancels ctx before it waits for the goroutine, so a listen that
// returns after the cancel is dealt with here: its listener is closed
// rather than served, and its failure is not reported. That failure is the
// node closing under a listen stop gave up waiting for, and it carries no
// cancellation for ctxerr to find, so the context is what tells it apart.
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

// tsnetH3Listener pairs one HTTP/3 (QUIC) server with the UDP
// PacketConn it serves on. Dual-stack tailnet nodes carry both an
// IPv4 (`100.x.y.z`) and IPv6 (`fd7a:...`) tailnet IP, and
// `tsnet.Server.ListenPacket` requires an explicit IP per call —
// so we bind one listener per assigned IP and run each on its own
// goroutine. The HTTP/2 path via `ListenTLS(cfg.ListenAddress)`
// accepts on every tailnet IP for free (it gets a port-only
// unspecified-IP form); HTTP/3 doesn't have that shortcut.
type tsnetH3Listener struct {
	srv  *http3.Server
	conn net.PacketConn
}

// tsnetFront is serve's tailnet side. One goroutine (run) brings the
// embedded node up, then serves the API on it over HTTP/3 and HTTPS, and
// stop, which runServe defers, stops that goroutine and waits for it
// BEFORE it closes the node. The goroutine writes the node's state dir
// while it brings the node up, and upstream's Close "must not be called
// before or concurrently with Start", so it is joined like every other
// background writer runServe starts.
//
// The goroutine runs on a context of its own, which stop cancels on EVERY
// exit path. serve's context does not do that: on an error exit it is
// still live while the teardown runs (runServe's own cancel is its first
// defer, so it runs last, after the node is closed), so a start waiting
// for interactive auth went on waiting, and a listen that landed was
// served.
//
// What the goroutine hands stop to shut down, it publishes under mu, and a
// publication is refused once stop has begun: stop takes what was
// published in the critical section that begins it, so a server published
// after that would serve on a node that is closing, with nothing left to
// shut it down. Nothing is served before it is published.
type tsnetFront struct {
	node     tsnetNode
	handler  http.Handler
	addr     string // cfg.ListenAddress: where HTTPS listens, and the HTTP/3 port
	useHTTP3 bool   // !cfg.DisableHTTP3
	stderr   io.Writer

	// serveErr carries the HTTPS server's Serve result to runServe's
	// select. Buffered, since it can be sent after runServe has stopped
	// selecting.
	serveErr chan error

	cancel context.CancelFunc
	done   chan struct{}  // closed once run, and every HTTP/3 Serve it started, have returned
	h3     sync.WaitGroup // the HTTP/3 Serve goroutines run starts

	mu        sync.Mutex
	stopping  bool // stop has begun; nothing is published after it
	h3Servers []tsnetH3Listener
	https     *http.Server
}

// startTsnetFront starts serve's tailnet side on node. ctx is serve's; the
// goroutine's context derives from it, so a shutdown reaches the goroutine
// before stop does.
func startTsnetFront(ctx context.Context, node tsnetNode, handler http.Handler, addr string, useHTTP3 bool, stderr io.Writer) *tsnetFront {
	runCtx, cancel := context.WithCancel(ctx)
	f := &tsnetFront{
		node:     node,
		handler:  handler,
		addr:     addr,
		useHTTP3: useHTTP3,
		stderr:   stderr,
		serveErr: make(chan error, 1),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(f.done)
		defer f.h3.Wait()
		f.run(runCtx)
	}()
	return f
}

// run is the goroutine. Up() blocks on interactive auth on first run,
// which is why it runs beside the LAN listener instead of before it:
// the LAN listener is already serving while the operator visits the
// AuthURL. A failure anywhere here is logged and non-fatal — the LAN
// listener keeps the bridge usable even if the tailnet never comes up.
func (f *tsnetFront) run(ctx context.Context) {
	if !bringTsnetUp(ctx, f.node, f.stderr) {
		return
	}
	// Wire the metrics tsnet collector so /metrics +
	// /v1/diagnostics surfaces report the live tailnet
	// state. The provider is a structural interface match:
	// the node's `MetricsState` / `MetricsPeersOnline` /
	// `MetricsDERPLatencies` methods satisfy
	// `metrics.tsnetStatusProvider` without an explicit
	// import in either direction at the interface level.
	metrics.RegisterTsnetProvider(f.node)
	if f.useHTTP3 {
		f.serveHTTP3(ctx)
	}
	lis, ok := tsnetListen(ctx, f.node, f.addr, f.stderr)
	if !ok {
		return
	}
	// A sibling of the LAN http.Server, on the same handler; the
	// read/write timeout shape mirrors it (see the rationale there),
	// down to the slow-loris defence (PR-C tightened ReadHeaderTimeout
	// 10s → 5s).
	srv := &http.Server{
		Handler:           f.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if !f.publishHTTPS(srv) {
		_ = lis.Close()
		return
	}
	f.serveErr <- srv.Serve(lis)
}

// serveHTTP3 serves HTTP/3 (QUIC) over the tailnet. It runs AFTER Start
// succeeded, because the wrapper's ListenPacket returns "called before
// Start" otherwise (the synchronous PR #264 placement always tripped that
// guard at boot).
//
// Three things it is responsible for that the pre-fix shape got wrong:
//
//  1. Upstream tsnet.Server.ListenPacket requires an explicit tailnet IP
//     (not the ":port" shorthand the LAN path uses with net.ListenUDP) —
//     the listener binds to the virtual tailnet interface specifically,
//     and a bare ":port" fails with "address must be a valid IP". We
//     query Status() for the assigned IPs and bind one PacketConn per IP.
//     Status() can take a few hundred ms to settle right after Start, so
//     it uses a bounded context.
//
//  2. Dual-stack tailnet nodes carry both IPv4 (100.x.y.z) and IPv6
//     (fd7a:...). HTTP/2 via `ListenTLS(cfg.ListenAddress)` accepts on
//     both for free (port-only unspecified-IP form); HTTP/3 needs one
//     explicit bind per IP — otherwise dual-stack clients connecting over
//     the unbound address family fall back to HTTP/2 silently.
//
//  3. The H3 port MUST match the H2 port (extracted from
//     cfg.ListenAddress). The Alt-Svc header `apiSrv` emits advertises h3
//     at the request's port; if H3 listened on a different port (the
//     pre-fix shape hardcoded :443) clients would dial the wrong port and
//     never upgrade.
//
// A per-IP bind failure is non-fatal: the listeners that did bind are
// served. Total bind failure degrades to HTTP/2 over the tailnet. The
// servers start only once they are published, so a shutdown that lands
// while the rest are binding finds none serving: one that did would go on
// serving as the node closed under it, and report `h3 serve tsnet` with
// the transport's closed error.
func (f *tsnetFront) serveHTTP3(ctx context.Context) {
	status, statusOK := tsnetH3Status(ctx, f.node)
	_, port, splitErr := net.SplitHostPort(f.addr)
	switch {
	case !statusOK:
		return // tsnetH3Status has said why.
	case status == nil || status.Self == nil || len(status.Self.TailscaleIPs) == 0:
		logger.Warn("tsnet status returned no tailnet IPs for h3 bind, running HTTP/2 only on tailnet")
		return
	case splitErr != nil || port == "":
		logger.Warn("Failed to parse port from cfg.ListenAddress for tsnet h3 bind, running HTTP/2 only on tailnet", "addr", f.addr, "err", splitErr)
		return
	}
	listeners := f.bindHTTP3(ctx, status.Self.TailscaleIPs, port)
	if len(listeners) == 0 {
		if ctx.Err() == nil {
			logger.Warn("No tsnet HTTP/3 listeners bound on any tailnet IP, running HTTP/2 only on tailnet")
		}
		return
	}
	if !f.publishHTTP3(listeners) {
		for _, l := range listeners {
			_ = l.conn.Close()
		}
		return
	}
	for _, l := range listeners {
		f.h3.Add(1)
		go func() {
			defer f.h3.Done()
			if err := l.srv.Serve(l.conn); err != nil &&
				!errors.Is(err, http.ErrServerClosed) &&
				!strings.Contains(err.Error(), "server closed") {
				logger.Error("h3 serve tsnet", "err", err)
			}
		}()
	}
	logger.Info("tsnet HTTP/3 listeners bound", "count", len(listeners), "ipsReported", len(status.Self.TailscaleIPs), "port", port)
}

// bindHTTP3 binds one HTTP/3 server per tailnet address, and reports a
// bind that fails. It binds no more once the shutdown has begun, and a
// bind that fails after that is not reported: it is the node closing under
// it, and the wrapper's "called before Start" carries no cancellation for
// ctxerr to find, so the context is what tells it apart (tsnetListen's
// rule). What it did bind goes to publishHTTP3, which refuses it once
// stop has begun.
func (f *tsnetFront) bindHTTP3(ctx context.Context, ips []netip.Addr, port string) []tsnetH3Listener {
	listeners := make([]tsnetH3Listener, 0, len(ips))
	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		bindAddr := net.JoinHostPort(ip.String(), port)
		pconn, err := f.node.ListenPacket("udp", bindAddr)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("Failed to bind tsnet UDP socket for h3, continuing with remaining IPs", "addr", bindAddr, "err", err)
			}
			continue
		}
		listeners = append(listeners, tsnetH3Listener{
			srv:  &http3.Server{Handler: f.handler, TLSConfig: f.node.HTTP3TLSConfig()},
			conn: pconn,
		})
	}
	return listeners
}

// publishHTTP3 hands ls to stop, or reports that stop has begun, in which
// case the caller closes them.
func (f *tsnetFront) publishHTTP3(ls []tsnetH3Listener) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopping {
		return false
	}
	f.h3Servers = ls
	return true
}

// publishHTTPS is publishHTTP3 for the HTTPS server.
func (f *tsnetFront) publishHTTPS(srv *http.Server) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopping {
		return false
	}
	f.https = srv
	return true
}

// http3Listeners returns the HTTP/3 listeners published so far, for the
// shutdown branch's early drain; stop takes them again. Nil-safe, since
// serve has no tailnet side outside tsnet mode.
func (f *tsnetFront) http3Listeners() []tsnetH3Listener {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h3Servers
}

// stop is serve's teardown of its tailnet side, deferred so it runs on
// EVERY exit path, and in this order: cancel the goroutine; refuse any
// later publication and take what was published; drain that, HTTP/3 first,
// then HTTPS, so in-flight requests on either get a clean
// http.ErrServerClosed instead of a mid-flight socket / QUIC reset; wait
// for the goroutine and every HTTP/3 Serve it started; and only then close
// the node, which drains magicsock / netcheck / the control plane.
//
// The wait is BOUNDED by grace. A goroutine still running after it (a
// start stuck in the part of upstream's start that takes no context, or a
// listen) costs the grace and a line, never a hung exit; the node is then
// closed under it, and the wrapper stops a start that Close lands on,
// while tsnetListen closes a listener that lands after it.
func (f *tsnetFront) stop(grace time.Duration) {
	f.cancel()
	f.mu.Lock()
	f.stopping = true
	h3, https := f.h3Servers, f.https
	f.mu.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for _, l := range h3 {
		_ = l.srv.Shutdown(shutdownCtx)
		_ = l.conn.Close()
	}
	if https != nil {
		_ = https.Shutdown(shutdownCtx)
	}

	// NewTimer + defer Stop, NOT time.After, for the reason the bgWriters
	// join gives: runServe is re-entered by the launcher menu.
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-f.done:
	case <-timer.C:
		fmt.Fprintln(f.stderr, "shutdown: the tsnet goroutine did not stop within grace; closing its node anyway")
	}
	if err := f.node.Close(); err != nil {
		fmt.Fprintf(f.stderr, "tsnet close: %v\n", err)
	}
}
