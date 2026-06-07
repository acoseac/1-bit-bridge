package upnpproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// --- pure helper tests (no upstream) ---

func TestBuildProxyURL(t *testing.T) {
	cases := []struct {
		name, host, res, want string
		wantErr               bool
	}{
		{"full URL stored; host swaps in", "192.168.0.62:8200",
			"http://oldhost:8200/MediaItems/5.flac",
			"http://192.168.0.62:8200/MediaItems/5.flac", false},
		{"path-only stored", "192.168.0.62:8200",
			"/MediaItems/5.flac",
			"http://192.168.0.62:8200/MediaItems/5.flac", false},
		{"bare relative (no leading slash) → treated as absolute", "192.168.0.62:8200",
			"MediaItems/5.flac",
			"http://192.168.0.62:8200/MediaItems/5.flac", false},
		{"empty hostport", "", "/x.flac", "", true},
		{"empty res", "h:8200", "", "", true},
		{"query preserved from full URL", "192.168.0.62:8200",
			"http://oldhost:8200/get?id=5&fmt=flac",
			"http://192.168.0.62:8200/get?id=5&fmt=flac", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildProxyURL(c.host, c.res)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v; wantErr=%v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("got %q; want %q", got, c.want)
			}
		})
	}
}

func TestIsHopByHopHeader(t *testing.T) {
	hopByHop := []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"TE", "Trailer", "Transfer-Encoding", "Upgrade",
	}
	for _, h := range hopByHop {
		if !isHopByHopHeader(h) {
			t.Errorf("%q should be hop-by-hop", h)
		}
	}
	endToEnd := []string{
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Range", "If-Range", "X-Custom",
	}
	for _, h := range endToEnd {
		if isHopByHopHeader(h) {
			t.Errorf("%q should NOT be hop-by-hop", h)
		}
	}
}

// --- Serve() end-to-end tests using a stub upstream ---

type stubHostResolver struct {
	host string
	ok   bool
}

func (s *stubHostResolver) LiveHost(_ string) (string, bool) { return s.host, s.ok }

// newStubUpstream returns an httptest server that records each request
// and replies with the given (status, body, headers).
func newStubUpstream(t *testing.T, status int, body []byte, headers map[string]string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var records []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records = append(records, r.Clone(context.Background()))
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &records
}

// hostPortOf extracts host:port from an httptest.Server's URL.
func hostPortOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	const prefix = "http://"
	if !strings.HasPrefix(srv.URL, prefix) {
		t.Fatalf("unexpected stub URL %q", srv.URL)
	}
	return srv.URL[len(prefix):]
}

func TestProxy_Serve_HappyPath(t *testing.T) {
	body := []byte("fLaC")
	upstream, recs := newStubUpstream(t, 200, body, map[string]string{
		"Content-Type":  "audio/x-flac",
		"Accept-Ranges": "bytes",
	})
	p := New(&stubHostResolver{host: hostPortOf(t, upstream), ok: true}, nil)

	rt := &manifest.UPnPRouting{ServerUDN: "uuid:x", ResURL: "/MediaItems/5.flac"}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)

	if perr := p.Serve(context.Background(), rec, r.Method, r.Header, rt); perr != nil {
		t.Fatalf("unexpected PreStreamError: %v", perr)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "fLaC" {
		t.Errorf("body = %q; want fLaC", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/x-flac" {
		t.Errorf("Content-Type = %q; want audio/x-flac", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q; want bytes", got)
	}
	if len(*recs) != 1 {
		t.Fatalf("upstream got %d requests; want 1", len(*recs))
	}
	if (*recs)[0].URL.Path != "/MediaItems/5.flac" {
		t.Errorf("upstream path = %q; want /MediaItems/5.flac", (*recs)[0].URL.Path)
	}
}

func TestProxy_Serve_PassesRangeHeader(t *testing.T) {
	upstream, recs := newStubUpstream(t, 206, []byte("partial"), map[string]string{
		"Content-Range": "bytes 0-6/100",
	})
	p := New(&stubHostResolver{host: hostPortOf(t, upstream), ok: true}, nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)
	r.Header.Set("Range", "bytes=0-6")
	r.Header.Set("If-Range", `"abc"`)

	if perr := p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: "/x.flac"}); perr != nil {
		t.Fatalf("unexpected PreStreamError: %v", perr)
	}
	if rec.Code != 206 {
		t.Errorf("status = %d; want 206", rec.Code)
	}
	if got := (*recs)[0].Header.Get("Range"); got != "bytes=0-6" {
		t.Errorf("upstream Range = %q; want bytes=0-6", got)
	}
	if got := (*recs)[0].Header.Get("If-Range"); got != `"abc"` {
		t.Errorf("upstream If-Range = %q; want %q", got, `"abc"`)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-6/100" {
		t.Errorf("Content-Range = %q; want bytes 0-6/100", got)
	}
}

func TestProxy_Serve_StripsHopByHop(t *testing.T) {
	upstream, _ := newStubUpstream(t, 200, []byte("x"), map[string]string{
		"Connection":        "keep-alive",
		"Transfer-Encoding": "chunked",
		"Content-Type":      "audio/x-flac",
	})
	p := New(&stubHostResolver{host: hostPortOf(t, upstream), ok: true}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)

	_ = p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: "/x.flac"})

	if v := rec.Header().Get("Connection"); v != "" {
		t.Errorf("Connection should be stripped, got %q", v)
	}
	if v := rec.Header().Get("Transfer-Encoding"); v != "" {
		t.Errorf("Transfer-Encoding should be stripped, got %q", v)
	}
	if v := rec.Header().Get("Content-Type"); v != "audio/x-flac" {
		t.Errorf("end-to-end Content-Type should be preserved, got %q", v)
	}
}

func TestProxy_Serve_HEADSkipsBody(t *testing.T) {
	upstream, _ := newStubUpstream(t, 200, []byte("fLaC body"), map[string]string{
		"Content-Type": "audio/x-flac",
	})
	p := New(&stubHostResolver{host: hostPortOf(t, upstream), ok: true}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/dlna/file/abc", nil)

	if perr := p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: "/x.flac"}); perr != nil {
		t.Fatalf("unexpected PreStreamError: %v", perr)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response should have empty body, got %d bytes", rec.Body.Len())
	}
}

func TestProxy_Serve_HostUnreachableReturnsServiceUnavailable(t *testing.T) {
	p := New(&stubHostResolver{ok: false}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)

	perr := p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: "/x.flac"})
	if perr == nil {
		t.Fatal("expected PreStreamError; got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", perr.Status)
	}
	if perr.Code != "upnp_server_offline" {
		t.Errorf("code = %q; want upnp_server_offline", perr.Code)
	}
	if rec.Code != 200 {
		// 200 = the default httptest.Recorder code when WriteHeader isn't called
		t.Errorf("ResponseWriter should not have been touched; got status %d", rec.Code)
	}
}

func TestProxy_Serve_BadResURL_ReturnsBadGateway(t *testing.T) {
	p := New(&stubHostResolver{host: "h:8200", ok: true}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)

	perr := p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: ""})
	if perr == nil {
		t.Fatal("expected PreStreamError; got nil")
	}
	if perr.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", perr.Status)
	}
	if perr.Code != "upnp_bad_res_url" {
		t.Errorf("code = %q; want upnp_bad_res_url", perr.Code)
	}
}

func TestProxy_Serve_UpstreamUnreachable_ReturnsBadGateway(t *testing.T) {
	// Point at a port nothing's listening on.
	p := New(&stubHostResolver{host: "127.0.0.1:1", ok: true}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)

	perr := p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: "/x.flac"})
	if perr == nil {
		t.Fatal("expected PreStreamError; got nil")
	}
	if perr.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (got %d)", http.StatusBadGateway, perr.Status)
	}
	if perr.Code != "upnp_upstream_unreachable" {
		t.Errorf("code = %q; want upnp_upstream_unreachable", perr.Code)
	}
}

func TestProxy_Serve_NilRoutingRow_ReturnsInternalError(t *testing.T) {
	p := New(&stubHostResolver{host: "h:8200", ok: true}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)

	perr := p.Serve(context.Background(), rec, r.Method, r.Header, nil)
	if perr == nil {
		t.Fatal("expected PreStreamError; got nil")
	}
	if perr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", perr.Status)
	}
}

func TestProxy_Serve_ForwardsUserAgentTag(t *testing.T) {
	upstream, recs := newStubUpstream(t, 200, []byte("x"), nil)
	p := New(&stubHostResolver{host: hostPortOf(t, upstream), ok: true}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dlna/file/abc", nil)
	r.Header.Set("User-Agent", "iOS/1.0") // caller's UA — proxy overrides

	_ = p.Serve(context.Background(), rec, r.Method, r.Header,
		&manifest.UPnPRouting{ServerUDN: "u", ResURL: "/x.flac"})

	if got := (*recs)[0].Header.Get("User-Agent"); got != "1-bit-bridge/upnp-proxy" {
		t.Errorf("upstream User-Agent = %q; want 1-bit-bridge/upnp-proxy", got)
	}
}

// Sanity: the PreStreamError.Error() shape exposes the underlying
// cause so log lines carry the chain.
func TestPreStreamError_ErrorString(t *testing.T) {
	e := &PreStreamError{Status: 502, Code: "upnp_bad_res_url", Message: "bad url"}
	if got := e.Error(); !strings.Contains(got, "upnp_bad_res_url") || !strings.Contains(got, "bad url") {
		t.Errorf("Error() = %q; missing code/message", got)
	}
	e.Cause = io.EOF
	if got := e.Error(); !strings.Contains(got, "EOF") {
		t.Errorf("Error() = %q; missing cause", got)
	}
	if e.Unwrap() != io.EOF {
		t.Errorf("Unwrap() != io.EOF")
	}
}
