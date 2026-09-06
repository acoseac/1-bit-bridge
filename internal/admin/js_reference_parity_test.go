package admin

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// app.js is 5,000 lines of untyped, unbundled, untested JavaScript loaded
// straight from //go:embed. Nothing checks it: no compiler, no linter in
// CI, no test runner. A helper whose definition is deleted while callers
// remain is therefore completely silent until someone opens the right page
// and reads the console.
//
// That is not hypothetical — it is why this test exists. #753 retired the
// Library Inspector and deleted 3,362 lines of app.js with it, including
// `function humanBytes(n)`. NINE callers survived, in three different
// pages' code:
//
//	applyStats            — throws on EVERY SSE stats frame, on every page,
//	                        aborting the rest of the apply
//	the Duplicates page   — two row-rendering sites
//	the Roots page        — five sites, including the variant-cache tiles
//	                        that #752 had added the day before
//
// Observed on a live console: "variants-free" and "variants-clear-bytes"
// stuck on their server-rendered em-dash placeholder, and the console
// filling with `ReferenceError: humanBytes is not defined`.
//
// The scan is a heuristic, not a parser — Go cannot parse JavaScript and
// this repo will not grow a Node build step for one file (the whole
// project is "one static binary, no runtime deps"). It strips comments
// and string literals, collects every bare `name(` call, and collects
// declarations from the forms this file actually uses. What it cannot
// resolve goes in the allowlist below, WITH which kind it is.

var (
	jsBlockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	jsLineCommentRe  = regexp.MustCompile(`(?m)//.*$`)
	jsTemplateRe     = regexp.MustCompile("`(?:[^`\\\\]|\\\\.)*`")
	jsDQStringRe     = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"`)
	jsSQStringRe     = regexp.MustCompile(`'(?:[^'\\\n]|\\.)*'`)

	// A bare `name(` — not preceded by a dot, so `x.foo()` is a property
	// access and none of this test's business.
	jsCallRe = regexp.MustCompile(`(^|[^.\w$])([a-z][A-Za-z0-9_$]*)\s*\(`)

	jsDeclFuncRe   = regexp.MustCompile(`\bfunction\s+([A-Za-z0-9_$]+)`)
	jsDeclBindRe   = regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z0-9_$]+)\s*=`)
	jsDeclPropRe   = regexp.MustCompile(`\b([A-Za-z0-9_$]+)\s*[:=]\s*(?:async\s*)?(?:function\b|\()`)
	jsDeclMethodRe = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][A-Za-z0-9_$]*)\s*\([^()]{0,300}\)\s*\{`)
	jsArrowArgsRe  = regexp.MustCompile(`\(([^()]{0,300})\)\s*=>`)
	// Parameters of a `function` declaration. The arrow form above was
	// already collected; this is the same class for the keyword form, and
	// without it every callback parameter of a plain function reads as an
	// undeclared call — `chunkAppend(container, items, make, gen)` in
	// ui.js, `run(button, status, call, describe, onChanged)` in
	// variants.js, the destructured `{ setCrumb }` of renderAlbum in
	// views.js. Listing those names as blind spots would fix nine
	// instances and leave the class; this removes the class.
	// `\bfunction\b` — the trailing boundary is load-bearing. With
	// `function\s*` the pattern also matches the CALL `functionCall(arg)`,
	// reading "Call" as the name and adding `arg` to the declared set, so a
	// deleted `arg()` helper would pass the guard. The boundary rejects it
	// (`n` and `C` are both word characters) while still admitting the
	// anonymous `function(a)` form, where the boundary sits between `n` and
	// `(`. `\*?` keeps generator declarations.
	jsFuncArgsRe = regexp.MustCompile(`\bfunction\b\s*\*?\s*[A-Za-z0-9_$]*\s*\(([^()]{0,300})\)`)
	jsArrowOneRe = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][A-Za-z0-9_$]*)\s*=>`)
	jsIdentRe    = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)
)

// jsCallKeywords read as `name(` but are language constructs.
var jsCallKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "typeof": true, "new": true, "await": true, "async": true,
	"function": true, "else": true, "do": true, "delete": true, "in": true,
	"of": true, "case": true, "throw": true,
}

// jsKnownGlobals are browser / language globals, which are of course not
// declared in the file. Add a name here only if it really is one.
var jsKnownGlobals = map[string]bool{
	"alert": true, "confirm": true, "fetch": true, "parseInt": true,
	"parseFloat": true, "encodeURIComponent": true, "decodeURIComponent": true,
	"setTimeout": true, "clearTimeout": true, "setInterval": true,
	"clearInterval": true, "requestAnimationFrame": true, "isNaN": true,
	"structuredClone": true, "queueMicrotask": true,
}

// jsScannerBlindSpots are names the heuristic cannot resolve to their
// declaration. Each is a LOCAL — a parameter or a destructured binding —
// so a call to it can never be the missing-definition bug this catches.
// Add here only after confirming the name is declared somewhere the scan
// does not look; if it is a helper whose definition went away, that is the
// bug, not a blind spot.
var jsScannerBlindSpots = map[string]string{
	"action": "callback parameter of wireJobButton",
}

// jsSourceFiles returns every shipped .js file under static/, so a module
// added later is covered on arrival.
//
// This walks rather than listing names because the list WAS a hardcoded
// pair ("static/app.js", "static/player/boot.js") and the seven other
// player modules — 3,920 lines, `views.js` alone 1,920 — were unscanned.
// The defect this guards is not hypothetical there: `humanBytes` was
// deleted out from under nine callers (2026-08-25) and threw on every SSE
// stats frame. The sibling CSS guard (player_css_parity_test.go) already
// walks the directory, so the asymmetry was accidental.
func jsSourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir("static", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".js") {
			out = append(out, filepath.ToSlash(p))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk static: %v", err)
	}
	// Nine modules ship today, and the floor is the count rather than a
	// loose lower bound: at 5 the guard still passed after four modules
	// disappeared. Deleting one should be a deliberate act that edits this
	// number — the same reasoning as the declared-hook list in the sibling
	// template-class guard. Adding modules is free.
	//
	// This is a different job from the aggregate call floor below: this one
	// answers "are all the modules still here", that one answers "is the
	// regex still matching". The aggregate cannot do this job — losing
	// views.js drops the total from 551 to 438, still above any threshold
	// loose enough not to trip on an ordinary refactor.
	const jsModulesShippingToday = 9
	if len(out) < jsModulesShippingToday {
		t.Fatalf("found %d .js files under static/, expected at least %d — either "+
			"the walk has stopped matching, or a module was deleted and this "+
			"count was not updated with it", len(out), jsModulesShippingToday)
	}
	sort.Strings(out)
	return out
}

// TestJSDeclarationsDoesNotTreatAFunctionCallAsADeclaration is the
// near-miss regression for jsFuncArgsRe's word boundary.
//
// `functionCall(arg)` is a CALL. Without the boundary the parameter
// collector read it as a declaration named "Call" taking a parameter
// "arg", which would silently add `arg` to the declared set and let a
// deleted `arg()` helper through the guard this file exists to be.
func TestJSDeclarationsDoesNotTreatAFunctionCallAsADeclaration(t *testing.T) {
	decls := jsDeclarations(stripJSNoise(`
		functionCall(deletedHelper);
		myfunction(alsoNotDeclared);
	`))
	for _, name := range []string{"deletedHelper", "alsoNotDeclared"} {
		if decls[name] {
			t.Errorf("%q was collected as a declaration, but it is an argument to a "+
				"CALL whose name merely ends in/starts with \"function\" — a helper "+
				"deleted out from under it would now pass the guard", name)
		}
	}

	// The forms that must still be collected, or the fix trades one hole
	// for another.
	for _, src := range []string{
		"function named(a) {}",
		"function (b) {}",
		"async function* gen(c) {}",
	} {
		got := jsDeclarations(stripJSNoise(src))
		var want string
		switch {
		case strings.Contains(src, "named"):
			want = "a"
		case strings.Contains(src, "gen"):
			want = "c"
		default:
			want = "b"
		}
		if !got[want] {
			t.Errorf("parameter %q of %q was not collected — the boundary fix "+
				"broke a real declaration form", want, src)
		}
	}
}

func TestAppJSHasNoCallsToDeletedHelpers(t *testing.T) {
	var totalCalls atomic.Int64
	t.Cleanup(func() {
		// The real "did the scan break" assertion. Per-file floors have to
		// tolerate the smallest module; this does not, and a regex that
		// stopped matching collapses it to near zero.
		if n := totalCalls.Load(); n < 400 {
			t.Errorf("only %d calls scraped across every static/**/*.js file — "+
				"the scan has stopped matching, which would make this whole "+
				"guard pass while checking nothing", n)
		}
	})
	for _, name := range jsSourceFiles(t) {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			body := stripJSNoise(string(b))

			called := map[string]bool{}
			for _, m := range jsCallRe.FindAllStringSubmatch(body, -1) {
				called[m[2]] = true
			}
			// Floor of 8, not the 20 this carried while it scanned only
			// app.js: the smallest real module (nowplaying.js) has 14
			// distinct calls, and a per-file number tuned to the largest
			// file makes adding a small module fail for no reason. What
			// the guard is actually for — "the scan silently stopped
			// matching" — yields ~0 for every file, so 8 catches it, and
			// the aggregate assertion below catches it far more strongly.
			if len(called) < 8 {
				t.Fatalf("only %d calls scraped from %s — the scan has stopped "+
					"matching, which would make this pass while checking nothing",
					len(called), name)
			}
			totalCalls.Add(int64(len(called)))

			declared := jsDeclarations(body)

			var missing []string
			for c := range called {
				switch {
				case jsCallKeywords[c], jsKnownGlobals[c], declared[c]:
					continue
				}
				if _, ok := jsScannerBlindSpots[c]; ok {
					continue
				}
				missing = append(missing, c)
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s calls names it never declares: %v\n"+
					"If one is a browser global, add it to jsKnownGlobals. If one is a "+
					"local the scan can't see (a parameter, a destructured binding), add "+
					"it to jsScannerBlindSpots with which. Otherwise it is a helper whose "+
					"definition was deleted while its callers were left behind — a "+
					"ReferenceError at runtime, on whichever page happens to call it.",
					name, missing)
			}
		})
	}
}

// stripJSNoise removes comments and string literals so an identifier
// mentioned in prose or inside a string is not mistaken for code.
// jsFunctionBody returns the source of the named top-level function in
// static/app.js, from its declaration to the next one, with COMMENTS
// stripped and CRLF normalised.
//
// Comments only — deliberately NOT stripJSNoise, which also blanks string
// literals. Every caller here is asking a question ABOUT a literal ("does
// the payload allowlist name this field", "does this still render
// 'nothing to reclaim'"), so blanking them would make the scan pass
// vacuously. Stripping comments is not optional either: this repo's
// commentary names the identifiers and quotes the strings it discusses,
// so an unstripped window answers yes to everything.
//
// CRLF is normalised at the read because nothing pins eol in a
// .gitattributes — a Windows checkout would otherwise make the
// newline-anchored window search find nothing and every assertion pass.
func jsFunctionBody(t *testing.T, decl string) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(js, decl)
	if i < 0 {
		t.Fatalf("%s not found in app.js — the scan is broken", decl)
	}
	body := js[i:]
	if j := strings.Index(body[1:], "\nfunction "); j > 0 {
		body = body[:j+1]
	}
	body = jsBlockCommentRe.ReplaceAllString(body, " ")
	return jsLineCommentRe.ReplaceAllString(body, " ")
}

func stripJSNoise(s string) string {
	s = jsBlockCommentRe.ReplaceAllString(s, " ")
	s = jsLineCommentRe.ReplaceAllString(s, " ")
	s = jsTemplateRe.ReplaceAllString(s, "``")
	s = jsDQStringRe.ReplaceAllString(s, `""`)
	s = jsSQStringRe.ReplaceAllString(s, "''")
	return s
}

// jsDeclarations collects every name the file binds, across the forms this
// codebase uses: function declarations, const/let/var bindings, object
// properties holding a function, method shorthand, and arrow parameters.
func jsDeclarations(body string) map[string]bool {
	out := map[string]bool{}
	add := func(ms [][]string, group int) {
		for _, m := range ms {
			out[m[group]] = true
		}
	}
	add(jsDeclFuncRe.FindAllStringSubmatch(body, -1), 1)
	add(jsDeclBindRe.FindAllStringSubmatch(body, -1), 1)
	add(jsDeclPropRe.FindAllStringSubmatch(body, -1), 1)
	add(jsDeclMethodRe.FindAllStringSubmatch(body, -1), 2)
	add(jsArrowOneRe.FindAllStringSubmatch(body, -1), 2)
	for _, m := range jsArrowArgsRe.FindAllStringSubmatch(body, -1) {
		for _, id := range jsIdentRe.FindAllString(m[1], -1) {
			out[id] = true
		}
	}
	for _, m := range jsFuncArgsRe.FindAllStringSubmatch(body, -1) {
		for _, id := range jsIdentRe.FindAllString(m[1], -1) {
			out[id] = true
		}
	}
	// import { a, b } from "./x.js" — boot.js and the player modules.
	for _, m := range regexp.MustCompile(`(?s)import\s*\{([^}]*)\}`).
		FindAllStringSubmatch(body, -1) {
		for _, id := range jsIdentRe.FindAllString(m[1], -1) {
			out[id] = true
		}
	}
	for _, m := range regexp.MustCompile(`import\s+\*\s+as\s+([A-Za-z0-9_$]+)`).
		FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	// const [title, render] = … — array destructuring, which the router
	// uses for its route table.
	for _, m := range regexp.MustCompile(`\b(?:const|let|var)\s*\[([^\]]{0,200})\]`).
		FindAllStringSubmatch(body, -1) {
		for _, id := range jsIdentRe.FindAllString(m[1], -1) {
			out[id] = true
		}
	}
	// const { a, b } = … — object destructuring, which every view's ctx
	// unpack uses.
	for _, m := range regexp.MustCompile(`\b(?:const|let|var)\s*\{([^}]{0,300})\}`).
		FindAllStringSubmatch(body, -1) {
		for _, id := range jsIdentRe.FindAllString(m[1], -1) {
			out[id] = true
		}
	}
	return out
}
