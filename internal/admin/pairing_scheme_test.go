package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/pairingcode"
)

// TestThePairingLinkIsOnebitPairWithTheQueryItAlwaysCarried pins the whole
// link for one input. Its scheme is onebit (B627): iOS 27 hands a bridge://
// URL to Apple's Watch app (com.apple.Bridge), so the Camera opened that
// and not 1-bit, while 1-bit 2.2 takes onebit://pair exactly as it takes
// bridge://pair (the app's BridgePairingURL.pairingSchemes, its #2369).
// Everything after the scheme is byte for byte what buildPairURL wrote
// behind bridge:// for this input, captured at f6782cd3: the host `pair`,
// no path, and the same query, so nothing but the scheme moved.
func TestThePairingLinkIsOnebitPairWithTheQueryItAlwaysCarried(t *testing.T) {
	const primary = "https://nuc.local:7788"
	got := buildPairURL(primary,
		"AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE",
		"ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZyXwA",
		"AB:CD:EF:01:23:45:67:89",
		"My Library",
		[]string{primary, "https://192.168.50.24:7788", "https://nuc.tail0123.ts.net:7788"})
	const want = "onebit://pair?code=ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZyXwA" +
		"&fingerprint=AB%3ACD%3AEF%3A01%3A23%3A45%3A67%3A89" +
		"&name=My%20Library" +
		"&token=AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE" +
		"&url=https%3A%2F%2Fnuc.local%3A7788" +
		"&urls=https%3A%2F%2Fnuc.local%3A7788%0Ahttps%3A%2F%2F192.168.50.24%3A7788%0Ahttps%3A%2F%2Fnuc.tail0123.ts.net%3A7788"
	if got != want {
		t.Errorf("buildPairURL =\n  %s\nwant\n  %s", got, want)
	}
}

// TestMintAndRotateQRsCarryAnOnebitPairLink drives the two handlers that
// make a pairing QR, the mint and the rotation. Each answer's pairURL is
// onebit://pair and its query, and its QR is that link and nothing else:
// the PNG the handler sent is the one qrDataURL renders from the pairURL
// beside it, and go-qrcode renders one text to one image. So the code the
// Camera reads opens 1-bit, not the Watch app.
func TestMintAndRotateQRsCarryAnOnebitPairLink(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.PairingCodes = pairingcode.New()
	h := srv.Handler()

	var mint pairResult
	if code := doJSON(t, h, "POST", "/api/tokens", map[string]string{"name": "iPhone"}, &mint); code != http.StatusCreated {
		t.Fatalf("mint: %d", code)
	}
	assertQRCarriesAnOnebitPairLink(t, "mint", mint)

	var rotated pairResult
	if code := doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate", map[string]string{}, &rotated); code != http.StatusOK {
		t.Fatalf("rotate: %d", code)
	}
	assertQRCarriesAnOnebitPairLink(t, "rotate", rotated)
}

// assertQRCarriesAnOnebitPairLink fails unless res's pairURL is
// onebit://pair with a query and no other part, and res's QR is exactly
// that pairURL.
func assertQRCarriesAnOnebitPairLink(t *testing.T, op string, res pairResult) {
	t.Helper()
	u, err := url.Parse(res.PairURL)
	if err != nil {
		t.Fatalf("%s: parse %q: %v", op, res.PairURL, err)
	}
	if u.Scheme != "onebit" || u.Host != "pair" || u.Path != "" || u.Opaque != "" ||
		u.User != nil || u.Fragment != "" || u.RawQuery == "" ||
		!strings.HasPrefix(res.PairURL, "onebit://pair?") {
		t.Errorf("%s: pairURL = %q, want onebit://pair?<query>", op, res.PairURL)
	}
	qr, err := qrDataURL(res.PairURL)
	if err != nil {
		t.Fatalf("%s: render %q: %v", op, res.PairURL, err)
	}
	if res.QRDataURL != qr {
		t.Errorf("%s: the QR is not the pairURL beside it", op)
	}
}
