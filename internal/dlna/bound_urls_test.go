package dlna

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// urlTestListener stands in for a bound TCP listener without opening a socket.
// Accept waits for shutdown; requests run through Start's actual handler.
type urlTestListener struct {
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

func (l *urlTestListener) Addr() net.Addr { return l.addr }
func (l *urlTestListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}
func (l *urlTestListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// TestContentDirectoryURLsFollowTheBoundListener checks the URL authority
// through Start, Browse and Search, with stand-in interfaces and SSDP.
func TestContentDirectoryURLsFollowTheBoundListener(t *testing.T) {
	for _, tc := range []struct{ name, listen, bound, base string }{
		{"wildcard_default", ":7790", "[::]:7790", ""},
		{"wildcard_ipv4", "0.0.0.0:7790", "0.0.0.0:7790", ""},
		{"wildcard_ipv6", "[::]:7790", "[::]:7790", ""},
		{"wildcard_mapped", "[::ffff:0.0.0.0]:7790", "[::ffff:0.0.0.0]:7790", ""},
		{"private_ipv4", "192.168.1.20:7790", "192.168.1.20:7790", "http://192.168.1.20:7790"},
		{"private_ipv6", "[fd12:3456::20]:7790", "[fd12:3456::20]:7790", "http://[fd12:3456::20]:7790"},
		{"loopback", "127.0.0.1:7790", "127.0.0.1:7790", "http://127.0.0.1:7790"},
		{"public_ipv4", "203.0.113.5:7790", "203.0.113.5:7790", "http://203.0.113.5:7790"},
		{"public_ipv6", "[2001:db8::20]:7790", "[2001:db8::20]:7790", "http://[2001:db8::20]:7790"},
		{"resolved_name_ephemeral_port", "bridge-box.lan:0", "192.168.1.20:49152", "http://bridge-box.lan:49152"},
		{"zoned_ipv6", "[fe80::20%en0]:7790", "[fe80::20%en0]:7790", "http://[fe80::20%25en0]:7790"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track := testTrack("t1", "Test Track")
			track.ArtworkKey = "cover"
			l := &urlTestListener{addr: tcpAddr(tc.bound), closed: make(chan struct{})}
			addrs := cidrAddrs(t, "192.168.1.20/24", "10.1.2.3/24", "fd12:3456::20/64", "fe80::20/64", "203.0.113.5/24", "203.0.113.6/24", "2001:db8::20/64", "100.64.0.7/10")
			s, err := NewServer(ServerConfig{
				Library: newTestLib(track), Artwork: &recordingArtworkSource{},
				UDN: "uuid:bound-urls", ListenAddress: tc.listen, Listener: l,
				ServerURL: "http://192.168.1.20:7790", Interface: en0,
				SSDP: fakeSubsystem(t, newFakeNet()), Logger: discardLogger(),
				Interfaces:         func() ([]net.Interface, error) { return []net.Interface{*en0}, nil },
				InterfaceAddresses: func(*net.Interface) ([]net.Addr, error) { return addrs, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Stop(context.Background()); err != nil {
					t.Error(err)
				}
			})
			h := s.httpServer.Handler
			for _, host := range []string{"10.1.2.3:7790", "[fd12:3456::20]:7790", "192.168.1.20:9999", "192.168.1.20", "", "localhost:9999"} {
				for _, action := range []string{"Browse", "Search"} {
					req := buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", 0, 10)
					if action == "Search" {
						req = buildSearchRequest(t, "0", `dc:title contains "Test"`, 0, 10)
					}
					req.Host = host
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("%s Host %q: status %d: %s", action, host, rec.Code, rec.Body.String())
					}
					want := tc.base
					if want == "" {
						want = "http://" + host
						if host == "" {
							want = s.cfg.ServerURL
						}
					}
					for _, path := range []string{FilePathPrefix + "t1", ArtworkPathPrefix + "cover"} {
						if !strings.Contains(rec.Body.String(), want+path) {
							t.Errorf("%s Host %q: missing URL %q", action, host, want+path)
						}
					}
				}
			}
			// An in-flight handler must retain its authority when Stop clears
			// the listener's accepting state.
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.base != "" {
				req := buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", 0, 10)
				req.Host = "localhost:9999"
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.base+FilePathPrefix+"t1") {
					t.Error("the retained handler lost its bound URL during Stop")
				}
			}
			for _, host := range []string{"evil.example:7790", "10.9.9.9:7790", "203.0.113.6:7790", "100.64.0.7:7790"} {
				req := buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", 0, 10)
				req.Host = host
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusMisdirectedRequest {
					t.Errorf("Host %q: status %d, want 421", host, rec.Code)
				}
			}
		})
	}
}
