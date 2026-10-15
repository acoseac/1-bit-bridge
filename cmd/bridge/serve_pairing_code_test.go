package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeRedeemsThePairingLinksCode pins the two lines no package test
// can see: runServe hands ONE pairingcode.Store to the console
// (admin.Deps.PairingCodes) and to the v1 API (apiSrv.WithPairingCodes).
// Wired to two stores, or to one of the two halves only, every test in
// internal/admin and internal/api stays green while the console's QR
// carries a code the API has never heard of, and a phone that redeems it
// is refused.
//
// Booting the real server, it does what the app does with a console QR:
// read `token` and `code` from the link the way the app reads a query
// (RFC 3986, a "+" kept as a plus), redeem the code over TLS, and use the
// token that comes back. The token the link carried must be dead
// afterwards, and the code spent.
func TestServeRedeemsThePairingLinksCode(t *testing.T) {
	s := bootPairingServe(t, "")
	mint, linkToken, code := s.mintLink(t)
	stderr := s.stderr
	phone := pairingPhone()
	base := s.base
	if got := listStatus(t, phone, base, linkToken); got != http.StatusOK {
		t.Fatalf("fixture broken: the link's token answers %d before any redemption, want 200", got)
	}

	status, raw := postRedeem(t, phone, base, code)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/pairing/redeem = %d: %s; the console's code is not one the API can "+
			"redeem, so the two are not wired to one store; stderr=%s", status, raw, stderr.String())
	}
	var redeemed struct {
		Token   string `json:"token"`
		TokenID string `json:"tokenId"`
	}
	if err := json.Unmarshal(raw, &redeemed); err != nil {
		t.Fatalf("decode redeem: %v: %s", err, raw)
	}
	if redeemed.TokenID != mint.ID || redeemed.Token == "" || redeemed.Token == linkToken {
		t.Fatalf("redeemed (%q, %q), want a fresh token for the minted record %q", redeemed.TokenID, redeemed.Token, mint.ID)
	}
	if got := listStatus(t, phone, base, redeemed.Token); got != http.StatusOK {
		t.Errorf("the redeemed token answers %d, want 200", got)
	}
	if got := listStatus(t, phone, base, linkToken); got != http.StatusUnauthorized {
		t.Errorf("the link's token answers %d after the redemption, want 401", got)
	}
	if status, raw := postRedeem(t, phone, base, code); status != http.StatusGone {
		t.Errorf("a second redemption of the code = %d: %s, want 410", status, raw)
	}
}

// TestServeRefusesTheCodeOfATokenTheCLIRotated pins backlog B160 where it
// happens. `bridge token rotate` is another process, with its own auth.Store
// on the same tokens.json, so it cannot reach the serving process's code
// store. Its rotation must still end the old QR's code: redeeming that code
// rotated the token AGAIN, handing whoever holds the old QR (the leak the
// operator rotated for) a working secret, while the secret the CLI printed
// stopped validating.
func TestServeRefusesTheCodeOfATokenTheCLIRotated(t *testing.T) {
	s := bootPairingServe(t, "")
	mint, linkToken, code := s.mintLink(t)

	var out, errOut bytes.Buffer
	if rc := tokenRotateCmd([]string{"--config", s.cfgPath, mint.ID}, &out, &errOut); rc != 0 {
		t.Fatalf("bridge token rotate = %d: %s", rc, errOut.String())
	}
	cliToken := printedToken(t, out.String())

	phone := pairingPhone()
	status, raw := postRedeem(t, phone, s.base, code)
	if status != http.StatusGone || !strings.Contains(string(raw), "pairing_code_invalid") {
		t.Errorf("redeeming the old QR's code after a CLI rotation = %d: %s, want 410 pairing_code_invalid", status, raw)
	}
	if strings.Contains(string(raw), `"token"`) {
		t.Errorf("the redemption handed over a token: %s", raw)
	}
	if got := listStatus(t, phone, s.base, cliToken); got != http.StatusOK {
		t.Errorf("the token the CLI printed answers %d, want 200: the redemption rotated it again", got)
	}
	if got := listStatus(t, phone, s.base, linkToken); got != http.StatusUnauthorized {
		t.Errorf("the link's token answers %d after the CLI rotation, want 401", got)
	}
}

// pairingServe is a running `bridge serve` for the pairing-link tests: its
// config file (for the CLI's commands), its LAN base URL and its admin
// address. The server stops at the test's cleanup.
//
// Both ports are real listeners, held open and handed to serve (backlog
// B313): ReachableEndpoints answers nil for a `:0` listen address, which
// would make health and the QR trivially agree on an empty list. Shutdown is
// a cleanup, not a tail (drainServeOnCleanup has the reasoning), and every
// pairing boot test reads this one boot so they cannot drift apart.
type pairingServe struct {
	cfgPath, base, admin string
	ctx                  context.Context
	stderr               *safeBuffer
}

// bootPairingServe boots serve on a fresh library and data directory, with
// extraConfig (YAML lines, or "") appended to its config.
func bootPairingServe(t *testing.T, extraConfig string) pairingServe {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	lan, admin := holdLoopback(t), holdLoopback(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nadminAddress: %s\n",
		lib, filepath.Join(dir, "data"), admin.addr) + extraConfig
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{
			configPath: cfgPath, addrOverride: lan.addr,
			lanListener: lan.ln, adminListener: admin.ln,
		}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	addr, _ := waitForListening(t, stdout, exited, done, stderr)
	waitForAdminReady(t, admin.addr, done, stderr)
	return pairingServe{cfgPath: cfgPath, base: "https://" + addr, admin: admin.addr, ctx: ctx, stderr: stderr}
}

// mintLink mints a token in the console, as the operator does for a new
// device, and returns the record with the token and code its link carries.
func (s pairingServe) mintLink(t *testing.T) (mint pairResponse, linkToken, code string) {
	t.Helper()
	mint = pairViaAdmin(t, s.ctx, &http.Client{Timeout: 10 * time.Second},
		"http://"+s.admin+"/api/tokens",
		`{"name":"boot test"}`, http.StatusCreated, s.stderr)
	items := linkQueryItems(t, mint.PairURL)
	linkToken, code = items["token"], items["code"]
	if linkToken == "" || code == "" {
		t.Fatalf("the console's link carries token=%q code=%q, want both: %s", linkToken, code, mint.PairURL)
	}
	return mint, linkToken, code
}

// pairingPhone is the app's HTTPS client, minus the pin.
func pairingPhone() *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

// printedToken is the bearer token `bridge token rotate` printed: the line
// after its "New bearer token" header.
func printedToken(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "New bearer token") && i+1 < len(lines) {
			if tok := strings.TrimSpace(lines[i+1]); tok != "" {
				return tok
			}
		}
	}
	t.Fatalf("no token in the rotate output: %q", out)
	return ""
}

// linkQueryItems reads a pairing link's query the way the app does
// (Foundation's URLComponents.queryItems): split on "&" and "=", then
// percent-decode each part, keeping a "+" as a plus.
func linkQueryItems(t *testing.T, link string) map[string]string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	out := map[string]string{}
	for _, pair := range strings.Split(u.RawQuery, "&") {
		k, v, _ := strings.Cut(pair, "=")
		key, kerr := url.PathUnescape(k)
		value, verr := url.PathUnescape(v)
		if kerr != nil || verr != nil {
			t.Fatalf("the link's %q does not percent-decode", pair)
		}
		out[key] = value
	}
	return out
}

// listStatus is the status GET /v1/list answers a request bearing token.
func listStatus(t *testing.T, client *http.Client, base, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/list?path=", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/list: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// postRedeem posts code to POST /v1/pairing/redeem with no bearer, as the
// app does, and returns the status and body.
func postRedeem(t *testing.T, client *http.Client, base, code string) (int, []byte) {
	t.Helper()
	resp, err := client.Post(base+"/v1/pairing/redeem", "application/json",
		strings.NewReader(`{"code":"`+code+`"}`))
	if err != nil {
		t.Fatalf("POST /v1/pairing/redeem: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read redeem body: %v", err)
	}
	return resp.StatusCode, raw
}
