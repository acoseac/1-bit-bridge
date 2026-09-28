package integrity

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOrphanSidecarSweeperStatusFollowsTheRefusalLatch — the console's
// Jobs card said "on" while every tick refused, and only the journal said
// otherwise. Status reports the latch: which kind of refusal a streak is,
// and since when; a streak of another kind restarts the clock, and an
// empty catalog over a tree that holds sidecar files is one; a tick that
// decided nothing leaves it; the first tick that proceeds clears it.
func TestOrphanSidecarSweeperStatusFollowsTheRefusalLatch(t *testing.T) {
	skipWhereModesDenyNothing(t)
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 20)
	stranded := seedTestSidecarTree(t, dir, "stranded-", 15)
	locked := filepath.Join(dir, "locked")
	hidden := seedTestSidecarTree(t, locked, "stranded-", 1000)
	ageFixtures(t, dir)
	l := &switchableLister{rows: rowsNaming(live, stranded, hidden)}
	s := NewOrphanSidecarSweeper(l, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond

	if got := s.Status(); got != (OrphanSweepStatus{}) {
		t.Fatalf("before any tick: %+v, want the zero status", got)
	}
	s.tick(context.Background())
	if got := s.Status(); got != (OrphanSweepStatus{}) {
		t.Fatalf("a tick that proceeds: %+v, want not refusing", got)
	}

	// The rows for the 1,015 stranded files are lost: a lost index.
	l.rows = rowsNaming(live)
	s.tick(context.Background())
	first := s.Status()
	if first.Refusing != OrphanRefusalMassOrphans || first.Since.IsZero() {
		t.Fatalf("a refused tick: %+v, want massOrphans with a start", first)
	}
	s.tick(context.Background())
	if got := s.Status(); got != first {
		t.Errorf("the streak's second tick moved the status: %+v, want %+v", got, first)
	}

	// A tick that decided nothing is evidence of nothing.
	l.err = errors.New("database is locked")
	s.tick(context.Background())
	l.err = nil
	if got := s.Status(); got != first {
		t.Errorf("a failed listing moved the status: %+v, want %+v", got, first)
	}

	// An empty catalog over a tree that holds sidecar files refuses, and is a
	// streak of its own kind, with its own start.
	l.rows = nil
	s.tick(context.Background())
	empty := s.Status()
	if empty.Refusing != OrphanRefusalEmptyCatalog || !empty.Since.After(first.Since) {
		t.Errorf("an empty catalog: %+v, want emptyCatalog since after %v", empty, first.Since)
	}

	// The locked directory hides 1,000 of them: 15 of 35 against 20 rows
	// passes the mass-orphan check, and the walk is partial. A streak of
	// the other kind starts its own clock.
	lockDir(t, locked)
	l.rows = rowsNaming(live)
	s.tick(context.Background())
	partial := s.Status()
	if partial.Refusing != OrphanRefusalPartialWalk || !partial.Since.After(empty.Since) {
		t.Errorf("after the walk turned partial: %+v, want partialWalk since after %v", partial, empty.Since)
	}

	// The rows come back and the directory opens: the tick proceeds.
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	l.rows = rowsNaming(live, stranded, hidden)
	s.tick(context.Background())
	if got := s.Status(); got != (OrphanSweepStatus{}) {
		t.Errorf("a tick that proceeds after the streak: %+v, want not refusing", got)
	}
}

// TestOrphanSidecarSweeperStatusIsReadableBesideTheRunningLoop — the Jobs
// handler reads Status on its own goroutine while the sweep's run goroutine
// moves the latch; under -race this is the test that says the two do not
// share a variable unsynchronised.
func TestOrphanSidecarSweeperStatusIsReadableBesideTheRunningLoop(t *testing.T) {
	dir, live := strandedTree(t)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	ticked := make(chan struct{}, 1)
	s.SetOnTickComplete(func(int) {
		select {
		case ticked <- struct{}{}:
		default:
		}
	})
	stop := s.Start(context.Background())
	defer stop()

	deadline := time.After(5 * time.Second)
	for {
		if s.Status().Refusing == OrphanRefusalMassOrphans {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the boot tick of a stranded tree never showed as refusing: %+v", s.Status())
		case <-time.After(time.Millisecond):
		}
	}
	<-ticked
	if (*OrphanSidecarSweeper)(nil).Status() != (OrphanSweepStatus{}) {
		t.Error("a nil sweeper's status is not the zero status")
	}
}

// TestEveryOrphanRefusalKindIsListed — OrphanRefusalKinds is the list the
// console's wording test runs over (TestEveryOrphanRefusalKindIsWorded, in
// internal/admin), so a kind the sweep can report and the list leaves out
// would reach the Jobs card as its bare key with every test green. This
// reads the package's source for every constant of type OrphanRefusalKind
// and requires the list to hold exactly those.
func TestEveryOrphanRefusalKindIsListed(t *testing.T) {
	declared := declaredOrphanRefusalKinds(t)
	if len(declared) < 3 {
		t.Fatalf("found %d OrphanRefusalKind constant(s); the scan has drifted, so this test proves nothing", len(declared))
	}
	listed := map[string]bool{}
	for _, k := range OrphanRefusalKinds() {
		listed[string(k)] = true
		if !declared[string(k)] {
			t.Errorf("OrphanRefusalKinds lists %q, which no constant declares", k)
		}
	}
	for k := range declared {
		if !listed[k] {
			t.Errorf("the kind %q is declared and not in OrphanRefusalKinds: the console's wording test cannot see it", k)
		}
	}
}

// declaredOrphanRefusalKinds returns the value of every constant this
// package's non-test source declares with type OrphanRefusalKind. A file is
// chosen by its name before it is opened: the go tool compiles no `.`- or
// `_`-prefixed file, and an editor's `.#name.go` lock can be a dangling
// symlink.
func declaredOrphanRefusalKinds(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "OrphanRefusalKind" {
				return false
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: an OrphanRefusalKind constant whose value is not a string literal; this test cannot read it", name)
					continue
				}
				if s, err := strconv.Unquote(lit.Value); err == nil {
					out[s] = true
				}
			}
			return false
		})
	}
	return out
}
