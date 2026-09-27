package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// appQueryItems reads a pairing URL's query the way the app reads it:
// Foundation's URLComponents.queryItems splits on "&" and at the first "=",
// then percent-decodes each part and leaves a "+" as a plus (RFC 3986). A
// form decoder, url.ParseQuery (so u.Query()) or a browser's
// URLSearchParams, reads that "+" as a space, which is how the bridge's own
// tests passed for as long as buildPairURL wrote every space as one.
// Measured on 2026-09-27 with the app's BridgePairingURL.parseResult,
// compiled from its repo: `name=My+Library` is read as "My+Library" and
// `name=My%20Library` as "My Library". A value whose percent-decoding is not
// UTF-8 has no value there, which the app reports as a missing field, so it
// is left out here too.
func appQueryItems(t *testing.T, pairURL string) map[string][]string {
	t.Helper()
	u, err := url.Parse(pairURL)
	if err != nil {
		t.Fatalf("parse %q: %v", pairURL, err)
	}
	items := map[string][]string{}
	for _, pair := range strings.Split(u.RawQuery, "&") {
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, kerr := url.PathUnescape(rawKey)
		value, verr := url.PathUnescape(rawValue)
		if kerr != nil || verr != nil || !utf8.ValidString(value) {
			continue
		}
		items[key] = append(items[key], value)
	}
	return items
}

// assertAppReads fails unless the app reads exactly want for key.
func assertAppReads(t *testing.T, items map[string][]string, key, want string) {
	t.Helper()
	if got := items[key]; len(got) != 1 || got[0] != want {
		t.Errorf("the app reads %s= as %q, want %q", key, got, want)
	}
}

// TestBuildPairURLWritesWhatTheAppReads: every field of the pairing URL
// reaches the app as the bridge holds it. The library name is the field
// this is about: it is the pre-fill for the name the app saves the bridge
// under, and a space in it was written as "+", which the app keeps, so
// "My Library" arrived as "My+Library" and the default name as
// "1-bit+Bridge". A literal "+" was always written %2B, so every "+" in the
// old query was a space, and %20 carries it to both kinds of reader.
func TestBuildPairURLWritesWhatTheAppReads(t *testing.T) {
	const (
		primary = "https://nuc.local:7788"
		token   = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE"
		fp      = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
	)
	alts := []string{primary, "https://192.168.0.24:7788", "https://[fd7a:115c:a1e0::1]:7788"}
	for _, name := range []string{
		"My Library",
		config.DefaultLibraryName,
		"C++ & Friends",
		"100% Hi-Res = FLAC?",
		"a/b#c;d,e",
		"Ünïcödé Álbum",
		"東京の音楽 ライブラリ",
		"Tab\tInside",
		"Listening 🎧 Room",
		"x",
	} {
		t.Run(name, func(t *testing.T) {
			out := buildPairURL(primary, token, fp, name, alts)
			u, err := url.Parse(out)
			if err != nil {
				t.Fatalf("parse %q: %v", out, err)
			}
			if strings.Contains(u.RawQuery, "+") {
				t.Errorf("the query carries a raw %q, which the app reads as a plus and a form decoder as a space: %s", "+", u.RawQuery)
			}
			items := appQueryItems(t, out)
			assertAppReads(t, items, "name", name)
			assertAppReads(t, items, "url", primary)
			assertAppReads(t, items, "token", token)
			assertAppReads(t, items, "fingerprint", fp)
			assertAppReads(t, items, "urls", strings.Join(alts, "\n"))
		})
	}
}

// TestMintAndRotateCarryTheLibraryNameTheAppReads drives the two handlers
// that hand out a pairing URL, with the fixture's name ("Test Library") and
// the default, which both hold a space: the name= the app reads is the
// bridge's name. Measured end to end on 2026-09-27 before the fix: a bridge
// named "My Library" minted `name=My+Library`, which the app's parser, fed
// that URL, returned as "My+Library".
func TestMintAndRotateCarryTheLibraryNameTheAppReads(t *testing.T) {
	for _, name := range []string{"Test Library", config.DefaultLibraryName} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			if name != srv.deps.CfgHolder.Load().LibraryName {
				next := config.Clone(srv.deps.CfgHolder.Load())
				next.LibraryName = name
				srv.deps.CfgHolder.Store(next)
			}
			h := srv.Handler()

			var mint pairResult
			if code := doJSON(t, h, "POST", "/api/tokens", map[string]string{"name": "iPhone"}, &mint); code != http.StatusCreated {
				t.Fatalf("mint: %d", code)
			}
			assertAppReads(t, appQueryItems(t, mint.PairURL), "name", name)

			var rotated pairResult
			if code := doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate", map[string]string{}, &rotated); code != http.StatusOK {
				t.Fatalf("rotate: %d", code)
			}
			assertAppReads(t, appQueryItems(t, rotated.PairURL), "name", name)
		})
	}
}
