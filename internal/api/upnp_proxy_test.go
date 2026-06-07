package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Pure helper tests (TestBuildProxyURL / TestIsHopByHopHeader) moved
// to `internal/upnpproxy` alongside the extracted helpers (PR-pending).
// The end-to-end integration tests below exercise the api Server's
// `proxyUPnP` shim — which now delegates to `*upnpproxy.Proxy.Serve`
// and translates `*PreStreamError` into the api's JSON envelope.

// --- end-to-end proxy tests using a stub upstream + the Server's hook ---

type stubRoutingLookup struct {
	m map[string]*manifest.UPnPRouting
}

func (s *stubRoutingLookup) GetUPnPRouting(_ context.Context, p string) (*manifest.UPnPRouting, error) {
	return s.m[p], nil
}

type stubHostResolver struct {
	host string
	ok   bool
}

func (s *stubHostResolver) LiveHost(_ string) (string, bool) { return s.host, s.ok }

// upstreamRecord captures one upstream request the proxy made.
type upstreamRecord struct {
	method  string
	url     string
	headers http.Header
}

// newStubUpstream returns an httptest server that records each request
// and replies with the given (status, body, headers).
func newStubUpstream(t *testing.T, status int, body []byte, headers map[string]string) (*httptest.Server, *[]upstreamRecord) {
	t.Helper()
	var records []upstreamRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records = append(records, upstreamRecord{
			method:  r.Method,
			url:     r.URL.String(),
			headers: r.Header.Clone(),
		})
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &records
}

// serverWithProxy builds a minimal *Server with the proxy wired but the
// rest stubbed/nil. Direct serveFile invocation skips routing through
// the full mux.
func serverWithProxy(t *testing.T, routing *stubRoutingLookup, host *stubHostResolver) *Server {
	t.Helper()
	s := &Server{
		upnpRouting:      routing,
		upnpHostResolver: host,
	}
	// The shim's proxy cache is normally populated by `WithUPnPHostResolver`
	// in production; tests construct the Server directly so we wire the
	// cache by hand here. Same effect, one fewer t.Helper() round-trip.
	s.refreshUPnPProxy()
	return s
}

// proxyFixture is the shared scaffold every TestProxyUPnP_* test uses:
// stub upstream + routing row + server-with-proxy + a default request.
// Extracted so each test stays focused on its own assertions instead of
// repeating the 6-line setup boilerplate (4.9% → 3.1% → ≤3% SonarCloud
// duplication on PR #352).
type proxyFixture struct {
	server   *Server
	upstream *httptest.Server
	records  *[]upstreamRecord
	routing  *manifest.UPnPRouting
}

func newProxyFixture(t *testing.T, status int, body []byte, headers map[string]string) *proxyFixture {
	t.Helper()
	upstream, records := newStubUpstream(t, status, body, headers)
	uURL, _ := url.Parse(upstream.URL)
	rt := &manifest.UPnPRouting{
		SourcePath: "p", ServerUDN: "uuid:test", ObjectID: "x",
		ResURL: "http://stored-host:8200/MediaItems/1.flac",
	}
	s := serverWithProxy(t,
		&stubRoutingLookup{m: map[string]*manifest.UPnPRouting{"p": rt}},
		&stubHostResolver{host: uURL.Host, ok: true})
	return &proxyFixture{server: s, upstream: upstream, records: records, routing: rt}
}

func TestProxyUPnP_PassesThroughBitExact(t *testing.T) {
	body := []byte{0x66, 0x4c, 0x61, 0x43, 0xDE, 0xAD, 0xBE, 0xEF} // "fLaC" + raw
	f := newProxyFixture(t, http.StatusOK, body, map[string]string{
		"Content-Type":   "audio/x-flac",
		"Content-Length": "8",
		"Accept-Ranges":  "bytes",
	})
	w := httptest.NewRecorder()
	f.server.proxyUPnP(w, httptest.NewRequest(http.MethodGet, "/x", nil), f.routing)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/x-flac" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := w.Body.Bytes(); string(got) != string(body) {
		t.Errorf("body mismatch:\n got=% x\nwant=% x", got, body)
	}
	if len(*f.records) < 1 {
		t.Fatalf("upstream never called")
	}
	// The proxy rebuilt host:port from the live resolver, not the stored URL.
	if !strings.HasSuffix((*f.records)[len(*f.records)-1].url, "/MediaItems/1.flac") {
		t.Errorf("upstream URL = %q", (*f.records)[len(*f.records)-1].url)
	}
}

func TestProxyUPnP_ForwardsRangeHeader(t *testing.T) {
	f := newProxyFixture(t, http.StatusPartialContent, []byte("partial"),
		map[string]string{
			"Content-Type":  "audio/x-flac",
			"Content-Range": "bytes 0-6/8",
			"Accept-Ranges": "bytes",
		})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=0-6")
	w := httptest.NewRecorder()
	f.server.proxyUPnP(w, req, f.routing)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d; want 206", w.Code)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 0-6/8" {
		t.Errorf("Content-Range = %q", cr)
	}
	if got := (*f.records)[len(*f.records)-1].headers.Get("Range"); got != "bytes=0-6" {
		t.Errorf("upstream Range = %q; want %q", got, "bytes=0-6")
	}
}

func TestProxyUPnP_HEADReturnsHeadersOnly(t *testing.T) {
	f := newProxyFixture(t, http.StatusOK, []byte("payload"), map[string]string{
		"Content-Type":   "audio/x-flac",
		"Content-Length": "7",
		"Accept-Ranges":  "bytes",
	})
	w := httptest.NewRecorder()
	f.server.proxyUPnP(w, httptest.NewRequest(http.MethodHead, "/x", nil), f.routing)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD must not return a body; got %d bytes", w.Body.Len())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/x-flac" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestProxyUPnP_HostResolverMissReturns503(t *testing.T) {
	rt := &manifest.UPnPRouting{
		SourcePath: "p", ServerUDN: "uuid:offline", ObjectID: "x",
		ResURL: "http://upstream/x.flac",
	}
	s := serverWithProxy(t,
		&stubRoutingLookup{m: map[string]*manifest.UPnPRouting{"p": rt}},
		&stubHostResolver{ok: false})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	s.proxyUPnP(w, req, rt)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", w.Code)
	}
}

func TestProxyUPnP_UpstreamErrorReturns502(t *testing.T) {
	rt := &manifest.UPnPRouting{
		SourcePath: "p", ServerUDN: "uuid:test", ObjectID: "x",
		ResURL: "http://upstream/x.flac",
	}
	// "host:0" is unreachable — any TCP connection to it fails immediately.
	s := serverWithProxy(t,
		&stubRoutingLookup{m: map[string]*manifest.UPnPRouting{"p": rt}},
		&stubHostResolver{host: "127.0.0.1:0", ok: true})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	s.proxyUPnP(w, req, rt)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", w.Code)
	}
}

func TestProxyUPnP_StripsHopByHopHeaders(t *testing.T) {
	f := newProxyFixture(t, http.StatusOK, []byte("ok"), map[string]string{
		"Content-Type":      "audio/x-flac",
		"Connection":        "keep-alive", // hop-by-hop, must NOT relay
		"Transfer-Encoding": "chunked",    // hop-by-hop, must NOT relay
	})
	w := httptest.NewRecorder()
	f.server.proxyUPnP(w, httptest.NewRequest(http.MethodGet, "/x", nil), f.routing)

	if v := w.Header().Get("Connection"); v != "" {
		t.Errorf("Connection should be stripped; got %q", v)
	}
	if v := w.Header().Get("Transfer-Encoding"); v != "" {
		t.Errorf("Transfer-Encoding should be stripped; got %q", v)
	}
	if v := w.Header().Get("Content-Type"); v != "audio/x-flac" {
		t.Errorf("Content-Type should pass through; got %q", v)
	}
}

func TestUpnpProxyEnabled(t *testing.T) {
	cases := []struct {
		name    string
		routing UPnPRoutingLookup
		host    UPnPServerHostResolver
		want    bool
	}{
		{"both wired", &stubRoutingLookup{}, &stubHostResolver{}, true},
		{"only routing", &stubRoutingLookup{}, nil, false},
		{"only host", nil, &stubHostResolver{}, false},
		{"neither", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{upnpRouting: c.routing, upnpHostResolver: c.host}
			if got := s.upnpProxyEnabled(); got != c.want {
				t.Errorf("got %v; want %v", got, c.want)
			}
		})
	}
}

// silence "imported but unused" if a future test goes through a different path.
var _ = io.Copy
