package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/admin"
)

// What a fingerprint pass does with what reaches it after enrich.enabled is
// switched off (backlog B617). The pass's context asks the switch once an
// hour in these tests (switchEvery), so only AcoustID's client, which asks
// the switch at its send and at its answer, and the sweep's writes, which
// ask it again, can stop the pass.

// switchAcoustIDNoMatch is AcoustID's clean answer for a fingerprint it does
// not know.
const switchAcoustIDNoMatch = `{"status":"ok","results":[]}`

// sweepEnded waits for the sweeper's pass under way to end.
func sweepEnded(t *testing.T, status *sweepStatus[admin.FingerprintSweepCounts]) {
	t.Helper()
	eventually(t, func() bool {
		running, _, lastEnd, _, _ := status.snapshot()
		return !running && !lastEnd.IsZero()
	}, "the sweep to end")
}

// verdicts reads what the store keeps of AcoustID's answers: the files with a
// no-match verdict and the files whose match was accepted.
func (f *sweepSwitchFixture) verdicts(t *testing.T) (noMatch, matched []string) {
	t.Helper()
	ctx := context.Background()
	nm, err := f.store.FreshAcoustIDNoMatches(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for p := range nm {
		noMatch = append(noMatch, p)
	}
	m, err := f.store.AcoustIDMatchedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for p := range m {
		matched = append(matched, p)
	}
	return noMatch, matched
}

// TestAnAcoustIDAnswerAfterTheFlipIsNotKept: a lookup AcoustID answers after
// enrich.enabled is switched off, before anything else has asked the switch,
// is not taken: the client's gate refuses the answer and stops the pass, so
// the next file is not looked up, and nothing is cached, persisted or
// re-queued, a no-match included, which would have kept the file from every
// sweep for 30 days. Switched on again, the next pass asks about the same
// files and keeps what AcoustID says (backlog B617).
func TestAnAcoustIDAnswerAfterTheFlipIsNotKept(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		match        bool
	}{
		{name: "a match", answer: switchAcoustIDMatch, match: true},
		{name: "a no-match", answer: switchAcoustIDNoMatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSweepSwitchFixtureAnswering(t, tc.answer, "a.flac", "b.flac")
			f.sweeper.switchEvery = time.Hour
			var on atomic.Bool
			on.Store(true)
			holding, release := make(chan struct{}), make(chan struct{})
			var n atomic.Int64
			f.acoustid.setHold(func(r *http.Request) {
				if n.Add(1) != 1 {
					return
				}
				close(holding)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			status := &sweepStatus[admin.FingerprintSweepCounts]{}
			nudge := make(chan struct{}, 1)
			runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nudge, status)
			select {
			case <-holding:
			case <-time.After(10 * time.Second):
				t.Fatal("the sweep's first lookup never reached AcoustID")
			}
			on.Store(false)
			close(release)
			sweepEnded(t, status)

			if got := f.acoustid.requests(); len(got) != 1 {
				t.Errorf("AcoustID was asked %d time(s), want the held lookup alone:\n%s", len(got), strings.Join(got, "\n"))
			}
			if n := f.sweeper.cache.Len(); n != 0 {
				t.Errorf("the pass the switch stopped cached %d outcome(s)", n)
			}
			if noMatch, matched := f.verdicts(t); len(noMatch)+len(matched) != 0 {
				t.Errorf("the pass the switch stopped persisted no-match %v and match %v", noMatch, matched)
			}
			for _, name := range []string{"a.flac", "b.flac"} {
				if f.requeued(t, name) {
					t.Errorf("the pass the switch stopped re-queued %s", name)
				}
			}

			on.Store(true)
			nudge <- struct{}{}
			if tc.match {
				eventually(t, func() bool { return f.requeued(t, "a.flac") && f.requeued(t, "b.flac") },
					"both files to be resolved and re-queued once enrichment is on")
			} else {
				eventually(t, func() bool { noMatch, _ := f.verdicts(t); return len(noMatch) == 2 },
					"both no-matches to be kept once enrichment is on")
			}
			if got := f.acoustid.count(); got != 3 {
				t.Errorf("AcoustID was asked %d time(s), want 3: the refused lookup, then both files again:\n%s",
					got, strings.Join(f.acoustid.requests(), "\n"))
			}
		})
	}
}

// TestALookupAfterTheFlipIsRefusedAtItsSend: enrich.enabled switched off
// while the pass decodes its second file (so after the first file's answer
// was taken and kept) stops the second lookup at AcoustID's client, before
// anything else has asked the switch. The pass re-queues nothing, and forgets
// the first file's match it did not hand over, which kept the file out of
// every later pass in this process; its provenance, recorded while on, stays.
// Switched on again, both files are looked up and re-queued (backlog B617).
func TestALookupAfterTheFlipIsRefusedAtItsSend(t *testing.T) {
	f := newSweepSwitchFixture(t, "a.flac", "b.flac")
	f.sweeper.switchEvery = time.Hour
	var on atomic.Bool
	on.Store(true)
	decode := f.sweeper.compute
	var decodes atomic.Int64
	f.sweeper.compute = func(ctx context.Context, abs string, length time.Duration) (acoustid.Fingerprint, error) {
		if decodes.Add(1) == 2 {
			on.Store(false)
		}
		return decode(ctx, abs, length)
	}
	status := &sweepStatus[admin.FingerprintSweepCounts]{}
	nudge := make(chan struct{}, 1)
	runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nudge, status)
	eventually(t, func() bool { return decodes.Load() >= 2 }, "the sweep to decode its second file")
	sweepEnded(t, status)

	if got := f.acoustid.requests(); len(got) != 1 {
		t.Errorf("AcoustID was asked %d time(s), want the first file's lookup alone:\n%s", len(got), strings.Join(got, "\n"))
	}
	if n := f.sweeper.cache.Len(); n != 0 {
		t.Errorf("the pass the switch stopped left %d outcome(s) cached", n)
	}
	if _, matched := f.verdicts(t); len(matched) != 1 {
		t.Errorf("the provenance of the match taken while on = %v, want one file", matched)
	}
	for _, name := range []string{"a.flac", "b.flac"} {
		if f.requeued(t, name) {
			t.Errorf("the pass the switch stopped re-queued %s", name)
		}
	}

	on.Store(true)
	nudge <- struct{}{}
	eventually(t, func() bool { return f.requeued(t, "a.flac") && f.requeued(t, "b.flac") },
		"both files to be resolved and re-queued once enrichment is on")
	if got := f.acoustid.count(); got != 3 {
		t.Errorf("AcoustID was asked %d time(s), want 3: the first file, then both again:\n%s",
			got, strings.Join(f.acoustid.requests(), "\n"))
	}
}

// TestAFingerprintPassRunsOnTheSwitchsContext pins what the behavioural
// tests above cannot see, since each sets switchEvery: runFingerprintSweeper
// runs each pass on enrichgate.CancelWhenOff over enrich.enabled at the
// sweeper's switchEvery, and nothing outside a test sets that field, so a
// production pass asks the switch at enrichgate.Interval, once a second
// (enrichgate's own tests pin the interval and its bound) (backlog B617).
func TestAFingerprintPassRunsOnTheSwitchsContext(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var calls []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name != "runFingerprintSweeper" || n.Body == nil {
					return true
				}
				ast.Inspect(n.Body, func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "enrichgate.CancelWhenOff" {
						calls = append(calls, types.ExprString(call))
					}
					return true
				})
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "switchEvery" {
						t.Errorf("%s sets switchEvery: %s", fset.Position(n.Pos()), types.ExprString(lhs))
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "switchEvery" {
					t.Errorf("%s sets switchEvery in a literal", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	want := "enrichgate.CancelWhenOff(ctx, enrichmentOn, s.switchEvery)"
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("runFingerprintSweeper's calls of enrichgate.CancelWhenOff = %q, want %q alone", calls, want)
	}
}
