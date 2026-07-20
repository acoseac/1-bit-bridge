package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// eventsTestServer spins up a Server with an attached event broker
// and a valid bearer token. Caller is responsible for stopping the
// broker via the returned stopFn; httptest.Server cleanup is
// auto-deferred via t.Cleanup.
func eventsTestServer(t *testing.T) (hs *httptest.Server, broker *eventBroker, token string, stopBroker func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{dir},
		ListenAddress: ":0",
		LibraryName:   "Test",
	}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("test-client")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp")
	stop := srv.StartEventBroker()
	hs = httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, srv.eventBroker, raw, stop
}

// TestEventsRouteRequiresAuth: hitting /v1/events without a bearer
// returns 401. Same shape as every other authed /v1/* endpoint.
func TestEventsRouteRequiresAuth(t *testing.T) {
	hs, _, _, stop := eventsTestServer(t)
	defer stop()

	resp, err := http.Get(hs.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestEventsRouteReturns404WhenBrokerNotWired: a Server constructed
// without StartEventBroker returns 404 events_not_supported. iOS
// falls back to polling on this shape.
func TestEventsRouteReturns404WhenBrokerNotWired(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{dir},
		ListenAddress: ":0",
		LibraryName:   "Test",
	}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := store.Mint("test-client")
	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	req, _ := http.NewRequest("GET", hs.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no broker wired)", resp.StatusCode)
	}
}

// TestEventsResponseIsNotGzipped: pins the contract that a future
// global gzip middleware would silently break SSE (buffers the
// response until close, defeats Flush()). The handler explicitly sets
// Content-Encoding: identity; this test is the tripwire if anyone
// wires gzip middleware later.
func TestEventsResponseIsNotGzipped(t *testing.T) {
	hs, _, raw, stop := eventsTestServer(t)
	defer stop()

	req, _ := http.NewRequest("GET", hs.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	// Some clients send Accept-Encoding: gzip — we MUST NOT honour
	// it on this endpoint.
	req.Header.Set("Accept-Encoding", "gzip")

	// Use a context with a short deadline so the handler closes
	// quickly. We're checking response headers, not body content.
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		// Expected — the connection times out client-side. Re-issue
		// without the timeout to inspect headers.
		req2, _ := http.NewRequest("GET", hs.URL+"/v1/events", nil)
		req2.Header.Set("Authorization", "Bearer "+raw)
		req2.Header.Set("Accept-Encoding", "gzip")
		client2 := &http.Client{Timeout: 100 * time.Millisecond}
		resp, _ = client2.Do(req2)
	}
	if resp == nil {
		t.Fatal("no response received")
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		t.Errorf("Content-Encoding = %q, want identity (or empty)", ce)
	}
	if cb := resp.Header.Get("X-Accel-Buffering"); cb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", cb)
	}
}

// TestEventsDeliversPushedEvent: subscribe via /v1/events, publish an
// event server-side, assert iOS receives the wire-shaped frame within
// 100ms.
func TestEventsDeliversPushedEvent(t *testing.T) {
	hs, broker, raw, stop := eventsTestServer(t)
	defer stop()

	req, _ := http.NewRequest("GET", hs.URL+"/v1/events?topics=upscale", nil)
	req.Header.Set("Authorization", "Bearer "+raw)

	// Use a longer-timeout HTTP client so we can read the stream.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Give the handler a moment to register the subscription.
	time.Sleep(50 * time.Millisecond)

	broker.Publish("upscale.stats", map[string]int{"queued": 7})

	// Read frames until we see the upscale.stats event or time out.
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	var sawEvent, sawData bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: upscale.stats") {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"queued":7`) {
			sawData = true
		}
		if sawEvent && sawData {
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	t.Errorf("did not receive upscale.stats event with payload (sawEvent=%v sawData=%v)",
		sawEvent, sawData)
}

// TestEventsTopicFilteringDropsOtherTopics: subscriber with
// ?topics=upscale must NOT see pairing events.
func TestEventsTopicFilteringDropsOtherTopics(t *testing.T) {
	hs, broker, raw, stop := eventsTestServer(t)
	defer stop()

	req, _ := http.NewRequest("GET", hs.URL+"/v1/events?topics=upscale", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	broker.Publish("pairing.abc", map[string]string{"state": "approved"})
	broker.Publish("upscale.stats", map[string]int{"queued": 2})

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(1 * time.Second)
	var sawPairing, sawUpscale bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: pairing") {
			sawPairing = true
		}
		if strings.HasPrefix(line, "event: upscale.stats") {
			sawUpscale = true
		}
		if sawUpscale {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if sawPairing {
		t.Error("subscriber with ?topics=upscale received a pairing event")
	}
	if !sawUpscale {
		t.Error("subscriber missed the upscale event it was subscribed to")
	}
}

// TestParseTopicsParam covers the small parsing helper.
func TestParseTopicsParam(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"upscale", []string{"upscale"}},
		{"upscale,pairing", []string{"upscale", "pairing"}},
		{" upscale , pairing ", []string{"upscale", "pairing"}},
		{",,upscale,,", []string{"upscale"}},
		{"  ,  ", nil},
	}
	for _, tc := range cases {
		got, err := parseTopicsParam(tc.in)
		if err != nil {
			t.Errorf("parseTopicsParam(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseTopicsParam(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseTopicsParam(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
