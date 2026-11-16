package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
// each with the route constant its log line names, an object it takes and one
// holding a value of another type.
var playerBodyRoutes = []struct{ path, route, object, wrongType string }{
	{"/api/output/player/enabled", playerRouteEnabled, `{"enabled":true}`, `{"enabled":"yes"}`},
	{"/api/output/player/openhome", playerRouteOpenHome, `{"openhome":true}`, `{"openhome":1}`},
	{"/api/output/player/dac", playerRouteDAC, `{"selection":"tok","binding":"serial"}`, `{"selection":1,"binding":"serial"}`},
	{toneRoute, playerRouteTone, `{"level":-40,"seconds":10}`, `{"level":-40,"seconds":1.5}`},
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

// bodyReadErrors are reader failures of every shape of address the body-read
// line's reviews named, and of every class, each with the class its type
// makes and what of its words must reach no record: an address, a port, a
// zone, a sentinel, or the class word its text says where its type is not
// that class's.
var bodyReadErrors = []struct {
	name   string
	err    error
	class  string
	hidden []string
}{
	{"words with an address after a colon, saying reset", errors.New("peer addr:2001:db8::9 reset"), bodyReadOther,
		[]string{"2001:db8::9", "peer addr", "reset"}},
	{"a zoned link-local address", errors.New("src:fe80::1%eth0"), bodyReadOther, []string{"fe80::1", "eth0"}},
	{"a full-form address with a port", errors.New("2001:db8:0:0:0:0:0:1:443"), bodyReadOther, []string{"2001:db8:0:0:0:0:0:1", "443"}},
	{"a dotted IPv6 address with a zone", errors.New("fe80::192.0.2.1%eth0"), bodyReadOther, []string{"fe80::192.0.2.1", "192.0.2.1", "eth0"}},
	{"an IPv4-mapped address", errors.New("::ffff:192.0.2.1"), bodyReadOther, []string{"::ffff:192.0.2.1", "192.0.2.1"}},
	{"a sentinel with an address and a port", errors.New("sentinel-5e6f the reader failed at 203.0.113.7:8443"), bodyReadOther,
		[]string{"sentinel-5e6f", "203.0.113.7", "8443"}},
	{"a socket's timeout with both ends",
		&net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("192.0.2.10", 47789), Addr: tcpAddr("198.51.100.23", 54321), Err: os.ErrDeadlineExceeded},
		bodyReadTimeout, []string{"192.0.2.10", "198.51.100.23", "47789", "54321"}},
	{"a socket's reset over IPv6 with both ends",
		&net.OpError{Op: "read", Net: "tcp6", Source: tcpAddr("2001:db8::10", 47789), Addr: tcpAddr("2001:db8::23", 54321), Err: os.NewSyscallError("read", syscall.ECONNRESET)},
		bodyReadReset, []string{"2001:db8::10", "2001:db8::23", "47789", "54321"}},
	{"a broken pipe over loopback",
		&net.OpError{Op: "read", Net: "tcp", Source: tcpAddr("127.0.0.1", 47789), Addr: tcpAddr("::1", 54321), Err: os.NewSyscallError("read", syscall.EPIPE)},
		bodyReadClosed, []string{"127.0.0.1", "::1", "47789", "54321"}},
	{"a closed connection wrapped with a sentinel", fmt.Errorf("sentinel-7f3a read on fe80::7%%eth1: %w", net.ErrClosed), bodyReadClosed,
		[]string{"sentinel-7f3a", "fe80::7", "eth1"}},
	{"a body cut short wrapped with a sentinel", fmt.Errorf("sentinel-1b2d from 198.51.100.8: %w", io.ErrUnexpectedEOF), bodyReadEOF,
		[]string{"sentinel-1b2d", "198.51.100.8"}},
	{"a context's deadline wrapped with an address", fmt.Errorf("gave up near [2001:db8::42]:9: %w", context.DeadlineExceeded), bodyReadTimeout,
		[]string{"2001:db8::42", "gave up"}},
}

// TestAPlayerWriteWhoseBodyCannotBeReadNamesNoAddress (B601's rounds 2 and 4):
// a write whose body's reader fails, a client that stops sending until the
// read times out or that resets the connection, is answered 400 bad_json in
// fixed words (playerBodyUnread), and logs one record and only one: at Info,
// its message the fixed line, and its attributes exactly the admin logger's
// component, the write's route constant and the class its error's type makes
// (bodyReadClass), so nothing of the request or of the error's words, which
// can name both ends of its socket, reaches the log. Until round 4 the line
// carried the error's words with its addresses scrubbed, and three rounds of
// review each found a shape of address the scrub missed. For each of the four
// routes, each error of bodyReadErrors, the read failing inside the object and
// after it (where the second decode reads), nothing is written and the
// provider is asked nothing.
func TestAPlayerWriteWhoseBodyCannotBeReadNamesNoAddress(t *testing.T) {
	for _, route := range playerBodyRoutes {
		srv, fake, cfgPath := newPlayerTestServer(t)
		want := readFileOrNil(cfgPath)
		for _, read := range bodyReadErrors {
			for _, at := range []struct{ name, head string }{
				{"inside the object", route.object[:len(route.object)-1]},
				{"after the object", route.object},
			} {
				t.Run(route.route+"/"+read.name+"/"+at.name, func(t *testing.T) {
					logs := loggingtest.Record(t)
					w, env := playerBodyRequest(t, srv, route.path, &unreadBody{head: at.head, err: read.err})
					if w.Code != http.StatusBadRequest || env.Error != errCodeBadJSON || env.Message != playerBodyUnread {
						t.Errorf("= %d %q %q; want 400 %s %q", w.Code, env.Error, env.Message, errCodeBadJSON, playerBodyUnread)
					}
					requireTheBodyReadRecord(t, logs, route.route, read.class)
					for _, h := range read.hidden {
						if strings.Contains(w.Body.String(), h) {
							t.Errorf("the response holds %q: %s", h, w.Body)
						}
					}
					if carried := logs.Carrying(read.hidden...); len(carried) != 0 {
						t.Errorf("a record holds what the error's words said: %q", carried)
					}
					unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, route.path)
					requireNothingAsked(t, fake)
				})
			}
		}
	}
}

// requireTheBodyReadRecord fails the test unless logs holds one record, at
// Info, whose message is playerBodyUnreadLine and whose attributes are exactly
// component=admin (the admin logger's own), route=route and class=class.
func requireTheBodyReadRecord(t *testing.T, logs *loggingtest.Recorder, route, class string) {
	t.Helper()
	recs := logs.Records()
	if len(recs) != 1 {
		t.Fatalf("the request logged %d records: %q; want the one body-read line", len(recs), logs.All())
	}
	rec := recs[0]
	got := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.Resolve().String()
		return true
	})
	wantAttrs := map[string]string{"component": "admin", "route": route, "class": class}
	if rec.Level != slog.LevelInfo || rec.Message != playerBodyUnreadLine || rec.NumAttrs() != len(wantAttrs) || !maps.Equal(got, wantAttrs) {
		t.Errorf("the record is %s %q %v (%d attributes); want INFO %q %v", rec.Level, rec.Message, got, rec.NumAttrs(), playerBodyUnreadLine, wantAttrs)
	}
}

// netError is a net.Error whose Timeout answers timeout, and whose words say
// a timeout whatever it answers.
type netError struct{ timeout bool }

func (e netError) Error() string   { return "i/o timeout at 192.0.2.99:7" }
func (e netError) Timeout() bool   { return e.timeout }
func (e netError) Temporary() bool { return false }

// TestABodyReadFailureIsClassedByItsType (B601's round 4): bodyReadClass names
// a reader's failure by its type through its chain, never its words: each
// type of timeout, reset, closed and eof, bare and wrapped, is its class, and
// words that say a class over a type that is none of them, or a net.Error that
// did not time out, are other.
func TestABodyReadFailureIsClassedByItsType(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a socket's timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, bodyReadTimeout},
		{"os's deadline", os.ErrDeadlineExceeded, bodyReadTimeout},
		{"a context's deadline, wrapped", fmt.Errorf("gave up: %w", context.DeadlineExceeded), bodyReadTimeout},
		{"a net.Error that timed out", netError{timeout: true}, bodyReadTimeout},
		{"a reset", os.NewSyscallError("read", syscall.ECONNRESET), bodyReadReset},
		{"a socket's reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, bodyReadReset},
		{"a closed connection", net.ErrClosed, bodyReadClosed},
		{"a closed pipe, wrapped", fmt.Errorf("read: %w", io.ErrClosedPipe), bodyReadClosed},
		{"a closed file", os.ErrClosed, bodyReadClosed},
		{"a socket's broken pipe", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.EPIPE)}, bodyReadClosed},
		{"a body cut short", io.ErrUnexpectedEOF, bodyReadEOF},
		{"a body cut short, wrapped", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), bodyReadEOF},
		{"words that say a reset", errors.New("read tcp: connection reset by peer"), bodyReadOther},
		{"words that say a timeout", errors.New("i/o timeout"), bodyReadOther},
		{"words that say closed", errors.New("use of closed network connection"), bodyReadOther},
		{"words that say a body cut short", errors.New("unexpected EOF"), bodyReadOther},
		{"a net.Error that did not time out", netError{timeout: false}, bodyReadOther},
		{"a permission refusal", os.ErrPermission, bodyReadOther},
	} {
		if got := bodyReadClass(tc.err); got != tc.want {
			t.Errorf("%s: bodyReadClass(%v) = %q; want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestAPlayerWritesJSONFaultKeepsItsWords (B601's round 2): a fault of the JSON
// inside the object, or of its size inside it or after it, keeps the
// decoder's words, which name nothing but the body (a syntax error, a value
// of another type, a key the write does not take, a body cut short, one past
// the bound inside its object or after it, none at all); trailing JSON or
// content after the object (a stray character, a second value) is "one JSON
// object, and nothing after it", word for word (B601's round 3); and none of
// them logs a read failure, whose fixed words are a failed reader's alone. A
// body past the bound after its object said "one JSON object, and nothing
// after it" until round 2, which it may well be.
func TestAPlayerWritesJSONFaultKeepsItsWords(t *testing.T) {
	const trailing = "one JSON object, and nothing after it"
	for _, route := range playerBodyRoutes {
		open := route.object[:len(route.object)-1]
		for _, tc := range []struct {
			name, body, words string
			exact             bool // the message is words, not merely holds them
		}{
			{"a syntax error", open + "]", "invalid character", false},
			{"a value of another type", route.wrongType, "cannot unmarshal", false},
			{"a key it does not take", open + `,"x":1}`, `unknown field "x"`, false},
			{"a body cut short", open, "unexpected EOF", false},
			{"a body past the bound inside its object", open + `,"x":"` + strings.Repeat("a", adminMaxBodyBytes) + `"}`, "request body too large", false},
			{"a body past the bound after its object", route.object + strings.Repeat(" ", adminMaxBodyBytes), "request body too large", false},
			{"no body", "", "a JSON body is required", true},
			{"a stray character after the object", route.object + "x", trailing, true},
			{"a second value after the object", route.object + " 1", trailing, true},
		} {
			t.Run(route.path+"/"+tc.name, func(t *testing.T) {
				srv, fake, cfgPath := newPlayerTestServer(t)
				want := readFileOrNil(cfgPath)
				logs := loggingtest.Record(t)
				w, env := playerBodyRequest(t, srv, route.path, &unreadBody{head: tc.body, err: io.EOF})
				said := strings.Contains(env.Message, tc.words)
				if tc.exact {
					said = env.Message == tc.words
				}
				if w.Code != http.StatusBadRequest || env.Error != errCodeBadJSON || !said {
					t.Errorf("= %d %q %q; want 400 %s, %q", w.Code, env.Error, env.Message, errCodeBadJSON, tc.words)
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
