package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// configWriters names every function in the tree that writes bridge.yaml,
// with the call by which it takes the file's lock (backlog B506): the lock
// beside the config file that each read-modify-write of it holds, so that two
// writers take turns and none saves a copy read before its turn. A writer is
// a function that calls the config's Save or SaveIf, that saves through the
// live holder's Update (serve's console), or that writes a file while it names
// the config's path (configPathNames), as restore copies a snapshot's
// bridge.yaml over it.
//
// A new writer goes here with its lock, which the sweep then requires it to
// call. The console's writers all go through (*RuntimeConfig).Update, which
// takes the lock itself, so theirs is the holder's Update.
var configWriters = map[string]string{
	"cmd/bridge/library.go:libraryAddCmd":                                "config.Edit",
	"cmd/bridge/library.go:libraryRemoveCmd":                             "config.Edit",
	"cmd/bridge/init.go:saveInitConfig":                                  "config.LockFile",
	"cmd/bridge/main.go:writeAutoInitConfig":                             "config.LockFile",
	"cmd/bridge/output_dac_bind.go:runDACBind":                           "config.LockFile",
	"cmd/bridge/upnp_upstream_admin.go:upnpAdminAdapter.updateCfg":       "cfgHolder.Update",
	"internal/admin/handlers_api.go:Server.applyRootAddition":            "CfgHolder.Update",
	"internal/admin/handlers_api.go:Server.applyRootRemoval":             "CfgHolder.Update",
	"internal/admin/handlers_api.go:Server.apiSettingsPatch":             "CfgHolder.Update",
	"internal/admin/handlers_variants_dir.go:Server.apiVariantsDirPatch": "CfgHolder.Update",
	"internal/backup/backup.go:RestoreWithOptions":                       "config.LockFile",
	"internal/config/lock.go:Edit":                                       "LockFile",
	"internal/config/runtime.go:RuntimeConfig.Update":                    "LockFile",
}

// configSavePrimitive is the config's own save, in internal/config: the
// methods that write the file, which the sweep does not count as writers
// but requires to refuse a save in no turn of the lock (heldFor), so that
// a writer of the config cannot write without one.
var configSavePrimitive = map[string]bool{
	"internal/config/config.go:Config.Save":   true,
	"internal/config/config.go:Config.SaveIf": true,
	"internal/config/config.go:Config.save":   true,
}

// notConfigWriters are the functions that write a file while they name the
// config's path and still write no config file, each with what it writes
// instead. The sweep requires each to still be one of those, so an entry
// cannot outlive the reason it was written for.
var notConfigWriters = map[string]string{
	"internal/backup/backup.go:Snapshot": "reads bridge.yaml into a snapshot, and writes the snapshot's own files",
}

// configPathNames are the names this tree gives a config file's path: a
// function that writes a file while it names one of them is a writer of the
// config (the sweep's third kind).
var configPathNames = map[string]bool{
	"BridgeYAML": true, "CfgPath": true, "cfgPath": true, "configPath": true,
	"cfgFile": true, "resolvedCfgPath": true, "absCfgPath": true,
}

// fileWriteCalls are the calls that write a file's bytes or put a file in
// place, as the sweep renders them (callChain).
var fileWriteCalls = map[string]bool{
	"os.WriteFile": true, "os.Create": true, "os.OpenFile": true, "os.Rename": true,
	"atomicwrite.WriteBytes": true, "atomicwrite.WriteBytesDurable": true,
	"atomicwrite.RenameWithRetry": true, "atomicwrite.RenameWithRetryCtx": true,
	"copyFile": true, "writeFrom": true,
}

// The kinds of write the sweep tells apart.
const (
	writesBySave   = "calls the config's Save or SaveIf"
	writesByEdit   = "changes the config through config.Edit"
	writesByHolder = "saves through the live config holder's Update"
	writesByFile   = "writes a file while it names the config's path"
)

// configWriteSite is one function the sweep found writing the config.
type configWriteSite struct {
	pos   token.Position
	kinds []string
	// calls is every call the function makes, rendered by callChain.
	calls map[string]bool
}

// TestEveryConfigWriterTakesTheConfigLock is the pin of backlog B506: every
// function in the tree that writes bridge.yaml is named in configWriters, and
// calls the lock named there. Until B506 library add and remove and init
// saved with no lock, bind narrowed the window with SaveIf's byte comparison
// alone, and a serve's console save wrote its own copy over whatever another
// writer had saved, so two writers could each lose the other's update.
func TestEveryConfigWriterTakesTheConfigLock(t *testing.T) {
	root := repoRootForCitations(t)
	sites, primitive, files := findConfigWriters(t, root)
	// The floor: a sweep that reads nothing, or finds none of the writers it
	// exists for, passes whatever the code does.
	if files < 300 || len(sites) < len(configWriters)/2 {
		t.Fatalf("read %d files and found %d writers of the config; the sweep looks at nothing it was written for", files, len(sites))
	}
	for _, p := range configWriterProblems(sites, configWriters, notConfigWriters) {
		t.Error(p)
	}
	for key := range configSavePrimitive {
		calls, ok := primitive[key]
		switch {
		case !ok:
			t.Errorf("the sweep found no %s, the config's own save: configSavePrimitive is out of date", key)
		case key == "internal/config/config.go:Config.save" && !callsLock(calls, "heldFor"):
			t.Errorf("%s does not ask heldFor, so a save could write in no turn of bridge.yaml's lock", key)
		}
	}
}

// configWriterProblems compares what the sweep found with the writers named
// and the functions excused, and says what does not agree, in a fixed order.
func configWriterProblems(sites map[string]configWriteSite, named, excused map[string]string) []string {
	var problems []string
	for key, why := range excused {
		if site, ok := sites[key]; !ok || len(site.kinds) != 1 || site.kinds[0] != writesByFile {
			problems = append(problems, fmt.Sprintf("notConfigWriters excuses %s (%s), which the sweep no longer finds "+
				"writing a file while it names the config's path, and nothing else: remove it", key, why))
		}
	}
	for key, site := range sites {
		if _, ok := excused[key]; ok {
			continue
		}
		lock, ok := named[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s %s, and configWriters does not name it: "+
				"take bridge.yaml's lock around its read-modify-write (config.Edit, or config.LockFile and a read under it), "+
				"then name it there with that call", site.pos, key, strings.Join(site.kinds, " and ")))
			continue
		}
		if !callsLock(site.calls, lock) {
			problems = append(problems, fmt.Sprintf("%s: %s %s and never calls %s, the lock configWriters names for it",
				site.pos, key, strings.Join(site.kinds, " and "), lock))
		}
	}
	for key := range named {
		if _, ok := sites[key]; !ok {
			problems = append(problems, fmt.Sprintf("configWriters names %s, which the sweep no longer finds writing the config: "+
				"remove it, or rename it to the function that writes now", key))
		}
	}
	sort.Strings(problems)
	return problems
}

// callsLock reports whether calls holds lock: the chain itself, or a chain
// that ends with it after a dot (s.deps.CfgHolder.Update ends with
// CfgHolder.Update).
func callsLock(calls map[string]bool, lock string) bool {
	for c := range calls {
		if c == lock || strings.HasSuffix(c, "."+lock) {
			return true
		}
	}
	return false
}

// findConfigWriters sweeps the Go files this module builds, test files
// excepted, and returns the writers of the config it found, keyed as
// configWriters keys them, the calls each part of the config's own save
// makes (configSavePrimitive), and how many files it read.
func findConfigWriters(t *testing.T, root string) (map[string]configWriteSite, map[string]map[string]bool, int) {
	t.Helper()
	sites := map[string]configWriteSite{}
	primitive := map[string]map[string]bool{}
	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return moduleDirRule(root, path, d.Name())
		}
		if name := d.Name(); !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files++
		found, calls, err := scanConfigWriters(filepath.ToSlash(rel), strings.ReplaceAll(string(raw), "\r\n", "\n"))
		if err != nil {
			return err
		}
		for k, v := range found {
			sites[k] = v
		}
		for k, v := range calls {
			primitive[k] = v
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites, primitive, files
}

// scanConfigWriters returns the functions in the file name (its slash path
// from the module root) that write the config, keyed "name:Func" or
// "name:Recv.Method", and the calls of each part of the config's own save it
// declares (configSavePrimitive), which are not counted as writers. A
// function literal is part of the function it is written in. Inside
// internal/config a call of the unexported save is a save too. A method
// declared Save or SaveIf on anything but the config is itself reported,
// under writesBySave's premise: that the config's are the only ones.
func scanConfigWriters(name, src string) (map[string]configWriteSite, map[string]map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", name, err)
	}
	inConfig := strings.HasPrefix(name, "internal/config/")
	out := map[string]configWriteSite{}
	primitive := map[string]map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		key := name + ":" + funcDeclKey(fn)
		if fn.Recv != nil && (fn.Name.Name == "Save" || fn.Name.Name == "SaveIf") && !configSavePrimitive[key] {
			out[key] = configWriteSite{pos: fset.Position(fn.Pos()),
				kinds: []string{"declares a method named " + fn.Name.Name + " on a type that is not the config, which the sweep would read as the config's"}}
			continue
		}
		calls := map[string]bool{}
		kinds := map[string]bool{}
		namesPath, writesFile := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				chain := callChain(x.Fun)
				calls[chain] = true
				if chain == "config.Edit" || (inConfig && chain == "Edit") {
					kinds[writesByEdit] = true
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					switch {
					case sel.Sel.Name == "Save" || sel.Sel.Name == "SaveIf" || (inConfig && sel.Sel.Name == "save"):
						kinds[writesBySave] = true
					case sel.Sel.Name == "Update":
						if holder := callChain(sel.X); strings.HasSuffix(strings.ToLower(holder), "cfgholder") {
							kinds[writesByHolder] = true
						}
					}
				}
				if fileWriteCalls[chain] {
					writesFile = true
				}
			case *ast.Ident:
				if configPathNames[x.Name] {
					namesPath = true
				}
			}
			return true
		})
		if namesPath && writesFile {
			kinds[writesByFile] = true
		}
		if configSavePrimitive[key] {
			primitive[key] = calls
			continue
		}
		if len(kinds) == 0 {
			continue
		}
		site := configWriteSite{pos: fset.Position(fn.Pos()), calls: calls}
		for k := range kinds {
			site.kinds = append(site.kinds, k)
		}
		sort.Strings(site.kinds)
		out[key] = site
	}
	return out, primitive, nil
}

// funcDeclKey is fn's name, after its receiver's type name for a method.
func funcDeclKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return "?." + fn.Name.Name
}

// callChain renders a call's function as its dotted chain of names
// (config.LockFile, s.deps.CfgHolder.Update), "?" standing for any part that
// is not a name (a call's result, an index).
func callChain(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return callChain(x.X) + "." + x.Sel.Name
	case *ast.ParenExpr:
		return callChain(x.X)
	case *ast.StarExpr:
		return callChain(x.X)
	}
	return "?"
}

// TestTheConfigWriterSweepReportsEveryShape runs the sweep over synthetic
// source. On a clean tree it reports nothing, so the tree alone cannot show
// that it still reports anything; this shows each shape it exists to catch,
// and each it must let through.
func TestTheConfigWriterSweepReportsEveryShape(t *testing.T) {
	const src = `package x

import (
	"os"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

type server struct{ deps struct{ CfgHolder *config.RuntimeConfig; CfgPath string } }

func savesUnnamed(cfg *config.Config, l *config.FileLock) { _ = cfg.Save(l) }

func savesNamedWithItsLock(path string) {
	l, _ := config.LockFile(path)
	defer l.Unlock()
	cfg, _ := config.Load(path)
	_ = cfg.SaveIf(l, nil)
}

func savesNamedWithoutItsLock(cfg *config.Config, l *config.FileLock) { _ = cfg.Save(l) }

func editsUnnamed(path string) {
	_ = config.Edit(path, func(*config.Config) (bool, error) { return true, nil })
}

func (s *server) savesThroughTheHolder() {
	_ = s.deps.CfgHolder.Update(s.deps.CfgPath, func(*config.Config) error { return nil })
}

func writesTheConfigsPath(cfgPath string) { _ = os.WriteFile(cfgPath, nil, 0o600) }

func writesAnotherFile(path string) { _ = os.WriteFile(path, nil, 0o600) }

func namesThePathAndWritesNothing(cfgPath string) { _, _ = os.ReadFile(cfgPath) }

func copiesThePathElsewhere(cfgPath, bundle string) { _ = os.WriteFile(bundle, []byte(cfgPath), 0o600) }

type other struct{}

func (other) Save() error { return nil }
`
	sites, _, err := scanConfigWriters("x/x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]string{
		"x/x.go:savesNamedWithItsLock":        "config.LockFile",
		"x/x.go:savesNamedWithoutItsLock":     "config.LockFile",
		"x/x.go:server.savesThroughTheHolder": "CfgHolder.Update",
		"x/x.go:gone":                         "config.LockFile",
	}
	excused := map[string]string{
		"x/x.go:copiesThePathElsewhere": "writes the bundle",
		"x/x.go:writesAnotherFile":      "writes no config file, and names no config path either",
	}
	got := configWriterProblems(sites, named, excused)
	want := []string{
		"configWriters names x/x.go:gone",
		"notConfigWriters excuses x/x.go:writesAnotherFile",
		"x/x.go:editsUnnamed changes the config through config.Edit, and configWriters does not name it",
		"x/x.go:savesNamedWithoutItsLock calls the config's Save or SaveIf and never calls config.LockFile",
		"x/x.go:savesUnnamed calls the config's Save or SaveIf, and configWriters does not name it",
		"x/x.go:writesTheConfigsPath writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:other.Save declares a method named Save on a type that is not the config",
	}
	if len(got) != len(want) {
		t.Fatalf("the sweep reported %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.Contains(g, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("the sweep did not report %q; it reported:\n%s", w, strings.Join(got, "\n"))
		}
	}
	for _, through := range []string{"writesAnotherFile", "namesThePathAndWritesNothing"} {
		if _, ok := sites["x/x.go:"+through]; ok {
			t.Errorf("the sweep took %s for a writer of the config", through)
		}
	}
	if site := sites["x/x.go:server.savesThroughTheHolder"]; len(site.kinds) != 1 || site.kinds[0] != writesByHolder {
		t.Errorf("a save through the holder was read as %q, want %q", site.kinds, writesByHolder)
	}
}
