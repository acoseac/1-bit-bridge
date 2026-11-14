package sweeptest

import (
	"fmt"
	"go/ast"
	"go/doc/comment"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEveryWalkFromTheRootIsListed reads every Go file of this module, test
// files and the programs under scripts/ and .github/ included, finds each
// function that walks a directory tree, and requires each to be one of two
// kinds. A walk that can start at the checkout's root is one MakeTestTmpDir's
// doc lists, with the tests that make it from the root and the rule by which
// it skips make test's TMPDIR, held in its walk's branch for a directory; any
// other walk is one walksElsewhere names, with what it walks instead. A walk
// of neither kind fails, and so does an item of either that names no walk, a
// listed test that does not reach its walk, a rule its walk's branch for a
// directory does not reach, and a walk a test file holds at package level,
// which the sweeps took as their seam until they were handed their walk
// (the B567 follow-up).
//
// B740's record said every Go-file sweep from the root skipped the TMPDIR,
// and B567 found three that read it: nothing listed the walks that start at
// the root, so the claim rested on one reading of the tree by hand. This is
// that list, kept whole by the sweep. A walk it cannot see is one that runs
// a command (go list ./...) or keeps a queue of the directories it has yet
// to list: walks shows what it finds.
func TestEveryWalkFromTheRootIsListed(t *testing.T) {
	root := moduleRoot(t)
	mod, err := readModule(filepath.WalkDir, root)
	if err != nil {
		t.Fatalf("reading the Go files under %s: %v", root, err)
	}
	// A sweep that stops seeing the tree finds no walk, and fails on every
	// item of both lists as naming none; these say why first. The module
	// held 882 non-test and 2,046 test files when they were set.
	if mod.files < 500 || mod.tests < 1000 {
		t.Fatalf("read %d non-test and %d test Go files under %s, want >=500 and >=1000: the sweep is not seeing the tree",
			mod.files, mod.tests, root)
	}
	doc, err := inventoryDoc("sweeptest.go")
	if err != nil {
		t.Fatal(err)
	}
	listed, problems := rootWalkInventory(doc)
	if len(listed) < 10 {
		t.Fatalf("MakeTestTmpDir's doc lists %d walks from the root, want >=10: the list is not being read", len(listed))
	}
	for _, p := range append(problems, checkWalks(mod, listed, walksElsewhere)...) {
		t.Error(p)
	}
}

// walksElsewhere names every walk of the module that never starts at the
// checkout's root, a function's or one a file holds at package level, with
// what it walks instead: the bridge's own code walks what it is configured
// with; a fixture's walk, a tree its test made; and a few walks start below
// the root, where make test's TMPDIR is not, or outside the checkout.
var walksElsewhere = map[string]string{
	// The bridge's code, which fsutil's
	// TestEveryWalkOfALibraryRootStartsFromWalkableRoot classifies by
	// whether a walk starts from WalkableRoot, and this package's stand-in.
	"cmd/bridge/artwork.go artworkCacheHasOrphans":         "the artwork cache, which artworkWalkRoot resolves",
	"cmd/bridge/artwork.go runArtworkGC":                   "the artwork cache, which artworkWalkRoot resolves",
	"cmd/bridge/artwork.go sweepArtworkCache":              "the artwork cache, which artworkWalkRoot resolves",
	"internal/api/upscale.go (*Server).upscaleRequest":     "a folder of a library root, from WalkableRoot's answer",
	"internal/doctor/cgroup.go findCgroups":                "the cgroup2 mount",
	"internal/doctor/inotify_linux.go countDirs":           "a library root, from WalkableRoot's answer",
	"internal/fsutil/walkroot.go WalkConfigured":           "the path WalkableRoot returned",
	"internal/fsutil/walkroot.go walkResolvedChain":        "the directory a separator path's link chain names",
	"internal/fsutil/walkroot.go (package level)":          "walkResolvedDirectory, nil in production, which fsutil's tests set so a resolved chain's walk lists no volume root",
	"internal/integrity/inventory.go TakeSidecarInventory": "the variants directory, which resolveSidecarRoot resolves",
	"internal/manifest/scanner.go (*Scanner).ScanSubtree":  "a folder of a library root, from WalkableRoot's answer",
	"internal/manifest/scanner.go (*Scanner).walkRoot":     "a library root, from WalkableRoot's answer",
	"internal/manifest/watcher.go walkWatchTree":           "a watched library root, from WalkableRoot's answer",
	"internal/sweeptest/sweeptest.go (*Removal).WalkDir":   "whatever tree a sweep's test walks with it, as the sweep's walk: the sweep's own item says where that starts",
	"internal/trash/restore.go (*Manager).Sweep":           "one stamp directory of a library root's trash",
	"internal/trash/restore.go (*Manager).pruneEmptyStamp": "one stamp directory of a library root's trash",
	"internal/trash/trash.go (*Manager).List":              "one stamp directory of a library root's trash",

	// Fixtures: each walks a tree its own test made below t.TempDir, or
	// copied or wrote there.
	"cmd/bridge/artwork_cache_walks_test.go cacheFilesUnder":                                                         "a test's artwork cache",
	"cmd/bridge/data_dir_away_commands_test.go requireNothingMadeThere":                                              "a test's mount point for a data dir's volume",
	"cmd/bridge/doctor_variants_index_test.go seedVariantCatalog":                                                    "a test's variants directory",
	"cmd/bridge/gc_empty_catalog_test.go regularFilesUnderDir":                                                       "a test's variants directory",
	"cmd/bridge/gc_partial_walk_test.go regularFilesUnder":                                                           "a test's variants directory",
	"cmd/bridge/gc_walk_names_test.go seedVariantCopies":                                                             "a test's variants directory",
	"cmd/bridge/output_test.go TestOutputDACRawDumpReadsBackAsTheSameReading":                                        "a raw dump laid out below t.TempDir",
	"cmd/bridge/root_owner_jobs_test.go chownTree":                                                                   "a test's library and data trees",
	"cmd/bridge/root_owner_jobs_test.go snapshotTrees":                                                               "a test's library and data trees",
	"cmd/bridge/root_owner_test.go TestCLIRunAsRootKeepsTheInstallOwner":                                             "a test's config directory",
	"cmd/bridge/serve_data_dir_identity_test.go treeOf":                                                              "a test's data dir or its volume's mount point",
	"cmd/bridge/serve_data_dir_prepare_test.go copyTree":                                                             "a test's data dir",
	"cmd/bridge/serve_data_dir_refusal_test.go TestTheRefusalsNoteMakesNoDirectory":                                  "a test's mount point for a data dir's volume",
	"cmd/bridge/serve_variants_away_pool_unix_test.go TestServesOwnPoolWritesNothingIntoAVariantsVolumeThatIsAway":   "a test's mount point for a variants volume",
	"internal/analyze/repair_r2_test.go regularFilesUnder":                                                           "a test's analysis output directory",
	"internal/analyze/repair_test.go entriesUnder":                                                                   "a test's analysis output directory",
	"internal/atomicwrite/medium_linux_test.go inOverlayNamespace":                                                   "a test's overlay layers",
	"internal/atomicwrite/replace_test.go TestReplaceDurable_RefusesBeforeItStages":                                  "a test's root, before and after a refused replace",
	"internal/backup/player_seal_heal_test.go copyTree":                                                              "a test's data dir or snapshot",
	"internal/fsutil/resolve_windows_test.go TestAVolumeGUIDPathIsWalkable":                                          "a test's directory through its volume GUID path",
	"internal/fsutil/walkroot_test.go TestAChainThatResolvesToTheVolumeRootIsWalkedThere":                            "a test's link chain",
	"internal/fsutil/walkroot_test.go TestWalkConfiguredKeepsAVolumeRootOnTheLstatWalk":                              "a test's volume root stand-in",
	"internal/fsutil/walkroot_test.go TestWalkableRootDescendsALinkedRoot":                                           "a test's linked library root",
	"internal/fsutil/walkroot_test.go walkedNames":                                                                   "a test's library root",
	"internal/integrity/sidecars_test.go ageFixtures":                                                                "a test's variants directory",
	"internal/integrity/sidecars_test.go countFiles":                                                                 "a test's variants directory",
	"internal/manifest/golden_wavpack_ape_test.go writeB489GoldenLibrary":                                            "the golden library its test writes",
	"internal/manifest/nested_roots_case_watch_test.go openTwice":                                                    "a test's library root",
	"internal/manifest/watcher_kqueue_test.go libraryFiles":                                                          "a test's library root",
	"internal/player/openhome/onebit_test.go copyTree":                                                               "a test's data dir",
	"internal/sweeptest/sweeptest_test.go TestARemovalRemovesWhatTheWalkReaches":                                     "a test's tree",
	"internal/transcode/output_guard_again_unix_test.go TestTheGuardIsAskedAgainJustBeforeTheSidecarDirectoryIsMade": "a test's mount point for a variants volume",
	"internal/trash/trash_test.go TestTrashTTLReadsStampDirNotFileMtime":                                             "a test's trash stamp directory",
	"scripts/install-test/install_test.go tree":                                                                      "the installer rig's home and state directories",

	// Below the root, where make test's TMPDIR is not, or outside the
	// checkout.
	"cmd/bridge/embed_patterns_test.go plantEditorDetritus":                                "each directory go list names for a package that embeds files",
	"cmd/bridge/output_dsdtest_parser_test.go TestCandidateConstructorIsCLIOnly":           "internal/ and cmd/bridge/",
	"internal/acoustid/control_test.go collectAudio":                                       "BRIDGE_ACOUSTID_CONTROL_DIR, outside the checkout, run by hand",
	"internal/admin/feature_tray_parity_test.go trayScriptBodies":                          "the package's static/ directory",
	"internal/admin/js_reference_parity_test.go jsSourceFiles":                             "the package's static/ directory",
	"internal/admin/restart_anchor_test.go TestAppJSSettingsAnchorsExistInTheRenderedPage": "the console's embedded static files",
	"internal/admin/static_assets_test.go filesEmbedded":                                   "the console's embedded static files",
	"internal/admin/static_assets_test.go filesOnDisk":                                     "the package's static files on disk",
	"internal/admin/templates_href_test.go collectTemplateHrefs":                           "the console's embedded templates",
	"internal/alsa/fuzz_parsers_test.go seedCaptures":                                      "the package's testdata/",
	"internal/alsa/reader_test.go copyTree":                                                "a capture in the package's testdata/",
	"internal/api/outputs_reading_test.go copyTree":                                        "a capture in internal/alsa/testdata/",
	"internal/manifest/playlist_covers_test.go TestPrunePlaylistCoversExceptStaysUnwired":  "internal/ and cmd/",
	"internal/player/decode/corpus_test.go TestPrivateFormatCorpus":                        "the private corpus, BRIDGE_FORMAT_CORPUS or ~/medialibtest-formats, outside the checkout",
}

// moduleSource is what readModule read under a root: each Go file the go
// tool would build there, test files included, parsed, by its directory.
type moduleSource struct {
	dirs         map[string]*packageDir // by slash path below the root, "." for the root's own
	files, tests int                    // how many non-test and test files it read
}

// packageDir is one directory's Go files, its test files among them, and
// what the sweep knows of the names they declare.
type packageDir struct {
	files      []*sourceFile
	funcs      map[string][]funcRef // its functions and methods, by bare name
	walkTypes  map[string]bool      // the func types it declares in filepath.WalkDir's shape
	walkFields map[string]bool      // the struct fields it declares in that shape
	callbacks  map[string]bool      // its functions and methods in a walk callback's shape
}

// sourceFile is one parsed file, with its slash path below the root and its
// names for the packages a walk is read by.
type sourceFile struct {
	rel   string
	file  *ast.File
	names imports
}

// funcRef is a function or method declaration with the file it is in.
type funcRef struct {
	decl *ast.FuncDecl
	file *sourceFile
}

// imports holds a file's names for the packages the sweep reads a walk by,
// "" for one it does not import, and every name it imports a package as.
type imports struct {
	filepath, fs, os, strings, fsutil string
	packages                          map[string]bool
}

// readModule reads, with walk, every Go file below root that the go tool
// would build, test files included, and indexes what each directory
// declares. walk is filepath.WalkDir, or in a test a Removal's WalkDir.
func readModule(walk func(root string, fn fs.WalkDirFunc) error, root string) (*moduleSource, error) {
	mod := &moduleSource{dirs: map[string]*packageDir{}}
	fset := token.NewFileSet()
	err := walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipsDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !readsFile(d.Name()) {
			return nil
		}
		return mod.read(fset, root, path)
	})
	if err != nil {
		return nil, err
	}
	for _, dir := range mod.dirs {
		dir.index()
	}
	return mod, nil
}

// skipsDir reports whether readModule leaves a directory below root unread,
// decided before the walk lists it: make test's TMPDIR, by its name alone
// (IsMakeTestTmpDir); the repository's metadata; what no build of this
// module compiles, a name beginning with "_", testdata, vendor and
// node_modules; another checkout (IsOtherCheckout); and another module. A
// dot-directory is read: .github holds a Go program, and a walk there is
// this checkout's as much as any. The root itself never is skipped.
func skipsDir(root, path, name string) bool {
	if filepath.Clean(path) == filepath.Clean(root) {
		return false
	}
	if IsMakeTestTmpDir(root, path) {
		return true
	}
	if name == ".git" || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules" {
		return true
	}
	if IsOtherCheckout(root, path) {
		return true
	}
	fi, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil && !fi.IsDir()
}

// readsFile reports whether readModule parses a file of this name: a Go file
// the go tool would compile, so not one whose name begins with "." or "_",
// as an editor's lock beside a file does.
func readsFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// read parses the Go file at path into its directory's packageDir.
func (m *moduleSource) read(fset *token.FileSet, root, path string) error {
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if strings.HasSuffix(rel, "_test.go") {
		m.tests++
	} else {
		m.files++
	}
	key := "."
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		key = rel[:i]
	}
	dir := m.dirs[key]
	if dir == nil {
		dir = &packageDir{}
		m.dirs[key] = dir
	}
	dir.files = append(dir.files, &sourceFile{rel: rel, file: f, names: importsOf(f)})
	return nil
}

// importsOf is f's names for the packages a walk is read by.
func importsOf(f *ast.File) imports {
	n := imports{packages: map[string]bool{}}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		n.packages[name] = true
		switch p {
		case "path/filepath":
			n.filepath = name
		case "io/fs":
			n.fs = name
		case "os":
			n.os = name
		case "strings":
			n.strings = name
		case "github.com/acoseac/1-bit-bridge/internal/fsutil":
			n.fsutil = name
		}
	}
	return n
}

// index fills in what d's files declare: its functions by bare name, its
// types and struct fields in filepath.WalkDir's shape, and its functions in
// a walk callback's.
func (d *packageDir) index() {
	d.funcs = map[string][]funcRef{}
	d.walkTypes = map[string]bool{}
	d.walkFields = map[string]bool{}
	d.callbacks = map[string]bool{}
	// The types first: a field or a parameter may be of a type another file
	// declares.
	for _, f := range d.files {
		for _, decl := range f.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if ft, ok := ts.Type.(*ast.FuncType); ok && f.names.isWalk(ft) {
					d.walkTypes[ts.Name.Name] = true
				}
			}
		}
	}
	for _, f := range d.files {
		for _, decl := range f.file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				d.funcs[fd.Name.Name] = append(d.funcs[fd.Name.Name], funcRef{fd, f})
				if f.names.isCallback(fd.Type) {
					d.callbacks[fd.Name.Name] = true
				}
			}
		}
		ast.Inspect(f.file, func(node ast.Node) bool {
			if st, ok := node.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					if d.isWalkType(f.names, field.Type) {
						for _, name := range field.Names {
							d.walkFields[name.Name] = true
						}
					}
				}
			}
			return true
		})
	}
}

// walkSite is a function of the module that walks a directory tree, or a
// walk a file holds at package level.
type walkSite struct {
	dir  *packageDir
	file *sourceFile
	decl ast.Decl // an *ast.FuncDecl, or the *ast.GenDecl of a walk held at package level
	why  []string // what makes it a walk
}

// packageLevel is how the key of a walk a file holds at package level ends.
const packageLevel = " (package level)"

// walks returns every function of m that walks a directory tree, keyed
// "<slash path> <function>" as fsutil's tables are, with what makes it one:
// a call of filepath.WalkDir, filepath.Walk, fs.WalkDir or
// fsutil.WalkConfigured; a walk callback written in it; a call through a
// walk it is handed or holds (a parameter, variable or field in
// filepath.WalkDir's shape); a callback its package declares, handed on as
// a value; or a directory it lists in a function that calls itself. A function that
// only hands a walk on, as a test hands a sweep filepath.WalkDir or a
// Removal's WalkDir, makes none: the function it hands it to does. A walk a
// file holds at package level, the seam a test would replace, is keyed by
// the file and packageLevel.
func (m *moduleSource) walks() map[string]walkSite {
	out := map[string]walkSite{}
	for _, d := range m.dirs {
		for _, f := range d.files {
			for _, decl := range f.file.Decls {
				why := d.walkReasons(f, decl)
				if len(why) == 0 {
					continue
				}
				key := f.rel + packageLevel
				if fd, ok := decl.(*ast.FuncDecl); ok {
					key = f.rel + " " + funcName(fd)
				}
				if prev, ok := out[key]; ok {
					prev.why = append(prev.why, why...)
					out[key] = prev
					continue
				}
				out[key] = walkSite{dir: d, file: f, decl: decl, why: why}
			}
		}
	}
	return out
}

// walkReasons is what makes decl, in f, a walk, and nothing for one that is
// not.
func (d *packageDir) walkReasons(f *sourceFile, decl ast.Decl) []string {
	n := f.names
	var why []string
	add := func(s string) {
		if !slices.Contains(why, s) {
			why = append(why, s)
		}
	}
	fd, ok := decl.(*ast.FuncDecl)
	if !ok {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return nil
		}
		ast.Inspect(gd, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.ValueSpec:
				if x.Type != nil && d.isWalkType(n, x.Type) {
					add("a variable in filepath.WalkDir's shape")
				}
			case *ast.SelectorExpr:
				if name, ok := d.walkValue(n, x); ok {
					add("holds " + name)
				}
			case *ast.FuncLit:
				if n.isCallback(x.Type) {
					add("a walk callback")
				}
			}
			return true
		})
		return why
	}
	if fd.Body == nil {
		return nil
	}
	walks := d.walkNames(n, fd)
	ast.Inspect(fd, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.FuncLit:
			if n.isCallback(x.Type) {
				add("a walk callback")
			}
		case *ast.CallExpr:
			if name, ok := n.knownWalk(x.Fun); ok {
				add("calls " + name)
			}
			switch fun := x.Fun.(type) {
			case *ast.Ident:
				if walks[fun.Name] {
					add("calls " + fun.Name + ", a walk it is handed")
				}
			case *ast.SelectorExpr:
				if d.walkFields[fun.Sel.Name] && !n.isPackage(fun.X) {
					add("calls " + fun.Sel.Name + ", a walk it holds")
				}
			}
			for _, arg := range x.Args {
				if name, ok := d.namedCallback(n, arg); ok {
					add("hands on " + name + ", a walk callback")
				}
			}
		}
		return true
	})
	if listsDir(n, fd.Body) && callsItself(fd) {
		add("lists a directory and calls itself")
	}
	ast.Inspect(fd.Body, func(node ast.Node) bool {
		as, ok := node.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			lit, isLit := rhs.(*ast.FuncLit)
			id, isIdent := as.Lhs[i].(*ast.Ident)
			if isLit && isIdent && listsDir(n, lit.Body) && calls(lit.Body, id.Name) {
				add("lists a directory in " + id.Name + ", which calls itself")
			}
		}
		return true
	})
	return why
}

// walkNames is the names fd binds to a walk: a parameter in
// filepath.WalkDir's shape, its own or a function literal's, and a variable
// declared in that shape or given a walk.
func (d *packageDir) walkNames(n imports, fd *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(fd, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.FuncType:
			if x.Params == nil {
				return true
			}
			for _, field := range x.Params.List {
				if d.isWalkType(n, field.Type) {
					for _, id := range field.Names {
						names[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if (x.Type != nil && d.isWalkType(n, x.Type)) || (i < len(x.Values) && d.isWalkValue(n, x.Values[i])) {
					names[id.Name] = true
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) != len(x.Rhs) {
				return true
			}
			for i, rhs := range x.Rhs {
				if id, ok := x.Lhs[i].(*ast.Ident); ok && d.isWalkValue(n, rhs) {
					names[id.Name] = true
				}
			}
		}
		return true
	})
	return names
}

// isWalkType reports whether e, a type in a file with names n, is in
// filepath.WalkDir's shape: written so, or a type d declares so.
func (d *packageDir) isWalkType(n imports, e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.FuncType:
		return n.isWalk(x)
	case *ast.Ident:
		return d.walkTypes[x.Name]
	}
	return false
}

// isWalkValue reports whether e is a walk as a value.
func (d *packageDir) isWalkValue(n imports, e ast.Expr) bool {
	_, ok := d.walkValue(n, e)
	return ok
}

// walkValue names e when it is a walk as a value: filepath.WalkDir and its
// kin, or a WalkDir method value, a Removal's.
func (d *packageDir) walkValue(n imports, e ast.Expr) (string, bool) {
	if name, ok := n.knownWalk(e); ok {
		return name, true
	}
	if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == "WalkDir" && !n.isPackage(sel.X) {
		return "a WalkDir method", true
	}
	return "", false
}

// namedCallback names arg when it is a callback d declares, a function or a
// method in a walk callback's shape, handed on as a value.
func (d *packageDir) namedCallback(n imports, arg ast.Expr) (string, bool) {
	switch a := arg.(type) {
	case *ast.Ident:
		return a.Name, d.callbacks[a.Name]
	case *ast.SelectorExpr:
		return a.Sel.Name, d.callbacks[a.Sel.Name] && !n.isPackage(a.X)
	}
	return "", false
}

// knownWalk names e when it is filepath.WalkDir, filepath.Walk, fs.WalkDir
// or fsutil.WalkConfigured, by the file's names for their packages.
func (n imports) knownWalk(e ast.Expr) (string, bool) {
	switch {
	case isSelector(e, n.filepath, "WalkDir"):
		return "filepath.WalkDir", true
	case isSelector(e, n.filepath, "Walk"):
		return "filepath.Walk", true
	case isSelector(e, n.fs, "WalkDir"):
		return "fs.WalkDir", true
	case isSelector(e, n.fsutil, "WalkConfigured"):
		return "fsutil.WalkConfigured", true
	}
	return "", false
}

// isPackage reports whether e is the name a file imports a package as.
func (n imports) isPackage(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && n.packages[id.Name]
}

// isCallback reports whether ft is a walk callback's signature:
// func(string, fs.DirEntry or fs.FileInfo, error) error, os's names for
// either taken too.
func (n imports) isCallback(ft *ast.FuncType) bool {
	p, r := flatTypes(ft.Params), flatTypes(ft.Results)
	if len(p) != 3 || len(r) != 1 || !isIdent(p[0], "string") || !isIdent(p[2], "error") || !isIdent(r[0], "error") {
		return false
	}
	for _, pkg := range []string{n.fs, n.os} {
		if isSelector(p[1], pkg, "DirEntry") || isSelector(p[1], pkg, "FileInfo") {
			return true
		}
	}
	return false
}

// isWalk reports whether ft is in filepath.WalkDir's shape: a root, a
// callback, and an error.
func (n imports) isWalk(ft *ast.FuncType) bool {
	p, r := flatTypes(ft.Params), flatTypes(ft.Results)
	if len(p) != 2 || len(r) != 1 || !isIdent(p[0], "string") || !isIdent(r[0], "error") {
		return false
	}
	if cb, ok := p[1].(*ast.FuncType); ok {
		return n.isCallback(cb)
	}
	return isSelector(p[1], n.fs, "WalkDirFunc") || isSelector(p[1], n.filepath, "WalkFunc")
}

// flatTypes is a field list's types, one for each name it declares.
func flatTypes(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range fl.List {
		n := max(len(f.Names), 1)
		for range n {
			out = append(out, f.Type)
		}
	}
	return out
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// isSelector reports whether e is pkg.name, for a pkg the file imports.
func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || pkg == "" || sel.Sel.Name != name {
		return false
	}
	return isIdent(sel.X, pkg)
}

// listsDir reports whether body lists a directory: os.ReadDir, fs.ReadDir,
// or a ReadDir, Readdir or Readdirnames method.
func listsDir(n imports, body ast.Node) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return !found
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "ReadDir":
				found = found || isSelector(sel, n.os, "ReadDir") || isSelector(sel, n.fs, "ReadDir") || !n.isPackage(sel.X)
			case "Readdir", "Readdirnames":
				found = found || !n.isPackage(sel.X)
			}
		}
		return !found
	})
	return found
}

// callsItself reports whether fd calls itself: a function by its name, a
// method through its receiver.
func callsItself(fd *ast.FuncDecl) bool {
	recv := ""
	if fd.Recv != nil && len(fd.Recv.List) == 1 && len(fd.Recv.List[0].Names) == 1 {
		recv = fd.Recv.List[0].Names[0].Name
	}
	found := false
	ast.Inspect(fd.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			found = found || fd.Recv == nil && fun.Name == fd.Name.Name
		case *ast.SelectorExpr:
			found = found || recv != "" && isIdent(fun.X, recv) && fun.Sel.Name == fd.Name.Name
		}
		return !found
	})
	return found
}

// calls reports whether body calls the identifier name.
func calls(body ast.Node, name string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isIdent(call.Fun, name) {
			found = true
		}
		return !found
	})
	return found
}

// funcName is a function's name as fsutil's tables spell it: `f`, or `(*T).m`
// and `(T).m` for a method.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch recv := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := recv.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	case *ast.Ident:
		return "(" + recv.Name + ")." + fn.Name.Name
	}
	return "(?)." + fn.Name.Name
}

// listedWalk is one item of MakeTestTmpDir's list of the walks from the
// root.
type listedWalk struct {
	key   string   // the walk: its file's slash path and its function
	tests []string // the tests that make it from the root
	rule  string   // ruleByName or ruleDotDirectory
	in    string   // the function that holds the rule: the walk's own, or one it calls
}

// ruleByName and ruleDotDirectory are the two rules a walk from the root
// skips make test's TMPDIR by, as MakeTestTmpDir's doc words them.
const (
	ruleByName       = "by its name"
	ruleDotDirectory = "as a dot-directory"
)

// ruleCheck is what each rule's function must hold, as a problem says it.
var ruleCheck = map[string]string{
	ruleByName:       "a call of IsMakeTestTmpDir (in the census, its makeTestTmpDir)",
	ruleDotDirectory: `strings.HasPrefix of a name and "."`,
}

// inventoryItem is an item of that list, its lines joined: the walk's file
// and function, the tests that make it from the root, its rule and the
// function that holds it.
var inventoryItem = regexp.MustCompile(`^(\S+\.go) (\S+), from (Test\w+(?:(?:, | and )Test\w+)*): (` +
	ruleByName + `|` + ruleDotDirectory + `), in (\w+)\.$`)

// inventoryDoc is the doc comment of MakeTestTmpDir in the file at path.
func inventoryDoc(path string) (string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		return "", err
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Names) != 1 || vs.Names[0].Name != "MakeTestTmpDir" {
				continue
			}
			doc := vs.Doc
			if doc == nil {
				doc = gd.Doc
			}
			if doc == nil {
				return "", fmt.Errorf("%s: MakeTestTmpDir has no doc comment, which lists the walks from the root", path)
			}
			return doc.Text(), nil
		}
	}
	return "", fmt.Errorf("%s declares no MakeTestTmpDir", path)
}

// rootWalkInventory reads the list in doc, MakeTestTmpDir's doc comment: a
// listedWalk for each item, and a problem for each item it cannot read, or
// for a doc that holds no list, or more than one.
func rootWalkInventory(doc string) ([]listedWalk, []string) {
	var lists []*comment.List
	for _, block := range new(comment.Parser).Parse(doc).Content {
		if l, ok := block.(*comment.List); ok {
			lists = append(lists, l)
		}
	}
	if len(lists) != 1 {
		return nil, []string{fmt.Sprintf("MakeTestTmpDir's doc holds %d lists, want one: the walks from the root", len(lists))}
	}
	var listed []listedWalk
	var problems []string
	for _, item := range lists[0].Items {
		text := itemText(item)
		m := inventoryItem.FindStringSubmatch(text)
		if m == nil {
			problems = append(problems, fmt.Sprintf("MakeTestTmpDir's doc lists %q, which does not read as "+
				"\"<file> <function>, from <tests>: %s|%s, in <function>.\"", text, ruleByName, ruleDotDirectory))
			continue
		}
		tests := strings.Split(strings.ReplaceAll(m[3], " and ", ", "), ", ")
		listed = append(listed, listedWalk{key: m[1] + " " + m[2], tests: tests, rule: m[4], in: m[5]})
	}
	return listed, problems
}

// itemText is a list item's text, its lines joined by single spaces, with
// a NUL where it holds anything but plain text, which no item reads as.
func itemText(item *comment.ListItem) string {
	var b strings.Builder
	for _, block := range item.Content {
		p, ok := block.(*comment.Paragraph)
		if !ok {
			b.WriteString("\x00")
			continue
		}
		for _, t := range p.Text {
			if plain, ok := t.(comment.Plain); ok {
				b.WriteString(string(plain))
			} else {
				b.WriteString("\x00")
			}
		}
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// checkWalks checks the walks mod holds against the walks from the root
// listed and the walks elsewhere names: one sentence a problem.
func checkWalks(mod *moduleSource, listed []listedWalk, elsewhere map[string]string) []string {
	found := mod.walks()
	inList := map[string]bool{}
	var problems []string
	for _, l := range listed {
		if inList[l.key] {
			problems = append(problems, fmt.Sprintf("MakeTestTmpDir's doc lists %s twice", l.key))
		}
		inList[l.key] = true
	}
	for _, key := range slices.Sorted(maps.Keys(found)) {
		_, other := elsewhere[key]
		why := strings.Join(found[key].why, "; ")
		switch {
		case strings.HasSuffix(key, packageLevel) && strings.HasSuffix(found[key].file.rel, "_test.go"):
			problems = append(problems, fmt.Sprintf("%s holds a walk at package level in a test file (%s): hand the walk "+
				"to the sweep as a parameter, so that no test replaces a walk its package holds", key, why))
		case inList[key] && other:
			problems = append(problems, fmt.Sprintf("%s is a walk from the root in MakeTestTmpDir's doc and a walk elsewhere in walksElsewhere: "+
				"it is one or the other", key))
		case !inList[key] && !other && strings.HasSuffix(key, packageLevel):
			problems = append(problems, fmt.Sprintf("%s holds a walk at package level (%s), the seam a test would replace: "+
				"hand the walk to the sweep as a parameter instead", key, why))
		case !inList[key] && !other:
			problems = append(problems, fmt.Sprintf("%s walks a directory tree (%s) and is named neither in MakeTestTmpDir's doc, "+
				"as a walk from the root, nor in walksElsewhere: if it can start at the checkout's root, skip MakeTestTmpDir "+
				"there by IsMakeTestTmpDir before the walk lists a directory, and list it in that doc with the tests that make "+
				"it from the root and its rule; otherwise name it in walksElsewhere with what it walks", key, why))
		}
	}
	for _, l := range listed {
		w, ok := found[l.key]
		if !ok {
			problems = append(problems, fmt.Sprintf("MakeTestTmpDir's doc lists %s, which walks no tree: "+
				"remove the item, or name the function that walks", l.key))
			continue
		}
		problems = append(problems, w.checkListed(l)...)
	}
	for _, key := range slices.Sorted(maps.Keys(elsewhere)) {
		if _, ok := found[key]; !ok {
			problems = append(problems, fmt.Sprintf("walksElsewhere names %s, which walks no tree: remove it", key))
		}
	}
	return problems
}

// checkListed checks the walk w against its item l: each test l names must
// be a test of w's directory that reaches w, and w's branch for a directory
// must reach l's rule through the function l says holds it.
func (w walkSite) checkListed(l listedWalk) []string {
	fd, ok := w.decl.(*ast.FuncDecl)
	if !ok {
		return []string{fmt.Sprintf("MakeTestTmpDir's doc lists %s, a walk held at package level, which makes no walk by itself", l.key)}
	}
	var problems []string
	for _, test := range l.tests {
		if !w.dir.reachesFrom(test, fd.Name.Name) {
			problems = append(problems, fmt.Sprintf("MakeTestTmpDir's doc says %s makes %s from the root, and no test of "+
				"that name in its directory reaches it", test, l.key))
		}
	}
	if !w.ruleHeld(fd, l) {
		problems = append(problems, fmt.Sprintf("MakeTestTmpDir's doc says %s skips the TMPDIR %s, in %s, and no branch "+
			"its walk takes for a directory (an if on the entry's IsDir) reaches %s through %s", l.key, l.rule, l.in,
			ruleCheck[l.rule], l.in))
	}
	return problems
}

// reachesFrom reports whether d declares a test named test that is target or
// calls it, through the calls of the functions d declares.
func (d *packageDir) reachesFrom(test, target string) bool {
	if !strings.HasPrefix(test, "Test") {
		return false
	}
	for _, ref := range d.funcs[test] {
		if ref.decl.Recv != nil || ref.decl.Body == nil {
			continue
		}
		if test == target || d.reached(ref.decl.Body)[target] {
			return true
		}
	}
	return false
}

// ruleHeld reports whether a branch fd's walk takes for a directory
// (dirBranches) reaches l's rule: in the branch itself, or in what it calls,
// where l says fd holds it, and otherwise through the function l names,
// which must hold it.
func (w walkSite) ruleHeld(fd *ast.FuncDecl, l listedWalk) bool {
	for _, branch := range dirBranches(w.file.names, fd) {
		if l.in == fd.Name.Name {
			if w.dir.holdsRule(branch, w.file, l.rule) {
				return true
			}
			continue
		}
		if !w.dir.reached(branch)[l.in] {
			continue
		}
		for _, ref := range w.dir.funcs[l.in] {
			if ref.decl.Body != nil && w.dir.holdsRule(ref.decl.Body, ref.file, l.rule) {
				return true
			}
		}
	}
	return false
}

// dirBranches is every branch fd's walk callbacks take for a directory: an
// if whose condition asks an entry's IsDir, its init, its condition and its
// body, and not its else.
func dirBranches(n imports, fd *ast.FuncDecl) []ast.Node {
	var out []ast.Node
	ast.Inspect(fd, func(node ast.Node) bool {
		lit, ok := node.(*ast.FuncLit)
		if !ok || !n.isCallback(lit.Type) {
			return true
		}
		ast.Inspect(lit.Body, func(node ast.Node) bool {
			if is, ok := node.(*ast.IfStmt); ok && asksIsDir(is.Cond) {
				branch := &ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: is.Cond}, is.Body}}
				if is.Init != nil {
					branch.List = append(branch.List, is.Init)
				}
				out = append(out, branch)
			}
			return true
		})
		return true
	})
	return out
}

// asksIsDir reports whether cond holds for a directory alone: an entry's
// IsDir, alone or beside another condition that must hold with it.
func asksIsDir(cond ast.Expr) bool {
	switch c := cond.(type) {
	case *ast.CallExpr:
		sel, ok := c.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "IsDir" && len(c.Args) == 0
	case *ast.BinaryExpr:
		return c.Op == token.LAND && (asksIsDir(c.X) || asksIsDir(c.Y))
	case *ast.ParenExpr:
		return asksIsDir(c.X)
	}
	return false
}

// holdsRule reports whether node, in file, holds the check rule names, or
// reaches a function of d that does.
func (d *packageDir) holdsRule(node ast.Node, file *sourceFile, rule string) bool {
	if holdsCheck(node, file.names, rule) {
		return true
	}
	for name := range d.reached(node) {
		for _, ref := range d.funcs[name] {
			if ref.decl.Body != nil && holdsCheck(ref.decl.Body, ref.file.names, rule) {
				return true
			}
		}
	}
	return false
}

// holdsCheck reports whether node holds rule's check itself: a call of
// IsMakeTestTmpDir, or the census's makeTestTmpDir, for ruleByName; a
// strings.HasPrefix of a name and ".", for ruleDotDirectory.
func holdsCheck(node ast.Node, n imports, rule string) bool {
	found := false
	ast.Inspect(node, func(x ast.Node) bool {
		if found {
			return false
		}
		switch v := x.(type) {
		case *ast.CallExpr:
			switch rule {
			case ruleByName:
				found = calleeName(v) == "IsMakeTestTmpDir"
			case ruleDotDirectory:
				if isSelector(v.Fun, n.strings, "HasPrefix") && len(v.Args) == 2 {
					lit, ok := v.Args[1].(*ast.BasicLit)
					found = ok && lit.Kind == token.STRING && lit.Value == `"."`
				}
			}
		case *ast.Ident:
			found = rule == ruleByName && v.Name == "makeTestTmpDir"
		}
		return !found
	})
	return found
}

// calleeName is the bare name of the function call calls: an identifier's,
// or a selector's last element, and "" for any other.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// reached is the bare name of every function start calls, and of every
// function those of d's call in turn.
func (d *packageDir) reached(start ast.Node) map[string]bool {
	seen := map[string]bool{}
	queue := calleeNames(start)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, ref := range d.funcs[name] {
			if ref.decl.Body != nil {
				queue = append(queue, calleeNames(ref.decl.Body)...)
			}
		}
	}
	return seen
}

// calleeNames is the bare name of every function node calls: an
// identifier's, or a selector's last element.
func calleeNames(node ast.Node) []string {
	var out []string
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) != "" {
			out = append(out, calleeName(call))
		}
		return true
	})
	return out
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
