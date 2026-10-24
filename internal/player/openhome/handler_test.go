package openhome

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOnlyOpenHomePathsAreServed: the surface is /openhome/ alone: the
// device description and each service's description answer GET and HEAD
// (HEAD with no body), any other method there is 405; every path outside
// them is 404, the admin's, /v1's, DLNA's and media's included, and so is
// a path that only begins like one of them. No reply carries a CORS header
// (serve checks).
func TestOnlyOpenHomePathsAreServed(t *testing.T) {
	h := testHandler()
	for _, path := range []string{
		"/openhome/description.xml", "/openhome/product/scpd.xml", "/openhome/playlist/scpd.xml",
		"/openhome/info/scpd.xml", "/openhome/time/scpd.xml",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != `text/xml; charset="utf-8"` || rec.Body.Len() == 0 {
			t.Errorf("GET %s: %d, %q, %d bytes", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, request(http.MethodHead, path, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d with %d bytes", path, rec.Code, rec.Body.Len())
		}
		if got := serve(t, h, request(http.MethodPost, path, strings.NewReader("x"))); got.status != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %v, want 405", path, got)
		}
	}
	for _, path := range []string{
		"/", "/openhome", "/openhome/", "/openhome/product", "/openhome/product/", "/openhome/volume/control",
		"/openhome/product/control/", "/openhome/product/control/x", "/openhome/Product/control", "/openhome/description.xml/",
		"/openhome/product/description.xml", "/v1/health", "/v1/manifest", "/api/status", "/dlna/description.xml",
		"/dlna/file/0123456789abcdef", "/metrics", "/favicon.ico",
	} {
		if got := serve(t, h, request(http.MethodGet, path, nil)); got.status != http.StatusNotFound {
			t.Errorf("GET %s: %v, want 404", path, got)
		}
	}
}

// TestARequestAfterTheStopsJoinIsRefusedLikeEveryReply: the stop joins the
// handlers, not net/http's goroutine per connection, so a request that
// goroutine read just before the stop closed its connection can reach the
// handler after the join: it is 503, and carries what every reply carries,
// the EXT header among it.
func TestARequestAfterTheStopsJoinIsRefusedLikeEveryReply(t *testing.T) {
	h := testHandler()
	h.flight.close()
	if got := serve(t, h, request(http.MethodGet, "/openhome/description.xml", nil)); got.status != http.StatusServiceUnavailable {
		t.Errorf("a request after the stop's join: %v, want 503", got)
	}
}

// TestGENAIsRefusedAndNoSubscriptionIsHeld: each service's eventSubURL
// answers SUBSCRIBE (initial and renewal) and UNSUBSCRIBE 501 Not
// Implemented, with no SID or TIMEOUT, so no controller holds a
// subscription; any other method there is 405.
func TestGENAIsRefusedAndNoSubscriptionIsHeld(t *testing.T) {
	h := testHandler()
	for _, s := range services {
		path := "/openhome/" + s.path + "/event"
		for _, tc := range []struct {
			method string
			hdr    map[string]string
			want   int
		}{
			{"SUBSCRIBE", map[string]string{"CALLBACK": "<http://192.168.1.50:49152/cb>", "NT": "upnp:event", "TIMEOUT": "Second-1800"}, http.StatusNotImplemented},
			{"SUBSCRIBE", map[string]string{"SID": "uuid:0", "TIMEOUT": "Second-1800"}, http.StatusNotImplemented},
			{"UNSUBSCRIBE", map[string]string{"SID": "uuid:0"}, http.StatusNotImplemented},
			{http.MethodGet, nil, http.StatusMethodNotAllowed},
			{http.MethodPost, nil, http.StatusMethodNotAllowed},
		} {
			r := request(tc.method, path, nil)
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			body, _ := io.ReadAll(rec.Result().Body)
			if rec.Code != tc.want || rec.Header().Get("SID") != "" || rec.Header().Get("TIMEOUT") != "" {
				t.Errorf("%s %s: %d, SID %q, TIMEOUT %q, %q; want %d and no subscription", tc.method, path, rec.Code,
					rec.Header().Get("SID"), rec.Header().Get("TIMEOUT"), body, tc.want)
			}
			if _, ok := rec.Header()["EXT"]; !ok {
				t.Errorf("%s %s: no EXT header", tc.method, path)
			}
		}
	}
}
