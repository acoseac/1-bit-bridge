package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func variantsIndexCheck(t *testing.T, d Deps) Check {
	t.Helper()
	c := checkVariantsIndex(t.Context(), d)
	if c.Name != checkNameVariantsIndex {
		t.Fatalf("check name = %q, want %q", c.Name, checkNameVariantsIndex)
	}
	return c
}

// TestVariantsIndexAgreeingCountsAreOK — the ordinary bridge. The summary
// still carries both numbers, because "they agree" is only meaningful
// beside what they are.
func TestVariantsIndexAgreeingCountsAreOK(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{Rows: 1024, Files: 1024, Known: 1024}, nil
	}})
	if c.Status != OK {
		t.Fatalf("status=%v summary=%q", c.Status, c.Summary)
	}
	for _, want := range []string{"1024 variant row(s)", "1024 sidecar file(s)"} {
		if !strings.Contains(c.Summary, want) {
			t.Errorf("summary %q does not carry %q", c.Summary, want)
		}
	}
}

// TestVariantsIndexWarnsOnOrphansAndNamesExamples — an ordinary orphan
// crop. `--gc` would reclaim them, and the hint says so rather than
// alarming about a state the sweep handles.
func TestVariantsIndexWarnsOnOrphansAndNamesExamples(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{
			Rows: 900, Files: 1000, Known: 900, Orphans: 100,
			OrphanSample: []string{"A/Al/01.flac.upscaled-v1-192000-24.flac", "A/Al/02.flac.upscaled-v1-192000-24.flac"},
			VariantsDir:  "/srv/bridge-variants",
		}, nil
	}})
	if c.Status != Warn {
		t.Fatalf("status=%v, want warn: %q", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "100 of 1000 sidecar file(s)") || !strings.Contains(c.Summary, "900 row(s)") {
		t.Errorf("summary does not carry both counts: %q", c.Summary)
	}
	if !strings.Contains(c.Hint, "bridge upscale --gc` reclaims them") {
		t.Errorf("an ordinary crop should point at the sweep: %q", c.Hint)
	}
	if !strings.Contains(c.Hint, "A/Al/01.flac") {
		t.Errorf("hint names no example: %q", c.Hint)
	}
	if !strings.Contains(c.Hint, "+98 more") {
		t.Errorf("hint does not account for the orphans it did not name: %q", c.Hint)
	}
}

// TestVariantsIndexSaysWhenTheSweepWouldRefuse — the 2026-09-20 shape.
// The operator must not be sent to `--gc` here, and specifically must not
// be sent to --allow-mass-orphans, because the files are the only copy.
func TestVariantsIndexSaysWhenTheSweepWouldRefuse(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{
			Rows: 200, Files: 10248, Known: 200, Orphans: 10048,
			OrphanSample:      []string{"A/Al/01.flac.upscaled-v2-176400-24.flac"},
			WouldRefuseGC:     true,
			OrphansExceedRows: true,
			VariantsDir:       "/srv/bridge-variants",
		}, nil
	}})
	if c.Status != Warn {
		t.Fatalf("status=%v, want warn", c.Status)
	}
	if !strings.Contains(c.Hint, "LOST INDEX") || !strings.Contains(c.Hint, "REFUSES") {
		t.Errorf("hint does not say what this shape is: %q", c.Hint)
	}
	if strings.Contains(c.Hint, "reclaims them") {
		t.Errorf("the hint sends the operator to a sweep that would destroy the only copy: %q", c.Hint)
	}
}

// TestVariantsIndexScopesATruncatedWalk — a report built from part of a
// tree must say so. An all-clear that silently covered 20,000 of 200,000
// files is the confident-wrong-answer shape.
func TestVariantsIndexScopesATruncatedWalk(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{Rows: 90000, Files: 20000, Known: 20000, Truncated: true}, nil
	}})
	if c.Status != OK {
		t.Fatalf("status=%v", c.Status)
	}
	if !strings.Contains(c.Summary, "the first 20000 file(s)") || !strings.Contains(c.Summary, "the tree is larger") {
		t.Errorf("a truncated walk answered for the whole tree: %q", c.Summary)
	}
}

// TestVariantsIndexReportsAnUnreadableDirectory — its contents are absent
// from every count, so the counts are about part of the tree.
func TestVariantsIndexReportsAnUnreadableDirectory(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{Rows: 10, Files: 10, Known: 10, Unreadable: 3}, nil
	}})
	if !strings.Contains(c.Summary, "3 director(y/ies) could not be read") {
		t.Errorf("summary hides that the walk could not see part of the tree: %q", c.Summary)
	}
}

// TestVariantsIndexProbeFailureIsNotAnAllClear — "don't know" is not
// "fine", the same reading checkSidecarPaths takes.
func TestVariantsIndexProbeFailureIsNotAnAllClear(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{}, errors.New("permission denied")
	}})
	if c.Status != Warn {
		t.Fatalf("status=%v, want warn — a failed probe answered as ok", c.Status)
	}
	if !strings.Contains(c.Hint, "permission denied") {
		t.Errorf("hint drops the cause: %q", c.Hint)
	}
}

// TestVariantsIndexSkipsWithoutAProbeOrOnAManagedBridge — a fresh install
// has no manifest to compare, and a tenant has no shell for the commands
// the hint names (the log-file-size reason).
func TestVariantsIndexSkipsWithoutAProbeOrOnAManagedBridge(t *testing.T) {
	c := variantsIndexCheck(t, Deps{})
	if c.Status != OK || !strings.Contains(c.Summary, "run after the first scan") {
		t.Errorf("no probe: status=%v summary=%q", c.Status, c.Summary)
	}
	called := false
	c = variantsIndexCheck(t, Deps{Managed: true, VariantsIndex: func(context.Context) (VariantsIndex, error) {
		called = true
		return VariantsIndex{Rows: 1, Files: 9999, Orphans: 9998, WouldRefuseGC: true}, nil
	}})
	if c.Status != OK {
		t.Errorf("a managed bridge got a warning it cannot act on: %q", c.Summary)
	}
	if called {
		t.Error("a managed bridge still paid for the walk")
	}
}

// TestVariantsIndexWarnsWhenTheTreeIsEmptyButTheCatalogIsNot — the other
// direction, which nothing else reports: checkSidecarPaths sees only rows
// recorded OUTSIDE the current directory, and these point inside one that
// is empty or gone. "all referenced" would be a strange thing to say
// about no files at all.
func TestVariantsIndexWarnsWhenTheTreeIsEmptyButTheCatalogIsNot(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{Rows: 4096, Files: 0, VariantsDir: "/mnt/bridge-variants"}, nil
	}})
	if c.Status != Warn {
		t.Fatalf("status=%v, want warn: %q", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "4096 variant row(s)") || !strings.Contains(c.Summary, "no sidecar files") {
		t.Errorf("summary: %q", c.Summary)
	}
	if strings.Contains(c.Summary, "all referenced") {
		t.Errorf("an empty tree was reported as fully referenced: %q", c.Summary)
	}
	// It must not read as an emergency: both sweeps refuse to reap while
	// the directory looks unmounted, so nothing is being deleted.
	if !strings.Contains(c.Hint, "unmounted") || !strings.Contains(c.Hint, "nothing is being deleted") {
		t.Errorf("hint does not say the sweeps are already standing down: %q", c.Hint)
	}
	// An empty catalog over an empty tree is a bridge that never
	// transcoded anything — the one state this must stay quiet about.
	c = variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{}, nil
	}})
	if c.Status != OK {
		t.Errorf("a bridge that never transcoded anything warns: %q", c.Summary)
	}
}

// TestVariantsIndexWillNotGuessTheSweepsVerdictFromAPartialWalk — the
// sweep's ratio is over the WHOLE tree, and this probe walks under a
// budget. A truncated prefix that is 90% orphans can be followed by a
// tree of referenced files, so a verdict derived from it could tell an
// operator that `--gc` refuses when it would proceed — the
// confident-wrong-answer shape the check itself exists to catch, one
// level up. (CodeRabbit on #940.)
//
// What must SURVIVE the hedge is the lost-index warning: the terms behind
// it are monotone in the walk, and truncation is certain on exactly the
// large trees where the warning matters most. Downgrading those to
// "`--gc` reclaims them" would be the opposite advice about the one shape
// where the files are the only copy.
func TestVariantsIndexWillNotGuessTheSweepsVerdictFromAPartialWalk(t *testing.T) {
	c := variantsIndexCheck(t, Deps{VariantsIndex: func(context.Context) (VariantsIndex, error) {
		return VariantsIndex{
			Rows: 200, Files: 20000, Known: 200, Orphans: 19800,
			Truncated: true, Budget: 20000,
			// The probe leaves WouldRefuseGC false on a truncated walk;
			// the monotone lower bound it may still assert.
			OrphansExceedRows: true,
			OrphanSample:      []string{"A/Al/01.flac.upscaled-v2-176400-24.flac"},
			VariantsDir:       "/srv/bridge-variants",
		}, nil
	}})
	if c.Status != Warn {
		t.Fatalf("status=%v, want warn", c.Status)
	}
	if !strings.Contains(c.Hint, "LOST INDEX") {
		t.Errorf("the monotone warning was lost with the verdict: %q", c.Hint)
	}
	if strings.Contains(c.Hint, "REFUSES") {
		t.Errorf("a definite refusal was claimed from a partial walk: %q", c.Hint)
	}
	if strings.Contains(c.Hint, "reclaims them") {
		t.Errorf("a partial walk was reported as an ordinary orphan crop: %q", c.Hint)
	}
	if !strings.Contains(c.Hint, "cannot be told from a partial walk") {
		t.Errorf("hint does not say the verdict is out of reach: %q", c.Hint)
	}
	// The instruction it gives must be safe to follow: --gc refuses by
	// default, so "run it" cannot destroy anything.
	if !strings.Contains(c.Hint, "unlinks nothing when it refuses") {
		t.Errorf("hint does not say the suggested command is safe to run: %q", c.Hint)
	}
}
