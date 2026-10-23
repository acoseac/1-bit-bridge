package openhome

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

// TestAnIdleConnectionAndAStalledHeaderAreClosed: a kept-alive connection
// that sends nothing is closed at the idle deadline, and one that stops
// half-way through its headers at the header deadline; neither holds a
// cap past them.
func TestAnIdleConnectionAndAStalledHeaderAreClosed(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{Idle: 300 * time.Millisecond, ReadHeader: 300 * time.Millisecond}, nil)
	at := listening(t, s, eth0)[eth0]
	closedByTheServer := func(name string, c io.Reader) {
		t.Helper()
		b, err := io.ReadAll(c)
		if isTimeout(err) {
			t.Errorf("%s: still open after 15 s (%d bytes)", name, len(b))
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
	_ = idle.SetReadDeadline(time.Now().Add(15 * time.Second))
	closedByTheServer("a kept-alive connection that sends nothing", br)
	stalled, err := box.Dial(at, tablet)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if _, err := io.WriteString(stalled, "GET /openhome/description.xml HTTP/1.1\r\nHost: "); err != nil {
		t.Fatal(err)
	}
	_ = stalled.SetReadDeadline(time.Now().Add(15 * time.Second))
	closedByTheServer("a connection that stops in its headers", stalled)
	awaitStatus(t, s, 15*time.Second, "neither connection counted", func(st Status) bool { return st.Open == 0 })
}
