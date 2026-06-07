// Package upnpproxy contains the HTTP byte proxy that fronts an
// upstream UPnP MediaServer's `<res>` URL with bit-exact passthrough.
//
// **Why a standalone package.** Two surfaces serve track bytes today
// and both need the proxy:
//
//  1. `internal/api`'s authed `/v1/download` path serves iOS clients;
//     UPnP-routed tracks proxy via this package from the
//     `serveFile` fast-path.
//  2. `internal/dlna`'s unauth `/dlna/file/{trackID}` path serves
//     DLNA renderers (the bridge's own MediaServer; LAN-only bind);
//     UPnP-routed tracks also need to proxy here so iOS can cast a
//     2Go track to any DLNA renderer via the bridge. Pre-this-pkg
//     `/dlna/file/{trackID}` 404'd on UPnP-routed tracks because the
//     handler only knew the filesystem resolver — exposed by the
//     post-pair-A operator verification of PR #732.
//
// Moving the proxy here avoids the `internal/dlna` → `internal/api`
// import cycle (api already depends on dlna for the file handler
// wiring); both surfaces import this package as equals.
//
// **Range / If-Range / If-Modified-Since flow through unchanged**;
// the upstream's status / Content-Type / Content-Length /
// Content-Range / Accept-Ranges flow back unchanged. This is the
// bit-exact contract — the bridge serves the 2Go's bytes verbatim.
package upnpproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// RoutingLookup is the manifest-side query for "is this manifest
// path actually a UPnP-sourced track?". Production wiring passes a
// `*manifest.Store` (which implements GetUPnPRouting); tests pass an
// in-memory stub. Returning (nil, nil) is the explicit "this is a
// filesystem track, not UPnP" signal — the proxy then doesn't engage.
type RoutingLookup interface {
	GetUPnPRouting(ctx context.Context, sourcePath string) (*manifest.UPnPRouting, error)
}

// HostResolver returns the live `host:port` for a given UPnP server
// UDN — the host part of the upstream's URL floats with DHCP, Wi-Fi /
// hotspot toggles, etc. Production wiring is the SSDP discovery
// cache. Returns ("", false) when the server isn't currently
// reachable; callers surface 503 so iOS reconciles on the next play
// tap.
type HostResolver interface {
	LiveHost(udn string) (hostport string, ok bool)
}

// PreStreamError is returned from `Proxy.Serve` when the proxy
// failed BEFORE any response headers were written — the caller is
// then expected to write its preferred error envelope shape:
//
//   - `internal/api` uses a structured JSON envelope so iOS can
//     decode typed errors;
//   - `internal/dlna` uses `http.Error` plain-text so DLNA renderers
//     get the HTTP status they expect.
//
// Mid-stream failures (the upstream hangs up after sending response
// headers) are logged internally and `Serve` returns nil — there's
// no way to relay a clean error after WriteHeader has gone out.
type PreStreamError struct {
	Status  int
	Code    string
	Message string
	Cause   error
}

func (e *PreStreamError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("upnp proxy %s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("upnp proxy %s: %s", e.Code, e.Message)
}

func (e *PreStreamError) Unwrap() error { return e.Cause }

// Proxy holds the configuration shared across consumers. Constructed
// once at startup and shared.
type Proxy struct {
	hostResolver HostResolver
	client       *http.Client
	log          *slog.Logger
}

// New constructs a Proxy with the default streaming-tuned HTTP
// client. Pass `nil` for the logger to drop mid-stream copy errors
// silently (the response status is already on the wire, so there's
// nothing user-actionable to surface).
func New(hostResolver HostResolver, log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Proxy{
		hostResolver: hostResolver,
		client:       defaultClient(),
		log:          log,
	}
}

// Serve fetches the upstream MediaServer's bytes for the given
// routing row + forwards them to `w` with bit-exact passthrough.
// `Range` / `If-Range` / `If-Modified-Since` headers from the
// request flow through unchanged; the upstream's status /
// Content-Type / Content-Length / Content-Range / Accept-Ranges
// flow back unchanged.
//
// Returns `nil` when the response is already on the wire (success
// OR a mid-stream failure that was logged) — the caller MUST NOT
// touch `w` further. Returns a non-nil `*PreStreamError` when the
// failure happened before any headers were written; the caller
// writes its preferred error envelope.
//
// `ctx` carries client-side cancellation so a scrubber drag / app
// background tears the upstream connection down too.
func (p *Proxy) Serve(ctx context.Context, w http.ResponseWriter, method string, header http.Header, rt *manifest.UPnPRouting) *PreStreamError {
	if rt == nil {
		return &PreStreamError{
			Status:  http.StatusInternalServerError,
			Code:    "internal",
			Message: "the bridge tried to proxy a nil routing row",
		}
	}
	hostport, ok := p.hostResolver.LiveHost(rt.ServerUDN)
	if !ok {
		return &PreStreamError{
			Status:  http.StatusServiceUnavailable,
			Code:    "upnp_server_offline",
			Message: "the upstream UPnP server isn't currently reachable; try again in a moment",
		}
	}
	target, err := buildProxyURL(hostport, rt.ResURL)
	if err != nil {
		return &PreStreamError{
			Status:  http.StatusBadGateway,
			Code:    "upnp_bad_res_url",
			Message: "the upstream's <res> URL couldn't be assembled",
			Cause:   err,
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return &PreStreamError{
			Status:  http.StatusInternalServerError,
			Code:    "internal",
			Message: "the bridge couldn't build the proxy request",
			Cause:   err,
		}
	}
	forwardRequestHeaders(req, header)

	resp, err := p.client.Do(req)
	if err != nil {
		return &PreStreamError{
			Status:  http.StatusBadGateway,
			Code:    "upnp_upstream_unreachable",
			Message: "the bridge couldn't reach the upstream UPnP server",
			Cause:   err,
		}
	}
	defer resp.Body.Close()

	// From here on every error must be communicated via the response
	// stream itself — we've already committed to writing a response.
	relayResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	if method == http.MethodHead {
		return nil
	}
	p.copyResponseBody(w, resp, rt)
	return nil
}

// forwardRequestHeaders copies the Range / If-Range / If-Modified-Since
// headers from the inbound request to the upstream proxy request. These
// drive 206 / 304 semantics on the upstream side — the bit-exact
// contract requires they flow through unchanged. Also tags the outbound
// User-Agent so a remote operator inspecting the upstream's access log
// can tell the request came from the bridge.
//
// Extracted from `Proxy.Serve` to drop its cognitive complexity (Sonar
// S3776 on PR #356). Behavior unchanged.
func forwardRequestHeaders(req *http.Request, src http.Header) {
	for _, h := range []string{"Range", "If-Range", "If-Modified-Since"} {
		if v := src.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("User-Agent", "1-bit-bridge/upnp-proxy")
}

// relayResponseHeaders copies the upstream's response headers onto the
// caller's ResponseWriter, dropping hop-by-hop entries (RFC 7230 §6.1).
// The bit-exact contract requires Content-Type / Content-Length /
// Content-Range / Accept-Ranges flow back unchanged — this loop is the
// chokepoint that enforces it.
//
// Extracted from `Proxy.Serve` to drop its cognitive complexity (Sonar
// S3776 on PR #356). Behavior unchanged.
func relayResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// copyResponseBody streams the upstream's body to the caller via
// io.Copy + logs (but does not propagate) mid-stream copy errors —
// they're typically client-side disconnects (iOS scrubber drag, app
// close) and `Serve` has no way to surface a clean error after
// WriteHeader has gone out.
//
// Extracted from `Proxy.Serve` so the error-handling depth doesn't
// inflate the parent function's cognitive complexity (Sonar S3776 on
// PR #356). Behavior unchanged.
func (p *Proxy) copyResponseBody(w http.ResponseWriter, resp *http.Response, rt *manifest.UPnPRouting) {
	if _, copyErr := io.Copy(w, resp.Body); copyErr != nil {
		if !errors.Is(copyErr, context.Canceled) {
			p.log.Debug("upnp proxy: mid-stream copy error",
				"err", copyErr, "udn", rt.ServerUDN, "objectID", rt.ObjectID)
		}
	}
}

// defaultClient returns the streaming-tuned HTTP client used for
// upstream requests. No body-read timeout (a long album track takes
// minutes); aggressive idle-conn timeout so a stale upstream
// connection doesn't block the pool. Connection limit matches
// MiniDLNA's small libmicrohttpd pool — overloading the upstream is
// documented to cause socket timeouts in real DLNA traffic.
func defaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Explicit DialContext with a 5 s connect timeout. Without
			// it the zero-value net.Dialer waits forever on a
			// firewalled or offline upstream, leaking goroutines per
			// request while the upstream is asleep (Gemini on PR #352).
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   4,
			MaxConnsPerHost:       4, // matches MiniDLNA's typical concurrent-stream ceiling
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
}

// buildProxyURL combines the live host:port (from the SSDP registry)
// with the stored `<res>` URL's path + query. The host:port floats;
// the path suffix stays stable until the upstream's library is
// rebuilt. Accepts either a stored full URL ("http://oldhost:8200/MediaItems/5.flac")
// or a path-only fragment ("/MediaItems/5.flac") to remain robust
// against future ingest changes.
func buildProxyURL(liveHostPort, storedRes string) (string, error) {
	if liveHostPort == "" {
		return "", errors.New("upnp proxy: empty live hostport")
	}
	if storedRes == "" {
		return "", errors.New("upnp proxy: empty stored res URL")
	}
	if strings.HasPrefix(storedRes, "/") {
		// Path-only stored value.
		return "http://" + liveHostPort + storedRes, nil
	}
	u, err := url.Parse(storedRes)
	if err != nil {
		return "", fmt.Errorf("upnp proxy: parse stored res: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		// Bare relative path without leading slash — treat as absolute.
		return "http://" + liveHostPort + "/" + storedRes, nil
	}
	// Strip the stored host:port (which has likely floated); reconstruct
	// from the live host. Path + RawQuery preserve everything downstream.
	u.Scheme = "http"
	u.Host = liveHostPort
	return u.String(), nil
}

// isHopByHopHeader matches the canonical hop-by-hop header names per
// RFC 7230 §6.1 plus the WebSocket trio. We don't relay any of them
// — the upstream's per-connection state isn't the caller's concern.
func isHopByHopHeader(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
