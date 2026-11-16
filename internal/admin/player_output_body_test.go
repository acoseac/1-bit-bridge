package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// playerBodyRoutes are the four writes whose body decodePlayerBody decodes,
// each with an object it takes and one holding a value of another type.
var playerBodyRoutes = []struct{ path, object, wrongType string }{
	{"/api/output/player/enabled", `{"enabled":true}`, `{"enabled":"yes"}`},
	{"/api/output/player/openhome", `{"openhome":true}`, `{"openhome":1}`},
	{"/api/output/player/dac", `{"selection":"tok","binding":"serial"}`, `{"selection":1,"binding":"serial"}`},
	{toneRoute, `{"level":-40,"seconds":10}`, `{"level":-40,"seconds":1.5}`},
}

// unreadBody is a request body whose reader gives head and then answers err
// at every read, as a connection's does once its client stops sending and the
// read times out, or resets it: net/http hands the handler the socket's error,
// a *net.OpError that names both of its ends.
type unreadBody struct {
	head string
	err  error
}

func (b *unreadBody) Read(p []byte) (int, error) {
	if b.head != "" {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	return 0, b.err
}

// tcpAddr is ip and port as a TCP address.
func tcpAddr(ip string, port int) *net.TCPAddr {
	return &net.TCPAddr{IP: net.ParseIP(ip), Port: port}
}

// playerBodyRequest posts body to path on srv as the Player page's fetch does
// and answers the response, its status and its error envelope.
func playerBodyRequest(t *testing.T, srv *Server, path string, body *unreadBody) (*httptest.ResponseRecorder, saveRefusalBody) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, body)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:41000"
	r.Host = testConsoleHost
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var env saveRefusalBody
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("= %d: %s, which is no error envelope: %v", w.Code, w.Body, err)
	}
	return w, env
}

// requireNothingAsked fails the test if a refused body reached the provider.
func requireNothingAsked(t *testing.T, fake *fakePlayerOutput) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 0 {
		t.Errorf("a refused body reached the provider: %v", fake.calls)
	}
}

// TestAPlayerWriteWhoseBodyCannotBeReadNamesNoAddress (B601's round 2): a
// write whose body's reader fails, a client that stops sending until the read
// times out or that resets the connection, is answered 400 bad_json in fixed
// words (playerBodyUnread), and the failure is logged once at Info with its
// route and its cause and no address, loopback's included. The response
// carried the socket error's text until then, which names both ends of the
// connection, and no console response names a host. For each of the four
// routes, the read failing inside the object and after it (where the second
// decode reads), nothing is written and the provider is asked nothing.
func TestAPlayerWriteWhoseBodyCannotBeReadNamesNoAddress(t *testing.T) {
	for _, sock := range []struct {
		name   string
		err    error
		cause  string   // what the log line keeps of the error
		hidden []string // what reaches neither the response nor the log
	}{
		{"a timeout over loopback",
			&net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("127.0.0.1", 47789), Addr: tcpAddr("127.0.0.1", 54321), Err: os.ErrDeadlineExceeded},
			"read tcp: i/o timeout", []string{"127.0.0.1", "47789", "54321"}},
		{"a reset over IPv4",
			&net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("192.0.2.10", 47789), Addr: tcpAddr("198.51.100.23", 54321), Err: os.NewSyscallError("read", syscall.ECONNRESET)},
			"read tcp: read: " + syscall.ECONNRESET.Error(), []string{"192.0.2.10", "198.51.100.23", "47789", "54321"}},
		{"a timeout over IPv6",
			&net.OpError{Op: "read", Net: "tcp6", Source: tcpAddr("2001:db8::10", 47789), Addr: tcpAddr("2001:db8::23", 54321), Err: os.ErrDeadlineExceeded},
			"read tcp6: i/o timeout", []string{"2001:db8::10", "2001:db8::23", "47789", "54321"}},
		{"a timeout over IPv6 loopback",
			&net.OpError{Op: "read", Net: "tcp6", Source: tcpAddr("::1", 47789), Addr: tcpAddr("::1", 54321), Err: os.ErrDeadlineExceeded},
			"read tcp6: i/o timeout", []string{"::1", "47789", "54321"}},
		{"an error whose own words name addresses",
			errors.New("stream from 203.0.113.7:443 and [2001:db8::7]:443 cut short"),
			"cut short", []string{"203.0.113.7", "2001:db8::7"}},
		// An IPv6 address in an error's words with no brackets, beside a time
		// of day the scrub must leave as it is (B601's round 3).
		{"an error whose own words name IPv6 addresses bare",
			errors.New("a reset from 2001:db8::9 and ::1, seen at 10:42:07"),
			"seen at 10:42:07", []string{"2001:db8::9", "::1"}},
	} {
		for _, route := range playerBodyRoutes {
			for _, at := range []struct{ name, head string }{
				{"inside the object", route.object[:len(route.object)-1]},
				{"after the object", route.object},
			} {
				t.Run(sock.name+"/"+route.path+"/"+at.name, func(t *testing.T) {
					srv, fake, cfgPath := newPlayerTestServer(t)
					want := readFileOrNil(cfgPath)
					logs := loggingtest.Record(t)
					w, env := playerBodyRequest(t, srv, route.path, &unreadBody{head: at.head, err: sock.err})
					if w.Code != http.StatusBadRequest || env.Error != errCodeBadJSON || env.Message != playerBodyUnread {
						t.Errorf("= %d %q %q; want 400 %s %q", w.Code, env.Error, env.Message, errCodeBadJSON, playerBodyUnread)
					}
					for _, h := range sock.hidden {
						if strings.Contains(w.Body.String(), h) {
							t.Errorf("the response names %q: %s", h, w.Body)
						}
					}
					if carried := logs.Carrying(sock.hidden...); len(carried) != 0 {
						t.Errorf("a log line names an address: %q", carried)
					}
					lines := logs.Lines(playerBodyUnreadLine)
					if len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO ") || !strings.Contains(lines[0], route.path) || !strings.Contains(lines[0], sock.cause) {
						t.Errorf("the log holds %q; want one Info line %q naming %s and %q", logs.All(), playerBodyUnreadLine, route.path, sock.cause)
					}
					unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, route.path)
					requireNothingAsked(t, fake)
				})
			}
		}
	}
}

// TestTheBodyReadScrubTakesOnlyWhatParsesAsAnIPv6Address (B601's round 3):
// scrubBareIPv6 replaces an IPv6 address written without brackets, a zone
// with it and a colon after it kept, and leaves a run of colons that is no
// address (a time of day, a hardware address) as it is.
func TestTheBodyReadScrubTakesOnlyWhatParsesAsAnIPv6Address(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a reset from 2001:db8::9 and ::1, seen at 10:42:07", "a reset from <ip> and <ip>, seen at 10:42:07"},
		{"read from 2001:db8::9: connection reset", "read from <ip>: connection reset"},
		{"from fe80::1%eth0 at 10:42", "from <ip> at 10:42"},
		{"the full form 2001:0db8:0000:0000:0000:0000:0000:0001.", "the full form <ip>."},
		{"a hardware address 00:1a:2b:3c:4d:5e", "a hardware address 00:1a:2b:3c:4d:5e"},
		{"read tcp6: i/o timeout", "read tcp6: i/o timeout"},
	} {
		if got := scrubBareIPv6(tc.in); got != tc.want {
			t.Errorf("scrubBareIPv6(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestAPlayerWritesJSONFaultKeepsItsWords (B601's round 2): what
// decodePlayerBody refuses in the JSON itself or for its size keeps the
// decoder's words, which name nothing but the body (a syntax error, a value
// of another type, a key the write does not take, a body cut short, one past
// the bound inside its object or after it, none at all), and logs no read
// failure: only a body whose reader failed is answered in fixed words. A body
// past the bound after its object said "one JSON object, and nothing after
// it" until then, which it may well be.
func TestAPlayerWritesJSONFaultKeepsItsWords(t *testing.T) {
	for _, route := range playerBodyRoutes {
		open := route.object[:len(route.object)-1]
		for _, tc := range []struct{ name, body, words string }{
			{"a syntax error", open + "]", "invalid character"},
			{"a value of another type", route.wrongType, "cannot unmarshal"},
			{"a key it does not take", open + `,"x":1}`, `unknown field "x"`},
			{"a body cut short", open, "unexpected EOF"},
			{"a body past the bound inside its object", open + `,"x":"` + strings.Repeat("a", adminMaxBodyBytes) + `"}`, "request body too large"},
			{"a body past the bound after its object", route.object + strings.Repeat(" ", adminMaxBodyBytes), "request body too large"},
			{"no body", "", "a JSON body is required"},
		} {
			t.Run(route.path+"/"+tc.name, func(t *testing.T) {
				srv, fake, cfgPath := newPlayerTestServer(t)
				want := readFileOrNil(cfgPath)
				logs := loggingtest.Record(t)
				w, env := playerBodyRequest(t, srv, route.path, &unreadBody{head: tc.body, err: io.EOF})
				if w.Code != http.StatusBadRequest || env.Error != errCodeBadJSON || !strings.Contains(env.Message, tc.words) {
					t.Errorf("= %d %q %q; want 400 %s in the decoder's words, %q", w.Code, env.Error, env.Message, errCodeBadJSON, tc.words)
				}
				if lines := logs.Lines(playerBodyUnreadLine); len(lines) != 0 {
					t.Errorf("a fault of the JSON was logged as a body not read: %q", lines)
				}
				unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, route.path)
				requireNothingAsked(t, fake)
			})
		}
	}
}
