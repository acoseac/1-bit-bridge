package dlna

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Test_callbackHostMatchesSource pins the NARROW predicate #818 planned to
// move callbackHostAllowed to. The interesting rows are the ones where the
// two predicates disagree: since backlog B39 that is only a private address
// other than the source, which the held half of step two will start
// refusing and the observation warning exists to surface first.
func Test_callbackHostMatchesSource(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		remoteAddr string
		want       bool
		// wide records what callbackHostAllowed answers, so the table
		// doubles as the diff between the two predicates.
		wide bool
	}{
		{"same_ip", "192.168.1.4", "192.168.1.4:49152", true, true},
		{"same_ip_no_port", "192.168.1.4", "192.168.1.4", true, true},
		{"same_public_ip", "8.8.8.8", "8.8.8.8:1234", true, true},
		{"ipv6_same", "fe80::1", "[fe80::1]:49152", true, true},
		// A link-local peer's RemoteAddr carries its zone and its callback
		// cannot: it is still its own address, so no divergence is logged.
		{"ipv6_same_zoned_source", "fe80::1", "[fe80::1%en0]:49152", true, true},

		// The divergence left — accepted today, refused after step two.
		{"other_private_host", "192.168.1.250", "192.168.1.4:49152", false, true},

		// Closed by B39: this machine and the link, from another address.
		{"loopback_from_lan_source", "127.0.0.1", "192.168.1.9:49152", false, false},
		{"link_local_from_lan_source", "169.254.1.1", "192.168.1.4:49152", false, false},

		// Refused by both.
		{"hostname", "example.com", "192.168.1.4:49152", false, false},
		{"public_mismatch", "8.8.8.8", "192.168.1.4:49152", false, false},
		{"garbage_remote", "8.8.8.8", "garbage", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callbackHostMatchesSource(tc.host, tc.remoteAddr); got != tc.want {
				t.Errorf("callbackHostMatchesSource(%q, %q) = %v, want %v",
					tc.host, tc.remoteAddr, got, tc.want)
			}
			// Cross-check against the wide predicate so a change to
			// either one that silently converges them fails here.
			if got := callbackHostAllowed(tc.host, tc.remoteAddr); got != tc.wide {
				t.Errorf("callbackHostAllowed(%q, %q) = %v, want %v (the private arm is held; only B39's rows moved)",
					tc.host, tc.remoteAddr, got, tc.wide)
			}
		})
	}
}

// newLogCaptureServer returns a Server whose logger writes into buf.
func newLogCaptureServer(buf *bytes.Buffer) *Server {
	return &Server{
		log: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
}

func Test_noteCallbackDivergence_SilentWhenCallbackIsTheSource(t *testing.T) {
	var buf bytes.Buffer
	s := newLogCaptureServer(&buf)
	for i := 0; i < 5; i++ {
		s.noteCallbackDivergence("cds", "192.168.1.4", "192.168.1.4:49152")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log for a matching callback; got:\n%s", buf.String())
	}
}

// callbackNote is one of the two warnings that share the observation set,
// with a callback host that reaches it from the handler: a private address
// other than the source for the divergence, and since backlog B39 a
// loopback address from another source for the refusal.
type callbackNote struct {
	name, host, message string
	note                func(s *Server, callbackHost, remoteAddr string)
}

var callbackNotes = []callbackNote{
	{"divergence", "192.168.1.250", "GENA callback host differs",
		func(s *Server, host, remoteAddr string) { s.noteCallbackDivergence("cds", host, remoteAddr) }},
	{"refusal", "127.0.0.1", "GENA callback on this machine or a link-local address refused",
		func(s *Server, host, remoteAddr string) { s.noteCallbackRefusal("cds", host, remoteAddr) }},
}

func Test_callbackNotes_WarnOncePerPair(t *testing.T) {
	for _, n := range callbackNotes {
		t.Run(n.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newLogCaptureServer(&buf)

			// Same pair, many times — but with a DIFFERENT EPHEMERAL PORT
			// each time, which is what a real subscription renewal looks
			// like: a new TCP connection from the same host. A fixture that
			// reuses one port passes against a key that includes the port,
			// and therefore proves nothing. (That is exactly how the first
			// version of this test went green against the bug review
			// caught.)
			for i := 0; i < 20; i++ {
				n.note(s, n.host, "192.168.1.9:"+itoa(49152+i))
			}
			if c := strings.Count(buf.String(), n.message); c != 1 {
				t.Fatalf("want exactly 1 warning for a repeated pair, got %d:\n%s", c, buf.String())
			}
			// Both addresses have to be in the line — the whole point is
			// that a field report names the device and what it asked for.
			line := buf.String()
			if !strings.Contains(line, "callbackHost="+n.host) || !strings.Contains(line, "subscribeSource=192.168.1.9") {
				t.Fatalf("warning must carry both addresses; got:\n%s", line)
			}

			// A DIFFERENT SOURCE HOST is a different observation and gets
			// its own line — the dedup must collapse ports, not addresses.
			n.note(s, n.host, "192.168.1.10:49152")
			if c := strings.Count(buf.String(), n.message); c != 2 {
				t.Fatalf("want 2 warnings after a second distinct pair, got %d:\n%s", c, buf.String())
			}
		})
	}
}

// Test_callbackNotes_CapSuppressesRatherThanFloods pins BOTH halves of the
// bound: the map stops growing, AND the log stops. Logging on the capped
// path would let a host manufacturing unique addresses turn a diagnostic
// into exactly the flood the cap exists to prevent, and a refusal is the
// one a peer reaches at will.
func Test_callbackNotes_CapSuppressesRatherThanFloods(t *testing.T) {
	for _, n := range callbackNotes {
		t.Run(n.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newLogCaptureServer(&buf)
			const over = callbackDivergeSeenCap * 3
			for i := 0; i < over; i++ {
				// Distinct SOURCE HOSTS (not just ports) so each is
				// genuinely a new observation rather than a dedup hit.
				n.note(s, n.host, "10.0."+itoa(i/256)+"."+itoa(i%256)+":49152")
			}
			s.callbackDivergeMu.Lock()
			seen := len(s.callbackDivergeSeen)
			s.callbackDivergeMu.Unlock()
			if seen > callbackDivergeSeenCap {
				t.Errorf("observation set grew past its cap: %d > %d", seen, callbackDivergeSeenCap)
			}
			lines := strings.Count(buf.String(), n.message)
			if lines > callbackDivergeSeenCap {
				t.Errorf("logged %d lines for %d distinct sources; the cap must suppress, not just stop recording", lines, over)
			}
		})
	}
}

// Test_callbackNotes_LogTheAddressWithoutThePort pins what a field report
// actually needs: the source IP. The ephemeral port changes on every
// renewal and is noise.
func Test_callbackNotes_LogTheAddressWithoutThePort(t *testing.T) {
	for _, n := range callbackNotes {
		t.Run(n.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newLogCaptureServer(&buf)
			n.note(s, n.host, "192.168.1.9:49152")
			out := buf.String()
			if !strings.Contains(out, "subscribeSource=192.168.1.9") {
				t.Errorf("want the bare source IP in the line; got:\n%s", out)
			}
			if strings.Contains(out, "49152") {
				t.Errorf("ephemeral port leaked into the line; got:\n%s", out)
			}
		})
	}
}

func Test_sourceIPOf(t *testing.T) {
	cases := map[string]string{
		"192.168.1.9:49152": "192.168.1.9",
		"[fe80::1]:49152":   "fe80::1",
		"192.168.1.9":       "192.168.1.9", // no port — reverse proxy / test shape
		"":                  "",
	}
	for in, want := range cases {
		if got := sourceIPOf(in); got != want {
			t.Errorf("sourceIPOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func Test_callbackNotes_ConcurrentIsRaceFree(t *testing.T) {
	var buf bytes.Buffer
	// Serialize the writer: bytes.Buffer is not concurrency-safe, and this
	// test is about the map, not the sink.
	s := &Server{log: slog.New(slog.NewTextHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := callbackNotes[i%len(callbackNotes)]
			for j := 0; j < 32; j++ {
				n.note(s, n.host, "10.0.0."+itoa(i)+":49152")
			}
		}(i)
	}
	wg.Wait()
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
