package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// UPnPRoutingLookup is the manifest-side query for "is this manifest
// path actually a UPnP-sourced track?". Production wiring passes a
// `*manifest.Store` (which implements GetUPnPRouting); tests pass an
// in-memory stub. Returning (nil, nil) is the explicit "this is a
// filesystem track, not UPnP" signal — the proxy then doesn't engage.
type UPnPRoutingLookup interface {
	GetUPnPRouting(ctx context.Context, sourcePath string) (*manifest.UPnPRouting, error)
}

// UPnPServerHostResolver returns the live `host:port` for a given UPnP
// server UDN — the host part of the upstream's URL floats with DHCP,
// Wi-Fi/hotspot toggles, etc. Production wiring is the SSDP discovery
// cache. Returns (empty, false) when the server isn't currently
// reachable — the proxy then surfaces 503 to iOS so the next play tap
// reconciles cleanly.
type UPnPServerHostResolver interface {
	LiveHost(udn string) (hostport string, ok bool)
}

// WithUPnPRouting wires the manifest-side routing lookup. Passing nil
// disables the UPnP proxy entirely (serveFile then routes every request
// through the filesystem resolver as before).
func (s *Server) WithUPnPRouting(l UPnPRoutingLookup) *Server {
	s.upnpRouting = l
	return s
}

// WithUPnPHostResolver wires the live-host lookup. Required alongside
// WithUPnPRouting for the proxy to engage; the routing-only setup
// surfaces 503 ("server not currently reachable") for every UPnP track.
func (s *Server) WithUPnPHostResolver(r UPnPServerHostResolver) *Server {
	s.upnpHostResolver = r
	return s
}

// WithUPnPUpstreamPublicProvider wires the read-only "public" view of
// the upstream-MediaServer feature for advertisement on `/v1/health`.
// Independent of WithUPnPRouting / WithUPnPHostResolver: the proxy
// wiring is OPTIONAL on the advertisement path (a bridge could
// theoretically advertise upstreams without proxying their bytes —
// though in practice cmd/bridge wires all three together).
//
// Passing nil omits the `upnpUpstreamServers` field from the wire shape
// (so pre-feature behaviour is preserved on disabled deploys). The
// provider is consulted on every /v1/health call — implementations
// should keep the work proportional to the configured-server count.
func (s *Server) WithUPnPUpstreamPublicProvider(p UPnPUpstreamPublicProvider) *Server {
	s.upnpPublicProvider = p
	return s
}

// upnpProxyEnabled returns true when both halves are wired. The
// serveFile fast-path checks this before any database hit, so a deploy
// that hasn't opted into the upstream-MediaServer feature pays nothing.
func (s *Server) upnpProxyEnabled() bool {
	return s.upnpRouting != nil && s.upnpHostResolver != nil
}

// proxyUPnP fetches the upstream MediaServer's bytes for the given
// routing row + forwards them to w with bit-exact passthrough. Range /
// If-Range / If-Modified-Since headers from the iOS request flow
// through unchanged; the upstream's status / Content-Type /
// Content-Length / Content-Range / Accept-Ranges flow back unchanged.
// On any pre-stream failure we surface a structured error envelope
// matching the rest of the API; mid-stream failures (the upstream
// hangs up after sending headers) are logged but cannot be relayed —
// io.Copy stops, the client sees a truncated body.
func (s *Server) proxyUPnP(w http.ResponseWriter, r *http.Request, rt *manifest.UPnPRouting) {
	hostport, ok := s.upnpHostResolver.LiveHost(rt.ServerUDN)
	if !ok {
		writeError(w, http.StatusServiceUnavailable,
			"upnp_server_offline",
			"the upstream UPnP server isn't currently reachable; try again in a moment")
		return
	}
	target, err := buildProxyURL(hostport, rt.ResURL)
	if err != nil {
		writeErrorLog(w, r, http.StatusBadGateway, "upnp_bad_res_url",
			"the upstream's <res> URL couldn't be assembled", err)
		return
	}

	// Build the upstream request. Use the iOS request's context so
	// client-side cancellation (scrubber drag, app backgrounded) tears
	// the upstream connection down too.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't build the proxy request", err)
		return
	}
	// Forward the iOS Range / If-Range / If-Modified-Since headers
	// verbatim — these drive 206 / 304 semantics on the upstream side.
	for _, h := range []string{"Range", "If-Range", "If-Modified-Since"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	// User-Agent tags the bridge so a remote operator inspecting the
	// 2Go's access log can tell the request came from us.
	req.Header.Set("User-Agent", "1-bit-bridge/upnp-proxy")

	client := upnpProxyClient()
	resp, err := client.Do(req)
	if err != nil {
		writeErrorLog(w, r, http.StatusBadGateway, "upnp_upstream_unreachable",
			"the bridge couldn't reach the upstream UPnP server", err)
		return
	}
	defer resp.Body.Close()

	// Bit-exact passthrough: copy the upstream's response headers, then
	// status, then body via io.Copy. ResponseWriter's Content-Length /
	// chunked handling is preserved by writing headers explicitly +
	// using io.Copy (which doesn't buffer beyond standard chunking).
	for k, vs := range resp.Header {
		// Strip hop-by-hop headers per RFC 7230 §6.1. The upstream is
		// HTTP/1.x with no per-connection metadata that iOS should see.
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if _, copyErr := io.Copy(w, resp.Body); copyErr != nil {
		// Mid-stream copy errors are typically client-side disconnects
		// (iOS scrubber drag, app close). Log at debug; the response
		// status is already on the wire so there's nothing to surface.
		if !errors.Is(copyErr, context.Canceled) {
			httpLogger.Debug("upnp proxy: mid-stream copy error",
				"err", copyErr, "udn", rt.ServerUDN, "objectID", rt.ObjectID)
		}
	}
}

// upnpProxyClient returns the shared HTTP client used for upstream
// requests. Tuned for sustained streaming: no body-read timeout (a long
// album track takes minutes); aggressive idle-conn timeout so a stale
// upstream connection doesn't block the pool. The connection limit
// matches MiniDLNA's small libmicrohttpd pool — overloading the
// upstream is documented to cause socket timeouts in real DLNA traffic.
var upnpProxyClientShared = &http.Client{
	Transport: &http.Transport{
		// Explicit DialContext with a 5 s connect timeout — without
		// it the zero-value net.Dialer waits forever on a firewalled
		// or offline upstream, which would leak goroutines for every
		// request that lands while the 2Go is asleep. Per Gemini on PR #352.
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

func upnpProxyClient() *http.Client { return upnpProxyClientShared }

// buildProxyURL combines the live host:port (from the SSDP registry)
// with the stored <res> URL's path + query. The host:port floats; the
// path suffix stays stable until the upstream's library is rebuilt.
// We accept either a stored full URL ("http://oldhost:8200/MediaItems/5.flac")
// or a path-only fragment ("/MediaItems/5.flac") to remain robust against
// future ingest changes.
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
// RFC 7230 §6.1 plus the WebSocket trio. We don't relay any of them —
// the upstream's per-connection state isn't iOS's concern.
func isHopByHopHeader(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
