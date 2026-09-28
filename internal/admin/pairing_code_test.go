package admin

import (
	"errors"
	"net/http"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/pairingcode"
)

// TestBuildPairURLCarriesACodeOnlyWhenOneIsIssued pins the link's shape:
// `code` is present exactly when there is one, and the app reads it back
// verbatim. A link without a code is the shape every shipped app pairs
// with, so an empty code must leave no trace of the parameter.
func TestBuildPairURLCarriesACodeOnlyWhenOneIsIssued(t *testing.T) {
	const code = "ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZyXwV"
	alts := []string{"https://host:7788"}

	without := appQueryItems(t, buildPairURL("https://host:7788", "tok", "", "AB:CD", "Home", alts))
	if got, ok := without["code"]; ok {
		t.Errorf("a link with no code carries code=%q", got)
	}
	assertAppReads(t, without, "token", "tok")

	with := appQueryItems(t, buildPairURL("https://host:7788", "tok", code, "AB:CD", "Home", alts))
	assertAppReads(t, with, "code", code)
	assertAppReads(t, with, "token", "tok")
}

// TestMintAndRotateIssueACodeForTheTokenTheyPair drives the real handlers
// with a real code store: the link a mint returns carries a code bound to
// the token it mints, and a rotation's link carries a fresh one that
// replaces it, so the token's earlier QR has nothing left to redeem.
func TestMintAndRotateIssueACodeForTheTokenTheyPair(t *testing.T) {
	srv, _, _ := newTestServer(t)
	codes := pairingcode.New()
	srv.deps.PairingCodes = codes
	h := srv.Handler()

	var mint pairResult
	if status := doJSON(t, h, "POST", "/api/tokens", map[string]string{"name": "iPhone"}, &mint); status != http.StatusCreated {
		t.Fatalf("mint: %d", status)
	}
	minted := appQueryItems(t, mint.PairURL)
	assertAppReads(t, minted, "token", mint.RawToken)
	if len(minted["code"]) != 1 || !pairingcode.ValidShape(minted["code"][0]) {
		t.Fatalf("the minted link carries code=%q, want one code of the issued shape", minted["code"])
	}
	mintCode := minted["code"][0]

	var rotated pairResult
	if status := doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate", map[string]string{}, &rotated); status != http.StatusOK {
		t.Fatalf("rotate: %d", status)
	}
	rotatedItems := appQueryItems(t, rotated.PairURL)
	assertAppReads(t, rotatedItems, "token", rotated.RawToken)
	if len(rotatedItems["code"]) != 1 || rotatedItems["code"][0] == mintCode {
		t.Fatalf("the rotated link carries code=%q, want one fresh code", rotatedItems["code"])
	}

	if _, ok := codes.Take(mintCode); ok {
		t.Error("the mint's code still redeems after a rotation issued a new one")
	}
	if id, ok := codes.Take(rotatedItems["code"][0]); !ok || id != mint.ID {
		t.Errorf("the rotated link's code redeems as (%q, %v), want (%q, true)", id, ok, mint.ID)
	}
}

// failingIssuer is a PairingCodeIssuer whose every issue fails.
type failingIssuer struct{}

func (failingIssuer) Issue(string) (string, error) { return "", errors.New("no entropy") }

// TestALinkWithoutACodeStillPairs pins the two ways a link comes without a
// code: a bridge that issues none (Deps.PairingCodes nil) and an issue that
// fails. Either way the mint succeeds and the link carries its token, the
// shape every shipped app pairs with.
func TestALinkWithoutACodeStillPairs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issuer PairingCodeIssuer
	}{
		{"no issuer", nil},
		{"issue fails", failingIssuer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			srv.deps.PairingCodes = tc.issuer
			h := srv.Handler()
			var mint pairResult
			if status := doJSON(t, h, "POST", "/api/tokens", map[string]string{"name": "iPhone"}, &mint); status != http.StatusCreated {
				t.Fatalf("mint: %d", status)
			}
			items := appQueryItems(t, mint.PairURL)
			if got, ok := items["code"]; ok {
				t.Errorf("the link carries code=%q, want none", got)
			}
			assertAppReads(t, items, "token", mint.RawToken)
		})
	}
}
