package api

import (
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
)

// UPnPRoutingLookup is the manifest-side query for "is this manifest
// path actually a UPnP-sourced track?". Production wiring passes a
// `*manifest.Store` (which implements GetUPnPRouting); tests pass an
// in-memory stub. Returning (nil, nil) is the explicit "this is a
// filesystem track, not UPnP" signal — the proxy then doesn't engage.
//
// Type alias to `upnpproxy.RoutingLookup` — same interface, different
// name preserved for backward compatibility with the existing api
// wiring (`Server.upnpRouting` field, `WithUPnPRouting` setter, and
// the `serveFile` fast-path consumer).
type UPnPRoutingLookup = upnpproxy.RoutingLookup

// UPnPServerHostResolver returns the live `host:port` for a given UPnP
// server UDN — the host part of the upstream's URL floats with DHCP,
// Wi-Fi/hotspot toggles, etc. Production wiring is the SSDP discovery
// cache. Returns (empty, false) when the server isn't currently
// reachable — the proxy then surfaces 503 to iOS so the next play tap
// reconciles cleanly.
//
// Type alias to `upnpproxy.HostResolver` — see UPnPRoutingLookup above.
type UPnPServerHostResolver = upnpproxy.HostResolver

// WithUPnPRouting wires the manifest-side routing lookup. Passing nil
// disables the UPnP proxy entirely (serveFile then routes every request
// through the filesystem resolver as before).
func (s *Server) WithUPnPRouting(l UPnPRoutingLookup) *Server {
	s.upnpRouting = l
	s.refreshUPnPProxy()
	return s
}

// WithUPnPHostResolver wires the live-host lookup. Required alongside
// WithUPnPRouting for the proxy to engage; the routing-only setup
// surfaces 503 ("server not currently reachable") for every UPnP track.
func (s *Server) WithUPnPHostResolver(r UPnPServerHostResolver) *Server {
	s.upnpHostResolver = r
	s.refreshUPnPProxy()
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

// refreshUPnPProxy rebuilds the cached `*upnpproxy.Proxy` from the
// currently-wired host resolver. Called from the With* setters so the
// proxy is ready by the time `serveFile` looks at it. Storing the
// proxy avoids re-allocating the HTTP client (with its connection
// pool) per request.
func (s *Server) refreshUPnPProxy() {
	if s.upnpHostResolver == nil {
		s.upnpProxy = nil
		return
	}
	s.upnpProxy = upnpproxy.New(s.upnpHostResolver, httpLogger)
}

// proxyUPnP is a thin shim that delegates to `*upnpproxy.Proxy.Serve`
// and translates the returned `*upnpproxy.PreStreamError` into the
// api's structured JSON error envelope. Mid-stream failures (return
// `nil`) are already on the wire and need no further handling.
//
// The proxy logic itself lives in `internal/upnpproxy` so that
// `internal/dlna/file_handler.go` can use it too (the DLNA cast path
// from PR-pending — pre-fix, the DLNA file handler returned 404 for
// UPnP-routed tracks because it only knew the filesystem resolver).
func (s *Server) proxyUPnP(w http.ResponseWriter, r *http.Request, rt *manifest.UPnPRouting) {
	if s.upnpProxy == nil {
		// Defensive: serveFile's `upnpProxyEnabled()` check should
		// prevent this branch, but a future code path that bypasses
		// it shouldn't crash on the nil pointer.
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge UPnP proxy isn't wired", nil)
		return
	}
	perr := s.upnpProxy.Serve(r.Context(), w, r.Method, r.Header, rt)
	if perr == nil {
		return
	}
	if perr.Cause != nil {
		writeErrorLog(w, r, perr.Status, perr.Code, perr.Message, perr.Cause)
		return
	}
	writeError(w, perr.Status, perr.Code, perr.Message)
}
