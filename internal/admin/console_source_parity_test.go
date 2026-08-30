package admin

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
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
