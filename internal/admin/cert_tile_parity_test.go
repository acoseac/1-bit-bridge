package admin

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// certFieldRe finds `info.<field>` reads — the name refreshCertInfo binds
// the decoded /api/cert body to.
var certFieldRe = regexp.MustCompile(`\binfo\??\.([A-Za-z_][A-Za-z0-9_]*)`)

// jsBracketReadRe finds `root["field"]` / `root?.["field"]`, the form the
// dot-property scans above cannot see.
//
// It is matched against COMMENT-STRIPPED source rather than stripJSNoise'd
// source, and that is the whole difficulty: stripJSNoise blanks string
// literals, so `info["notYetValid"]` arrives as `info[""]` with the field
// name already destroyed. Same reason statsKeysReadBy walks the Go AST
// instead of scanning — when the subject IS a string literal, the tool that
// protects scans from their own commentary is the tool that erases it.
//
// Comments still have to go, for the reason this package's other guards
// record: the prose beside a rule quotes the rule.
//
// Whitespace is permitted everywhere JS permits it — `info?. ["x"]` and
// `info ?. ["x"]` are valid optional bracket access (verified with
// `node --check`), and a scan that required `[` to abut `?.` would miss
// them. Non-capturing `(?:…)` so the field stays in m[1]; RE2 supports
// it, contrary to the review that raised the whitespace gap.
var jsBracketReadRe = regexp.MustCompile(`\b%s\s*(?:\?\.)?\s*\[\s*(?:"([^"]*)"|'([^']*)')\s*\]`)

// jsBracketFieldReads returns the field names read off `root` in bracket
// form. CodeRabbit raised the gap on PR #951 against the cert guard below;
// the jobs guard beside it had the same blind spot, so the helper is shared
// rather than the one flagged site being patched.
//
// NOT covered, deliberately and stated rather than implied: the alias-
// resolving machinery in console_source_parity_test.go (jsMemberReadRe and
// the jobsScopes/snapshotScopes walk it feeds) is dot-only too. Extending it
// means teaching path resolution about bracket segments, which is a real
// change to a different mechanism — claiming it here without doing it would
// be the false-safety-claim shape TestEveryCitedTestNameExists exists for.
func jsBracketFieldReads(t *testing.T, src, root string) map[string]bool {
	t.Helper()
	re, err := regexp.Compile(fmt.Sprintf(jsBracketReadRe.String(), regexp.QuoteMeta(root)))
	if err != nil {
		t.Fatalf("compile bracket-read scan for %q: %v", root, err)
	}
	stripped := jsLineCommentRe.ReplaceAllString(jsBlockCommentRe.ReplaceAllString(src, " "), " ")
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(stripped, -1) {
		// Two alternatives, one per quote style — whichever matched is
		// the key. RE2 has no backreference, so a single `["']…["']`
		// class would also accept a mismatched pair.
		key := m[1]
		if key == "" {
			key = m[2]
		}
		if key != "" {
			out[key] = true
		}
	}
	return out
}

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
	src := certTileSource(t)
	seen := map[string]bool{}
	for _, m := range certFieldRe.FindAllStringSubmatch(stripJSNoise(src), -1) {
		seen[m[1]] = true
	}
	// Bracket form too, off the comment-stripped source — see
	// jsBracketFieldReads for why it cannot share the stripJSNoise pass.
	for f := range jsBracketFieldReads(t, src, "info") {
		seen[f] = true
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

// TestJSBracketFieldReadsSeesEveryBracketForm pins the helper directly,
// because the two guards that use it can only fail on a field name that
// is ALSO invalid — so a form the scan cannot see is indistinguishable,
// from their side, from a codebase that does not use it. That is the
// vacuous-pass shape, and it is how the `?.` whitespace gap survived the
// first round: CodeRabbit raised it on PR #951 against a helper whose
// only coverage was two callers that happened not to exercise it.
//
// Every accepted form here is valid JavaScript — `info?. ["x"]` and
// `info ?. ["x"]` are optional bracket access with the whitespace JS
// allows between tokens, verified with `node --check` rather than
// assumed.
func TestJSBracketFieldReadsSeesEveryBracketForm(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
	}{
		{`const a = info["plain"];`, "plain"},
		{`const a = info ["spaced"];`, "spaced"},
		{`const a = info?.["optional"];`, "optional"},
		{`const a = info?. ["optionalSpaced"];`, "optionalSpaced"},
		{`const a = info ?. ["bothSpaced"];`, "bothSpaced"},
		{`const a = info['singleQuoted'];`, "singleQuoted"},
		{`const a = info[ "padded" ];`, "padded"},
		// Not identifier-shaped, and therefore invisible to a scan that
		// assumes a json tag looks like a JS identifier. Nothing on
		// CertInfo carries a hyphen today; a guard that can only see the
		// tags that exist is one that stops working the day one changes.
		{`const a = info["not-yet-valid"];`, "not-yet-valid"},
		{`const a = info?.["kebab-case-key"];`, "kebab-case-key"},
	} {
		got := jsBracketFieldReads(t, tc.src, "info")
		if !got[tc.want] {
			t.Errorf("jsBracketFieldReads(%q) missed %q, saw %v", tc.src, tc.want, sortedKeys(got))
		}
	}

	// A comment naming a field must NOT count — this package's guards
	// strip comments precisely because the prose beside a rule quotes
	// the rule, and this helper does its own stripping.
	if got := jsBracketFieldReads(t, `// info["fromAComment"] is not a read`, "info"); len(got) != 0 {
		t.Errorf("a commented-out read counted: %v", sortedKeys(got))
	}
	if got := jsBracketFieldReads(t, `/* info["fromABlock"] */`, "info"); len(got) != 0 {
		t.Errorf("a block-commented read counted: %v", sortedKeys(got))
	}
	// A different root must not be picked up, or scoping means nothing.
	if got := jsBracketFieldReads(t, `const a = other["notOurs"];`, "info"); len(got) != 0 {
		t.Errorf("read off a different root counted: %v", sortedKeys(got))
	}
	// A mismatched quote pair is not a key. RE2 has no backreference, so
	// a single `["']…["']` character class would accept this.
	if got := jsBracketFieldReads(t, "const a = info[\"mismatched'];", "info"); len(got) != 0 {
		t.Errorf("a mismatched quote pair counted as a key: %v", sortedKeys(got))
	}
}
