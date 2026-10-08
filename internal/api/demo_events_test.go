package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

// demoEventsRawToken is the bearer every demo client shares. The
// digest is what SetStaticToken stores; the raw value is what the
// test sends, the same split production uses.
const demoEventsRawToken = "demo-raw-token-fixture"

// demoEventsServer is a demo bridge with the event broker and the
// pairing store both wired, which is what serve does when
// demo.enabled is set. The static token is the public demo bearer.
func demoEventsServer(t *testing.T) (*httptest.Server, *Server, *auth.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{dir},
		ListenAddress: ":7788",
		LibraryName:   "Demo",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	sum := sha256.Sum256([]byte(demoEventsRawToken))
	if err := authStore.SetStaticToken(hex.EncodeToString(sum[:]), "Demo access (config)"); err != nil {
		t.Fatalf("set static token: %v", err)
	}
	pairingStore := pairing.NewStore(pairing.Options{
		TTL:         30 * time.Second,
		Grace:       time.Second,
		MaxPending:  4,
		RevokeToken: func(id string) error { return authStore.Revoke(id) },
	})
	t.Cleanup(pairingStore.Close)

	srv := New(cfg, authStore, nil, "fp").WithDemoMode(true).WithPairing(pairingStore)
	stopBroker := srv.StartEventBroker()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, srv, authStore, stopBroker
}

func brokerSubscribers(b *eventBroker) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// openEventStream GETs /v1/events and returns the response once the
// headers are in. A 200 is a held SSE stream; the caller closes it.
func openEventStream(ctx context.Context, client *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return client.Do(req)
}

// TestADemoTokenHolderCannotStarvePairingEvents opens one /v1/events
// stream per broker slot with the public demo bearer, then opens a
// pairing stream. The demo streams must be 404 events_not_supported
// and must leave the broker empty, so the pairing stream still
// connects. Holding a 200 would be the slot the pairing stream then
// cannot get.
func TestADemoTokenHolderCannotStarvePairingEvents(t *testing.T) {
	hs, srv, _, stop := demoEventsServer(t)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := &http.Client{}

	type result struct {
		status int
		code   string
		held   *http.Response
	}
	results := make([]result, maxBrokerSubscribers)
	var wg sync.WaitGroup
	wg.Add(maxBrokerSubscribers)
	for i := 0; i < maxBrokerSubscribers; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := openEventStream(ctx, client, hs.URL+"/v1/events", demoEventsRawToken)
			if err != nil {
				results[i].status = -1
				results[i].code = err.Error()
				return
			}
			results[i].status = resp.StatusCode
			if resp.StatusCode == http.StatusNotFound {
				var er ErrorResponse
				if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
					results[i].code = err.Error()
				} else {
					results[i].code = er.Error
				}
				resp.Body.Close()
				return
			}
			results[i].held = resp
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("demo /v1/events streams did not answer")
	}
	t.Cleanup(func() {
		for _, r := range results {
			if r.held != nil {
				r.held.Body.Close()
			}
		}
	})

	var not404, wrongCode, failed int
	for _, r := range results {
		switch {
		case r.status < 0:
			failed++
		case r.status != http.StatusNotFound:
			not404++
		case r.code != "events_not_supported":
			wrongCode++
		}
	}
	if failed != 0 || not404 != 0 || wrongCode != 0 {
		t.Errorf("demo /v1/events: %d failed, %d not 404, %d not events_not_supported (of %d)",
			failed, not404, wrongCode, maxBrokerSubscribers)
	}
	if n := brokerSubscribers(srv.eventBroker); n != 0 {
		t.Errorf("broker held %d subscribers after the demo streams", n)
	}

	id, raw := createPendingRequest(t, hs, "demo-slots")
	resp := connectPairingEvents(t, hs, id, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("pairing stream status = %d, want 200", resp.StatusCode)
	}
}

// TestAMintedTokenOnADemoBridgeGetsNoEventStream pins the refusal to
// the demo posture. A token minted on that bridge is the same route.
func TestAMintedTokenOnADemoBridgeGetsNoEventStream(t *testing.T) {
	hs, srv, store, stop := demoEventsServer(t)
	defer stop()
	raw, _, err := store.Mint("paired-on-demo")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := openEventStream(ctx, hs.Client(), hs.URL+"/v1/events", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Error != "events_not_supported" {
		t.Errorf("error code = %q, want events_not_supported", er.Error)
	}
	if n := brokerSubscribers(srv.eventBroker); n != 0 {
		t.Errorf("broker held %d subscribers", n)
	}
}

// TestAPairedDeviceEventStreamStillSubscribes is the non-demo half:
// a paired device's /v1/events still opens a stream and takes its slot.
func TestAPairedDeviceEventStreamStillSubscribes(t *testing.T) {
	hs, broker, token, stop := eventsTestServer(t)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := openEventStream(ctx, hs.Client(), hs.URL+"/v1/events", token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if n := brokerSubscribers(broker); n != 1 {
		t.Errorf("broker held %d subscribers, want 1", n)
	}
}
