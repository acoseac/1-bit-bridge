package openhome

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

// TestAHeaderPastItsBoundIsRefused: the surface reads 16 KiB of a
// request's headers at most: a header block past that is 431 and its
// connection closed, an ordinary one answered.
func TestAHeaderPastItsBoundIsRefused(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{}, nil)
	at := listening(t, s, eth0)[eth0]
	for _, tc := range []struct {
		name  string
		extra string
		want  int
	}{
		{"an ordinary header", "User-Agent: 1-bit/1.0\r\n", http.StatusOK},
		{"a header past the bound", "X-Pad: " + strings.Repeat("a", maxHeaderRead) + "\r\n", http.StatusRequestHeaderFieldsTooLarge},
	} {
		c, err := box.Dial(at, phone)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, "GET /openhome/description.xml HTTP/1.1\r\nHost: "+at.String()+"\r\n"+tc.extra+"\r\n"); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("%s: no answer: %v", tc.name, err)
		}
		_ = res.Body.Close()
		_ = c.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, res.StatusCode, tc.want)
		}
	}
}

// TestAPanicInTheListingStaysInTheSurface: a listing that panics ends the
// surface's following of the interfaces, and nothing else: the process
// lives, what is bound is served and then closed by the stop, and the line
// carries the stack and never the panic's value, which could quote what the
// listing read.
func TestAPanicInTheListingStaysInTheSurface(t *testing.T) {
	rec := loggingtest.Record(t)
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	var mu sync.Mutex
	calls := 0
	addresses := func() (lan.Inventory, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			panic("the listing read /secret/interface-name")
		}
		return box.Addresses()
	}
	s, err := Start(Config{Device: Device{UUID: testUUID}, Port: 7792, Addresses: addresses, Listen: box.Listen})
	if err != nil {
		t.Fatal(err)
	}
	at := listening(t, s, eth0)[eth0]
	s.Rescan()
	const says = "player: the OpenHome surface's listing panicked: it serves the addresses it has bound, follows the interfaces no more, and stops as before"
	give := within(t, 30*time.Second)
	for {
		ch := s.Changed()
		if len(rec.Lines(says)) > 0 {
			break
		}
		select {
		case <-ch:
		case <-give:
			t.Fatalf("no line says the listing panicked:\n%s", strings.Join(rec.All(), "\n"))
		}
	}
	if line := rec.Lines(says)[0]; strings.Contains(line, "/secret/") || !strings.Contains(line, "stack=") {
		t.Errorf("the line %q quotes the panic's value or has no stack", line)
	}
	res, err := box.Client(phone).Get("http://" + at.String() + "/openhome/description.xml")
	if err != nil {
		t.Fatalf("a bound address after the panic: %v", err)
	}
	_ = res.Body.Close()
	if rep := s.Stop(); rep.Abandoned != 0 || len(box.Open()) != 0 {
		t.Errorf("the stop after the panic: %+v, open %v", rep, box.Open())
	}
}

// TestAnIdleConnectionAndAStalledHeaderAreClosed: a kept-alive connection
// that sends nothing is closed at the idle deadline, and one that stops
// half-way through its headers at the header deadline; neither holds a
// cap past them.
func TestAnIdleConnectionAndAStalledHeaderAreClosed(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{Idle: 300 * time.Millisecond, ReadHeader: 300 * time.Millisecond}, nil)
	at := listening(t, s, eth0)[eth0]
	// Each is waited for 5 s, well inside the request's whole deadline
	// (Limits.Read, 10 s), which net/http also takes for an idle one when
	// none is set: the close is the idle and the header deadlines'.
	closedByTheServer := func(name string, c io.Reader) {
		t.Helper()
		b, err := io.ReadAll(c)
		if isTimeout(err) {
			t.Errorf("%s: still open after 5 s (%d bytes)", name, len(b))
		}
	}
	idle, err := box.Dial(at, phone)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	br := bufio.NewReader(idle)
	if code, err := get(idle, br, at.String()); code != 200 {
		t.Fatalf("%d, %v", code, err)
	}
	_ = idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	closedByTheServer("a kept-alive connection that sends nothing", br)
	stalled, err := box.Dial(at, tablet)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if _, err := io.WriteString(stalled, "GET /openhome/description.xml HTTP/1.1\r\nHost: "); err != nil {
		t.Fatal(err)
	}
	_ = stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	closedByTheServer("a connection that stops in its headers", stalled)
	awaitStatus(t, s, 15*time.Second, "neither connection counted", func(st Status) bool { return st.Open == 0 })
}
