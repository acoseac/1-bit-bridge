package dlna

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Test_callbackHostMatchesSource pins the NARROW predicate that
// callbackHostAllowed is being moved to. The interesting rows are the
// ones where the two predicates disagree — those are exactly what step
// two of the narrowing will start refusing, and what the observation
// warning exists to surface first.
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

		// The divergences — accepted today, refused after step two.
		{"loopback_from_lan_source", "127.0.0.1", "192.168.1.9:49152", false, true},
		{"other_private_host", "192.168.1.250", "192.168.1.4:49152", false, true},
		{"link_local_from_lan_source", "169.254.1.1", "192.168.1.4:49152", false, true},

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
				t.Errorf("callbackHostAllowed(%q, %q) = %v, want %v (the wide predicate must not move yet)",
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

func Test_noteCallbackDivergence_WarnsOncePerPair(t *testing.T) {
	var buf bytes.Buffer
	s := newLogCaptureServer(&buf)

	// Same divergent pair, many times: exactly one line. A control point
	// that renews its subscription on a timer must not flood the log.
	for i := 0; i < 20; i++ {
		s.noteCallbackDivergence("cds", "127.0.0.1", "192.168.1.9:49152")
	}
	if n := strings.Count(buf.String(), "GENA callback host differs"); n != 1 {
		t.Fatalf("want exactly 1 warning for a repeated pair, got %d:\n%s", n, buf.String())
	}
	// Both addresses have to be in the line — the whole point is that a
	// field report names the device that needs the wider form.
	line := buf.String()
	if !strings.Contains(line, "127.0.0.1") || !strings.Contains(line, "192.168.1.9") {
		t.Fatalf("warning must carry both addresses; got:\n%s", line)
	}

	// A DIFFERENT pair is a different observation and gets its own line.
	s.noteCallbackDivergence("cds", "127.0.0.1", "192.168.1.10:49152")
	if n := strings.Count(buf.String(), "GENA callback host differs"); n != 2 {
		t.Fatalf("want 2 warnings after a second distinct pair, got %d:\n%s", n, buf.String())
	}
}

func Test_noteCallbackDivergence_IsBounded(t *testing.T) {
	var buf bytes.Buffer
	s := newLogCaptureServer(&buf)
	// Well past the cap. The set must stop growing; the warning count is
	// allowed to keep rising (an unbounded map is the hazard, not an
	// unbounded log from a host generating unique addresses).
	for i := 0; i < callbackDivergeSeenCap*3; i++ {
		s.noteCallbackDivergence("cds", "127.0.0.1", "10.0.0.1:"+itoa(i))
	}
	s.callbackDivergeMu.Lock()
	n := len(s.callbackDivergeSeen)
	s.callbackDivergeMu.Unlock()
	if n > callbackDivergeSeenCap {
		t.Fatalf("observation set grew past its cap: %d > %d", n, callbackDivergeSeenCap)
	}
}

func Test_noteCallbackDivergence_ConcurrentIsRaceFree(t *testing.T) {
	var buf bytes.Buffer
	// Serialize the writer: bytes.Buffer is not concurrency-safe, and this
	// test is about the map, not the sink.
	s := &Server{log: slog.New(slog.NewTextHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				s.noteCallbackDivergence("cds", "127.0.0.1", "10.0.0."+itoa(i)+":49152")
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
