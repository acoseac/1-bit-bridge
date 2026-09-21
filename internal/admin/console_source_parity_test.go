package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readConsoleJS reads a console asset, normalising CRLF first. A Windows
// checkout carries CRLF (nothing pins eol), and every scan below does
// offset or line arithmetic — the page-init parity guard was
// permanently red on windows-latest for exactly this reason, which is a
// guard that checks nothing on the platform it looks like it covers.
func readConsoleJS(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// This file's scans reuse stripJSNoise from js_reference_parity_test.go
// rather than adding a second one — a repo with two "is this real code or
// prose?" heuristics has two things to keep honest.

var jobsFieldRe = regexp.MustCompile(`\bjobs\.([A-Za-z_][A-Za-z0-9_]*)`)

// TestSettingsPrereqsOnlyReadRealJobsFields is the regression gate for the
// bug that made this file worth writing.
//
// renderSettingsPrereqs painted the PCM-upscaling chip from
// `jobs.upscale.enabled`. /api/jobs has never had an `upscale` node —
// upscale has no sweeper, so it never grew a card there — so the read
// yielded undefined, `running` was permanently false, and the chip said
// "off — sox is available" on every bridge in the world, including ones
// with a live pool and eight thousand cached variants.
//
// Nothing failed. JS has no compiler to notice, and the chip rendered
// perfectly well; it just rendered the wrong answer. The irony is on the
// record in that function's own docblock, which says it exists because
// four endpoints once told four true stories while a feature did nothing
// for nine days.
//
// So: every `jobs.<field>` read in app.js must name a real field of the
// struct the endpoint actually marshals. Comparing against the Go type by
// reflection rather than a hand-written list means a renamed or removed
// JSON tag fails here too.
func TestSettingsPrereqsOnlyReadRealJobsFields(t *testing.T) {
	valid := map[string]bool{}
	rt := reflect.TypeOf(jobsSnapshotResponse{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		valid[strings.Split(tag, ",")[0]] = true
	}
	if len(valid) < 5 {
		t.Fatalf("only %d json fields found on jobsSnapshotResponse — the "+
			"reflection walk is broken, so this test proves nothing", len(valid))
	}

	body := stripJSNoise(readConsoleJS(t, "static/app.js"))
	seen := map[string]bool{}
	for _, m := range jobsFieldRe.FindAllStringSubmatch(body, -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatal("no jobs.<field> reads found in app.js — the scan is broken")
	}
	for f := range seen {
		if !valid[f] {
			t.Errorf("app.js reads jobs.%s, which /api/jobs does not return.\n"+
				"Fields it does return: %s\n"+
				"A read of a field that isn't there is undefined, not an error — "+
				"the control renders and silently reports the wrong state.",
				f, sortedKeys(valid))
		}
	}
}

var byteLadderRe = regexp.MustCompile(`BYTE_UNITS = (\[[^\]]*\])`)
var byteLoopRe = regexp.MustCompile(`while \(v >= (\d+) && u < BYTE_UNITS\.length - 1\)`)

// TestByteFormattersAgree pins the two byte formatters to each other.
//
// app.js `formatBytes` and player/format.js `bytes` are independent
// copies — app.js is a deferred classic script, the player is ES modules,
// and nothing bridges them. They had drifted: app.js used binary units
// and stopped at GB (so a petabyte mount rendered "1048576 GB free"),
// while the player used decimal and its docblock claimed it was
// "matching the operator pages". The same console showed a track at
// 43.8 MB and its volume at 209 GB under two different definitions of
// the unit.
//
// This is the repo's lockstep-mirror idiom, same as the dupes/manifest
// lossy-codec pair: two copies that must not diverge, compared by test
// because they cannot be shared.
func TestByteFormattersAgree(t *testing.T) {
	// The ladder is matched against the RAW source: stripJSNoise blanks
	// string literals, which is exactly the content under test here
	// ("B", "KB", …). Requiring a single match per file is what keeps
	// that safe — a prose mention of the array would make it two and
	// fail loudly rather than being silently picked over the real one.
	appRaw := readConsoleJS(t, "static/app.js")
	playerRaw := readConsoleJS(t, "static/player/format.js")
	appJS := stripJSNoise(appRaw)
	playerJS := stripJSNoise(playerRaw)

	appAll := byteLadderRe.FindAllStringSubmatch(appRaw, -1)
	playerAll := byteLadderRe.FindAllStringSubmatch(playerRaw, -1)
	if len(appAll) != 1 || len(playerAll) != 1 {
		t.Fatalf("want exactly one BYTE_UNITS assignment per file, got %d in app.js "+
			"and %d in player/format.js — the scan is ambiguous",
			len(appAll), len(playerAll))
	}
	appUnits, playerUnits := appAll[0], playerAll[0]
	if appUnits[1] != playerUnits[1] {
		t.Errorf("BYTE_UNITS differ:\n  app.js:           %s\n  player/format.js: %s",
			appUnits[1], playerUnits[1])
	}
	if !strings.Contains(appUnits[1], `"PB"`) {
		t.Errorf("BYTE_UNITS = %s — the ladder must reach PB. It stopped at GB "+
			"until 2026-08-30, which rendered a petabyte-class mount as "+
			`"1048576 GB free" in the sidebar of every page.`, appUnits[1])
	}

	appBase := byteLoopRe.FindStringSubmatch(appJS)
	playerBase := byteLoopRe.FindStringSubmatch(playerJS)
	if appBase == nil || playerBase == nil {
		t.Fatal("could not find the unit loop in both files — the scan is broken")
	}
	if appBase[1] != playerBase[1] {
		t.Errorf("unit base differs: app.js divides by %s, player/format.js by %s. "+
			"One console cannot hold two definitions of a megabyte.",
			appBase[1], playerBase[1])
	}
	if appBase[1] != "1024" {
		t.Errorf("unit base = %s, want 1024. Binary is the deliberate choice: these "+
			"numbers are compared against `df -h` on a Linux host, and every "+
			"operator page has shown binary for its whole life.", appBase[1])
	}
}

// jobsCardFieldRe finds the reads the JOBS PAGE makes. `renderJobCards` binds
// the snapshot to `j`, where `renderSettingsPrereqs` binds it to `jobs`, so the
// two directions of this contract need different anchors.
var jobsCardFieldRe = regexp.MustCompile(`\bj\.([A-Za-z_][A-Za-z0-9_]*)`)

// The three shapes a nested /api/jobs read takes in app.js, which is what the
// recursive half of the guard below has to see through:
//
//	const lyr = j.lyrics;              lyr.syncedRows        (local alias)
//	renderAnalysisCoverage(an.coverage) → function …(cov) { cov.eligible }
//	                                                          (parameter alias)
//	j.scanner?.intervalSec                                    (direct)
var (
	// const|let|var <alias> = <path>  — the path may chain, may use ?., and
	// may be a bare identifier: `const snap = j` binds the whole snapshot,
	// and a `+` here (at least one member) silently dropped exactly that
	// case, which a negative control caught before a review comment could.
	jsAliasRe = regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_]\w*)\s*=\s*([A-Za-z_]\w*(?:\??\.[A-Za-z_]\w*)*)`)
	// function <name>(<oneParam>)
	jsOneParamFnRe = regexp.MustCompile(`\bfunction\s+([A-Za-z_]\w*)\s*\(\s*([A-Za-z_]\w*)\s*\)`)
	// <ident>(?.).<prop> — every member read, root ident captured
	jsMemberReadRe = regexp.MustCompile(`\b([A-Za-z_]\w*)((?:\??\.[A-Za-z_]\w*)+)`)
)

// jobsFieldPaths walks a DTO and returns every JSON property path the console
// is expected to read, one per LEAF: "lyrics.syncedRows",
// "analysis.coverage.eligible", "scanner.intervalSec".
//
// It recurses only into the private jobs* structs — the DTOs that exist for
// this endpoint and nothing else, so every field on them is this page's to
// render. It stops at the exported shared types (*FingerprintJobState,
// *AutoOptimizeJobState, *JobRunState, *AnalysisSweepState): those have other
// consumers, and demanding the jobs page read every one of their fields would
// claim more than this contract is about. A container read is the honest bar
// there.
func jobsFieldPaths(rt reflect.Type, prefix string) []string {
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && strings.HasPrefix(ft.Name(), "jobs") {
			out = append(out, jobsFieldPaths(ft, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}

// jsTopLevelFnRe matches a top-level function declaration and captures its
// name and its (possibly empty) parameter list.
var jsTopLevelFnRe = regexp.MustCompile(`(?m)^function\s+([A-Za-z_]\w*)\s*\(([^)]*)\)`)

// jsFunctionBodies splits a console module into its top-level function
// declarations, name → source from the declaration to the next one. The
// same "\nfunction " boundary the reference-parity scan in this package
// already uses; it is coarse (a nested function is part of its parent) and
// that is fine here — a nested helper's reads belong to the parent's scope
// as far as aliases go.
func jsFunctionBodies(src string) map[string]string {
	out := map[string]string{}
	locs := jsTopLevelFnRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[name] = src[loc[0]:end]
	}
	return out
}

// jobsReadPaths returns every property path app.js reads off the /api/jobs
// snapshot, rooted at the snapshot and with aliases resolved: `lyr.syncedRows`
// after `const lyr = j.lyrics` comes back as "lyrics.syncedRows", and
// `cov.eligible` inside a one-parameter function that is called with
// `an.coverage` comes back as "analysis.coverage.eligible".
//
// Aliases are scoped to the FUNCTION BODY they are declared in. The first
// draft collected every one-parameter function's parameter into a single map
// keyed on the parameter name — and app.js has seven one-arg functions whose
// parameter is `s` and seven whose parameter is `r`. None is called with a
// snapshot path today, so nothing crossed; but the moment one was, reads
// inside the other six would have been attributed to it, which is a false
// pass in the direction that matters. So: each body resolves against its own
// const/let/var bindings plus its own parameter, and a parameter is bound
// only when some call site passes it an expression that itself resolves.
//
// Both roots are accepted (see newJobsScopes) — `j` is renderJobCards'
// binding and `jobs` is renderSettingsPrereqs' — so a field the settings
// prerequisites read counts as rendered, which is what the top-level guard
// already allowed.
//
// What it follows: member chains, `?.`, const/let/var aliases (including a
// bare re-alias of the root, `const snap = j`), and a one-parameter helper's
// parameter when a call site hands it a resolvable expression. What it does
// NOT follow: a value that comes back out of a call (`const lyr = pick(j)`),
// which would need return-flow analysis. That is a deliberate stop, and the
// direction it fails in is the safe one — such a read is reported as MISSING
// and names the path, so the author sees it at once; it can never quietly
// satisfy a field nothing reads.
func jobsReadPaths(src string) map[string]bool {
	sc := newJobsScopes(src)
	sc.bindParams()
	return sc.reads()
}

// jobsScopes is the per-function view of app.js the resolver works from:
// each body's const/let/var aliases, its single parameter name if it has
// exactly one, and — once bindParams has run — what that parameter is bound
// to at a resolvable call site.
//
// isRoot decides which identifier IS the payload, and takes the enclosing
// function because the two payloads this file guards need different answers.
// /api/jobs arrives under package-wide names (`j`, `jobs`) that nothing else
// in app.js uses. /api/stats arrives as applyStats' PARAMETER, which is `s` —
// and app.js has seven one-argument functions whose parameter is `s`, so a
// root that ignored the enclosing function would attribute six other
// functions' reads to the stats snapshot. That is a false PASS, the direction
// this file exists to avoid.
type jobsScopes struct {
	bodies map[string]string            // fn → source
	locals map[string]map[string]string // fn → alias → expression
	param  map[string]string            // fn → its one parameter name
	bound  map[string]string            // fn → snapshot path its parameter carries
	isRoot func(fn, ident string) bool  // is this identifier the payload here?
}

func newJobsScopes(src string) *jobsScopes {
	return newSnapshotScopes(src, func(_, ident string) bool {
		// Both roots are accepted — `j` is renderJobCards' binding and
		// `jobs` is renderSettingsPrereqs'.
		return ident == "j" || ident == "jobs"
	})
}

func newSnapshotScopes(src string, isRoot func(fn, ident string) bool) *jobsScopes {
	sc := &jobsScopes{
		bodies: jsFunctionBodies(src),
		locals: map[string]map[string]string{},
		param:  map[string]string{},
		bound:  map[string]string{},
		isRoot: isRoot,
	}
	for name, body := range sc.bodies {
		m := map[string]string{}
		for _, a := range jsAliasRe.FindAllStringSubmatch(body, -1) {
			m[a[1]] = jsNormPath(a[2])
		}
		sc.locals[name] = m
		if fm := jsOneParamFnRe.FindStringSubmatch(body); fm != nil && fm[1] == name {
			sc.param[name] = fm[2]
		}
	}
	return sc
}

// jsNormPath folds optional chaining into plain member access.
func jsNormPath(s string) string { return strings.ReplaceAll(s, "?.", ".") }

// resolve turns an expression seen inside fn into a snapshot-rooted path:
// "" for the root itself, "lyrics.syncedRows" for a chain, false when the
// expression does not lead back to `j` or `jobs` at all. Depth-bounded so a
// cycle (`const a = b.x; const b = a.y`) cannot spin.
func (sc *jobsScopes) resolve(fn, expr string) (string, bool) {
	return sc.walk(fn, jsNormPath(expr), 0)
}

func (sc *jobsScopes) walk(fn, expr string, depth int) (string, bool) {
	if depth > 8 {
		return "", false
	}
	root, rest, _ := strings.Cut(expr, ".")
	if sc.isRoot(fn, root) {
		// The root itself resolves to the empty path, so a direct re-alias
		// (`const snap = j`) and a helper handed the whole snapshot both
		// work; joinPath folds the empty base away.
		return rest, true
	}
	base, ok := sc.baseOf(fn, root, depth)
	if !ok {
		return "", false
	}
	return joinPath(base, rest), true
}

// baseOf resolves a bare identifier inside fn: a local alias first, then
// the function's own parameter if a call site has bound it.
func (sc *jobsScopes) baseOf(fn, ident string, depth int) (string, bool) {
	if expr := sc.locals[fn][ident]; expr != "" {
		return sc.walk(fn, expr, depth+1)
	}
	if sc.param[fn] == ident && sc.bound[fn] != "" {
		return sc.bound[fn], true
	}
	return "", false
}

func joinPath(base, rest string) string {
	switch {
	case rest == "":
		return base
	case base == "":
		return rest
	default:
		return base + "." + rest
	}
}

// bindParams discovers, from call sites, what each one-parameter function's
// parameter carries — resolved in the CALLER's scope. Iterated because a
// helper can pass its own parameter on (`function A(x) { B(x.sub) }`), so a
// binding can depend on one found in an earlier round; bounded, since app.js
// is not going to nest that more than a handful deep.
func (sc *jobsScopes) bindParams() {
	for round := 0; round < 4; round++ {
		if !sc.bindRound() {
			return
		}
	}
}

func (sc *jobsScopes) bindRound() (changed bool) {
	for callee, p := range sc.param {
		if p == "" || sc.bound[callee] != "" {
			continue
		}
		if path, ok := sc.findBinding(callee); ok {
			sc.bound[callee] = path
			changed = true
		}
	}
	return changed
}

// findBinding returns the first call site of callee whose argument resolves.
func (sc *jobsScopes) findBinding(callee string) (string, bool) {
	callRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(callee) + `\(\s*([A-Za-z_]\w*(?:\??\.[A-Za-z_]\w*)*)\s*\)`)
	for caller, body := range sc.bodies {
		c := callRe.FindStringSubmatch(body)
		if c == nil {
			continue
		}
		if path, ok := sc.resolve(caller, c[1]); ok {
			return path, true
		}
	}
	return "", false
}

// reads collects every member read that resolves to a snapshot path, with
// every prefix of a path counted too: `lyrics.syncedRows` also establishes
// `lyrics`, and `analysis.coverage.eligible` establishes `analysis.coverage`.
func (sc *jobsScopes) reads() map[string]bool {
	read := map[string]bool{}
	for fn, body := range sc.bodies {
		for _, m := range jsMemberReadRe.FindAllStringSubmatch(body, -1) {
			path, ok := sc.resolve(fn, m[1]+m[2])
			if !ok || path == "" {
				continue
			}
			parts := strings.Split(path, ".")
			for i := 1; i <= len(parts); i++ {
				read[strings.Join(parts[:i], ".")] = true
			}
		}
	}
	return read
}

// TestEveryJobsFieldIsRenderedSomewhere is the direction the sibling above did
// not have, and the one that would have caught #891 on the day it landed.
//
// That PR's stated purpose was to give `AtlasLyricsStats` a caller: "a Jobs
// card". It populated `jobsSnapshotResponse.Lyrics`, added a test asserting the
// JSON, and touched no template and no JS — so the field marshalled correctly
// into a response nothing read. `rg -i lyric` over templates/ and static/
// returned one hit, in an unrelated sentence about uploads. The feature was
// invisible, and the endpoint paid a full-table scan every thirty seconds to
// produce numbers no pixel consumed.
//
// The sibling walks JS → Go: every `jobs.<field>` names a real field. It cannot
// see a field nobody reads. This walks Go → JS, which is the repo's
// both-directions idiom (TestEveryDocumentedEndpointIsRouted /
// ...IsDocumented) applied to the one contract that had only one side.
//
// A test asserting the DTO is not evidence the console renders it. That is the
// "test passes while the wiring is dead" shape this tree keeps meeting, and it
// is why this reads the console source rather than the handler.
//
// It walks LEAVES, not top-level containers. The first version recorded
// twelve container names and was satisfied by `j.lyrics` alone — so the nine
// numbers inside it, the ones the card actually renders, were exactly as
// unguarded as `lyrics` had been before #900, one level down. CodeRabbit
// caught that on a post-merge review of #900 (its notice had been rate-limited
// at the time; see CLAUDE.md), and making it recursive found two fields already
// in that state: `analysis.intervalSec` and `analysis.coverage.totalLocal`,
// both computed on every snapshot for nobody. Both are gone.
func TestEveryJobsFieldIsRenderedSomewhere(t *testing.T) {
	paths := jobsFieldPaths(reflect.TypeOf(jobsSnapshotResponse{}), "")
	// Vacuous-pass guard, the same one the sibling carries: a reflection walk
	// that stops finding fields reports no problems, which is the single
	// outcome that hides the drift. The floor is set above the twelve
	// containers on purpose — a walk that stopped recursing would report
	// exactly twelve and look healthy.
	if len(paths) < 40 {
		t.Fatalf("only %d json leaf paths found under jobsSnapshotResponse — the "+
			"reflection walk has stopped recursing, so this test proves nothing", len(paths))
	}

	// Comments stripped, string literals KEPT. Most of the nested reads sit
	// inside template interpolations — `${lyr.syncedRows} synced` — and
	// stripJSNoise blanks every template literal along with the comments, so
	// under it nine of the lyrics card's reads vanished and the guard reported
	// them unrendered. CLAUDE.md already records that stripJSNoise is the wrong
	// tool when the literals are the subject; here they are.
	body := stripJSComments(readConsoleJS(t, "static/app.js"))
	read := jobsReadPaths(body)
	// Same floor on the JS side: alias resolution that silently stopped
	// working would leave only the direct `j.x` reads, which is under a dozen.
	nested := 0
	for p := range read {
		if strings.Contains(p, ".") {
			nested++
		}
	}
	if nested < 30 {
		t.Fatalf("only %d nested snapshot reads resolved in app.js — the alias "+
			"resolution is broken, so this test proves nothing", nested)
	}

	for _, p := range paths {
		if !read[p] {
			t.Errorf("/api/jobs returns %q and app.js never reads it.\n"+
				"A field the console does not render is a query the endpoint runs for "+
				"nobody — and if it was meant to be rendered, nothing else will say so.\n"+
				"Render it, or drop it from the response.", p)
		}
	}
}

// --- /api/stats -------------------------------------------------------------

// statsEntryFn is the app.js function the SSE `stats` frame is handed to. It
// is passed as a CALLBACK (`safeApply("stats", e.data, applyStats)`), so the
// resolver's call-site parameter binding cannot discover it the way it does
// for an ordinary helper — the argument at that call site is `e.data`, which
// resolves to nothing. Naming it here is that missing edge.
const statsEntryFn = "applyStats"

// statsCLIConsumer is the OTHER consumer of this payload, and the reason a
// stats guard is not a copy of the jobs one. `bridge status` fetches
// /api/stats into a map[string]any and prints ten of its fields; /api/jobs has
// no such second reader. A sweep that only looked at app.js would report
// libraryName, serverVersion, protocolVersion, uptimeSec, listenAddress,
// adminAddress and fingerprint as unrendered, all seven falsely.
const statsCLIConsumer = "../../cmd/bridge/status.go"

// statsFieldPaths returns every json name on statsResponse, and fails if one
// of them is anything but a scalar wire leaf.
//
// statsResponse is FLAT today, which is why the jobs guard's hardest design
// question — where to stop recursing — does not arise here. That is a fact
// about the current type, not a property of it, so it is asserted rather than
// assumed: a future nested field would otherwise be admitted silently as a
// container leaf, satisfied by one read of the container, with everything
// inside it exactly as unguarded as `lyrics` was before #900. Whoever adds one
// has to decide what the bar is for its contents, the way jobsFieldPaths'
// docblock had to.
func statsFieldPaths(t *testing.T, rt reflect.Type) []string {
	t.Helper()
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		// Fail CLOSED on every non-scalar, not merely on a nested struct: a
		// map, slice, array or interface marshals nested leaves just as a
		// struct does, and `reads` records every PREFIX of a path — so one
		// read of the container would count as covering all of them.
		// sourcesResponse.Servers is a []sourceServerRow, so this is not a
		// hypothetical shape for a payload in this package. (CodeRabbit on
		// #948.) time.Time is the one struct that IS a wire leaf (a string);
		// pointers were unwrapped above, so *time.Time and *string pass.
		if ft != reflect.TypeOf(time.Time{}) && !isScalarWireKind(ft.Kind()) {
			t.Fatalf("statsResponse.%s is not a scalar wire leaf (%s).\n"+
				"This guard walks statsResponse as a flat payload and would count "+
				"one read of %q as covering every JSON leaf inside it.\n"+
				"Decide what the bar is for those leaves — jobsFieldPaths recurses "+
				"into the private per-endpoint DTOs and stops at the shared ones — "+
				"and teach this walk the same.", f.Name, ft, name)
		}
		out = append(out, name)
	}
	return out
}

// isScalarWireKind reports whether a Go kind marshals to a JSON scalar — the
// allowlist half of the flatness assertion, so an unfamiliar kind is refused
// rather than admitted by an incomplete list of the ones to reject.
func isScalarWireKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.String:
		return true
	default:
		return false
	}
}

// statsReadPaths returns every property path app.js reads off the /api/stats
// snapshot, rooted at applyStats' parameter and scoped to that function.
func statsReadPaths(t *testing.T, src string) map[string]bool {
	t.Helper()
	// Built with a nil isRoot and wired afterwards rather than closing over
	// `sc` before its own assignment completes: the constructor never calls
	// isRoot, so both work, but the circular form reads as though it might.
	// (Gemini on #948.)
	sc := newSnapshotScopes(src, nil)
	sc.isRoot = func(fn, ident string) bool {
		return fn == statsEntryFn && ident != "" && ident == sc.param[statsEntryFn]
	}
	if sc.bodies[statsEntryFn] == "" {
		t.Fatalf("no top-level function %q found in app.js — it was renamed or "+
			"inlined, and this guard now proves nothing", statsEntryFn)
	}
	if sc.param[statsEntryFn] == "" {
		t.Fatalf("%s does not take exactly one parameter — the snapshot root "+
			"cannot be identified, so this guard now proves nothing", statsEntryFn)
	}
	sc.bindParams()
	return sc.reads()
}

// statsKeysReadBy returns every key the named Go file reads out of a
// `stats[...]` map, by AST.
//
// By AST and not by regex because the thing being matched is a STRING
// LITERAL: this repo's scan-based guards strip comments precisely because the
// prose beside a rule quotes the rule, and the tool that does it
// (stripJSNoise) blanks literals along with them — which would blank exactly
// the subject here. The AST has no such ambiguity, and it also ignores a field
// name merely mentioned in a docblock, which status.go does.
func statsKeysReadBy(t *testing.T, path string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ix, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		id, ok := ix.X.(*ast.Ident)
		if !ok || id.Name != "stats" {
			return true
		}
		lit, ok := ix.Index.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if key, err := strconv.Unquote(lit.Value); err == nil {
			out[key] = true
		}
		return true
	})
	return out
}

// TestEveryStatsFieldIsReadSomewhere is TestEveryJobsFieldIsRenderedSomewhere
// for the other live payload, and it is deliberately not a copy of it.
//
// /api/stats is the SSE stream's busiest frame — every five seconds, to every
// connected console — and four of its fields had no reader at all when this
// was written:
//
//   - `tracksUnreadable`, which readStatsDBPart runs a dedicated indexed
//     COUNT for and nothing else reads. A query per snapshot for nobody: the
//     #891 shape exactly, and the one the originating issue asked to surface.
//     It is now the dashboard's alarm row.
//   - `upnpRoutedTracks`, whose count IS earned (trackSourceCounts hands it
//     to /api/sources, which the dashboard renders) but whose scalar here was
//     a second spelling of `sources.routedTotal`.
//   - `startedAt`, a second spelling of `uptimeSec` on the same payload.
//   - `dbBytes`, an os.Stat per snapshot for a number /api/diagnostics
//     already serves and the console already reads from there.
//
// All three duplicates are gone rather than exempted, and that is the point:
// an exemption list is the path of least resistance that recreates the very
// failure this guards. A field that no named consumer reads is not on the
// payload.
//
// "Named consumer" is the one place this differs in SHAPE from the jobs
// guard, and it is not a relaxation — see statsCLIConsumer. A `bridge status
// --json` dump does not count: it passes the whole decoded map through
// verbatim, so it would exonerate every field including the four above, which
// is the same as having no guard.
func TestEveryStatsFieldIsReadSomewhere(t *testing.T) {
	fields := statsFieldPaths(t, reflect.TypeOf(statsResponse{}))
	// Vacuous-pass guard, the same one both siblings carry.
	if len(fields) < 12 {
		t.Fatalf("only %d json fields found on statsResponse — the reflection "+
			"walk is broken, so this test proves nothing", len(fields))
	}

	// Comments stripped, string literals KEPT: the alarm row's read sits
	// beside a template interpolation, and stripJSNoise blanks those.
	console := statsReadPaths(t, stripJSComments(readConsoleJS(t, "static/app.js")))
	if len(console) < 6 {
		t.Fatalf("only %d reads resolved inside %s — the root resolution is "+
			"broken, so this test proves nothing", len(console), statsEntryFn)
	}
	cli := statsKeysReadBy(t, statsCLIConsumer)
	if len(cli) < 6 {
		t.Fatalf("only %d stats keys found in %s — the AST scan is broken, so "+
			"this test proves nothing", len(cli), statsCLIConsumer)
	}

	for _, f := range fields {
		if console[f] || cli[f] {
			continue
		}
		t.Errorf("/api/stats returns %q and nothing reads it.\n"+
			"Not %s in static/app.js, and not %s.\n"+
			"This payload is rebuilt every five seconds for every connected "+
			"console, so a field nobody reads is work the endpoint does for "+
			"nobody — and if it was meant to be rendered, nothing else will say "+
			"so.\nRender it, or drop it from the response.",
			f, statsEntryFn, statsCLIConsumer)
	}
}

// TestBridgeStatusOnlyReadsRealStatsFields is the other direction, and it
// guards a failure that is silent in the same way the jobs one was.
//
// writeStatusHuman builds its rows from `stats["<key>"]` lookups on a
// map[string]any and then SKIPS every row whose value formatted empty — so a
// key that does not exist on the payload yields nil, formats to "", and the
// row simply is not printed. Renaming a json tag on statsResponse drops a line
// from `bridge status` with no error, no build failure and no failing test;
// it is the map-lookup twin of reading an undefined property in JS.
func TestBridgeStatusOnlyReadsRealStatsFields(t *testing.T) {
	valid := map[string]bool{}
	for _, f := range statsFieldPaths(t, reflect.TypeOf(statsResponse{})) {
		valid[f] = true
	}
	keys := statsKeysReadBy(t, statsCLIConsumer)
	if len(keys) < 6 {
		t.Fatalf("only %d stats keys found in %s — the AST scan is broken, so "+
			"this test proves nothing", len(keys), statsCLIConsumer)
	}
	for k := range keys {
		if !valid[k] {
			t.Errorf("%s reads stats[%q], which /api/stats does not return.\n"+
				"Fields it does return: %s\n"+
				"A missing key is nil, not an error — writeStatusHuman formats it "+
				"to \"\" and skips the row, so the line silently stops printing.",
				statsCLIConsumer, k, sortedKeys(valid))
		}
	}
}
