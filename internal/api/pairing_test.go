package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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

// pairingTestSetup wires a Server with a real auth.Store + pairing.Store
// (short TTL so timer-driven cases don't make the test slow) and returns
// an httptest.Server. The fingerprint matches what the cert-rotation
// guard tests need.
func pairingTestSetup(t *testing.T) (*httptest.Server, *auth.Store, *pairing.Store) {
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
		TTL:        100 * time.Millisecond,
		Grace:      300 * time.Millisecond,
		MaxPending: 4,
		RevokeToken: func(id string) error {
			return authStore.Revoke(id)
		},
	})
	t.Cleanup(pairingStore.Close)

	srv := New(cfg, authStore, nil, "AB:CD:EF:01:02:03:FF").WithPairing(pairingStore)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, authStore, pairingStore
}

func makePollSecret(t *testing.T, label string) (raw, hashHex string) {
	t.Helper()
	raw = "secret-" + label
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:])
}

func postPairing(t *testing.T, hs *httptest.Server, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", hs.URL+"/v1/pairing/requests", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getPairing(t *testing.T, hs *httptest.Server, id, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+"/v1/pairing/"+id, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func deletePairing(t *testing.T, hs *httptest.Server, id, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("DELETE", hs.URL+"/v1/pairing/"+id, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	return out
}

// happy path -----------------------------------------------------------

func TestPairingCreatePollDelete(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	raw, hashHex := makePollSecret(t, "happy")

	resp := postPairing(t, hs, map[string]string{
		"deviceName":     "iPhone",
		"clientVersion":  "1.4.0",
		"pollSecretHash": hashHex,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	created := decodeJSON[pairingCreateResponse](t, resp)
	if created.RequestID == "" {
		t.Error("empty requestId")
	}
	if len(created.VerificationCode) != 6 {
		t.Errorf("verificationCode len = %d, want 6", len(created.VerificationCode))
	}
	if created.TTLSeconds <= 0 {
		t.Errorf("ttlSeconds = %d, want > 0", created.TTLSeconds)
	}
	if created.BridgeStartedAt <= 0 {
		t.Errorf("bridgeStartedAt = %d, want > 0", created.BridgeStartedAt)
	}

	// Poll while pending.
	pollResp := getPairing(t, hs, created.RequestID, raw)
	defer pollResp.Body.Close()
	if pollResp.StatusCode != 200 {
		t.Fatalf("Poll status = %d, want 200", pollResp.StatusCode)
	}
	poll := decodeJSON[pairingPollResponse](t, pollResp)
	if poll.Status != "pending" {
		t.Errorf("status = %q, want pending", poll.Status)
	}
	if poll.Token != "" {
		t.Errorf("token = %q on pending, want empty", poll.Token)
	}
	if poll.BridgeStartedAt != created.BridgeStartedAt {
		t.Errorf("bridgeStartedAt drift: created=%d poll=%d", created.BridgeStartedAt, poll.BridgeStartedAt)
	}

	// Cancel via DELETE.
	delResp := deletePairing(t, hs, created.RequestID, raw)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", delResp.StatusCode)
	}
}

func TestPairingApprovedTokenDeliveryReadMany(t *testing.T) {
	// Locks the read-many delivery contract: a network blip that drops
	// the 200 OK with the token should be recoverable on the next poll.
	hs, authStore, pairingStore := pairingTestSetup(t)
	raw, hashHex := makePollSecret(t, "approved")

	resp := postPairing(t, hs, map[string]string{
		"deviceName":     "iPad",
		"pollSecretHash": hashHex,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
	created := decodeJSON[pairingCreateResponse](t, resp)

	// Admin Approve via the Store directly (the admin HTTP handler
	// lives in PR #2 — exercise the wire side of the contract here).
	mint := func(name string) (string, string, error) {
		raw, tok, err := authStore.Mint(name)
		if err != nil {
			return "", "", err
		}
		return raw, tok.ID, nil
	}
	if _, err := pairingStore.Approve(created.RequestID, "AB:CD:EF:01:02:03:FF", mint); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Two consecutive polls — both must surface the same token.
	var firstToken string
	for i := 0; i < 2; i++ {
		pollResp := getPairing(t, hs, created.RequestID, raw)
		defer pollResp.Body.Close()
		if pollResp.StatusCode != 200 {
			t.Fatalf("Poll #%d status = %d", i, pollResp.StatusCode)
		}
		poll := decodeJSON[pairingPollResponse](t, pollResp)
		if poll.Status != "approved" {
			t.Errorf("Poll #%d status = %q, want approved", i, poll.Status)
		}
		if poll.Token == "" {
			t.Errorf("Poll #%d token empty (read-many contract)", i)
		}
		if poll.TokenID == "" {
			t.Errorf("Poll #%d tokenId empty", i)
		}
		if i == 0 {
			firstToken = poll.Token
		} else if poll.Token != firstToken {
			t.Errorf("Poll #%d token drift: %q -> %q", i, firstToken, poll.Token)
		}
	}

	// And the delivered token actually validates against the auth store.
	if _, ok := authStore.Validate(firstToken); !ok {
		t.Error("delivered token does not validate against auth store")
	}

	// iOS sends DELETE as receipt acknowledgment.
	delResp := deletePairing(t, hs, created.RequestID, raw)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Errorf("ACK DELETE: %d", delResp.StatusCode)
	}
}

// auth + error mapping -------------------------------------------------

func TestPairingPollUnauthorized(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	_, hashHex := makePollSecret(t, "auth")
	resp := postPairing(t, hs, map[string]string{
		"deviceName":     "iPhone",
		"pollSecretHash": hashHex,
	})
	defer resp.Body.Close()
	created := decodeJSON[pairingCreateResponse](t, resp)

	// No bearer.
	r1 := getPairing(t, hs, created.RequestID, "")
	defer r1.Body.Close()
	if r1.StatusCode != 401 {
		t.Errorf("no bearer: status = %d, want 401", r1.StatusCode)
	}
	// Wrong bearer.
	r2 := getPairing(t, hs, created.RequestID, "wrong-secret")
	defer r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Errorf("wrong bearer: status = %d, want 401", r2.StatusCode)
	}
}

func TestPairingPollUnknownReturns404(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	r := getPairing(t, hs, "deadbeefdead", "anysecret")
	defer r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("unknown id: status = %d, want 404", r.StatusCode)
	}
}

func TestPairingCreateBadHash(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	cases := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"too short", "abc"},
		{"non-hex", strings.Repeat("Z", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := postPairing(t, hs, map[string]string{
				"deviceName":     "x",
				"pollSecretHash": tc.hash,
			})
			defer r.Body.Close()
			if r.StatusCode != 400 {
				t.Errorf("status = %d, want 400", r.StatusCode)
			}
		})
	}
}

func TestPairingCreateMissingDeviceName(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	_, hashHex := makePollSecret(t, "noname")
	r := postPairing(t, hs, map[string]string{
		"pollSecretHash": hashHex,
	})
	defer r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("status = %d, want 400", r.StatusCode)
	}
}

func TestPairingCreateQueueFull(t *testing.T) {
	hs, _, _ := pairingTestSetup(t) // MaxPending = 4
	for i := 0; i < 4; i++ {
		_, hashHex := makePollSecret(t, "q"+string(rune('a'+i)))
		r := postPairing(t, hs, map[string]string{
			"deviceName":     "x",
			"pollSecretHash": hashHex,
		})
		defer r.Body.Close()
		if r.StatusCode != 201 {
			t.Fatalf("create #%d: status = %d", i, r.StatusCode)
		}
	}
	_, hashHex := makePollSecret(t, "overflow")
	r := postPairing(t, hs, map[string]string{
		"deviceName":     "x",
		"pollSecretHash": hashHex,
	})
	defer r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("overflow status = %d, want 503", r.StatusCode)
	}
}

func TestPairingDeleteIdempotent(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	raw, hashHex := makePollSecret(t, "del")
	r := postPairing(t, hs, map[string]string{
		"deviceName":     "x",
		"pollSecretHash": hashHex,
	})
	defer r.Body.Close()
	created := decodeJSON[pairingCreateResponse](t, r)

	first := deletePairing(t, hs, created.RequestID, raw)
	defer first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Errorf("first DELETE: %d", first.StatusCode)
	}
	// Second DELETE must succeed (idempotent ack).
	second := deletePairing(t, hs, created.RequestID, raw)
	defer second.Body.Close()
	if second.StatusCode != http.StatusNoContent {
		t.Errorf("second DELETE: %d, want 204 (idempotent)", second.StatusCode)
	}
}

func TestPairingDeleteUnauthorizedStaysAuthorized(t *testing.T) {
	hs, _, _ := pairingTestSetup(t)
	raw, hashHex := makePollSecret(t, "delauth")
	r := postPairing(t, hs, map[string]string{
		"deviceName":     "x",
		"pollSecretHash": hashHex,
	})
	defer r.Body.Close()
	created := decodeJSON[pairingCreateResponse](t, r)

	bad := deletePairing(t, hs, created.RequestID, "wrong")
	defer bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Errorf("wrong secret DELETE: %d, want 401", bad.StatusCode)
	}

	// Row should still exist — verify with a poll.
	good := getPairing(t, hs, created.RequestID, raw)
	defer good.Body.Close()
	if good.StatusCode != 200 {
		t.Errorf("row missing after bad-auth delete: poll status = %d", good.StatusCode)
	}
}

// no-store path --------------------------------------------------------

func TestPairingNotSupportedWhenStoreUnwired(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	srv := New(cfg, authStore, nil, "FP") // no .WithPairing(...)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	_, hashHex := makePollSecret(t, "ns")
	resp := postPairing(t, hs, map[string]string{
		"deviceName":     "x",
		"pollSecretHash": hashHex,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404 (pairing_not_supported)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pairing_not_supported") {
		t.Errorf("body should carry pairing_not_supported short-code, got: %s", body)
	}
}

// cert-rotation guard via the wire ------------------------------------

func TestPairingCertRotationSurfacesViaPoll(t *testing.T) {
	hs, _, pairingStore := pairingTestSetup(t)
	raw, hashHex := makePollSecret(t, "rot")
	r := postPairing(t, hs, map[string]string{
		"deviceName":     "iPhone",
		"pollSecretHash": hashHex,
	})
	defer r.Body.Close()
	created := decodeJSON[pairingCreateResponse](t, r)

	// Admin attempts approve with a different fingerprint — Store
	// transitions to CertRotated and refuses the mint.
	mint := func(name string) (string, string, error) {
		t.Errorf("mint must not be called when cert rotated")
		return "", "", nil
	}
	_, err := pairingStore.Approve(created.RequestID, "DIFFERENT-FP", mint)
	if err == nil {
		t.Fatal("Approve should refuse with rotated cert")
	}

	pollResp := getPairing(t, hs, created.RequestID, raw)
	defer pollResp.Body.Close()
	if pollResp.StatusCode != 200 {
		t.Fatalf("Poll: %d", pollResp.StatusCode)
	}
	poll := decodeJSON[pairingPollResponse](t, pollResp)
	if poll.Status != "cert_rotated" {
		t.Errorf("status = %q, want cert_rotated", poll.Status)
	}
	if poll.Token != "" {
		t.Errorf("token surfaced under cert_rotated: %q", poll.Token)
	}
}
