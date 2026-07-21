package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	body, err := json.Marshal(map[string]string{
		"deviceName":     "Phone-" + label,
		"pollSecretHash": hashHex,
	})
	if err != nil {
		t.Fatalf("marshal pairing request body: %v", err)
	}
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

// pairingEventsRequestTimeout governs the per-test SSE connect
// context. Bumped from 5 s → 10 s on CodeRabbit round-4: the
// approval-push test legitimately consumes up to 5 s in its drain +
// approval-wait loops, leaving no scheduling slack on slower CI;
// 10 s gives ~5 s headroom while still failing fast on a wedged
// handler.
const pairingEventsRequestTimeout = 10 * time.Second

// connectPairingEvents opens a streaming GET to
// /v1/pairing/{id}/events with the pollSecret as bearer. Returns the
// response (so the caller can read its Body line-by-line via a
// Scanner) and a function to drop the connection.
//
// The context timeout protects the test suite from a hung handler
// that never responds to the initial request — without it, a
// regression in the SSE path could deadlock CI for the full test
// timeout window. CodeRabbit caught this on PR #137. The downstream
// Scanner loops have their own deadline checks for the streaming-read
// phase; this protects the connect phase only.
func connectPairingEvents(t *testing.T, hs *httptest.Server, requestID, pollSecret string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pairingEventsRequestTimeout)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/v1/pairing/"+requestID+"/events", nil)
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
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
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
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
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
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE stream: %v", err)
	}
	t.Errorf("did not receive initial pending event (sawEvent=%v sawData=%v)",
		sawPendingEvent, sawData)
}

// TestPairingEventsInitialEventCarriesIDZero — the synthetic
// initial-state event uses `id: 0` so iOS can distinguish it from
// real broker-published events (broker IDs are monotonic from 1+).
// Empty `id:` would omit the field entirely per writeEvent's
// contract — Gemini caught the comment/code mismatch on PR #137's
// first commit.
func TestPairingEventsInitialEventCarriesIDZero(t *testing.T) {
	hs, _, _, stop := pairingEventsTestSetup(t)
	defer stop()
	id, raw := createPendingRequest(t, hs, "initial-id-zero")

	resp := connectPairingEvents(t, hs, id, raw)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	var sawIDZero bool
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if line == "id: 0" {
			sawIDZero = true
		}
		if line == "" && sawIDZero {
			// Blank line terminates the initial event group; if we
			// saw `id: 0` before this terminator, the contract holds.
			return
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE stream: %v", err)
	}
	t.Errorf("initial event did not carry `id: 0` (sawIDZero=%v)", sawIDZero)
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
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE stream: %v", err)
	}
	t.Errorf("approval push missed (sawApproved=%v sawToken=%v)", sawApproved, sawToken)
}

// TestPairingApprovalSharedBusRedactsSecrets — the 2026-07-21 review
// H2 regression pin. The SAME approval transition is observed on both
// SSE surfaces:
//
//   - the shared bearer-authed /v1/events bus (subscribable by ANY
//     paired device via ?topics=pairing) MUST carry the state
//     transition but MUST NOT carry `token` / `tokenId` /
//     `verificationCode` — otherwise one leaked device token harvests
//     every future device's credentials;
//   - the pollSecret-gated /v1/pairing/{id}/events channel MUST still
//     deliver the minted token (that gate is the sanctioned
//     token-delivery path).
func TestPairingApprovalSharedBusRedactsSecrets(t *testing.T) {
	hs, authStore, pairingStore, stop := pairingEventsTestSetup(t)
	defer stop()
	id, raw := createPendingRequest(t, hs, "shared-bus-redact")

	// Shared-bus subscriber — an arbitrary bearer holder, NOT the
	// pairing device (it never sees the pollSecret).
	busToken, _, err := authStore.Mint("bus-subscriber")
	if err != nil {
		t.Fatalf("mint bus bearer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairingEventsRequestTimeout)
	t.Cleanup(cancel)
	busReq, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/v1/events?topics=pairing", nil)
	busReq.Header.Set("Authorization", "Bearer "+busToken)
	busResp, err := hs.Client().Do(busReq)
	if err != nil {
		t.Fatal(err)
	}
	defer busResp.Body.Close()
	if busResp.StatusCode != http.StatusOK {
		t.Fatalf("bus subscribe: status = %d, want 200", busResp.StatusCode)
	}

	// Dedicated pollSecret-gated channel for the pairing device itself.
	dedResp := connectPairingEvents(t, hs, id, raw)
	defer dedResp.Body.Close()
	// Drain the initial pending event (blank-line terminated) so the
	// assertions below only see the approval transition.
	dedScanner := bufio.NewScanner(dedResp.Body)
	drainDeadline := time.Now().Add(2 * time.Second)
	for dedScanner.Scan() && time.Now().Before(drainDeadline) {
		if dedScanner.Text() == "" {
			break
		}
	}

	// Trigger Approve — fires OnStateChange → broker.Publish → both
	// subscribers receive the same underlying envelope.
	if _, err := pairingStore.Approve(id, "AB:CD:EF:01:02:03:FF",
		func(name string) (string, string, error) {
			raw, tok, err := authStore.Mint(name)
			return raw, tok.ID, err
		}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Shared bus: the approved transition MUST arrive, stripped of all
	// three secret fields.
	busScanner := bufio.NewScanner(busResp.Body)
	deadline := time.Now().Add(3 * time.Second)
	var sawApproved, sawSecret bool
	for busScanner.Scan() && time.Now().Before(deadline) {
		line := busScanner.Text()
		if strings.Contains(line, `"status":"approved"`) {
			sawApproved = true
		}
		if strings.Contains(line, `"token"`) || strings.Contains(line, `"tokenId"`) ||
			strings.Contains(line, `"verificationCode"`) {
			sawSecret = true
		}
		if sawApproved {
			break
		}
	}
	if err := busScanner.Err(); err != nil {
		t.Fatalf("read bus stream: %v", err)
	}
	if !sawApproved {
		t.Error("shared bus subscriber missed the approved transition")
	}
	if sawSecret {
		t.Error("shared bus carried token/tokenId/verificationCode — H2 regression")
	}

	// Dedicated channel: the SAME transition MUST still carry the
	// minted token (the pollSecret gate is what authorises it).
	deadline = time.Now().Add(3 * time.Second)
	var dedApproved, dedToken bool
	for dedScanner.Scan() && time.Now().Before(deadline) {
		line := dedScanner.Text()
		if strings.Contains(line, `"status":"approved"`) {
			dedApproved = true
		}
		if strings.Contains(line, `"token":"`) {
			dedToken = true
		}
		if dedApproved && dedToken {
			break
		}
	}
	if err := dedScanner.Err(); err != nil {
		t.Fatalf("read dedicated stream: %v", err)
	}
	if !dedApproved || !dedToken {
		t.Errorf("dedicated channel lost the token delivery (approved=%v token=%v)",
			dedApproved, dedToken)
	}
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
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		// Connection cancelled by the timeout before headers landed
		// — re-issue with a slightly longer window. Capturing the
		// first error in `firstErr` so a re-issue failure can
		// surface both diagnoses on a true network problem.
		firstErr := err
		req2, _ := http.NewRequest("GET", hs.URL+"/v1/pairing/"+id+"/events", nil)
		req2.Header.Set("Authorization", "Bearer "+raw)
		req2.Header.Set("Accept-Encoding", "gzip")
		client2 := &http.Client{Timeout: 500 * time.Millisecond}
		resp, err = client2.Do(req2)
		if err != nil {
			t.Fatalf("retry failed: %v (first attempt: %v)", err, firstErr)
		}
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
