package admin

import (
	"os"
	"regexp"
	"sort"
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
	jsArrowOneRe   = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][A-Za-z0-9_$]*)\s*=>`)
	jsIdentRe      = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)
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

func TestAppJSHasNoCallsToDeletedHelpers(t *testing.T) {
	for _, name := range []string{"static/app.js", "static/player/boot.js"} {
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
			if len(called) < 20 {
				t.Fatalf("only %d calls scraped from %s — the scan has stopped "+
					"matching, which would make this pass while checking nothing",
					len(called), name)
			}

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
