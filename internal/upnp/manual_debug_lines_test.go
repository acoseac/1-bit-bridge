package upnp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestManualPollerDebugLinesNameTheHostAlone: the poller's two Debug lines,
// "description fetch failed" and "carries no ContentDirectory service",
// named the manual URL whole (backlog B66), which is the operator's and can
// carry a user name and password, a token written as the user name, or a
// token in the query or the fragment. The bridge prints no Debug line today
// (logging.Init fixes the level at Info), so it bit only the day one is
// added; they name the server and the URL's host now, as the poller's
// warnings do. The fetch's error rides the first line too, and it names the
// URL twice: discovery's own wrapping quotes it as written (`GET <url>: …`),
// and net/http's *url.Error quotes the request URL as the client
// re-serialized it, masking a password and nothing else. So every way the
// fetch can fail is driven, over the real client, against both shapes of
// URL, and every line at every level is searched without regard to case.
func TestManualPollerDebugLinesNameTheHostAlone(t *testing.T) {
	const secret = "s3cret-Pw"
	const (
		fetchFailed = "UPnP manual server: description fetch failed"
		noCDS       = "UPnP manual server: description carries no ContentDirectory service"
	)
	noCDSDesc := `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
<friendlyName>No CDS</friendlyName><UDN>uuid:no-cds</UDN>
<serviceList><service>
<serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>
<serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId>
<controlURL>/cm</controlURL></service></serviceList>
</device></root>`

	// hang holds a request until the test ends: the fetch gives up first.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })

	for _, mode := range []struct {
		name, line string
		handler    http.HandlerFunc // nil: a port nothing listens on
	}{
		{"a refused connection", fetchFailed, nil},
		{"a 404", fetchFailed, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }},
		{"a body that is not a description", fetchFailed, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "not a device description")
		}},
		{"no answer before the timeout", fetchFailed, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}},
		{"no ContentDirectory", noCDS, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, noCDSDesc) }},
	} {
		for _, shape := range []struct{ name, userinfo, rest string }{
			{"a password and a query", "user:" + secret + "@", "/rootDesc.xml?token=" + secret},
			{"a token as the user name and a fragment", secret + "@", "/rootDesc.xml#" + secret},
		} {
			t.Run(mode.name+", "+shape.name, func(t *testing.T) {
				var host string
				if mode.handler == nil {
					l, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					host = l.Addr().String()
					l.Close()
				} else {
					srv := httptest.NewServer(mode.handler)
					t.Cleanup(srv.Close)
					host = strings.TrimPrefix(srv.URL, "http://")
				}
				var buf bytes.Buffer
				p := NewManualPoller(ManualPollerConfig{
					Cache: NewServerCache(),
					Servers: func() []ManualServer {
						return []ManualServer{{Key: "manual:probe", Name: "Probe",
							DescriptionURL: "http://" + shape.userinfo + host + shape.rest}}
					},
					Timeout: 300 * time.Millisecond,
					Logger:  slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
				})
				p.PollOnce(context.Background())

				out := buf.String()
				lines := linesContaining(out, mode.line)
				if len(lines) != 1 {
					t.Fatalf("%d %q lines, want 1: the run did not reach the line it drives:\n%s", len(lines), mode.line, out)
				}
				if !strings.Contains(lines[0], "server=Probe") || !strings.Contains(lines[0], "host="+host) {
					t.Errorf("the line does not name server Probe and host %s: %s", host, lines[0])
				}
				if strings.Contains(strings.ToLower(out), strings.ToLower(secret)) {
					t.Errorf("a line carries the manual URL's secret:\n%s", out)
				}
			})
		}
	}
}
