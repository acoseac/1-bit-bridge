package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

// newPairingTestServer wires an admin Server with a real pairing.Store
// (short TTL so tests don't drag) plus the auth.Store the Approve
// callback hits. Returns the live admin Server, its handler, and the
// pairing Store so tests can seed pending requests directly.
func newPairingTestServer(t *testing.T, fingerprint string) (*Server, http.Handler, *pairing.Store, *auth.Store) {
	t.Helper()
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	cfg := &config.Config{
		LibraryRoots:    []string{lib},
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:7789",
		DataDir:         filepath.Join(tmp, "data"),
		ScanIntervalSec: 3600,
		LibraryName:     "Test",
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mstore, _ := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	t.Cleanup(func() { mstore.Close() })
	astore, _ := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	scanner := manifest.NewScanner(cfg.LibraryRoots, mstore, "")
	resolver := bridgefs.New(cfg.LibraryRoots)

	// TTL/Grace bumped to 5s each (was 100ms/300ms): the handler tests
	// don't wait on TTL expiration, so a tight TTL just exposes the
	// suite to nondeterministic CI scheduling — a slow runner could
	// expire the request between CreateRequest and the handler call,
	// flipping a test from 200 to 404. (Qodo on PR #104.)
	pstore := pairing.NewStore(pairing.Options{
		TTL:         5 * time.Second,
		Grace:       5 * time.Second,
		MaxPending:  4,
		RevokeToken: astore.Revoke,
	})
	t.Cleanup(pstore.Close)

	scanCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(Deps{
		CfgHolder:   config.NewRuntimeConfig(cfg),
		CfgPath:     cfgPath,
		Auth:        astore,
		Manifest:    mstore,
		Scanner:     scanner,
		Resolver:    resolver,
		Fingerprint: fingerprint,
		StartedAt:   time.Now().UTC(),
		Restart:     func() {},
		ScanCtx:     scanCtx,
		Pairing:     pstore,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, srv.Handler(), pstore, astore
}

// seedPending creates a pending request directly via the Store (skips
// the iOS-facing /v1/pairing/requests handler — that's covered in the
// api package's tests).
func seedPending(t *testing.T, ps *pairing.Store, deviceName, fp string) (id, raw string) {
	t.Helper()
	raw = "secret-" + deviceName
	sum := sha256.Sum256([]byte(raw))
	hashHex := hex.EncodeToString(sum[:])
	req, err := ps.CreateRequest(deviceName, "1.4.0", hashHex, "10.0.0.5", fp, "")
	if err != nil {
		t.Fatal(err)
	}
	return req.ID, raw
}

// pairingPost / pairingGet wrap doJSON with the loopback RemoteAddr +
// content-type that the admin csrfGuard requires for POSTs.
func pairingPost(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if out != nil && rw.Body.Len() > 0 {
		_ = json.NewDecoder(rw.Body).Decode(out)
	}
	return rw.Code
}

func pairingGet(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if out != nil && rw.Body.Len() > 0 {
		_ = json.NewDecoder(rw.Body).Decode(out)
	}
	return rw.Code
}

// list ----------------------------------------------------------------

func TestApiPairingListEmptyWhenNoRequests(t *testing.T) {
	_, h, _, _ := newPairingTestServer(t, "FP")
	var rows []pendingPairingRow
	code := pairingGet(t, h, "/api/pairing", &rows)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(rows) != 0 {
		t.Errorf("len = %d, want 0", len(rows))
	}
}

func TestApiPairingListReturnsPendingRow(t *testing.T) {
	_, h, ps, _ := newPairingTestServer(t, "FP")
	id, _ := seedPending(t, ps, "iPhone", "FP")

	var rows []pendingPairingRow
	code := pairingGet(t, h, "/api/pairing", &rows)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if len(rows) != 1 {
		t.Fatalf("len = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ID != id {
		t.Errorf("id = %q, want %q", row.ID, id)
	}
	if row.DeviceName != "iPhone" {
		t.Errorf("deviceName = %q", row.DeviceName)
	}
	if len(row.VerificationCode) != 6 {
		t.Errorf("verificationCode len = %d, want 6", len(row.VerificationCode))
	}
	if row.Status != "pending" {
		t.Errorf("status = %q", row.Status)
	}
	if row.SecondsUntilExpiry < 0 {
		t.Errorf("secondsUntilExpiry = %d, want >= 0", row.SecondsUntilExpiry)
	}
}

func TestApiPairingListNilStore(t *testing.T) {
	// When admin.Deps.Pairing is nil, the handler returns an empty array
	// (not a 503) — the JS-side renderer treats that as "no pending
	// requests", same as a wired-but-empty Store.
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	_ = os.MkdirAll(lib, 0o755)
	cfg := &config.Config{
		LibraryRoots:    []string{lib},
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:7789",
		DataDir:         filepath.Join(tmp, "data"),
		ScanIntervalSec: 3600,
		LibraryName:     "Test",
	}
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	_ = cfg.Save(cfgPath)
	_ = os.MkdirAll(cfg.DataDir, 0o755)
	mstore, _ := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	t.Cleanup(func() { mstore.Close() })
	astore, _ := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	scanner := manifest.NewScanner(cfg.LibraryRoots, mstore, "")
	resolver := bridgefs.New(cfg.LibraryRoots)
	srv, err := New(Deps{
		CfgHolder: config.NewRuntimeConfig(cfg), CfgPath: cfgPath, Auth: astore, Manifest: mstore,
		Scanner: scanner, Resolver: resolver,
		Fingerprint: "FP", StartedAt: time.Now(), Restart: func() {},
		ScanCtx: context.Background(),
		// Pairing intentionally nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows []pendingPairingRow
	code := pairingGet(t, srv.Handler(), "/api/pairing", &rows)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if rows == nil || len(rows) != 0 {
		t.Errorf("nil-store rows = %#v, want empty array", rows)
	}
}

// approve --------------------------------------------------------------

func TestApiPairingApprove(t *testing.T) {
	_, h, ps, astore := newPairingTestServer(t, "FP")
	id, raw := seedPending(t, ps, "iPad", "FP")
	_ = raw

	var resp map[string]any
	code := pairingPost(t, h, "/api/pairing/"+id+"/approve", &resp)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if resp["id"] != id {
		t.Errorf("response id = %v, want %q", resp["id"], id)
	}
	tokenID, _ := resp["tokenId"].(string)
	if tokenID == "" {
		t.Fatal("response missing tokenId")
	}
	// The minted token should be live in the auth store.
	tokens := astore.List()
	if len(tokens) != 1 || tokens[0].ID != tokenID {
		t.Errorf("auth.Store didn't get the minted token: tokens=%v", tokens)
	}
	if tokens[0].Name != "iPad" {
		t.Errorf("minted token name = %q, want iPad", tokens[0].Name)
	}
}

func TestApiPairingApproveCertRotated(t *testing.T) {
	srv, h, ps, astore := newPairingTestServer(t, "OLD-FP")
	id, _ := seedPending(t, ps, "iPad", "OLD-FP")

	// Simulate a cert rotation by mutating the admin Server's deps
	// fingerprint between request creation and Approve.
	srv.deps.Fingerprint = "NEW-FP"
	code := pairingPost(t, h, "/api/pairing/"+id+"/approve", nil)
	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (cert_rotated)", code)
	}
	// No token should have been minted.
	if tokens := astore.List(); len(tokens) != 0 {
		t.Errorf("auth.Store has %d tokens after cert-rotated refusal, want 0", len(tokens))
	}
}

func TestApiPairingApproveAlreadyDecidedReturns409(t *testing.T) {
	_, h, ps, _ := newPairingTestServer(t, "FP")
	id, _ := seedPending(t, ps, "iPad", "FP")
	if c := pairingPost(t, h, "/api/pairing/"+id+"/approve", nil); c != 200 {
		t.Fatalf("first approve: %d", c)
	}
	if c := pairingPost(t, h, "/api/pairing/"+id+"/approve", nil); c != http.StatusConflict {
		t.Errorf("second approve: %d, want 409", c)
	}
}

func TestApiPairingApproveUnknownReturns404(t *testing.T) {
	_, h, _, _ := newPairingTestServer(t, "FP")
	code := pairingPost(t, h, "/api/pairing/deadbeef/approve", nil)
	if code != 404 {
		t.Errorf("status = %d, want 404", code)
	}
}

// decline --------------------------------------------------------------

func TestApiPairingDecline(t *testing.T) {
	_, h, ps, astore := newPairingTestServer(t, "FP")
	id, _ := seedPending(t, ps, "iPad", "FP")
	if c := pairingPost(t, h, "/api/pairing/"+id+"/decline", nil); c != 200 {
		t.Errorf("status = %d", c)
	}
	if tokens := astore.List(); len(tokens) != 0 {
		t.Errorf("decline minted %d tokens, want 0", len(tokens))
	}
}

func TestApiPairingDeclineUnknownReturns404(t *testing.T) {
	_, h, _, _ := newPairingTestServer(t, "FP")
	code := pairingPost(t, h, "/api/pairing/deadbeef/decline", nil)
	if code != 404 {
		t.Errorf("status = %d, want 404", code)
	}
}

// fingerprintSuffix unit -----------------------------------------------

func TestFingerprintSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"AB:CD:EF:01:02:03:FF", "01:02:03:FF"},
		{"AA:BB:CC:DD", "AA:BB:CC:DD"}, // exactly 4 segments — return whole
		{"", ""},
		{"NOPCOLONS", "NOPCOLONS"},
	}
	for _, tc := range cases {
		got := fingerprintSuffix(tc.in)
		if got != tc.want {
			t.Errorf("fingerprintSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
