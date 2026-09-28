package upnpproxy

// Every byte fetch the proxy makes goes to the host:port LiveHost derives
// from a server's cached control URL, and a name there resolves again at
// each dial (backlog B36). So the proxy dials under the approval that URL was
// found under, through the device transport's dial check, and a connect to
// this machine or a link-local address it does not cover is refused.
// cmd/bridge's TestARebindingNameCannotTakeTheIngestOrAByteFetchToThisMachine
// drives the rebinding itself through the production LiveHost.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// serveOnce sends one byte fetch through p, as the /v1 and DLNA handlers do.
func serveOnce(p *Proxy) *PreStreamError {
	return p.Serve(context.Background(), httptest.NewRecorder(), http.MethodGet, http.Header{},
		&manifest.UPnPRouting{ServerUDN: "uuid:x", ResURL: "/MediaItems/5.flac"})
}

// TestProxy_Serve_ReachesThisMachineOnlyForAServerApprovedThere: the stub
// upstream on 127.0.0.1 stands in for this machine (the bridge's console, in
// the attack). A server whose approval came from a packet on the LAN is
// refused before a byte is sent; one announcing from 127.0.0.1 reaches it,
// which is what shows the refusal is the check.
func TestProxy_Serve_ReachesThisMachineOnlyForAServerApprovedThere(t *testing.T) {
	upstream, recs := newStubUpstream(t, http.StatusOK, []byte("fLaC"), nil)
	for _, tc := range []struct {
		from     string
		wantCode string // "" for a fetch that reaches the upstream
		wantSeen int
	}{
		{"192.0.2.7", "upnp_upstream_unreachable", 0},
		{"127.0.0.1", "", 1},
	} {
		*recs = nil
		p := New(&stubHostResolver{host: hostPortOf(t, upstream), ok: true, approval: announcedFrom(tc.from)}, nil)
		perr := serveOnce(p)
		if code := codeOf(perr); code != tc.wantCode || len(*recs) != tc.wantSeen {
			t.Errorf("approved as announced from %s: Serve = %v, upstream saw %d requests; want code %q and %d",
				tc.from, perr, len(*recs), tc.wantCode, tc.wantSeen)
		}
	}
}

// TestProxy_Serve_NeverCarriesARequestOnAConnectionAnotherApprovalOpened pins
// the proxy's kept-alive connections off. The first fetch is approved on this
// machine and reaches it; the second, for the same host:port under an
// approval that does not cover this machine, must dial again and be refused.
// With connections kept alive it rode the first fetch's idle connection past
// the dial check.
func TestProxy_Serve_NeverCarriesARequestOnAConnectionAnotherApprovalOpened(t *testing.T) {
	upstream, recs := newStubUpstream(t, http.StatusOK, []byte("fLaC"), nil)
	resolver := onThisMachine(hostPortOf(t, upstream))
	p := New(resolver, nil)
	if perr := serveOnce(p); perr != nil {
		t.Fatalf("first fetch, approved on this machine: %v", perr)
	}
	resolver.approval = announcedFrom("192.0.2.7")
	if perr := serveOnce(p); codeOf(perr) != "upnp_upstream_unreachable" {
		t.Errorf("second fetch, approved only from the LAN: Serve = %v, want upnp_upstream_unreachable", perr)
	}
	if len(*recs) != 1 {
		t.Errorf("the upstream saw %d requests, want 1: the second reached this machine without a dial", len(*recs))
	}
}

// TestProxyClientKeepsItsStreamingSettings pins what defaultClient sets on
// the device transport: no whole-request timeout (a long track streams for
// minutes), four connections per upstream, a ten-second wait for headers, and
// none of the settings that would connect around the dial check.
func TestProxyClientKeepsItsStreamingSettings(t *testing.T) {
	c := defaultClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", c.Transport)
	}
	if c.Timeout != 0 || tr.MaxConnsPerHost != 4 || tr.ResponseHeaderTimeout != 10*time.Second {
		t.Errorf("client Timeout %v, MaxConnsPerHost %d, ResponseHeaderTimeout %v; want 0, 4, 10s",
			c.Timeout, tr.MaxConnsPerHost, tr.ResponseHeaderTimeout)
	}
	if tr.Proxy != nil || tr.DialTLSContext != nil || !tr.DisableKeepAlives {
		t.Errorf("transport has a proxy (%v), a TLS dialer (%v) or kept-alive connections (%v): "+
			"each lets a request reach a device around the dial check",
			tr.Proxy != nil, tr.DialTLSContext != nil, !tr.DisableKeepAlives)
	}
}

// codeOf is a pre-stream error's code, or "" for none.
func codeOf(perr *PreStreamError) string {
	if perr == nil {
		return ""
	}
	return perr.Code
}
