package manifest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// sqlOpenSite is a production function that calls sql.Open (or sql.OpenDB),
// by its file, relative to the module root, and its name.
type sqlOpenSite struct{ file, fn string }

// sqlOpenSites are the sql.Open sites of production code, each with why it may
// open what it opens (backlog B653 PR 2a, T6): a fourth site must be added
// here with its reason, and a site that opens the live database only behind
// the lifetime lock's hold.
var sqlOpenSites = map[sqlOpenSite]string{
	{"internal/manifest/store_open_busy.go", "openStoreDB"}:              "the store's open, reached from OpenStoreAdmitted alone, after its hold",
	{"internal/manifest/migration_pending.go", "PendingMigrations"}:      "the live database's version, after the hold",
	{"internal/backup/vacuum.go", "vacuumIntoWithHook"}:                  "a live snapshot's source, reached from captureInto alone, under its hold",
	{"internal/backup/verify_db.go", "openSnapshotReader"}:               "a snapshot's database, read-only and immutable: a copy, never the live database",
	{"internal/manifest/test_seams.go", "WriteDatabaseAtVersionForTest"}: "test-only: a fixture's database",
	{"internal/backup/backuptest/backuptest.go", "*"}:                    "test-only: a package tests alone import",
}

// The admission functions, each with the calls that must come in this order
// by their position in its body: the hold first, the journal under it, and
// the call that leads to the open last (Codex on PR 2a's brief, finding 6:
// the hold and the open are in different functions, so the order is checked
// where the hold is taken, and the chain to the open apart).
var admissionOrder = map[sqlOpenSite][]string{
	{"internal/manifest/store.go", "OpenStoreAdmitted"}:             {"holdStoreShared", "CheckRestoreJournal", "symlinkAtStore", "Precreate", "lockLadder", "openStoreDB"},
	{"internal/manifest/migration_pending.go", "PendingMigrations"}: {"holdStoreShared", "CheckRestoreJournal", "Open"},
	{"internal/backup/backup.go", "captureInto"}:                    {"holdLiveStore", "completeCapturedPlayerSeal", "MkdirAll", "createUniqueSnapshotDir", "vacuumInto"},
	{"internal/backup/restore_store.go", "holdLiveStore"}:           {"HoldLiveStore", "CheckRestoreJournal"},
}

// The only production callers each opener of the live database may have:
// the chain from the admission function to the sql.Open.
var allowedCallers = map[string][]string{
	"openStoreDB":        {"OpenStoreAdmitted"},
	"vacuumIntoWithHook": {"vacuumInto"},
	"vacuumInto":         {"captureInto"},
}

// TestEveryOpenerOfTheLiveDatabaseTakesTheStoreLock (T6): a sweep, by the
// AST with no comments, over every non-test .go file of the module: each
// sql.Open is at a listed site with its reason; each opener of the live
// database is reached only through its admission function; and in each
// admission function the hold comes before the journal and both before what
// leads to the open, by position. So a fifth opener of the database cannot
// skip the lock without failing here.
func TestEveryOpenerOfTheLiveDatabaseTakesTheStoreLock(t *testing.T) {
	root := moduleRoot(t)
	found := map[sqlOpenSite]bool{}
	callers := map[string][]string{} // callee name -> production functions calling it
	calls := map[sqlOpenSite][]namedCall{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" ||
				name == "vendor" || sweeptest.IsOtherCheckout(root, path) || sweeptest.IsMakeTestTmpDir(root, path)) {
				return filepath.SkipDir
			}
			return nil
		}
		// Decided from the name before anything is opened.
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			site := sqlOpenSite{rel, fd.Name.Name}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := calleeName(call)
				if callee == "" {
					return true
				}
				calls[site] = append(calls[site], namedCall{callee, call.Pos()})
				if !slices.Contains(callers[callee], fd.Name.Name) {
					callers[callee] = append(callers[callee], fd.Name.Name)
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "sql" && (sel.Sel.Name == "Open" || sel.Sel.Name == "OpenDB") {
						found[site] = true
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("the sweep found no sql.Open at all: it read nothing")
	}
	for site := range found {
		if _, ok := sqlOpenSites[site]; ok {
			continue
		}
		if _, ok := sqlOpenSites[sqlOpenSite{site.file, "*"}]; ok {
			continue
		}
		t.Errorf("%s: %s calls sql.Open at no listed site: an opener of the live database takes the store's lifetime lock first "+
			"(holdStoreShared, manifest.HoldLiveStore); list it in sqlOpenSites with its reason", site.file, site.fn)
	}
	for site, why := range sqlOpenSites {
		if site.fn != "*" && !found[site] {
			t.Errorf("%s: %s is listed (%s) but opens nothing: the list is stale", site.file, site.fn, why)
		}
	}
	for callee, want := range allowedCallers {
		got := callers[callee]
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s is called by %v in production, want %v alone: an opener of the live database is reached only "+
				"through the admission function that holds the store's lock", callee, got, want)
		}
	}
	for site, order := range admissionOrder {
		var positions []token.Pos
		for _, want := range order {
			pos := token.NoPos
			for _, c := range calls[site] {
				if c.name == want {
					pos = c.pos
					break
				}
			}
			if pos == token.NoPos {
				t.Errorf("%s: %s calls no %s", site.file, site.fn, want)
				continue
			}
			positions = append(positions, pos)
		}
		if len(positions) == len(order) && !slices.IsSorted(positions) {
			t.Errorf("%s: %s calls %v out of this order (the hold, the journal, then what leads to the open)", site.file, site.fn, order)
		}
	}
}

// namedCall is a call by its callee's name, at its position.
type namedCall struct {
	name string
	pos  token.Pos
}

// calleeName is the name a call calls: f in f(…) and x.f(…).
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}
