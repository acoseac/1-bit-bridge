package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
)

// filterScopeCommands are the four whose scope flag is --filter and whose
// EMPTY scope means the whole library, each paired with its real entry point.
// Driving the actual function rather than a name lookup, so this cannot pass
// against a command that was renamed out from under it.
var filterScopeCommands = []struct {
	name string
	run  func(context.Context, []string, io.Writer, io.Writer) int
}{
	{"render", renderCmd},
	{"upscale", upscaleCmd},
	{"optimize", optimizeCmd},
	{"analyze", analyzeCmd},
}

// TestFilterCommandsRefuseAPositionalScope.
//
// flag.Parse stops at the first non-flag argument, so `bridge render "Kind of
// Blue"` parses cleanly with --filter still empty — and empty means every
// eligible track. The operator asked for one album and got the library: hours
// of work per DSD track, GBs of sidecars, and one indexed_at delta row per
// track to every paired device.
//
// Driving the real command functions, not scanning the source: a source scan
// would find this file's own commentary, and would not prove the guard runs
// before anything expensive.
//
// The config path is deliberately bogus. The guard must run BEFORE config
// resolution — it depends only on the parsed args — so a refusal that needed a
// loadable config would be a guard in the wrong place.
func TestFilterCommandsRefuseAPositionalScope(t *testing.T) {
	for _, tc := range filterScopeCommands {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := tc.run(context.Background(),
				[]string{"--config", "/nonexistent/bridge.yaml", "Kind of Blue"},
				&stdout, &stderr)

			if code != 2 {
				t.Fatalf("exit = %d, want 2 — a positional scope must be refused, not silently widened (stderr: %s)",
					code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "Kind of Blue") {
				t.Errorf("the refusal does not name the argument it refused: %s", stderr.String())
			}
			if !strings.Contains(stderr.String(), "--filter") {
				t.Errorf("the refusal does not name the flag to use instead: %s", stderr.String())
			}
			// The load-bearing half: nothing was walked or started. A guard
			// that refused AFTER classifying the library would still exit 2.
			if stdout.Len() != 0 {
				t.Errorf("the command produced output before refusing: %q", stdout.String())
			}
		})
	}
}

// TestFilterCommandsAcceptTheFlagForm is the negative control: the same scope
// given the documented way must NOT be refused by the guard. Without it, a
// command that refused every invocation would pass the test above.
//
// These reach config resolution and fail there (the path is bogus), which is
// past the guard — so the assertion is on the message, not on the exit code.
func TestFilterCommandsAcceptTheFlagForm(t *testing.T) {
	for _, tc := range filterScopeCommands {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			tc.run(context.Background(),
				[]string{"--config", "/nonexistent/bridge.yaml", "--filter", "Kind of Blue"},
				&stdout, &stderr)
			if strings.Contains(stderr.String(), "unexpected argument") {
				t.Errorf("the flag form was refused as a positional: %s", stderr.String())
			}
		})
	}
}

// flagSetDeclaresFilter matches a --filter flag declaration on a flagset.
//
// It matches the IDENTIFIER, not the flag name: readPackageFile runs the body
// through stripGoComments, which blanks string LITERALS as well as comments,
// so `fs.String("filter", "", "…")` reaches this scan as
// `fs.String("", "", "")`. A pattern anchored on the literal would match
// nothing and the sweep would pass vacuously — which is exactly what the
// checked == 0 floor below caught while this test was being written.
var flagSetDeclaresFilter = regexp.MustCompile(`(?m)^\s*filter\s*:?=\s*fs\.String\(`)

// TestEveryFilterFlagsetGuardsItsPositionals is the sweep that keeps the class
// closed. PR #856 added this guard to `enrichment retry` and the four --filter
// commands were not swept; the next command to grow a --filter would repeat it.
//
// Comments are stripped first, for the reason the CSS and JS guards in this
// repo already record: the prose beside a fix explains the defect BY QUOTING
// IT, so an unstripped scan finds the commentary and reports the bug as still
// present.
func TestEveryFilterFlagsetGuardsItsPositionals(t *testing.T) {
	checked := 0
	for _, f := range nonTestGoFilesInPackage(t) {
		body := readPackageFile(t, f) // comments already stripped
		if !flagSetDeclaresFilter.MatchString(body) {
			continue
		}
		checked++
		// parseTranscodeArgs, not refuseFilterPositional: the guard has to
		// run IMMEDIATELY after Parse, before the flag can be read, so the
		// two steps are one call. Requiring the combined helper is what
		// stops the next command from copying only the first half — which
		// is how these four came to be missing a guard `enrichment retry`
		// already had.
		if !strings.Contains(body, "parseTranscodeArgs(") {
			t.Errorf("%s declares a --filter flag but does not parse through parseTranscodeArgs. "+
				"flag.Parse stops at the first non-flag argument, so a positional scope parses "+
				"with --filter empty — and empty means the WHOLE LIBRARY.", f)
		}
		if strings.Contains(body, "fs.Parse(args)") && !strings.Contains(body, "func parseTranscodeArgs") {
			t.Errorf("%s calls fs.Parse directly on a --filter flagset. Route it through "+
				"parseTranscodeArgs so the positional guard cannot be left behind.", f)
		}
	}
	if checked == 0 {
		t.Fatal("no file declares a --filter flag — the scan is not seeing the package, " +
			"so this test would pass no matter what")
	}
}

// TestPathScopedCommandsRefuseAPositionalScope is the --path half of the family
// TestFilterCommandsRefuseAPositionalScope covers.
//
// `--filter` was never the whole class. `bridge duplicates "Miles Davis"` and
// `bridge enrichment misses "Blue Note"` both parse with --path EMPTY, and an
// empty path scope is the whole library: the prefix helpers emit a query with
// no WHERE clause at all, so the operator gets a full-table answer presented as
// the scoped one they asked for. Read-only, which is why these two survived
// #856 (which fixed their sibling `enrichment retry`) and #882 (which fixed the
// four --filter commands) — the class was closed twice and never swept.
func TestPathScopedCommandsRefuseAPositionalScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arg is what the operator typed, and each row asserts on ITS OWN
		// value: accepting either row's argument would pass a command that
		// echoed the wrong one back. (CodeRabbit on #897.)
		arg string
		run func(arg string, stdout, stderr io.Writer) int
	}{
		{"duplicates", "Miles Davis", func(arg string, stdout, stderr io.Writer) int {
			return duplicatesCmd(context.Background(), []string{arg}, stdout, stderr)
		}},
		{"enrichment misses", "Blue Note", func(arg string, stdout, stderr io.Writer) int {
			return enrichmentCmd(context.Background(), []string{"misses", arg}, stdout, stderr)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// Exit 2, not merely non-zero: that is this CLI's code for invalid
			// input, and 1 would mean the command ran and failed — which is
			// the outcome being refused.
			if code := tc.run(tc.arg, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2 — a positional scope must be refused as invalid "+
					"input, not run and reported as the scoped answer.\nstdout: %s",
					code, stdout.String())
			}
			// The message has to show the flag form, or the operator is told
			// "no" without being told how.
			if !strings.Contains(stderr.String(), "--path") {
				t.Errorf("refusal does not show the --path form:\n%s", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.arg) {
				t.Errorf("refusal does not quote back %q:\n%s", tc.arg, stderr.String())
			}
		})
	}
}
