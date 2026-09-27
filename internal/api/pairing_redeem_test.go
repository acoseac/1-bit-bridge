package api

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/pairingcode"
)

// redeemFixture is a bridge with one paired device (its token minted as the
// console mints one for a pairing link) and the code the link carries.
type redeemFixture struct {
	srv      *Server
	store    *auth.Store
	codes    *pairingcode.Store
	linkRaw  string // the token the link carries
	tokenID  string
	linkCode string // the code the link carries
}

func newRedeemFixture(t *testing.T, wire bool) redeemFixture {
	t.Helper()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := store.Mint("Phone")
	if err != nil {
		t.Fatal(err)
	}
	codes := pairingcode.New()
	code, err := codes.Issue(tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp")
	if wire {
		srv.WithPairingCodes(codes)
	}
	return redeemFixture{srv: srv, store: store, codes: codes, linkRaw: raw, tokenID: tok.ID, linkCode: code}
}

// redeem posts body to the redeem route with no bearer, as the app does,
// and returns the response with its body read and closed.
func redeem(t *testing.T, srv *Server, body string) (*http.Response, string) {
	t.Helper()
	resp := doReq(t, srv, http.MethodPost, "/v1/pairing/redeem", "", "", body)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func codeBody(code string) string { return `{"code":"` + code + `"}` }

// TestPairingRedeemSwapsTheLinkTokenForAFreshOne pins the exchange the
// pairing link's code exists for: no bearer is needed, the answer is a token
// for the same device record, and the token the link carried stops working
// in the same step while the new one works.
func TestPairingRedeemSwapsTheLinkTokenForAFreshOne(t *testing.T) {
	f := newRedeemFixture(t, true)
	resp, body := redeem(t, f.srv, codeBody(f.linkCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redeem = %d %s, want 200", resp.StatusCode, body)
	}
	var got pairingRedeemResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.TokenID != f.tokenID {
		t.Fatalf("tokenId = %q, want the link token's record %q", got.TokenID, f.tokenID)
	}
	if got.Token == "" || got.Token == f.linkRaw {
		t.Fatalf("token = %q: want a fresh token, not the one the link carried", got.Token)
	}
	if tok, ok := f.store.Validate(got.Token); !ok || tok.ID != f.tokenID || tok.Name != "Phone" {
		t.Fatalf("the redeemed token does not validate as the device's record (ok=%v tok=%+v)", ok, tok)
	}
	if _, ok := f.store.Validate(f.linkRaw); ok {
		t.Fatal("the token the link carried still validates after the code was redeemed")
	}
}

// TestPairingRedeemIsSingleUse pins that a code is redeemed once: a second
// presentation (a copy of the link used after the device) is refused, and
// the device's token from the first redemption keeps working.
func TestPairingRedeemIsSingleUse(t *testing.T) {
	f := newRedeemFixture(t, true)
	first, body := redeem(t, f.srv, codeBody(f.linkCode))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first redeem = %d %s", first.StatusCode, body)
	}
	var got pairingRedeemResponse
	_ = json.Unmarshal([]byte(body), &got)

	second, body2 := redeem(t, f.srv, codeBody(f.linkCode))
	if second.StatusCode != http.StatusGone || !strings.Contains(body2, "pairing_code_invalid") {
		t.Fatalf("second redeem = %d %s, want 410 pairing_code_invalid", second.StatusCode, body2)
	}
	if _, ok := f.store.Validate(got.Token); !ok {
		t.Fatal("a refused second redemption invalidated the device's token")
	}
}

// TestPairingRedeemRefusals pins every refusal and that none of them rotates
// anything: a bridge without codes (404), a body that is not JSON or a code
// of the wrong shape (400), a code nobody issued, and a code whose token the
// operator revoked, or that has expired, since (all 410, the same answer,
// and the revoked case consumes the code).
func TestPairingRedeemRefusals(t *testing.T) {
	t.Run("not wired", func(t *testing.T) {
		f := newRedeemFixture(t, false)
		resp, body := redeem(t, f.srv, codeBody(f.linkCode))
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "pairing_code_not_supported") {
			t.Fatalf("= %d %s, want 404 pairing_code_not_supported", resp.StatusCode, body)
		}
	})
	for _, tc := range []struct{ name, body string }{
		{"not JSON", "code=abc"},
		{"no code", `{}`},
		{"short code", codeBody("abc")},
		{"not base64url", codeBody(strings.Repeat("+", 43))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeemFixture(t, true)
			resp, body := redeem(t, f.srv, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("= %d %s, want 400", resp.StatusCode, body)
			}
			if _, ok := f.store.Validate(f.linkRaw); !ok {
				t.Fatal("a refused redeem rotated the link token")
			}
		})
	}
	t.Run("never issued", func(t *testing.T) {
		f := newRedeemFixture(t, true)
		resp, body := redeem(t, f.srv, codeBody(strings.Repeat("A", 43)))
		if resp.StatusCode != http.StatusGone || !strings.Contains(body, "pairing_code_invalid") {
			t.Fatalf("= %d %s, want 410 pairing_code_invalid", resp.StatusCode, body)
		}
		if _, ok := f.store.Validate(f.linkRaw); !ok {
			t.Fatal("an unknown code rotated the link token")
		}
	})
	t.Run("token expired since", func(t *testing.T) {
		f := newRedeemFixture(t, true)
		past := time.Now().Add(-time.Minute)
		if _, err := f.store.SetExpiry(f.tokenID, &past); err != nil {
			t.Fatal(err)
		}
		resp, body := redeem(t, f.srv, codeBody(f.linkCode))
		if resp.StatusCode != http.StatusGone || !strings.Contains(body, "pairing_code_invalid") {
			t.Fatalf("= %d %s, want 410 pairing_code_invalid", resp.StatusCode, body)
		}
		if strings.Contains(body, `"token"`) {
			t.Fatalf("a refused redemption handed over a token: %s", body)
		}
	})
	t.Run("token revoked since", func(t *testing.T) {
		f := newRedeemFixture(t, true)
		if err := f.store.Revoke(f.tokenID); err != nil {
			t.Fatal(err)
		}
		resp, body := redeem(t, f.srv, codeBody(f.linkCode))
		if resp.StatusCode != http.StatusGone || !strings.Contains(body, "pairing_code_invalid") {
			t.Fatalf("= %d %s, want 410 pairing_code_invalid", resp.StatusCode, body)
		}
		if _, ok := f.codes.Take(f.linkCode); ok {
			t.Fatal("the refused redemption left the code redeemable")
		}
	})
}

// TestPairingRedeemIsRateLimitedPerIP pins that the unauthenticated route
// draws from the per-IP pairing limiter: past its burst a caller is told to
// wait, before its body is read.
func TestPairingRedeemIsRateLimitedPerIP(t *testing.T) {
	f := newRedeemFixture(t, true)
	sawLimit := false
	for i := 0; i < pairingRateBurst+1; i++ {
		resp, body := redeem(t, f.srv, codeBody(strings.Repeat("B", 43)))
		if resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "rate_limited") {
				t.Fatalf("429 without Retry-After or rate_limited: %v %s", resp.Header, body)
			}
			sawLimit = true
			break
		}
	}
	if !sawLimit {
		t.Fatalf("%d redemptions from one address in a burst were never limited", pairingRateBurst+1)
	}
}
