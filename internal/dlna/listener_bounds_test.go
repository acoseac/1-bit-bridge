package dlna

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// startLoopbackDLNA boots a real listener on 127.0.0.1 with its SSDP
// advertiser pinned to the loopback interface. A host that will not
// join that group skips, the same way the lifecycle test does.
func startLoopbackDLNA(t *testing.T, cfg ServerConfig, tune func(*Server)) *Server {
	t.Helper()
	if cfg.Library == nil {
		cfg.Library = newTestLib(testTrack("t1", "Test Track"))
	}
	addr := findFreePort(t)
	cfg.UDN = "uuid:listener-bounds"
	cfg.FriendlyName = "Listener Bounds"
	cfg.ListenAddress = addr
	cfg.ServerURL = "http://" + addr
	cfg.AdvertiseEndpoints = []AdvertiseEndpoint{{
		Interface: loopbackInterface(t),
		ServerURL: "http://" + addr,
	}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if tune != nil {
		tune(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
		cancel()
	})
	if err := s.Start(ctx); err != nil {
		cancel()
		t.Skipf("Start failed (SSDP on this host): %v", err)
	}
	return s
}

// TestASubscribeFloodDoesNotSpawnAGoroutinePerNotify floods SUBSCRIBE
// against a callback that accepts a NOTIFY and then blocks. Every
// SUBSCRIBE is 200. The goroutine delta is taken the moment the flood
// returns, while the accepted NOTIFYs are still inside the callback.
func TestASubscribeFloodDoesNotSpawnAGoroutinePerNotify(t *testing.T) {
	s := startLoopbackDLNA(t, ServerConfig{}, nil)

	block := make(chan struct{})
	var got atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	t.Cleanup(func() { close(block) })

	n := genaNotifyPool * 6
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	t.Cleanup(client.CloseIdleConnections)
	callback := "<" + sink.URL + "/cb>"
	before := runtime.NumGoroutine()
	for i := 0; i < n; i++ {
		req, err := http.NewRequest(http.MethodPost, "http://"+s.cfg.ListenAddress+"/dlna/cds/event", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Method = "SUBSCRIBE"
		req.Header.Set("CALLBACK", callback)
		req.Header.Set("NT", "upnp:event")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("SUBSCRIBE %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("SUBSCRIBE %d status %d", i, resp.StatusCode)
		}
	}
	client.CloseIdleConnections()
	delta := runtime.NumGoroutine() - before
	// One goroutine per SUBSCRIBE grew this process by 190. The pool,
	// the callback handlers it is serving, and the accept loop sit
	// around 32. The flood size is the ceiling between those.
	if delta > n {
		t.Fatalf("goroutines after %d SUBSCRIBEs grew by %d (pool %d)", n, delta, genaNotifyPool)
	}

	deadline := time.Now().Add(2 * time.Second)
	var stable int32
	for {
		cur := got.Load()
		runtime.Gosched()
		if cur > 0 && cur == got.Load() {
			stable = cur
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("NOTIFY count did not settle (last %d)", got.Load())
		}
	}
	if stable > int32(genaNotifyPool) {
		t.Fatalf("delivered %d NOTIFYs, pool is %d", stable, genaNotifyPool)
	}
	t.Logf("goroutines +%d after %d SUBSCRIBEs, delivered %d", delta, n, stable)
}

// TestTheListenerUsesTheAPIDeadlines reads the listener's deadlines
// after a real Start. WriteTimeout stays unset because a renderer streams.
func TestTheListenerUsesTheAPIDeadlines(t *testing.T) {
	s := startLoopbackDLNA(t, ServerConfig{}, nil)
	if s.httpServer.ReadTimeout != dlnaReadTimeout {
		t.Errorf("ReadTimeout = %s, want %s", s.httpServer.ReadTimeout, dlnaReadTimeout)
	}
	if s.httpServer.IdleTimeout != dlnaIdleTimeout {
		t.Errorf("IdleTimeout = %s, want %s", s.httpServer.IdleTimeout, dlnaIdleTimeout)
	}
	if s.httpServer.ReadHeaderTimeout != dlnaReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %s, want %s", s.httpServer.ReadHeaderTimeout, dlnaReadHeaderTimeout)
	}
	if s.httpServer.MaxHeaderBytes != dlnaMaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", s.httpServer.MaxHeaderBytes, dlnaMaxHeaderBytes)
	}
	if s.httpServer.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want unset", s.httpServer.WriteTimeout)
	}
}

// TestTheListenerClosesAnIdleKeepAliveAndAStalledBody shortens the two
// deadlines on this server only, then waits for the socket to close. A
// timeout on the client's read means the listener left it open.
func TestTheListenerClosesAnIdleKeepAliveAndAStalledBody(t *testing.T) {
	const short = 200 * time.Millisecond
	s := startLoopbackDLNA(t, ServerConfig{}, func(s *Server) {
		s.readTimeout = short
		s.idleTimeout = short
	})
	addr := s.cfg.ListenAddress

	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { idle.Close() })
	if _, err := fmt.Fprintf(idle, "GET /dlna/description.xml HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", addr); err != nil {
		t.Fatalf("write: %v", err)
	}
	idleBR := bufio.NewReader(idle)
	res, err := http.ReadResponse(idleBR, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if !listenerClosed(t, idle, idleBR) {
		t.Error("idle keep-alive stayed open")
	}

	stall, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { stall.Close() })
	if _, err := fmt.Fprintf(stall, "POST /dlna/cds/control HTTP/1.1\r\nHost: %s\r\nContent-Length: 100000\r\n\r\n", addr); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	if _, err := stall.Write([]byte{0x3c}); err != nil {
		t.Fatalf("write body byte: %v", err)
	}
	if !listenerClosed(t, stall, bufio.NewReader(stall)) {
		t.Error("stalled body stayed open")
	}
}

// listenerClosed reports whether the listener closed conn. Bytes that
// arrive before the close, such as an error response, are drained. A
// read that times out means the listener left the socket open.
func listenerClosed(t *testing.T, conn net.Conn, br *bufio.Reader) bool {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [256]byte
	for {
		_, err := br.Read(b[:])
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false
		}
		return true
	}
}

// TestTelemetryKeepsAHundredRunesOfEachHeader sends headers past the
// 100-rune cut and under the header-size cap, through a real GET.
func TestTelemetryKeepsAHundredRunesOfEachHeader(t *testing.T) {
	store := NewTelemetryStore(8)
	s := startLoopbackDLNA(t, ServerConfig{TelemetryStore: store}, nil)
	ua := strings.Repeat("é", 150)
	accept := strings.Repeat("B", 150)
	rng := "bytes=" + strings.Repeat("C", 144)
	features := strings.Repeat("D", 150)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.cfg.ListenAddress+"/dlna/description.xml", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", accept)
	req.Header.Set("Range", rng)
	req.Header.Set("getContentFeatures.dlna.org", features)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("stored %d entries", len(snap))
	}
	e := snap[0]
	checkRunes := func(name, got, full string) {
		t.Helper()
		if utf8.RuneCountInString(got) != dlnaLoggedFieldRunes {
			t.Errorf("%s runes = %d, want %d", name, utf8.RuneCountInString(got), dlnaLoggedFieldRunes)
		}
		if !strings.HasPrefix(full, got) {
			t.Errorf("%s is not a prefix of the header", name)
		}
	}
	checkRunes("UserAgent", e.UserAgent, ua)
	checkRunes("Accept", e.AcceptHeader, accept)
	checkRunes("Range", e.RangeHeader, rng)
	checkRunes("ContentFeatures", e.ContentFeaturesAccept, features)
}

// TestTheListenerRefusesAnOversizedHeader sends a User-Agent the default
// 1 MiB header cap would accept. The listener answers 431 and stores nothing.
func TestTheListenerRefusesAnOversizedHeader(t *testing.T) {
	store := NewTelemetryStore(8)
	s := startLoopbackDLNA(t, ServerConfig{TelemetryStore: store}, nil)
	before := heapAlloc()
	var status int
	for i := 0; i < 8; i++ {
		code := oversizedUserAgentStatus(t, s.cfg.ListenAddress)
		if i == 0 {
			status = code
		} else if code != status {
			t.Errorf("request %d status %d, first was %d", i, code, status)
		}
	}
	after := heapAlloc()
	growth := int64(after) - int64(before)
	if status != http.StatusRequestHeaderFieldsTooLarge || store.Len() != 0 {
		t.Fatalf("status %d stored %d heap growth %d bytes; want 431 and an empty ring", status, store.Len(), growth)
	}
	t.Logf("status %d stored %d heap growth %d", status, store.Len(), growth)
}

func heapAlloc() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func oversizedUserAgentStatus(t *testing.T, addr string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "GET /dlna/description.xml HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nConnection: close\r\n\r\n",
		addr, strings.Repeat("U", 900<<10))
	_, _ = conn.Write(buf.Bytes())
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("status line: %v", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("status line %q", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("status %q: %v", fields[1], err)
	}
	return code
}
