package admin

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
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

// certExpiryWindowConstRe reads the milliseconds a `const NAME = a * b *
// … ;` product in app.js evaluates to. Only integer factors, which is
// every form these two constants have ever taken.
var certExpiryWindowConstRe = regexp.MustCompile(`const\s+(CERT_[A-Z_]+_MS)\s*=\s*([0-9_ *]+);`)

// TestCertExpiryBandsMirrorTheGoWindow pins the console's 30-day band to
// servertls.ExpiryWarningWindow — the value `bridge doctor`, `bridge
// cert info` and the startup warning all grade against.
//
// The Go side moved off a day count in #951 because DaysUntilExpiry
// truncates toward zero: a certificate with 30 days 23 hours left reads
// as 30, so a surface keyed on `<= 30` calls it expiring while every
// surface comparing the DURATION stays quiet. Same certificate, same
// host, two answers. The three console tiles were still on the day
// count, and the threshold was spelled three times, which is how two of
// them rendered the 30-day band in `.badge.running` — green, the
// healthy colour, under the word "expiring" — while the third had been
// corrected.
//
// Reading the constant out of the source rather than asserting a
// literal 2592000000 is what makes this a PARITY test: the Go window is
// the subject, and a change to it has to reach the console or fail
// here.
// rejectedCertGradingShapes are the two ways a cert tile drifts from the
// CLI, as REGEXPS rather than substrings.
//
// `strings.Contains("days <= ")` was the first form and is bypassed by
// `days<=30` or `days  <=  30` — spacing this repo happens not to use
// today, which is exactly the kind of thing that changes without anyone
// deciding to. A guard that can be stepped over by a formatting choice
// is a guard whose absence looks identical to its presence. (Gemini on
// #952; the suggested pattern is not taken verbatim — it carried a
// doubled `>` that would have matched nothing at all, which is the same
// failure one level down.)
//
// `\b` on the identifier so `elapsedDays <= n` in some future helper is
// not reported as this defect, and `<[^=]` so the `<` alternative does
// not also fire on every `<=` and double every message.
var rejectedCertGradingShapes = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`\bdays\s*<=`),
		"grades on a truncated day count; compare the remaining duration against CERT_EXPIRY_WARNING_MS"},
	{regexp.MustCompile(`\bdays\s*<[^=]`),
		"grades on a truncated day count; compare the remaining duration against CERT_EXPIRY_WARNING_MS"},
	{regexp.MustCompile(`\bdaysUntilExpiry\s*<=?`),
		"grades on the server's truncated day count; /api/cert also carries notAfter"},
	{regexp.MustCompile(`badge\s+running"\s*>\s*expiring`),
		"renders an expiry warning in the healthy green badge; .badge.warn is the yellow one"},
}

func TestCertExpiryBandsMirrorTheGoWindow(t *testing.T) {
	js := stripJSNoise(readConsoleJS(t, "static/app.js"))
	got := map[string]int64{}
	for _, m := range certExpiryWindowConstRe.FindAllStringSubmatch(js, -1) {
		var product int64 = 1
		for _, f := range strings.Fields(strings.ReplaceAll(strings.ReplaceAll(m[2], "_", ""), "*", " ")) {
			n, err := strconv.ParseInt(f, 10, 64)
			if err != nil {
				t.Fatalf("%s: factor %q is not an integer: %v", m[1], f, err)
			}
			product *= n
		}
		got[m[1]] = product
	}
	wantWarn := servertls.ExpiryWarningWindow.Milliseconds()
	switch v, ok := got["CERT_EXPIRY_WARNING_MS"]; {
	case !ok:
		t.Fatal("CERT_EXPIRY_WARNING_MS is not declared in app.js — the cert tiles have " +
			"no shared threshold, which is the state in which two of the three drifted")
	case v != wantWarn:
		t.Errorf("CERT_EXPIRY_WARNING_MS = %d ms, want %d ms (servertls.ExpiryWarningWindow). "+
			"The console and the CLI must grade the same certificate the same way.", v, wantWarn)
	}
	if v, ok := got["CERT_EXPIRING_SOON_MS"]; !ok {
		t.Error("CERT_EXPIRING_SOON_MS is not declared in app.js")
	} else if v <= 0 || v >= wantWarn {
		t.Errorf("CERT_EXPIRING_SOON_MS = %d ms; the red band must sit strictly inside "+
			"the %d ms warning window", v, wantWarn)
	}
}

// TestCertTilesGradeOnTheRemainingDurationNotADayCount sweeps all three
// tiles for the two shapes that made them disagree with the CLI.
//
// The day count is the one that cannot be seen by reading either side
// alone: `Math.floor(ms / 86_400_000) <= 30` and `info.daysUntilExpiry
// <= 30` both look like the rule they implement, and both answer a
// different question from `NotAfter.Sub(now) <= ExpiryWarningWindow`.
// `.badge.running` is the other: it is green (--ok), so an arm emitting
// it under the word "expiring" renders a warning in the healthy colour —
// the defect #951 fixed on the self-signed tile and left standing on the
// two beside it.
//
// The ladder itself lives in ONE function now (certExpiryBadge), so the
// positive half of this test is that each tile calls it. The rejected
// shapes stay: a tile that stops calling it and spells its own ladder
// again is exactly the regression, and "does not call the helper" alone
// would not say what it did instead.
//
// Scanned per FUNCTION, because the scan has to be able to say which
// tile regressed, and because `days` is an ordinary local name that
// other code may legitimately compare. jsFunctionBody strips COMMENTS
// and keeps string literals, which is what both halves need: the
// comment beside each fix quotes the shape it replaced (so an unstripped
// window reports every tile as broken), and one of the rejected shapes
// IS a literal — stripJSNoise would blank the badge class and the scan
// would pass vacuously.
func TestCertTilesGradeOnTheRemainingDurationNotADayCount(t *testing.T) {
	for _, tile := range []struct{ name, decl string }{
		{"self-signed", "async function refreshCertInfo("},
		{"tailscale", "function renderTailscaleTile("},
		{"autocert", "async function refreshAutocertTile("},
	} {
		body := jsFunctionBody(t, tile.decl)
		if !strings.Contains(body, "certExpiryBadge(") {
			t.Errorf("the %s cert tile does not grade through certExpiryBadge — "+
				"a ladder spelled locally is how two of these three came to render "+
				"the 30-day band green, and how all three missed the expired arm",
				tile.name)
		}
		for _, bad := range rejectedCertGradingShapes {
			if bad.re.MatchString(body) {
				t.Errorf("the %s cert tile matches %s, which %s", tile.name, bad.re, bad.why)
			}
		}
	}
}

// TestRejectedCertGradingShapesSeeEverySpacing pins the patterns
// directly, for the reason TestJSBracketFieldReadsSeesEveryBracketForm
// exists: the guard that uses them can only fail on a tile that is ALSO
// wrong, so a spacing the scan cannot see is indistinguishable, from its
// side, from a codebase that does not use it. That is the vacuous-pass
// shape, and it is precisely how the substring form survived — its two
// callers happened to be spaced the way it expected.
//
// Both directions, because a pattern that matches everything is no
// better than one that matches nothing: `elapsedDays <= 30` in some
// future helper must not be reported as this defect, and neither must
// the accepted form.
func TestRejectedCertGradingShapesSeeEverySpacing(t *testing.T) {
	matches := func(s string) bool {
		for _, r := range rejectedCertGradingShapes {
			if r.re.MatchString(s) {
				return true
			}
		}
		return false
	}
	for _, bad := range []string{
		"if (days <= 30) badge",
		"if (days<=30) badge",
		"if (days  <=  30) badge",
		"if (days < 0) badge",
		"if (days<0) badge",
		"if (info.daysUntilExpiry <= 30)",
		"if (info.daysUntilExpiry<=30)",
		`badge running">expiring`,
		`badge running" >expiring`,
		`badge  running">expiring`,
	} {
		if !matches(bad) {
			t.Errorf("no rejected shape matched %q — a tile spelled this way would pass", bad)
		}
	}
	for _, ok := range []string{
		"if (left <= CERT_EXPIRY_WARNING_MS) badge",
		"if (left <= 0) badge",
		// The accepted form is now certValidityText's own arithmetic.
		// The line this used to list —
		// `const days = Math.max(0, Math.floor(left / 86_400_000));` —
		// no longer exists: it WAS the defect
		// TestCertTilesRenderTheirRemainingTimeThroughOneHelper closes,
		// and a false-positive control citing deleted code stops being
		// one.
		"const days = Math.floor(Math.abs(left) / 86_400_000);",
		"if (elapsedDays <= 30) somethingElse",
		`badge warn">expiring`,
		`badge danger">expired`,
	} {
		if matches(ok) {
			t.Errorf("a rejected shape matched the ACCEPTED form %q", ok)
		}
	}
}

// TestCertTilesRenderTheirRemainingTimeThroughOneHelper is the TEXT half
// of TestCertTilesGradeOnTheRemainingDurationNotADayCount, and it exists
// because the badge and the sentence beside it were computed from
// different numbers.
//
// #952 gave all three tiles one ladder (certExpiryBadge, which returns
// `expired` for `left <= 0`) and left each tile's wording alone. The two
// that clamped with `Math.max(0, …)` — a form that made sense only
// against the PRE-#952 ladder, which had no expired arm — then rendered
// `expired · expires in 0 days`: past tense against future tense, on the
// surface where an operator chooses between rotating now and scheduling
// it. The self-signed tile reached the same place from the other side,
// displaying the `daysUntilExpiry` sentinel Inspect forces negative past
// NotAfter, as `(-40 days)`.
//
// Neither is visible to a Go type check and neither breaks a page, which
// is why this is a source scan and not a render assertion.
//
// The rejected fragments are the three spellings that produced it, not
// "anything that mentions days": certValidityText computes a day count
// itself, and a tile is free to name one in any other sentence.
func TestCertTilesRenderTheirRemainingTimeThroughOneHelper(t *testing.T) {
	checked := 0
	for _, tile := range []struct{ name, decl string }{
		{"self-signed", "async function refreshCertInfo("},
		{"tailscale", "function renderTailscaleTile("},
		{"autocert", "async function refreshAutocertTile("},
	} {
		body := jsFunctionBody(t, tile.decl)
		checked++
		if !strings.Contains(body, "certValidityText(") {
			t.Errorf("the %s cert tile does not render its remaining time through certValidityText — "+
				"a phrase spelled locally is how two of these three came to say "+
				"`expires in 0 days` under an `expired` badge", tile.name)
		}
		for _, bad := range []struct{ frag, why string }{
			{"expires in ", "spells the phrase inline, so it cannot say `expired` when the badge does"},
			{"Math.max(0,", "clamps the remaining time, which is what rendered `expires in 0 days` for an expired cert"},
			{"daysUntilExpiry", "displays the sentinel Inspect forces NEGATIVE past NotAfter, which rendered `(-40 days)`"},
		} {
			if strings.Contains(body, bad.frag) {
				t.Errorf("the %s cert tile %s (%q)", tile.name, bad.why, bad.frag)
			}
		}
	}
	if checked != 3 {
		t.Fatalf("scanned %d tiles, want 3 — the scan is not seeing app.js and would pass no matter what", checked)
	}

	// "Today" is a CALENDAR question, not a 24-hour one. A duration-only
	// test labelled a certificate expiring at 00:30 tomorrow "expires
	// today" — beside a parenthesised date reading TOMORROW, the same
	// sentence disagreeing with itself, which is the defect this helper
	// exists for one unit down (CodeRabbit on #962).
	//
	// Structural, because the Go suite cannot run the helper: it can
	// still say that the calendar comparison is present, which is what a
	// regression to `Math.floor(ms / 86_400_000) === 0` would remove.
	helper := jsFunctionBody(t, "function certValidityText(")
	for _, need := range []string{"getFullYear()", "getMonth()", "getDate()"} {
		if !strings.Contains(helper, need) {
			t.Errorf("certValidityText no longer compares calendar dates (%s missing) — "+
				"a 24-hour test calls an expiry just after midnight tomorrow \"today\", "+
				"next to a date that says otherwise", need)
		}
	}
}
