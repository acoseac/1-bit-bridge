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
// call, and, for one that saves itself, to call before its save, to decide
// on what that turn found (turnReadCall), reading the config in the turn
// through nothing else, and to save a value taken from that turn
// (derivationProblem), unless comparedSaves lists the comparison it makes
// instead. The console's writers all go through
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
	"cmd/bridge/library.go:libraryAddCmd":                                    "config.Edit",
	"cmd/bridge/library.go:libraryRemoveCmd":                                 "config.Edit",
	"cmd/bridge/init.go:saveInitConfig":                                      "config.LockFile",
	"cmd/bridge/main.go:writeAutoInitConfig":                                 "config.LockFile",
	"cmd/bridge/output_dac_bind.go:runDACBind":                               "config.LockFile",
	"cmd/bridge/upnp_upstream_admin.go:upnpAdminAdapter.updateCfg":           "cfgHolder.Update",
	"internal/admin/handlers_api.go:Server.applyRootAddition":                "CfgHolder.UpdateChecked",
	"internal/admin/handlers_api.go:Server.applyRootRemoval":                 "CfgHolder.UpdateChecked",
	"internal/admin/handlers_api.go:Server.apiSettingsPatch":                 "CfgHolder.Update",
	"internal/admin/handlers_player_output.go:Server.apiPlayerOutputDAC":     "CfgHolder.UpdateChecked",
	"internal/admin/handlers_player_output.go:Server.apiPlayerOutputEnabled": "CfgHolder.UpdateChecked",
	"internal/admin/handlers_variants_dir.go:Server.apiVariantsDirPatch":     "CfgHolder.Update",
	"internal/backup/backup.go:RestoreWithOptions":                           "config.LockFile",
	"internal/config/lock.go:Edit":                                           "LockFile",
	"internal/config/runtime.go:RuntimeConfig.UpdateChecked":                 "LockFile",
}

// configSavePrimitive is the config's own save, in internal/config: the
// methods that write the file, which the sweep does not count as writers,
// each with the calls the sweep requires of it: the save refuses a turn of
// the lock it does not hold (heldFor), and the one write every save goes
// through compares the file with the one its turn found just before its
// rename (stillAsFound) and resolves the config's name again after that
// comparison (stillNamed), so a writer of the config can neither write
// without a turn, nor write over what another writer saved in it, nor write a
// file its name no longer opens.
var configSavePrimitive = map[string][]string{
	"internal/config/config.go:Config.Save":      {"save"},
	"internal/config/config.go:Config.SaveIf":    {"save"},
	"internal/config/config.go:Config.save":      {"heldFor", "write"},
	"internal/config/lock.go:FileLock.SaveBytes": {"write"},
	"internal/config/lock.go:FileLock.write":     {"heldFor", "stillAsFound", "stillNamed", "RenameWithRetryIf"},
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

// comparedSaves are the named writers that save a config they did not take
// from their own turn's bytes, each with the comparison it makes in its turn
// instead, of the turn's bytes with the read its config came from, on which
// the sweep requires it to refuse its save, asked of the turn or of what it
// found (derivationProblem, refusesInTurn): init saves the config it built
// from the file it read at its start, once configChangedSinceRead has found
// the turn's bytes to be that file's; a restore puts the snapshot's
// bridge.yaml over the file its turn found, which is what a restore is for,
// once configStillAsRead has found that file to be the one its caller read;
// and the live holder's UpdateChecked saves its snapshot, read at boot or
// last written by it, only while bytes.Equal finds Found's bytes to be the
// ones it last read or wrote.
// RuntimeConfig.Update saves through UpdateChecked and is no writer of the
// sweep's itself. Every other named writer that saves itself saves what its
// turn found: Edit, and bind, what LoadFound gave (bind's SaveIf comparison
// is a second guard beside the turn's own, not where its config comes from),
// and serve's seed a default only where its turn found no file (createOnly).
// A writer listed here that has come to save only what its turn found is
// reported, so the list holds only the writers that need it.
var comparedSaves = map[string]string{
	"cmd/bridge/init.go:saveInitConfig":                      "configChangedSinceRead",
	"internal/backup/backup.go:RestoreWithOptions":           "configStillAsRead",
	"internal/config/runtime.go:RuntimeConfig.UpdateChecked": "bytes.Equal",
}

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
	"loadCLIConfig": true, "loadCLIConfigFile": true, "loadServeConfigFile": true, "readConfigAsIs": true,
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
	// assigns are its assignments to a name, in source order; stores what
	// each of its own saves of the config saves, where, and through which
	// turn (storeAt); guards its ifs at the top level of its body whose
	// condition is a name alone and whose body ends in a return; and
	// comparisons its ifs whose body ends in a return, at the top level of
	// its body or directly inside a top-level check of a name against nil,
	// with the calls each refuses on.
	assigns     []assignAt
	stores      []storeAt
	guards      []guardAt
	comparisons []comparisonAt
}

// chainAt is one call a function makes, where, and what it is handed.
type chainAt struct {
	chain string
	pos   token.Pos
	args  []ast.Expr
}

// assignAt is one assignment to name, at pos: rhs is the value assigned, or
// the call whose results are assigned, index naming which of them; obj is the
// variable the parser resolved name to (nil where it resolved none), which
// tells a name shadowed in a block from the one outside it.
type assignAt struct {
	name  string
	obj   *ast.Object
	pos   token.Pos
	rhs   ast.Expr
	index int
}

// storeAt is one save of the config, at pos: value is what it saves (a
// Save's, SaveIf's or save's receiver, a SaveBytes's argument; nil for a
// SaveBytes made through a method value, whose argument the sweep cannot
// see), and turn the turn it saves in (a Save's, SaveIf's or save's first
// argument, a SaveBytes's receiver; nil for any save made through a method
// value, whose call the sweep cannot see).
type storeAt struct {
	pos   token.Pos
	value ast.Expr
	turn  ast.Expr
}

// guardAt is an if at pos, ending at end, whose condition is the name cond
// alone, read at condPos, and whose body ends in a return.
type guardAt struct {
	pos, condPos, end token.Pos
	cond              *ast.Ident
}

// comparisonAt is an if at pos, ending at end, whose body ends in a return,
// with the calls it refuses on (refusalCalls); under is the top-level
// `if name != nil` it sits directly inside, nil for an if at the top level of
// the function's body itself.
type comparisonAt struct {
	pos, end token.Pos
	calls    []chainAt
	under    *nilCheck
}

// nilCheck is a top-level `if name != nil` with no initializer and no else:
// where it is, and the name it checks.
type nilCheck struct {
	pos  token.Pos
	name *ast.Ident
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
	for _, p := range configWriterProblems(sites, configWriters, notConfigWriters, comparedSaves) {
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
					"over what another writer saved in it, or into a file its name no longer opens", key, want)
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

// configWriterProblems compares what the sweep found with the writers named,
// the functions excused and the writers listed as saving what they compared
// (compared), and says what does not agree, in a fixed order.
func configWriterProblems(sites map[string]configWriteSite, named, excused, compared map[string]string) []string {
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
			continue
		}
		if p := derivationProblem(key, site, lock, compared); p != "" {
			problems = append(problems, p)
		}
	}
	for key := range named {
		if _, ok := sites[key]; !ok {
			problems = append(problems, fmt.Sprintf("configWriters names %s, which the sweep no longer finds writing the config: "+
				"remove it, or rename it to the function that writes now", key))
		}
	}
	for key := range compared {
		site, ok := sites[key]
		switch {
		case !ok || len(site.stores) == 0:
			problems = append(problems, fmt.Sprintf("comparedSaves lists %s, which the sweep no longer finds saving the config itself: "+
				"remove it", key))
		case site.savesOnlyFromItsTurn(named[key]):
			problems = append(problems, fmt.Sprintf("comparedSaves lists %s, whose saves are made from what its turn found: "+
				"remove it, so the sweep holds it to that", key))
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
// what its turn found). Where the value it saves comes from is
// derivationProblem's. A writer that saves through Edit or the holder's
// Update is left out: Edit and Update are named writers themselves, and
// decide on their own turn's bytes. Positions are the source's, not the
// control flow's: a read in a loop after the save, or one through a helper
// configReadCalls does not name, is not seen.
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

// derivationProblem says what is wrong with where a named writer's own saves
// of the config come from, and "" when nothing is: each value it saves in its
// turn of lock must be taken from that turn, the config LoadFound gave, the
// bytes Found gave, or a value made from one of them (fromTurn, by the last
// assignment to that variable before it is read); or be saved only where the
// turn found no file (createOnly: a guard that is a statement of the
// function's own body, not of a block or a function literal inside it, so
// that every way to the save passes it); or the writer must be listed in
// compared and refuse its save, in its turn, on the comparison listed for it,
// asked of the turn or of what it found (refusesInTurn: init's, a restore's
// and the holder's, whose config comes from a read they compare with the
// turn's bytes). A comparison refuses when it decides an if whose body
// returns before the save, at the top level of the function's body, or
// directly inside a top-level check of the turn against nil (a restore's: a
// save through a nil turn writes nothing), in one of three forms: the call
// itself, or negated, in a condition of && and || (init's, and the holder's
// `fileKnown && (… || !bytes.Equal(…))`), or an error the if's initializer
// takes from it that the condition finds non-nil (a restore's). A comparison
// inside the nil check refuses only a save made through the very name the
// check tests, which nothing assigns between the check and the save
// (savesThroughUnmoved): a save through another turn name, the one the
// checked name was copied from among them, through that name assigned again
// since the check, or through a method value, goes past a check that let a
// nil name through without its comparison. A comparison whose answer is
// dropped, or that decides an if in a nested block, a function literal, or
// one that does not return, is no refusal.
//
// What it cannot see, each pinned as passing in
// TestTheConfigWriterSweepReportsEveryShape, and nothing else: a variable
// written through a pointer (`p := &cfg; *p = stale`) keeps its last
// assignment as its value; a loop's later pass is read as its first (a save
// before `cfg = stale` in the loop's body); a call handed what the turn
// found is taken as derived from it whatever else it is handed
// (`pick(stale, found)`); which answer of a comparison means the file changed
// (an if that returns when configChangedSinceRead finds no change passes as
// one that returns when it finds one); a conjunct beside the comparison that
// switches its refusal off (`enabled && configChangedSinceRead(…)`, the shape
// of the holder's fileKnown, which lets a holder told nothing save); an
// assignment to the nil-checked name that is not one to the sweep, through a
// pointer (`*p = other`) or in a function literal written before the check
// and called after it; and a helper in another function that writes the path
// a caller hands it (`writeBlob(cfgPath, data)`), which names no config path
// itself while its caller makes no write the sweep knows. Variables are told
// apart by the parser's resolution, so a name shadowed in a block is not the
// one outside it; positions are the source's, never the control flow's.
func derivationProblem(key string, site configWriteSite, lock string, compared map[string]string) string {
	lockAt, turns := site.lockAt(lock), site.turnVars(lock)
	check, listed := compared[key]
	for _, s := range site.stores {
		if s.pos < lockAt || site.fromTurn(s.value, s.pos, turns, 0) || site.createOnly(s.pos, lockAt, turns) {
			continue
		}
		if !listed {
			return fmt.Sprintf("%s: %s saves a config not derived from what its turn of %s found: save what FileLock.LoadFound "+
				"gave, or Found's bytes (SaveBytes), or a value made from them, or list in comparedSaves the comparison of "+
				"the turn's bytes it makes instead", site.pos, key, lock)
		}
		if !site.refusesInTurn(check, lockAt, s, turns) {
			return fmt.Sprintf("%s: %s is listed in comparedSaves, and never refuses its save on %s in its turn of %s: "+
				"an if before the save, at the top of the function or directly inside its check of the turn against nil "+
				"(the save then made through the name the check tests, assigned nothing since the check), whose body "+
				"returns and whose condition asks %s of the turn or of what it found (the call, or its negation, in a "+
				"condition of && and ||, or an error its initializer takes from it that the condition finds non-nil)",
				site.pos, key, check, lock, check)
		}
	}
	return ""
}

// savesOnlyFromItsTurn reports whether every save the site makes in its turn
// of lock is of a value taken from that turn, or one where the turn found no
// file: a writer comparedSaves need not list.
func (s configWriteSite) savesOnlyFromItsTurn(lock string) bool {
	lockAt, turns := s.lockAt(lock), s.turnVars(lock)
	for _, st := range s.stores {
		if st.pos > lockAt && !s.fromTurn(st.value, st.pos, turns, 0) && !s.createOnly(st.pos, lockAt, turns) {
			return false
		}
	}
	return true
}

// lockAt is where the site first calls lock, token.NoPos when it never does.
func (s configWriteSite) lockAt(lock string) token.Pos {
	for _, c := range s.at {
		if isChain(c.chain, lock) {
			return c.pos
		}
	}
	return token.NoPos
}

// turnVars are the names the site holds its turn of lock by: the first
// result of a call of lock, and a name assigned one of those.
func (s configWriteSite) turnVars(lock string) map[string]bool {
	turns := map[string]bool{}
	for _, a := range s.assigns {
		switch rhs := a.rhs.(type) {
		case *ast.CallExpr:
			if a.index == 0 && isChain(callChain(rhs.Fun), lock) {
				turns[a.name] = true
			}
		case *ast.Ident:
			if turns[rhs.Name] {
				turns[a.name] = true
			}
		}
	}
	return turns
}

// lastAssign is the last assignment before pos in the source to the variable
// id names: the one the parser resolved id to, or, where it resolved none, an
// assignment to that name the parser resolved none for either.
func (s configWriteSite) lastAssign(id *ast.Ident, pos token.Pos) (assignAt, bool) {
	var last assignAt
	found := false
	for _, a := range s.assigns {
		if a.name == id.Name && a.obj == id.Obj && a.pos < pos && (!found || a.pos > last.pos) {
			last, found = a, true
		}
	}
	return last, found
}

// fromTurn reports whether e, read at pos, holds a value the function took
// from its turn: what a turn's LoadFound or Found gave (its first result: the
// config, or the bytes, never Found's ok), or a value made from one of them,
// a call handed one or called on one, a field of one, or a name last
// assigned one.
func (s configWriteSite) fromTurn(e ast.Expr, pos token.Pos, turns map[string]bool, depth int) bool {
	if e == nil || depth > 16 {
		return false
	}
	switch x := e.(type) {
	case *ast.ParenExpr:
		return s.fromTurn(x.X, pos, turns, depth+1)
	case *ast.StarExpr:
		return s.fromTurn(x.X, pos, turns, depth+1)
	case *ast.UnaryExpr:
		return s.fromTurn(x.X, pos, turns, depth+1)
	case *ast.SelectorExpr:
		return s.fromTurn(x.X, pos, turns, depth+1)
	case *ast.Ident:
		a, ok := s.lastAssign(x, pos)
		if !ok {
			return false
		}
		if call, isCall := a.rhs.(*ast.CallExpr); isCall && turnRead(call, turns) {
			return a.index == 0
		}
		return s.fromTurn(a.rhs, a.pos, turns, depth+1)
	case *ast.CallExpr:
		if turnRead(x, turns) {
			return true
		}
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && s.fromTurn(sel.X, pos, turns, depth+1) {
			return true
		}
		for _, arg := range x.Args {
			if s.fromTurn(arg, pos, turns, depth+1) {
				return true
			}
		}
	}
	return false
}

// turnRead reports whether call is one of a turn's own reads (turnReadMethods)
// made on a name the function holds its turn by.
func turnRead(call *ast.CallExpr, turns map[string]bool) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && turnReadMethods[sel.Sel.Name] && turns[callChain(sel.X)]
}

// createOnly reports whether the save at pos is made only where its turn
// found no file: after an if in the turn, a statement of the function's own
// body (guards holds no other) ended before the save, that returns when the
// second result of the turn's Found, its condition alone, says a file was
// there. Nothing another writer saved can be written over there, and a file
// that appears meanwhile the save's own comparison refuses.
func (s configWriteSite) createOnly(pos, lockAt token.Pos, turns map[string]bool) bool {
	for _, g := range s.guards {
		if g.pos < lockAt || g.end > pos {
			continue
		}
		a, ok := s.lastAssign(g.cond, g.condPos)
		if !ok || a.index != 1 {
			continue
		}
		if call, isCall := a.rhs.(*ast.CallExpr); isCall && turnRead(call, turns) {
			if sel := call.Fun.(*ast.SelectorExpr); sel.Sel.Name == "Found" {
				return true
			}
		}
	}
	return false
}

// refusesInTurn reports whether the site refuses the save st on check, in its
// turn: one of its comparisons (an if whose body returns, at the top level of
// its body, or directly inside a top-level check against nil of a name it
// holds its turn by, st then saving through that name, unmoved since the
// check: savesThroughUnmoved) begins after it takes its lock and ends before
// the save, and refuses on a call of check handed the turn or something it
// found.
func (s configWriteSite) refusesInTurn(check string, lockAt token.Pos, st storeAt, turns map[string]bool) bool {
	for _, c := range s.comparisons {
		if c.pos <= lockAt || c.end > st.pos || (c.under != nil && !s.savesThroughUnmoved(*c.under, st, turns)) {
			continue
		}
		for _, call := range c.calls {
			if !isChain(call.chain, check) {
				continue
			}
			for _, arg := range call.args {
				if id, ok := arg.(*ast.Ident); ok && turns[id.Name] {
					return true
				}
				if s.fromTurn(arg, call.pos, turns, 0) {
					return true
				}
			}
		}
	}
	return false
}

// savesThroughUnmoved reports whether st, a save after the nil check n, is
// made through the very name n checks, one the site holds its turn by, and
// that name is assigned nothing between the check and the save: a save the
// check let past the comparison inside it is then one through the turn the
// comparison asked of, or through a nil turn, which writes nothing. A save
// through another name (the turn the checked name was copied from, while the
// checked name was nil), through that name assigned again since the check,
// or through a method value, whose call the sweep cannot see, is not.
func (s configWriteSite) savesThroughUnmoved(n nilCheck, st storeAt, turns map[string]bool) bool {
	id, ok := st.turn.(*ast.Ident)
	switch {
	case !ok || !turns[n.name.Name]:
		return false
	case id.Name != n.name.Name || id.Obj != n.name.Obj:
		return false
	}
	for _, a := range s.assigns {
		if a.name == n.name.Name && a.obj == n.name.Obj && a.pos > n.pos && a.pos < st.pos {
			return false
		}
	}
	return true
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
// calls it makes and where, where it reads the config and where it saves it,
// and what each of its saves saves (its assignments, its saves' values, its
// ifs at the top level of its body that return on a name, and those that
// return on a comparison). A save, an Edit or the holder's Update counts
// whether it is called or taken as a value (a method value is a save made
// later, through it).
func inspectConfigWriter(body ast.Node, inConfig bool) (configWriteSite, map[string]bool) {
	site := configWriteSite{calls: map[string]bool{}}
	kinds := map[string]bool{}
	namesPath := false
	var fileWrites []token.Pos
	// calledBytes are the SaveBytes selectors a call is made through, whose
	// saved value is the call's argument; calledSaves the Save, SaveIf and
	// save selectors a call is made through, with the call, whose first
	// argument is the turn the save is made in.
	calledBytes := map[*ast.SelectorExpr]bool{}
	calledSaves := map[*ast.SelectorExpr]*ast.CallExpr{}
	top, underNil := bodyStatements(body)
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			site.assigns = append(site.assigns, assignmentsOf(x.Lhs, x.Rhs, x.Pos())...)
		case *ast.RangeStmt:
			// A range assigns its key and value on every pass, as an
			// assignment does, so a range cannot move a nil-checked turn
			// unseen (round 6's review).
			var lhs []ast.Expr
			for _, e := range []ast.Expr{x.Key, x.Value} {
				if e != nil {
					lhs = append(lhs, e)
				}
			}
			site.assigns = append(site.assigns, assignmentsOf(lhs, []ast.Expr{x.X}, x.Pos())...)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(x.Names))
			for i, id := range x.Names {
				lhs[i] = id
			}
			site.assigns = append(site.assigns, assignmentsOf(lhs, x.Values, x.Pos())...)
		case *ast.IfStmt:
			if g, ok := guardOf(x); ok && top[x] {
				site.guards = append(site.guards, g)
			}
			if under, inside := underNil[x]; top[x] || inside {
				if c, ok := comparisonOf(x); ok {
					c.under = under
					site.comparisons = append(site.comparisons, c)
				}
			}
		case *ast.CallExpr:
			chain := callChain(x.Fun)
			site.calls[chain] = true
			site.at = append(site.at, chainAt{chain, x.Pos(), x.Args})
			if inConfig && chain == "Edit" {
				kinds[writesByEdit] = true
			}
			if fileWriteCall(x, chain) {
				fileWrites = append(fileWrites, x.Pos())
			}
			if configReadCall(x, chain, inConfig) {
				site.reads = append(site.reads, x.Pos())
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				switch {
				case sel.Sel.Name == "SaveBytes":
					calledBytes[sel] = true
					var value ast.Expr
					if len(x.Args) > 0 {
						value = x.Args[0]
					}
					site.stores = append(site.stores, storeAt{pos: sel.Pos(), value: value, turn: sel.X})
				case saveMethods[sel.Sel.Name] || (inConfig && sel.Sel.Name == "save"):
					calledSaves[sel] = x
				}
			}
		case *ast.SelectorExpr:
			switch {
			case callChain(x) == "config.Edit":
				kinds[writesByEdit] = true
			case saveMethods[x.Sel.Name] || (inConfig && x.Sel.Name == "save"):
				kinds[writesBySave] = true
				site.saves = append(site.saves, x.Pos())
				switch {
				case x.Sel.Name != "SaveBytes":
					var turn ast.Expr
					if call := calledSaves[x]; call != nil && len(call.Args) > 0 {
						turn = call.Args[0]
					}
					site.stores = append(site.stores, storeAt{pos: x.Pos(), value: x.X, turn: turn})
				case !calledBytes[x]:
					site.stores = append(site.stores, storeAt{pos: x.Pos()})
				}
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

// assignmentsOf are the assignments to a name that lhs and rhs, an
// assignment's or a var declaration's two sides, make at pos: one value per
// name, or one call's results, the i-th to the i-th name, each with the
// variable the parser resolved its name to. A blank name, and one assigned
// through a field, an index or a pointer, is no assignment to a name.
func assignmentsOf(lhs, rhs []ast.Expr, pos token.Pos) []assignAt {
	var out []assignAt
	for i, l := range lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		switch {
		case len(rhs) == len(lhs):
			out = append(out, assignAt{name: id.Name, obj: id.Obj, pos: pos, rhs: rhs[i]})
		case len(rhs) == 1:
			out = append(out, assignAt{name: id.Name, obj: id.Obj, pos: pos, rhs: rhs[0], index: i})
		}
	}
	return out
}

// bodyStatements are the statements of body, a function's or a function
// literal's, itself (top), and those directly inside each of them that is an
// if with no initializer and no else whose condition checks a name against
// nil (`if turn != nil`), each with that check (underNil): the ifs a guard
// and a comparison may be, the ifs that every way to a save after them
// passes, and for one inside a nil check, every way with that name not nil.
func bodyStatements(body ast.Node) (top map[ast.Stmt]bool, underNil map[ast.Stmt]*nilCheck) {
	top, underNil = map[ast.Stmt]bool{}, map[ast.Stmt]*nilCheck{}
	var list []ast.Stmt
	switch b := body.(type) {
	case *ast.BlockStmt:
		list = b.List
	case *ast.FuncLit:
		list = b.Body.List
	}
	for _, st := range list {
		top[st] = true
		x, ok := st.(*ast.IfStmt)
		if !ok || x.Init != nil || x.Else != nil {
			continue
		}
		if name, ok := nilCheckOf(x.Cond); ok {
			check := &nilCheck{pos: x.Pos(), name: name}
			for _, inner := range x.Body.List {
				underNil[inner] = check
			}
		}
	}
	return top, underNil
}

// nilCheckOf is the name cond finds non-nil, `name != nil`, and whether cond
// is that.
func nilCheckOf(cond ast.Expr) (*ast.Ident, bool) {
	b, ok := cond.(*ast.BinaryExpr)
	if !ok || b.Op != token.NEQ {
		return nil, false
	}
	x, y := b.X, b.Y
	if isNilIdent(x) {
		x, y = y, x
	}
	id, ok := x.(*ast.Ident)
	if !ok || !isNilIdent(y) {
		return nil, false
	}
	return id, true
}

// isNilIdent reports whether e is the name nil.
func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// guardOf is x as a guard (guardAt): an if with no else whose condition is a
// name alone and whose body ends in a return.
func guardOf(x *ast.IfStmt) (guardAt, bool) {
	cond, ok := x.Cond.(*ast.Ident)
	if !ok || x.Else != nil || !endsInReturn(x.Body) {
		return guardAt{}, false
	}
	return guardAt{pos: x.Pos(), condPos: x.Cond.Pos(), end: x.End(), cond: cond}, true
}

// comparisonOf is x as a comparison (comparisonAt): an if with no else whose
// body ends in a return, and the calls it refuses on (refusalCalls), when it
// refuses on any.
func comparisonOf(x *ast.IfStmt) (comparisonAt, bool) {
	if x.Else != nil || !endsInReturn(x.Body) {
		return comparisonAt{}, false
	}
	calls := refusalCalls(x)
	if len(calls) == 0 {
		return comparisonAt{}, false
	}
	return comparisonAt{pos: x.Pos(), end: x.End(), calls: calls}, true
}

// refusalCalls are the calls an if refuses on: each call its condition is
// made of through &&, || and ! (`if changed(…)`, `if known && !bytes.Equal(…)`),
// and the call its initializer assigns its one name from when the condition
// finds that name non-nil (`if err := check(…); err != nil`).
func refusalCalls(x *ast.IfStmt) []chainAt {
	var out []chainAt
	var walk func(e ast.Expr)
	walk = func(e ast.Expr) {
		switch c := e.(type) {
		case *ast.ParenExpr:
			walk(c.X)
		case *ast.UnaryExpr:
			if c.Op == token.NOT {
				walk(c.X)
			}
		case *ast.BinaryExpr:
			if c.Op == token.LAND || c.Op == token.LOR {
				walk(c.X)
				walk(c.Y)
			}
		case *ast.CallExpr:
			out = append(out, chainAt{callChain(c.Fun), c.Pos(), c.Args})
		}
	}
	walk(x.Cond)
	if init, ok := x.Init.(*ast.AssignStmt); ok && len(init.Lhs) == 1 && len(init.Rhs) == 1 {
		id, isName := init.Lhs[0].(*ast.Ident)
		call, isCall := init.Rhs[0].(*ast.CallExpr)
		if name, checks := nilCheckOf(x.Cond); isName && isCall && checks && name.Name == id.Name {
			out = append(out, chainAt{callChain(call.Fun), call.Pos(), call.Args})
		}
	}
	return out
}

// endsInReturn reports whether block's last statement is a return.
func endsInReturn(block *ast.BlockStmt) bool {
	if len(block.List) == 0 {
		return false
	}
	_, returns := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return returns
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
	"bytes"
	"context"
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

func discardsWhatItsTurnFound(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	l.Found()
	_ = cfg.Save(l)
}

func keepsWhatItsTurnFoundAside(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	found, _ := l.Found()
	_ = found
	_ = cfg.Save(l)
}

func checksWhatItsTurnFoundAndSavesItsOwn(cfgPath string) {
	cfg, _ := config.Load(cfgPath)
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if found, ok := l.Found(); !ok || len(found) == 0 {
		return
	}
	_ = cfg.Save(l)
}

func overwritesWhatItsTurnFound(cfgPath string, stale *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ := l.LoadFound()
	cfg = stale
	_ = cfg.Save(l)
}

func savesBytesItsTurnDidNotFind(cfgPath string, stale []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	found, _ := l.Found()
	_ = found
	_ = l.SaveBytes(stale)
}

func savesBytesMadeFromWhatItsTurnFound(cfgPath string) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	found, _ := l.Found()
	_ = l.SaveBytes(bytes.TrimSpace(found))
}

func decidesAsBindDoes(cfgPath string) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ := l.LoadFound()
	read, _ := l.Found()
	cfg.LibraryName = "Bound"
	_ = cfg.SaveIf(l, func(ctx context.Context) error {
		now, _ := config.ReadFile(ctx, cfgPath)
		if !bytes.Equal(now, read) {
			return context.Canceled
		}
		return nil
	})
}

func comparesWithItsOwnRead(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if configChangedSinceRead(asRead, l) {
		return
	}
	_ = cfg.Save(l)
}

func listedWithoutItsComparison(cfgPath string, cfg *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if _, ok := l.Found(); !ok {
		return
	}
	_ = cfg.Save(l)
}

func listedButDecidesOnItsTurn(cfgPath string) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ := l.LoadFound()
	_ = cfg.Save(l)
}

func guardsOnlyWhenVerbose(cfgPath string, stale *config.Config, verbose bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if verbose {
		if _, ok := l.Found(); ok {
			return
		}
	}
	_ = stale.Save(l)
}

func guardsInAClosureItNeverCalls(cfgPath string, stale *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	_ = func() {
		if _, ok := l.Found(); ok {
			return
		}
	}
	_ = stale.Save(l)
}

func shadowsWhatItsTurnFound(cfgPath string, stale *config.Config) {
	cfg := stale
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	{
		cfg, _ := l.LoadFound()
		_ = cfg
	}
	_ = cfg.Save(l)
}

func discardsItsComparison(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	_ = configChangedSinceRead(asRead, l)
	_ = cfg.Save(l)
}

func comparesOnlyWhenVerbose(cfgPath string, cfg *config.Config, asRead []byte, verbose bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if verbose {
		if configChangedSinceRead(asRead, l) {
			return
		}
	}
	_ = cfg.Save(l)
}

func comparesInAClosureItNeverCalls(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	_ = func() {
		if configChangedSinceRead(asRead, l) {
			return
		}
	}
	_ = cfg.Save(l)
}

func comparesAndSavesAnyway(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if configChangedSinceRead(asRead, l) {
		cfg.LibraryName = "Changed"
	}
	_ = cfg.Save(l)
}

func refusesOnAnErrorItsComparisonReturns(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if err := configStillAsRead(l, asRead); err != nil {
		return
	}
	_ = cfg.Save(l)
}

func refusesOnACompoundCondition(cfgPath string, cfg *config.Config, old []byte, known bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	found, ok := l.Found()
	if known && (!ok || !bytes.Equal(found, old)) {
		return
	}
	_ = cfg.Save(l)
}

func comparesInsideItsTurnsNilCheck(cfgPath string, asRead []byte) {
	var turn *config.FileLock
	if cfgPath != "" {
		l, _ := config.LockFile(cfgPath)
		defer l.Unlock()
		turn = l
	}
	if turn != nil {
		if err := configStillAsRead(turn, asRead); err != nil {
			return
		}
	}
	_ = turn.SaveBytes([]byte("libraryName: Snapshot\n"))
}

func comparesInsideAnotherNilCheck(cfgPath string, cfg, other *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if other != nil {
		if err := configStillAsRead(l, asRead); err != nil {
			return
		}
	}
	_ = cfg.Save(l)
}

func overwritesWhatItsTurnFoundThroughAPointer(cfgPath string, stale *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ := l.LoadFound()
	p := &cfg
	*p = stale
	_ = cfg.Save(l)
}

func savesAStaleValueOnALoopsSecondPass(cfgPath string, stale *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	cfg, _ := l.LoadFound()
	for range 2 {
		_ = cfg.Save(l)
		cfg = stale
	}
}

func picksAStaleValueBesideWhatItsTurnFound(cfgPath string, stale *config.Config) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	found, _ := l.LoadFound()
	cfg := pick(stale, found)
	_ = cfg.Save(l)
}

func comparesUnderAnOptionalAliasAndSavesThroughItsTurn(cfgPath string, stale *config.Config, asRead []byte, compareEnabled bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	var optionalTurn *config.FileLock
	if compareEnabled {
		optionalTurn = l
	}
	if optionalTurn != nil {
		if err := configStillAsRead(optionalTurn, asRead); err != nil {
			return
		}
	}
	_ = stale.Save(l)
}

func comparesUnderANilableAliasAndSavesThroughItsTurn(cfgPath string, cfg *config.Config, asRead []byte, disabled bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if disabled {
		t = nil
	}
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	_ = cfg.Save(l)
}

func reassignsItsNilCheckedTurnBeforeItSaves(cfgPath string, cfg *config.Config, asRead []byte, disabled bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if disabled {
		t = nil
	}
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	t = l
	_ = cfg.Save(t)
}

func reassignsItsNilCheckedTurnInARange(cfgPath string, cfg *config.Config, asRead []byte, disabled bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if disabled {
		t = nil
	}
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	for _, t = range []*config.FileLock{l} {
	}
	_ = cfg.Save(t)
}

func savesThroughAnotherNameAfterItsNilCheck(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	_ = cfg.Save(l)
}

func savesIfThroughAnotherNameAfterItsNilCheck(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	_ = cfg.SaveIf(l, nil)
}

func savesBytesThroughAnotherNameAfterItsNilCheck(cfgPath string, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	_ = l.SaveBytes([]byte("libraryName: Snapshot\n"))
}

func savesThroughAMethodValueAfterItsNilCheck(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	save := cfg.Save
	_ = save(t)
}

func savesThroughItsNilCheckedTurn(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	_ = cfg.Save(t)
}

func savesIfThroughItsNilCheckedTurn(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	_ = cfg.SaveIf(t, nil)
}

func refusesOnlyWhenItsComparisonFindsNoChange(cfgPath string, cfg *config.Config, asRead []byte) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if !configChangedSinceRead(asRead, l) {
		return
	}
	_ = cfg.Save(l)
}

func switchesItsRefusalOffWithAConjunct(cfgPath string, cfg *config.Config, asRead []byte, enabled bool) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	if enabled && configChangedSinceRead(asRead, l) {
		return
	}
	_ = cfg.Save(l)
}

func writesThroughAHelper(cfgPath string, data []byte) { _ = writeBlob(cfgPath, data) }

func writeBlob(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

func reassignsItsNilCheckedTurnThroughAPointer(cfgPath string, cfg *config.Config, asRead []byte, other *config.FileLock) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	p := &t
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	*p = other
	_ = cfg.Save(t)
}

func reassignsItsNilCheckedTurnInAClosure(cfgPath string, cfg *config.Config, asRead []byte, other *config.FileLock) {
	l, _ := config.LockFile(cfgPath)
	defer l.Unlock()
	t := l
	swap := func() { t = other }
	if t != nil {
		if err := configStillAsRead(t, asRead); err != nil {
			return
		}
	}
	swap()
	_ = cfg.Save(t)
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

		"x/x.go:discardsWhatItsTurnFound":             "config.LockFile",
		"x/x.go:keepsWhatItsTurnFoundAside":           "config.LockFile",
		"x/x.go:checksWhatItsTurnFoundAndSavesItsOwn": "config.LockFile",
		"x/x.go:overwritesWhatItsTurnFound":           "config.LockFile",
		"x/x.go:savesBytesItsTurnDidNotFind":          "config.LockFile",
		"x/x.go:savesBytesMadeFromWhatItsTurnFound":   "config.LockFile",
		"x/x.go:decidesAsBindDoes":                    "config.LockFile",
		"x/x.go:comparesWithItsOwnRead":               "config.LockFile",
		"x/x.go:listedWithoutItsComparison":           "config.LockFile",
		"x/x.go:listedButDecidesOnItsTurn":            "config.LockFile",

		"x/x.go:guardsOnlyWhenVerbose":                     "config.LockFile",
		"x/x.go:guardsInAClosureItNeverCalls":              "config.LockFile",
		"x/x.go:shadowsWhatItsTurnFound":                   "config.LockFile",
		"x/x.go:discardsItsComparison":                     "config.LockFile",
		"x/x.go:comparesOnlyWhenVerbose":                   "config.LockFile",
		"x/x.go:comparesInAClosureItNeverCalls":            "config.LockFile",
		"x/x.go:comparesAndSavesAnyway":                    "config.LockFile",
		"x/x.go:refusesOnAnErrorItsComparisonReturns":      "config.LockFile",
		"x/x.go:refusesOnACompoundCondition":               "config.LockFile",
		"x/x.go:comparesInsideItsTurnsNilCheck":            "config.LockFile",
		"x/x.go:comparesInsideAnotherNilCheck":             "config.LockFile",
		"x/x.go:overwritesWhatItsTurnFoundThroughAPointer": "config.LockFile",
		"x/x.go:savesAStaleValueOnALoopsSecondPass":        "config.LockFile",
		"x/x.go:picksAStaleValueBesideWhatItsTurnFound":    "config.LockFile",

		"x/x.go:comparesUnderAnOptionalAliasAndSavesThroughItsTurn": "config.LockFile",
		"x/x.go:comparesUnderANilableAliasAndSavesThroughItsTurn":   "config.LockFile",
		"x/x.go:reassignsItsNilCheckedTurnBeforeItSaves":            "config.LockFile",
		"x/x.go:reassignsItsNilCheckedTurnInARange":                 "config.LockFile",
		"x/x.go:savesThroughAnotherNameAfterItsNilCheck":            "config.LockFile",
		"x/x.go:savesIfThroughAnotherNameAfterItsNilCheck":          "config.LockFile",
		"x/x.go:savesBytesThroughAnotherNameAfterItsNilCheck":       "config.LockFile",
		"x/x.go:savesThroughAMethodValueAfterItsNilCheck":           "config.LockFile",
		"x/x.go:savesThroughItsNilCheckedTurn":                      "config.LockFile",
		"x/x.go:savesIfThroughItsNilCheckedTurn":                    "config.LockFile",
		"x/x.go:refusesOnlyWhenItsComparisonFindsNoChange":          "config.LockFile",
		"x/x.go:switchesItsRefusalOffWithAConjunct":                 "config.LockFile",
		"x/x.go:reassignsItsNilCheckedTurnThroughAPointer":          "config.LockFile",
		"x/x.go:reassignsItsNilCheckedTurnInAClosure":               "config.LockFile",
	}
	excused := map[string]string{
		"x/x.go:copiesThePathElsewhere": "writes the bundle",
		"x/x.go:writesAnotherFile":      "writes no config file, and names no config path either",
	}
	compared := map[string]string{
		"x/x.go:comparesWithItsOwnRead":     "configChangedSinceRead",
		"x/x.go:listedWithoutItsComparison": "configChangedSinceRead",
		"x/x.go:listedButDecidesOnItsTurn":  "configChangedSinceRead",
		"x/x.go:goneToo":                    "configChangedSinceRead",

		"x/x.go:discardsItsComparison":                "configChangedSinceRead",
		"x/x.go:comparesOnlyWhenVerbose":              "configChangedSinceRead",
		"x/x.go:comparesInAClosureItNeverCalls":       "configChangedSinceRead",
		"x/x.go:comparesAndSavesAnyway":               "configChangedSinceRead",
		"x/x.go:refusesOnAnErrorItsComparisonReturns": "configStillAsRead",
		"x/x.go:refusesOnACompoundCondition":          "bytes.Equal",
		"x/x.go:comparesInsideItsTurnsNilCheck":       "configStillAsRead",
		"x/x.go:comparesInsideAnotherNilCheck":        "configStillAsRead",

		"x/x.go:comparesUnderAnOptionalAliasAndSavesThroughItsTurn": "configStillAsRead",
		"x/x.go:comparesUnderANilableAliasAndSavesThroughItsTurn":   "configStillAsRead",
		"x/x.go:reassignsItsNilCheckedTurnBeforeItSaves":            "configStillAsRead",
		"x/x.go:reassignsItsNilCheckedTurnInARange":                 "configStillAsRead",
		"x/x.go:savesThroughAnotherNameAfterItsNilCheck":            "configStillAsRead",
		"x/x.go:savesIfThroughAnotherNameAfterItsNilCheck":          "configStillAsRead",
		"x/x.go:savesBytesThroughAnotherNameAfterItsNilCheck":       "configStillAsRead",
		"x/x.go:savesThroughAMethodValueAfterItsNilCheck":           "configStillAsRead",
		"x/x.go:savesThroughItsNilCheckedTurn":                      "configStillAsRead",
		"x/x.go:savesIfThroughItsNilCheckedTurn":                    "configStillAsRead",
		"x/x.go:refusesOnlyWhenItsComparisonFindsNoChange":          "configChangedSinceRead",
		"x/x.go:switchesItsRefusalOffWithAConjunct":                 "configChangedSinceRead",
		"x/x.go:reassignsItsNilCheckedTurnThroughAPointer":          "configStillAsRead",
		"x/x.go:reassignsItsNilCheckedTurnInAClosure":               "configStillAsRead",
	}
	got := configWriterProblems(sites, named, excused, compared)
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
		"x/x.go:discardsWhatItsTurnFound saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:keepsWhatItsTurnFoundAside saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:checksWhatItsTurnFoundAndSavesItsOwn saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:overwritesWhatItsTurnFound saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:savesBytesItsTurnDidNotFind saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:listedWithoutItsComparison is listed in comparedSaves, and never refuses its save on configChangedSinceRead",
		"comparedSaves lists x/x.go:listedButDecidesOnItsTurn, whose saves are made from what its turn found",
		"comparedSaves lists x/x.go:goneToo, which the sweep no longer finds saving the config itself",
		"x/x.go:guardsOnlyWhenVerbose saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:guardsInAClosureItNeverCalls saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:shadowsWhatItsTurnFound saves a config not derived from what its turn of config.LockFile found",
		"x/x.go:discardsItsComparison is listed in comparedSaves, and never refuses its save on configChangedSinceRead",
		"x/x.go:comparesOnlyWhenVerbose is listed in comparedSaves, and never refuses its save on configChangedSinceRead",
		"x/x.go:comparesInAClosureItNeverCalls is listed in comparedSaves, and never refuses its save on configChangedSinceRead",
		"x/x.go:comparesAndSavesAnyway is listed in comparedSaves, and never refuses its save on configChangedSinceRead",
		"x/x.go:comparesInsideAnotherNilCheck is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:comparesUnderAnOptionalAliasAndSavesThroughItsTurn is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:comparesUnderANilableAliasAndSavesThroughItsTurn is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:reassignsItsNilCheckedTurnBeforeItSaves is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:reassignsItsNilCheckedTurnInARange is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:savesThroughAnotherNameAfterItsNilCheck is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:savesIfThroughAnotherNameAfterItsNilCheck is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:savesBytesThroughAnotherNameAfterItsNilCheck is listed in comparedSaves, and never refuses its save on configStillAsRead",
		"x/x.go:savesThroughAMethodValueAfterItsNilCheck is listed in comparedSaves, and never refuses its save on configStillAsRead",
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
	for _, decided := range []string{"savesNamedWithItsLock", "decidesOnWhatItsTurnFound", "savesOnlyWhereItsTurnFoundNone",
		"savesBytesMadeFromWhatItsTurnFound", "decidesAsBindDoes", "comparesWithItsOwnRead",
		"refusesOnAnErrorItsComparisonReturns", "refusesOnACompoundCondition", "comparesInsideItsTurnsNilCheck",
		"savesThroughItsNilCheckedTurn", "savesIfThroughItsNilCheckedTurn"} {
		for _, g := range got {
			if strings.Contains(g, "x/x.go:"+decided+" ") {
				t.Errorf("the sweep reported %s, which decides on what its turn found: %s", decided, g)
			}
		}
	}
	// What the sweep cannot see (derivationProblem's list, shape for shape):
	// a value written through a pointer, a loop's later pass, a call handed a
	// stale value beside what the turn found, a comparison that refuses on the
	// wrong answer, a conjunct that switches the refusal off, and an
	// assignment to the nil-checked turn through a pointer or in a function
	// literal written before the check, each save a stale config and pass.
	// Pinned here as passing, so that a sweep that comes to see one changes
	// this list and derivationProblem's account of what it cannot see together.
	for _, unseen := range []string{"overwritesWhatItsTurnFoundThroughAPointer", "savesAStaleValueOnALoopsSecondPass",
		"picksAStaleValueBesideWhatItsTurnFound", "refusesOnlyWhenItsComparisonFindsNoChange",
		"switchesItsRefusalOffWithAConjunct", "reassignsItsNilCheckedTurnThroughAPointer",
		"reassignsItsNilCheckedTurnInAClosure"} {
		if _, ok := sites["x/x.go:"+unseen]; !ok {
			t.Errorf("the sweep found no %s, a writer of the shape derivationProblem says it cannot see", unseen)
		}
		for _, g := range got {
			if strings.Contains(g, "x/x.go:"+unseen+" ") {
				t.Errorf("the sweep reported %s, a shape derivationProblem says it cannot see: update that account and this list: %s", unseen, g)
			}
		}
	}
	// And the last entry of that list: a helper that writes the path a caller
	// hands it, while neither function names the path and writes in one: the
	// sweep, reading one function at a time, finds no writer in either.
	for _, unseen := range []string{"writesThroughAHelper", "writeBlob"} {
		if _, ok := sites["x/x.go:"+unseen]; ok {
			t.Errorf("the sweep took %s for a writer of the config, a helper in another function derivationProblem says it cannot see: "+
				"update that account and this list", unseen)
		}
	}
	if site := sites["x/x.go:server.savesThroughTheHolder"]; len(site.kinds) != 1 || site.kinds[0] != writesByHolder {
		t.Errorf("a save through the holder was read as %q, want %q", site.kinds, writesByHolder)
	}
}
