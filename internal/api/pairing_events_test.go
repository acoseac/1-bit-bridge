package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

// pairingEventsTestSetup is like pairingTestSetup but also starts the
// SSE event broker AND wires pairing.Store.OnStateChange to publish
// to it — so a state transition (Approve, Decline) flows through the
// broker to a `/v1/pairing/{id}/events` subscriber. Mirrors the
// production wiring in cmd/bridge/main.go.
//
// Returns the test server, raw auth-store handle (so a test can mint
// a bearer for non-pairing endpoints if needed), and the pairing
// store (for direct Approve/Decline calls).
func pairingEventsTestSetup(t *testing.T) (*httptest.Server, *auth.Store, *pairing.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{dir},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}

	pairingStore := pairing.NewStore(pairing.Options{
		TTL:        500 * time.Millisecond, // long enough for the SSE-subscribe round-trip
		Grace:      300 * time.Millisecond,
		MaxPending: 4,
		RevokeToken: func(id string) error {
			return authStore.Revoke(id)
		},
	})
	t.Cleanup(pairingStore.Close)

	srv := New(cfg, authStore, nil, "AB:CD:EF:01:02:03:FF").WithPairing(pairingStore)
	stopBroker := srv.StartEventBroker()

	// Production wiring shape from cmd/bridge/main.go: pairing.Store
	// fires OnStateChange → cmd/bridge translates to a broker
	// Publish call. Tests duplicate that translation closure so the
	// pairingEvents subscriber sees the event flow end-to-end.
	bridgeStartedAtUnix := srv.StartedAt().UnixMilli()
	pairingTTL := pairingStore.TTL()
	pairingStore.SetOnStateChange(func(snap pairing.Request) {
		ev := PairingStateEvent{
			Status:           snap.State.String(),
			BridgeStartedAt:  bridgeStartedAtUnix,
			VerificationCode: snap.VerificationCode,
		}
		if snap.State == pairing.StatePending {
			deadline := snap.CreatedAt.Add(pairingTTL)
			rem := time.Until(deadline)
			if rem < 0 {
				rem = 0
			}
			ev.TTLSecondsRemaining = int(rem / time.Second)
		}
		if snap.State == pairing.StateApproved {
			ev.Token = snap.RawToken
			ev.TokenID = snap.TokenID
		}
		srv.EventPublisher().Publish("pairing."+snap.ID, ev)
	})

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, authStore, pairingStore, stopBroker
}

// createPendingRequest is a small helper that POSTs a pairing
// request and returns (requestID, rawPollSecret).
func createPendingRequest(t *testing.T, hs *httptest.Server, label string) (string, string) {
	t.Helper()
	rawSecret := "secret-" + label
	sum := sha256.Sum256([]byte(rawSecret))
	hashHex := hex.EncodeToString(sum[:])

	body, _ := json.Marshal(map[string]string{
		"deviceName":     "Phone-" + label,
		"pollSecretHash": hashHex,
	})
	req, _ := http.NewRequest("POST", hs.URL+"/v1/pairing/requests", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing: status = %d", resp.StatusCode)
	}
	var created pairingCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created.RequestID, rawSecret
}

// connectPairingEvents opens a streaming GET to
// /v1/pairing/{id}/events with the pollSecret as bearer. Returns the
// response (so the caller can read its Body line-by-line via a
// Scanner) and a function to drop the connection.
func connectPairingEvents(t *testing.T, hs *httptest.Server, requestID, pollSecret string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+"/v1/pairing/"+requestID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+pollSecret)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestPairingEventsAuthRejectsMissingSecret — without an
// Authorization header, the endpoint returns 401. iOS treats 401 the
// same as on the polling endpoint (auth bug, surface to user).
func TestPairingEventsAuthRejectsMissingSecret(t *testing.T) {
	hs, _, _, stop := pairingEventsTestSetup(t)
	defer stop()
	id, _ := createPendingRequest(t, hs, "auth-missing")

	req, _ := http.NewRequest("GET", hs.URL+"/v1/pairing/"+id+"/events", nil)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestPairingEventsAuthRejectsWrongSecret — an Authorization header
// with a pollSecret that doesn't hash to the stored value returns
// 401. Same shape as the polling endpoint's auth gate.
func TestPairingEventsAuthRejectsWrongSecret(t *testing.T) {
	hs, _, _, stop := pairingEventsTestSetup(t)
	defer stop()
	id, _ := createPendingRequest(t, hs, "auth-wrong")

	resp := connectPairingEvents(t, hs, id, "wrong-secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestPairingEventsUnknownRequestReturns404 — a request ID that was
// never created returns 404. iOS treats 404 as terminal (gives up
// on this request, falls back to a fresh pair).
func TestPairingEventsUnknownRequestReturns404(t *testing.T) {
	hs, _, _, stop := pairingEventsTestSetup(t)
	defer stop()

	resp := connectPairingEvents(t, hs, "00000000000000000000000000000000", "irrelevant-secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestPairingEventsBrokerNotWiredReturns404 — when StartEventBroker
// hasn't been called (e.g., a config disables push), the endpoint
// returns 404 events_not_supported and iOS falls back to polling.
// Same back-compat shape /v1/events uses for pre-v1.2 bridges.
func TestPairingEventsBrokerNotWiredReturns404(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{dir},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	pairingStore := pairing.NewStore(pairing.Options{
		TTL: time.Second, Grace: time.Second, MaxPending: 4,
		RevokeToken: func(string) error { return nil },
	})
	defer pairingStore.Close()
	// Deliberately do NOT call StartEventBroker.
	srv := New(cfg, authStore, nil, "fp").WithPairing(pairingStore)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	// Need a real pending request so the auth path can succeed —
	// otherwise the test would hit the 404 unknown-request branch
	// instead of the 404 events-not-supported branch we're after.
	id, raw := createPendingRequest(t, hs, "no-broker")
	resp := connectPairingEvents(t, hs, id, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	// Body must specifically say events_not_supported (not the
	// unknown-request 404) so iOS's fallback-to-polling decoder
	// can distinguish.
	var er ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "events_not_supported" {
		t.Errorf("error code = %q, want events_not_supported", er.Error)
	}
}

// TestPairingEventsSendsInitialState — on connect, the handler
// emits the current pairing state as the FIRST SSE event. iOS uses
// this so it doesn't have to wait for the next state change to know
// what state the request is in (load-bearing for the user-foregrounds-
// app-mid-pairing flow).
func TestPairingEventsSendsInitialState(t *testing.T) {
	hs, _, _, stop := pairingEventsTestSetup(t)
	defer stop()
	id, raw := createPendingRequest(t, hs, "initial-state")

	resp := connectPairingEvents(t, hs, id, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read the first event off the wire — should be the pending
	// state. Bounded wait because httptest is in-process and the
	// handler flushes the initial event synchronously after the
	// 200 line.
	scanner := bufio.NewScanner(resp.Body)
	var sawPendingEvent, sawData bool
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: pairing.") && strings.HasSuffix(line, id) {
			sawPendingEvent = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"status":"pending"`) {
			sawData = true
		}
		if sawPendingEvent && sawData {
			return
		}
	}
	t.Errorf("did not receive initial pending event (sawEvent=%v sawData=%v)",
		sawPendingEvent, sawData)
}

// TestPairingEventsPushesApprovalWithToken — an Approve transition
// fires through the broker and lands on the SSE wire with the
// minted token. This is the headline contract: iOS gets the token
// via push, no follow-up GET needed.
func TestPairingEventsPushesApprovalWithToken(t *testing.T) {
	hs, authStore, pairingStore, stop := pairingEventsTestSetup(t)
	defer stop()
	id, raw := createPendingRequest(t, hs, "approve-push")

	resp := connectPairingEvents(t, hs, id, raw)
	defer resp.Body.Close()

	// Drain the initial pending event before we trigger Approve.
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		if scanner.Text() == "" { // blank line terminates the initial event group
			break
		}
	}

	// Trigger Approve — this fires OnStateChange → broker.Publish
	// → SSE handler writes the new event to the wire.
	if _, err := pairingStore.Approve(id, "AB:CD:EF:01:02:03:FF",
		func(name string) (string, string, error) {
			raw, tok, err := authStore.Mint(name)
			return raw, tok.ID, err
		}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Read until we see the approved event with a token. Bounded
	// wait so a regression hangs the test rather than the suite.
	deadline = time.Now().Add(3 * time.Second)
	var sawApproved, sawToken bool
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if strings.Contains(line, `"status":"approved"`) {
			sawApproved = true
		}
		if strings.Contains(line, `"token":"`) {
			sawToken = true
		}
		if sawApproved && sawToken {
			return
		}
	}
	t.Errorf("approval push missed (sawApproved=%v sawToken=%v)", sawApproved, sawToken)
}

// TestPairingEventsResponseIsNotGzipped — same tripwire as
// TestEventsResponseIsNotGzipped on the /v1/events handler. Defends
// against a future global gzip middleware that would buffer the SSE
// response until close (defeating Flush()).
func TestPairingEventsResponseIsNotGzipped(t *testing.T) {
	hs, _, _, stop := pairingEventsTestSetup(t)
	defer stop()
	id, raw := createPendingRequest(t, hs, "no-gzip")

	req, _ := http.NewRequest("GET", hs.URL+"/v1/pairing/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Accept-Encoding", "gzip")

	// Short timeout — we're inspecting headers only.
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, _ := client.Do(req)
	if resp == nil {
		// Connection cancelled by the timeout before headers landed
		// — re-issue with a slightly longer window.
		req2, _ := http.NewRequest("GET", hs.URL+"/v1/pairing/"+id+"/events", nil)
		req2.Header.Set("Authorization", "Bearer "+raw)
		req2.Header.Set("Accept-Encoding", "gzip")
		client2 := &http.Client{Timeout: 500 * time.Millisecond}
		resp, _ = client2.Do(req2)
	}
	if resp == nil {
		t.Fatal("no response received")
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		t.Errorf("Content-Encoding = %q, want identity (or empty)", ce)
	}
}

// Avoid unused-import for fmt in case I refactor the helpers.
var _ = fmt.Sprintf
