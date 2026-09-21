package admin

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// certFieldRe finds `info.<field>` reads — the name refreshCertInfo binds
// the decoded /api/cert body to.
var certFieldRe = regexp.MustCompile(`\binfo\??\.([A-Za-z_][A-Za-z0-9_]*)`)

// TestCertTileOnlyReadsRealCertFields is the /api/cert twin of
// TestSettingsPrereqsOnlyReadRealJobsFields, and it exists because the
// mistake was proposed on a real review.
//
// `apiCertInfo` marshals servertls.CertInfo straight out, so the wire
// shape is that struct's json tags and nothing else. Gemini read PR #951
// and suggested the cert tile consume `info.notYetValid`, `info.expired`
// and `info.expiringSoon`, on the belief that the backend now supplied
// them. It does not: those three are the `bridge cert info --json`
// envelope, built in cmd/bridge, and /api/cert has never carried a
// boolean at all.
//
// Applied, all three reads would be undefined — falsy — so the not-yet-
// valid branch would never fire, the expired badge would never render
// and the yellow band would vanish, silently reverting the console half
// of that PR. No error, no failing test, a tile that renders perfectly
// and says the wrong thing. That is the same failure the jobs guard was
// written for, one endpoint over.
//
// Compared against the Go type by reflection rather than a hand-written
// list, so a renamed or removed json tag fails here too.
func TestCertTileOnlyReadsRealCertFields(t *testing.T) {
	valid := map[string]bool{}
	rt := reflect.TypeOf(servertls.CertInfo{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		valid[strings.Split(tag, ",")[0]] = true
	}
	if len(valid) < 4 {
		t.Fatalf("only %d json fields found on servertls.CertInfo — the "+
			"reflection walk is broken, so this test proves nothing", len(valid))
	}

	// Scoped to refreshCertInfo: `info` is a common local name, and an
	// unscoped scan would grade every other function's variable against
	// this struct.
	body := stripJSNoise(certTileSource(t))
	seen := map[string]bool{}
	for _, m := range certFieldRe.FindAllStringSubmatch(body, -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatal("no info.<field> reads found in refreshCertInfo — the scan is broken")
	}
	for f := range seen {
		if !valid[f] {
			t.Errorf("the cert tile reads info.%s, which /api/cert does not return.\n"+
				"Fields it does return: %s\n"+
				"A read of a field that isn't there is undefined, not an error — the "+
				"badge renders and silently reports the wrong state. The notYetValid / "+
				"expired / expiringSoon booleans live on `bridge cert info --json`, "+
				"which is a different envelope built in cmd/bridge.",
				f, sortedKeys(valid))
		}
	}
}

// certTileSource returns refreshCertInfo's body from app.js.
//
// Bounded to the one function on purpose — see the scoping note above,
// and the /api/stats guard's own record of an unscoped root being a
// false PASS.
func certTileSource(t *testing.T) string {
	t.Helper()
	js := readConsoleJS(t, "static/app.js")
	const decl = "async function refreshCertInfo("
	i := strings.Index(js, decl)
	if i < 0 {
		t.Fatalf("%s not found in app.js — the scan is broken", decl)
	}
	body := js[i:]
	if j := strings.Index(body[1:], "\nfunction "); j > 0 {
		body = body[:j+1]
	}
	// The next declaration may be a plain `function`, so also stop at an
	// `async function` if one comes first.
	if j := strings.Index(body[1:], "\nasync function "); j > 0 && j+1 < len(body) {
		body = body[:j+1]
	}
	return body
}
