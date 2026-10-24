package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// configWriters names every function in the tree that writes bridge.yaml,
// with the call by which it takes the file's lock (backlog B506): the lock
// beside the config file that each read-modify-write of it holds, so that two
// writers take turns and none saves a copy read before its turn. A writer is
// a function, or a package-level variable's value, that calls the config's
// Save or SaveIf or its turn's SaveBytes, or takes one of them as a value
// (`save := cfg.Save`), that changes the config through config.Edit, that
// saves through the live holder's Update or UpdateChecked (serve's console),
// or that writes a file (fileWriteCall) while it names the config's path
// (configPathNames, or the literal "bridge.yaml"), as restore once copied a
// snapshot's bridge.yaml over it.
//
// A new writer goes here with its lock, which the sweep then requires it to
// call, and, for one that saves itself, to call before its save and to decide
// on what that turn found (turnReadCall), reading the config in the turn
// through nothing else. The console's writers all go through
// (*RuntimeConfig).UpdateChecked, Update's own, which takes the lock itself,
// so theirs is the holder's Update or UpdateChecked, and Edit's callers' is
// Edit, which decides on its own turn's bytes.
//
// The sweep reads one function at a time: a helper that is handed the path
// by a caller in another function, and writes it, is a writer it does not
// see, since the helper names no config path and the caller writes nothing
// (copyFile and writeFrom are listed by name for that reason). That is its
// one known blind spot.
var configWriters = map[string]string{
	"cmd/bridge/library.go:libraryAddCmd":                                "config.Edit",
	"cmd/bridge/library.go:libraryRemoveCmd":                             "config.Edit",
	"cmd/bridge/init.go:saveInitConfig":                                  "config.LockFile",
	"cmd/bridge/main.go:writeAutoInitConfig":                             "config.LockFile",
	"cmd/bridge/output_dac_bind.go:runDACBind":                           "config.LockFile",
	"cmd/bridge/upnp_upstream_admin.go:upnpAdminAdapter.updateCfg":       "cfgHolder.Update",
	"internal/admin/handlers_api.go:Server.applyRootAddition":            "CfgHolder.UpdateChecked",
	"internal/admin/handlers_api.go:Server.applyRootRemoval":             "CfgHolder.UpdateChecked",
	"internal/admin/handlers_api.go:Server.apiSettingsPatch":             "CfgHolder.Update",
	"internal/admin/handlers_variants_dir.go:Server.apiVariantsDirPatch": "CfgHolder.Update",
	"internal/backup/backup.go:RestoreWithOptions":                       "config.LockFile",
	"internal/config/lock.go:Edit":                                       "LockFile",
	"internal/config/runtime.go:RuntimeConfig.UpdateChecked":             "LockFile",
}

// configSavePrimitive is the config's own save, in internal/config: the
// methods that write the file, which the sweep does not count as writers,
// each with the calls the sweep requires of it: the save refuses a turn of
// the lock it does not hold (heldFor), and the one write every save goes
// through compares the file with the one its turn found just before its
// rename (stillAsFound), so a writer of the config can neither write without
// a turn nor write over what another writer saved in it.
var configSavePrimitive = map[string][]string{
	"internal/config/config.go:Config.Save":      {"save"},
	"internal/config/config.go:Config.SaveIf":    {"save"},
	"internal/config/config.go:Config.save":      {"heldFor", "write"},
	"internal/config/lock.go:FileLock.SaveBytes": {"write"},
	"internal/config/lock.go:FileLock.write":     {"heldFor", "stillAsFound", "RenameWithRetryIf"},
}

// turnReadMethods are the names of a turn's own reads, the bytes LockFile
// found (FileLock.Found) and the config loaded from them (FileLock.LoadFound),
// which are what a writer that saves itself decides on.
var turnReadMethods = map[string]bool{"Found": true, "LoadFound": true}

// turnReadHelpers are this tree's functions a writer calls to decide on what
// its turn found, each of which the sweep requires to make one of the turn's
// own reads itself: init's comparison of the file it read at its start with
// the one its turn found, and a restore's of the file its caller read.
var turnReadHelpers = map[string]bool{"configChangedSinceRead": true, "configStillAsRead": true}

// notConfigWriters are the functions that write a file while they name the
// config's path and still write no config file, each with what it writes
// instead. The sweep requires each to still be one of those, so an entry
// cannot outlive the reason it was written for.
var notConfigWriters = map[string]string{
	"internal/backup/backup.go:Snapshot": "reads bridge.yaml into a snapshot, and writes the snapshot's own files",
}

// configPathNames are the names this tree gives a config file's path: a
// function that writes a file while it names one of them, or the literal
// "bridge.yaml" (configFileLiteral), is a writer of the config (the sweep's
// fourth kind).
var configPathNames = map[string]bool{
	"BridgeYAML": true, "CfgPath": true, "cfgPath": true, "configPath": true,
	"cfgFile": true, "resolvedCfgPath": true, "absCfgPath": true, "defaultConfigPath": true,
}

// fileWriteCalls are the calls, besides the ones fileWriteCall knows by their
// shape, that write a file's bytes or put a file in place, as the sweep
// renders them (callChain): this tree's own helpers that do (restore's copy).
var fileWriteCalls = map[string]bool{
	"copyFile": true, "writeFrom": true,
}

// fileWriteMethods are the names of the methods and functions that write a
// file or put one in place whatever they are called on: os's and an
// *os.Root's (WriteFile, Create, Rename; OpenFile only with a flag that
// writes, writeFlags).
var fileWriteMethods = map[string]bool{"WriteFile": true, "Create": true, "Rename": true}

// writeFlags are the open flags that make an OpenFile a write.
var writeFlags = map[string]bool{"O_WRONLY": true, "O_RDWR": true, "O_CREATE": true, "O_TRUNC": true, "O_APPEND": true}

// configReadCalls are the calls that read the config file whatever they are
// handed, as the sweep renders them; os.ReadFile, os.Open, os.Stat, os.Lstat
// and fsutil.ReadAsFile are reads of it when they name its path, and inside
// internal/config the package's own reads (Load, LoadFile, loadData) and any
// os.ReadFile or fsutil.ReadAsFile are. None of them is a turn's own read
// (turnReadCall): a writer that saves itself reads the config in its turn
// through the turn alone, so a Stat, a Load or any read its saved copy does
// not come from is reported there.
var configReadCalls = map[string]bool{
	"config.Load": true, "config.LoadFile": true, "config.LoadFileAt": true, "config.ReadFile": true,
	"loadCLIConfig": true, "loadCLIConfigFile": true, "readConfigAsIs": true,
}

// pathReadCalls are the reads that are a read of the config when they name
// its path.
var pathReadCalls = map[string]bool{
	"os.ReadFile": true, "os.Open": true, "os.Stat": true, "os.Lstat": true, "fsutil.ReadAsFile": true,
}

// The kinds of write the sweep tells apart.
const (
	writesBySave   = "calls the config's Save or SaveIf"
	writesByEdit   = "changes the config through config.Edit"
	writesByHolder = "saves through the live config holder's Update or UpdateChecked"
	writesByFile   = "writes a file while it names the config's path"
)

// configWriteSite is one function the sweep found writing the config.
type configWriteSite struct {
	pos   token.Position
	kinds []string
	// calls is every call the function makes, rendered by callChain, and
	// at where each call is made, in source order.
	calls map[string]bool
	at    []chainAt
	// reads are where it reads the config (configReadCall), and saves where
	// it saves it itself: a Save, SaveIf or SaveBytes, or a file write while
	// it names the config's path.
	reads, saves []token.Pos
}

// chainAt is one call a function makes and where.
type chainAt struct {
	chain string
	pos   token.Pos
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
	for key, wants := range configSavePrimitive {
		calls, ok := primitive[key]
		if !ok {
			t.Errorf("the sweep found no %s, the config's own save: configSavePrimitive is out of date", key)
			continue
		}
		for _, want := range wants {
			if !callsLock(calls, want) {
				t.Errorf("%s does not call %s, so a save could write in no turn of bridge.yaml's lock, "+
					"or over what another writer saved in it", key, want)
			}
		}
	}
	for _, p := range turnReadHelperProblems(primitive) {
		t.Error(p)
	}
}

// turnReadHelperProblems says which of turnReadHelpers the sweep did not
// find, and which it found making none of a turn's own reads, so that a
// helper the sweep counts as one cannot come to decide on a read of its own.
func turnReadHelperProblems(found map[string]map[string]bool) []string {
	var problems []string
	for name := range turnReadHelpers {
		seen, reads := false, false
		for key, calls := range found {
			if !strings.HasSuffix(key, ":"+name) {
				continue
			}
			seen = true
			for c := range calls {
				if i := strings.LastIndexByte(c, '.'); i >= 0 && turnReadMethods[c[i+1:]] {
					reads = true
				}
			}
		}
		switch {
		case !seen:
			problems = append(problems, fmt.Sprintf("the sweep found no %s: turnReadHelpers is out of date", name))
		case !reads:
			problems = append(problems, fmt.Sprintf("%s makes none of a turn's own reads (FileLock.LoadFound, FileLock.Found), "+
				"so a writer the sweep counts as deciding on its turn's bytes may decide on a read of its own", name))
		}
	}
	sort.Strings(problems)
	return problems
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
			continue
		}
		if p := lockOrderProblem(key, site, lock); p != "" {
			problems = append(problems, p)
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
		if isChain(c, lock) {
			return true
		}
	}
	return false
}

// isChain reports whether the rendered call c is lock: the chain itself, or
// one that ends with it after a dot.
func isChain(c, lock string) bool {
	return c == lock || strings.HasSuffix(c, "."+lock)
}

// lockOrderProblem says what is wrong with the order of a named writer that
// saves the config itself, and "" when nothing is: it must take its lock
// before it saves, decide its save on what that turn found, by one of the
// turn's own reads (turnReadCall) between the lock and its first save, and
// read the config there through nothing else (a writer may read before its
// turn to decide what to ask, as bind and init do, if it decides again on
// what its turn found). A writer that saves through Edit or the holder's
// Update is left out: Edit and Update are named writers themselves, and
// decide on their own turn's bytes. Positions are the source's, not the
// control flow's: a read in a loop after the save, or one through a helper
// configReadCalls does not name, is not seen, and nor is which bytes a save is
// made from: a writer that makes a turn's read and saves a copy it read
// before is not told apart.
func lockOrderProblem(key string, site configWriteSite, lock string) string {
	if len(site.saves) == 0 {
		return ""
	}
	lockAt := token.NoPos
	for _, c := range site.at {
		if isChain(c.chain, lock) {
			lockAt = c.pos
			break
		}
	}
	saveAt := site.saves[0]
	if lockAt > saveAt {
		return fmt.Sprintf("%s: %s saves the config before it takes %s", site.pos, key, lock)
	}
	inTurn := func(p token.Pos) bool { return p > lockAt && p < saveAt }
	for _, r := range site.reads {
		if inTurn(r) {
			return fmt.Sprintf("%s: %s reads the config in its turn of %s through something other than the turn: "+
				"decide on what the turn found (FileLock.LoadFound, FileLock.Found)", site.pos, key, lock)
		}
	}
	for _, c := range site.at {
		if inTurn(c.pos) && turnReadCall(c.chain) {
			return ""
		}
	}
	return fmt.Sprintf("%s: %s saves the config without deciding on what its turn of %s found: "+
		"a read before the turn, or a probe in it, is not the turn's (FileLock.LoadFound, FileLock.Found)", site.pos, key, lock)
}

// turnReadCall reports whether the rendered call chain is one of a turn's own
// reads (turnReadMethods, on whatever the turn is called) or a helper that
// decides on one (turnReadHelpers).
func turnReadCall(chain string) bool {
	if turnReadHelpers[chain] {
		return true
	}
	i := strings.LastIndexByte(chain, '.')
	return i >= 0 && turnReadMethods[chain[i+1:]]
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
// "name:Recv.Method", and a package-level variable's value that does, keyed
// "name:Var"; and the calls of each part of the config's own save it
// declares (configSavePrimitive), which are not counted as writers. A
// function literal is part of the function it is written in. Inside
// internal/config a call of the unexported save is a save too. A method
// declared Save, SaveIf or SaveBytes on anything but the config and its turn
// is itself reported, under writesBySave's premise: that theirs are the only
// ones.
func scanConfigWriters(name, src string) (map[string]configWriteSite, map[string]map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", name, err)
	}
	inConfig := strings.HasPrefix(name, "internal/config/")
	out := map[string]configWriteSite{}
	primitive := map[string]map[string]bool{}
	keep := func(key string, pos token.Pos, body ast.Node) {
		site, kinds := inspectConfigWriter(body, inConfig)
		if _, ok := configSavePrimitive[key]; ok {
			primitive[key] = site.calls
			return
		}
		if turnReadHelpers[key[strings.LastIndexByte(key, ':')+1:]] {
			primitive[key] = site.calls
		}
		if len(kinds) == 0 {
			return
		}
		site.pos = fset.Position(pos)
		for k := range kinds {
			site.kinds = append(site.kinds, k)
		}
		sort.Strings(site.kinds)
		out[key] = site
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			key := name + ":" + funcDeclKey(d)
			if _, isPrimitive := configSavePrimitive[key]; d.Recv != nil && saveMethods[d.Name.Name] && !isPrimitive {
				out[key] = configWriteSite{pos: fset.Position(d.Pos()),
					kinds: []string{"declares a method named " + d.Name.Name + " on a type that is not the config, which the sweep would read as the config's"}}
				continue
			}
			keep(key, d.Pos(), d.Body)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						keep(name+":"+id.Name, id.Pos(), vs.Values[i])
					}
				}
			}
		}
	}
	return out, primitive, nil
}

// saveMethods are the names of the config's own saves and its turn's: Save
// and SaveIf on the config, SaveBytes on the turn.
var saveMethods = map[string]bool{"Save": true, "SaveIf": true, "SaveBytes": true}

// inspectConfigWriter reads body, a function's or a variable's value, for
// what makes it a writer of the config: the kinds of write it makes, the
// calls it makes and where, where it reads the config and where it saves it.
// A save, an Edit or the holder's Update counts whether it is called or
// taken as a value (a method value is a save made later, through it).
func inspectConfigWriter(body ast.Node, inConfig bool) (configWriteSite, map[string]bool) {
	site := configWriteSite{calls: map[string]bool{}}
	kinds := map[string]bool{}
	namesPath := false
	var fileWrites []token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			chain := callChain(x.Fun)
			site.calls[chain] = true
			site.at = append(site.at, chainAt{chain, x.Pos()})
			if inConfig && chain == "Edit" {
				kinds[writesByEdit] = true
			}
			if fileWriteCall(x, chain) {
				fileWrites = append(fileWrites, x.Pos())
			}
			if configReadCall(x, chain, inConfig) {
				site.reads = append(site.reads, x.Pos())
			}
		case *ast.SelectorExpr:
			switch {
			case callChain(x) == "config.Edit":
				kinds[writesByEdit] = true
			case saveMethods[x.Sel.Name] || (inConfig && x.Sel.Name == "save"):
				kinds[writesBySave] = true
				site.saves = append(site.saves, x.Pos())
			case x.Sel.Name == "Update" || x.Sel.Name == "UpdateChecked":
				if holder := callChain(x.X); strings.HasSuffix(strings.ToLower(holder), "cfgholder") {
					kinds[writesByHolder] = true
				}
			}
		case *ast.Ident:
			if configPathNames[x.Name] {
				namesPath = true
			}
		case *ast.BasicLit:
			if configFileLiteral(x) {
				namesPath = true
			}
		}
		return true
	})
	if namesPath && len(fileWrites) > 0 {
		kinds[writesByFile] = true
		site.saves = append(site.saves, fileWrites...)
		sort.Slice(site.saves, func(i, j int) bool { return site.saves[i] < site.saves[j] })
	}
	return site, kinds
}

// fileWriteCall reports whether call, rendered as chain, writes a file or
// puts one in place: a listed helper (fileWriteCalls); any writer of
// atomicwrite or fsutil (a function whose name begins Write or Rename, and
// Precreate, which makes the file a child process fills); os's or an
// *os.Root's WriteFile, Create and Rename, called on anything; and OpenFile,
// on anything, with a flag that writes, or one the sweep cannot read.
func fileWriteCall(call *ast.CallExpr, chain string) bool {
	if fileWriteCalls[chain] {
		return true
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name := sel.Sel.Name
	if pkg := callChain(sel.X); pkg == "atomicwrite" || pkg == "fsutil" {
		if strings.HasPrefix(name, "Write") || strings.HasPrefix(name, "Rename") || name == "Precreate" {
			return true
		}
	}
	if fileWriteMethods[name] {
		return true
	}
	return name == "OpenFile" && len(call.Args) >= 2 && opensForWriting(call.Args[1])
}

// opensForWriting reports whether flag, an open's flag argument, may write:
// it names a flag that writes (writeFlags), or is something other than the
// os package's flags the sweep can read (a variable), which it counts as one.
func opensForWriting(flag ast.Expr) bool {
	writes, unread := false, false
	ast.Inspect(flag, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if callChain(x.X) == "os" && strings.HasPrefix(x.Sel.Name, "O_") {
				writes = writes || writeFlags[x.Sel.Name]
				return false
			}
			unread = true
			return false
		case *ast.Ident:
			unread = true
		}
		return true
	})
	return writes || unread
}

// configReadCall reports whether call, rendered as chain, reads the config
// file: a read that reads it whatever it is handed (configReadCalls; inside
// internal/config also Load, LoadFile, loadData, and any os.ReadFile or
// fsutil.ReadAsFile), or a read of a path (pathReadCalls) that names the
// config's.
func configReadCall(call *ast.CallExpr, chain string, inConfig bool) bool {
	if configReadCalls[chain] {
		return true
	}
	if inConfig {
		switch chain {
		case "Load", "LoadFile", "loadData", "os.ReadFile", "fsutil.ReadAsFile":
			return true
		}
	}
	if !pathReadCalls[chain] || len(call.Args) == 0 {
		return false
	}
	named := false
	ast.Inspect(call.Args[0], func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			named = named || configPathNames[x.Name]
		case *ast.BasicLit:
			named = named || configFileLiteral(x)
		}
		return !named
	})
	return named
}

// configFileLiteral reports whether lit is a string that names the config
// file: "bridge.yaml", alone or as a path's last element.
func configFileLiteral(lit *ast.BasicLit) bool {
	if lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	return v == "bridge.yaml" || strings.HasSuffix(v, "/bridge.yaml") || strings.HasSuffix(v, `\bridge.yaml`)
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
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

type server struct{ deps struct{ CfgHolder *config.RuntimeConfig; CfgPath string } }

func savesUnnamed(cfg *config.Config, l *config.FileLock) { _ = cfg.Save(l) }

func savesNamedWithItsLock(path string) {
	l, _ := config.LockFile(path)
	defer l.Unlock()
	cfg, _ := l.LoadFound()
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

func writesTheLiteral(dir string) { _ = os.WriteFile(filepath.Join(dir, "bridge.yaml"), nil, 0o600) }

func writesThroughARoot(r *os.Root, cfgPath string) { _ = r.WriteFile(filepath.Base(cfgPath), nil, 0o600) }

func createsThroughARoot(r *os.Root, cfgPath string) {
	f, _ := r.Create(filepath.Base(cfgPath))
	_ = f.Close()
}

func renamesThroughARoot(r *os.Root, cfgPath string) { _ = r.Rename("staged", filepath.Base(cfgPath)) }

func opensForWritingThroughARoot(r *os.Root, cfgPath string) {
	f, _ := r.OpenFile(filepath.Base(cfgPath), os.O_WRONLY|os.O_TRUNC, 0)
	_ = f.Close()
}

func opensForReadingThroughARoot(r *os.Root, cfgPath string) {
	f, _ := r.OpenFile(filepath.Base(cfgPath), os.O_RDONLY, 0)
	_ = f.Close()
}

func writesThroughAtomicwrite(cfgPath string) { _ = atomicwrite.WriteSomethingNew(cfgPath, nil) }

func writesThroughFsutil(cfgPath string) { _ = fsutil.WriteSomethingNew(cfgPath, nil) }

func precreatesThePath(cfgPath string) { _ = fsutil.Precreate(cfgPath, 0o600, cfgPath) }

func renamesOverThePath(staged, cfgPath string) { _ = os.Rename(staged, cfgPath) }

var savesFromAFuncValue = func(cfg *config.Config, l *config.FileLock) error { return cfg.Save(l) }

func savesThroughAMethodValue(cfg *config.Config, l *config.FileLock) {
	save := cfg.Save
	_ = save(l)
}

func readsBeforeItsLock(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	_ = cfg.Save(l)
}

func readsAgainInItsTurn(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ = config.Load(cfgPath)
	_ = cfg.Save(l)
}

func statsInItsTurn(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	_, _ = os.Stat(cfgPath)
	_ = cfg.Save(l)
}

func decidesOnWhatItsTurnFound(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ = l.LoadFound()
	_ = cfg.Save(l)
}

func savesOnlyWhereItsTurnFoundNone(cfgPath string, cfg *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if _, ok := l.Found(); ok {
		return
	}
	_ = cfg.Save(l)
}
`
	sites, _, err := scanConfigWriters("x/x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]string{
		"x/x.go:savesNamedWithItsLock":          "config.LockFile",
		"x/x.go:savesNamedWithoutItsLock":       "config.LockFile",
		"x/x.go:server.savesThroughTheHolder":   "CfgHolder.Update",
		"x/x.go:gone":                           "config.LockFile",
		"x/x.go:readsBeforeItsLock":             "config.LockFile",
		"x/x.go:readsAgainInItsTurn":            "config.LockFile",
		"x/x.go:statsInItsTurn":                 "config.LockFile",
		"x/x.go:decidesOnWhatItsTurnFound":      "config.LockFile",
		"x/x.go:savesOnlyWhereItsTurnFoundNone": "config.LockFile",
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
		"x/x.go:writesTheLiteral writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:writesThroughARoot writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:createsThroughARoot writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:renamesThroughARoot writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:opensForWritingThroughARoot writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:writesThroughAtomicwrite writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:writesThroughFsutil writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:precreatesThePath writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:renamesOverThePath writes a file while it names the config's path, and configWriters does not name it",
		"x/x.go:savesFromAFuncValue calls the config's Save or SaveIf, and configWriters does not name it",
		"x/x.go:savesThroughAMethodValue calls the config's Save or SaveIf, and configWriters does not name it",
		"x/x.go:readsBeforeItsLock saves the config without deciding on what its turn of config.LockFile found",
		"x/x.go:readsAgainInItsTurn reads the config in its turn of config.LockFile through something other than the turn",
		"x/x.go:statsInItsTurn reads the config in its turn of config.LockFile through something other than the turn",
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
	for _, through := range []string{"writesAnotherFile", "namesThePathAndWritesNothing", "opensForReadingThroughARoot"} {
		if _, ok := sites["x/x.go:"+through]; ok {
			t.Errorf("the sweep took %s for a writer of the config", through)
		}
	}
	for _, decided := range []string{"savesNamedWithItsLock", "decidesOnWhatItsTurnFound", "savesOnlyWhereItsTurnFoundNone"} {
		for _, g := range got {
			if strings.Contains(g, "x/x.go:"+decided+" ") {
				t.Errorf("the sweep reported %s, which decides on what its turn found: %s", decided, g)
			}
		}
	}
	if site := sites["x/x.go:server.savesThroughTheHolder"]; len(site.kinds) != 1 || site.kinds[0] != writesByHolder {
		t.Errorf("a save through the holder was read as %q, want %q", site.kinds, writesByHolder)
	}
}
