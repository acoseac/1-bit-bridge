package admin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The matrix in ops/settings-apply-semantics.md is the contract the cloud
// control plane reads to decide which settings need a supervised restart.
// A wrong row there is worse than no row: it either sends an operator to
// bounce a bridge that already applied the change, or — the direction
// that actually costs something — tells a control plane a field is live
// when it is still waiting on a restart nobody scheduled.
//
// This repo has been bitten by exactly that shape before. A CLAUDE.md
// entry claiming the extractors did not read PCM geometry for WAV/AIFF
// stayed wrong for months while the fix was already merged, and a later
// session re-derived a whole "extractor gap" from it. The correction
// there records the lesson as "check the extractor before believing a doc
// about it". This test is the version of that check that runs on its own.
//
// It reads the doc's own table and drives the real handler for each row.

// matrixRowRe pulls `| \`field\` | class | \`status\` | notes |` out of the
// doc. The status cell can carry more than one code (`live` / `restart`
// for the conditional fields), so every code in the cell is collected and
// the observed one has to be among them.
var matrixRowRe = regexp.MustCompile("(?m)^\\|\\s*`([a-zA-Z][a-zA-Z0-9]*)`\\s*\\|([^|]*)\\|([^|]*)\\|")

// statusCodeRe finds the backticked status codes inside a status cell.
var statusCodeRe = regexp.MustCompile("`(live|restart|unchanged)`")

func TestMatrixDocMatchesWhatTheHandlerReports(t *testing.T) {
	doc, err := os.ReadFile("../../ops/settings-apply-semantics.md")
	if err != nil {
		t.Fatalf("read the matrix doc: %v", err)
	}

	// Scope the scan to the matrix section. The file carries a SECOND
	// three-column table (the control-plane restart list) whose rows have
	// the same shape, and whose third column is prose. Today none of that
	// prose contains a backticked status code, so those rows are skipped
	// — but "today" is the whole problem: one edit adding "`restart` at
	// provision time" to a recommendation would silently override the
	// real matrix row, and the vacuous-pass guard below would not notice
	// because the count stays the same.
	section := matrixSection(t, string(doc))

	rows := map[string][]string{} // field -> allowed statuses
	for _, m := range matrixRowRe.FindAllStringSubmatch(section, -1) {
		field, statusCell := m[1], m[3]
		var codes []string
		for _, c := range statusCodeRe.FindAllStringSubmatch(statusCell, -1) {
			codes = append(codes, c[1])
		}
		if len(codes) > 0 {
			rows[field] = codes
		}
	}
	// Vacuous-pass guard: a regex that stopped matching reports no
	// problems, which is the one outcome that hides the drift.
	if len(rows) < 20 {
		t.Fatalf("only %d matrix rows scraped (%v) — the table shape changed and this test "+
			"is no longer reading it", len(rows), keysOfStrings(rows))
	}

	// Every PATCH field must have a row. A field missing from the matrix
	// is a field the control plane has no answer for.
	for _, tag := range patchJSONTags(t) {
		if _, ok := patchTagsExemptFromReporting[tag]; ok {
			continue
		}
		if _, ok := rows[tag]; !ok {
			t.Errorf("field %q has no row in ops/settings-apply-semantics.md — the control "+
				"plane has no documented answer for it", tag)
		}
	}

	// And every documented status must match what the handler actually
	// does, on a bridge wired the way the matrix describes.
	for field, allowed := range rows {
		newValue, ok := differentValueFor(t, field)
		if !ok {
			continue // no mechanical "other value"; the notes column covers these
		}
		t.Run(field, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			// The matrix describes a bridge with the feature's wiring
			// present — that is the shape the notes column explains, and
			// the conditional rows say so in their own status cell.
			srv.deps.TriggerAutoOptimizeSweep = func() bool { return true }
			srv.deps.TriggerCadenceRearm = func() {}

			var resp settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
				map[string]any{field: newValue}, &resp); code != 200 {
				t.Fatalf("patch %s=%v: %d", field, newValue, code)
			}
			got := string(resp.Fields[field].Status)
			if got == string(applyUnchanged) {
				t.Fatalf("differentValueFor produced the stored value (%v), so this row "+
					"is not actually being checked", newValue)
			}
			for _, a := range allowed {
				if a == got {
					return
				}
			}
			t.Errorf("matrix says %v, handler reports %q — one of the two is telling the "+
				"control plane the wrong thing about whether a restart is needed",
				allowed, got)
		})
	}
}

func keysOfStrings(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// matrixSection returns the body of the "## The matrix" heading, up to the
// next second-level heading.
//
// Anchored on the heading rather than on the table's own delimiters
// because a table is easy to move and a heading is not: if the section is
// renamed the test fails loudly here instead of silently scanning nothing.
func matrixSection(t *testing.T, doc string) string {
	const heading = "\n## The matrix\n"
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("no %q heading in the matrix doc — the section was renamed and this test "+
			"would otherwise scan the wrong table", strings.TrimSpace(heading))
	}
	rest := doc[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
