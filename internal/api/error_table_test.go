package api

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestEverySentErrorCodeIsInTheProtocolTable pins PROTOCOL.md's error table
// to the code, as TestEveryRoutedEndpointIsDocumented pins the endpoints
// (backlog B426). The table is where a client author reads which `error`
// codes the bridge sends, under which status, and nothing kept it in step:
// before B413 (#1255) completed it by hand, it listed 22 codes while the /v1
// handlers sent 48 status and code pairs, and ten of its rows were wrong.
// Every pair a handler can send needs a row, so a new code fails here until
// its row lands, in the same PR; the app's copy of the spec follows as usual.
func TestEverySentErrorCodeIsInTheProtocolTable(t *testing.T) {
	sent := sentErrorPairs(t)
	documented := map[errorPair]bool{}
	for _, row := range protocolErrorRows(t) {
		documented[row.pair] = true
	}
	var missing []string
	for p, sites := range sent {
		if !documented[p] {
			missing = append(missing, fmt.Sprintf("%s, sent at %s", p, strings.Join(sites, "; ")))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("PROTOCOL.md's error table has no row for %d status and code pair(s) a /v1 handler sends:\n  %s\n\n"+
			"Add a row under \"## Error responses\" in the same PR.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestEveryProtocolErrorRowIsSent pins the other direction: every row names a
// pair a /v1 handler sends, except a row marked reserved, which nothing may
// send. A row for a pair nothing sends documents an answer no client gets.
func TestEveryProtocolErrorRowIsSent(t *testing.T) {
	sent := sentErrorPairs(t)
	for _, row := range protocolErrorRows(t) {
		sites, isSent := sent[row.pair]
		switch {
		case row.reserved && isSent:
			t.Errorf("PROTOCOL.md marks %s %q, but it is sent at %s: drop the marker and say when it is sent",
				row.pair, reservedRowMarker, strings.Join(sites, "; "))
		case !row.reserved && !isSent:
			t.Errorf("PROTOCOL.md's error table has a row for %s, which no /v1 handler sends: "+
				"correct or delete the row, or mark it %q", row.pair, reservedRowMarker)
		}
	}
}

// errorPair is an HTTP status and the `error` code an answer carries.
type errorPair struct {
	status int
	code   string
}

func (p errorPair) String() string { return strconv.Itoa(p.status) + " " + p.code }

// reservedRowMarker is how the error table marks a row no route sends, the
// way B413 kept `forbidden`.
const reservedRowMarker = "(reserved; no route sends it)"

// protocolErrorRow is one row of PROTOCOL.md's error table.
type protocolErrorRow struct {
	pair     errorPair
	reserved bool
}

// protocolErrorRows reads PROTOCOL.md's error table, failing the test when
// the table cannot be read or reads too short to guard anything.
func protocolErrorRows(t *testing.T) []protocolErrorRow {
	t.Helper()
	spec, err := os.ReadFile("../../PROTOCOL.md")
	if err != nil {
		t.Fatalf("read PROTOCOL.md: %v", err)
	}
	rows, err := parseErrorTable(string(spec))
	if err != nil {
		t.Fatalf("PROTOCOL.md's error table: %v", err)
	}
	if len(rows) < 40 {
		t.Fatalf("read only %d rows from PROTOCOL.md's error table; the parse is broken "+
			"and these guards would pass vacuously", len(rows))
	}
	return rows
}

// parseErrorTable reads the first table under the spec's "## Error
// responses" heading. Its header must be `| Status | `error` code | When |`,
// and every row one status and one backticked code; a pair with two rows is
// refused. The When cell is everything after the second column, so a `|`
// inside it does not split it.
func parseErrorTable(spec string) ([]protocolErrorRow, error) {
	lines := strings.Split(strings.ReplaceAll(spec, "\r\n", "\n"), "\n")
	start := slices.Index(lines, "## Error responses")
	if start < 0 {
		return nil, errors.New(`no "## Error responses" heading`)
	}
	var rows []protocolErrorRow
	seen := map[errorPair]bool{}
	header, aligned := false, false
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if header {
				break
			}
			continue
		}
		cells := strings.SplitN(line, "|", 4)
		if len(cells) < 4 {
			return nil, fmt.Errorf("line %d is not a three-column row: %q", i+1, line)
		}
		status, code := strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])
		when := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cells[3]), "|"))
		switch {
		case !header:
			if status != "Status" || code != "`error` code" || when != "When" {
				return nil, fmt.Errorf("line %d: the first table under the heading is not | Status | `error` code | When |: %q", i+1, line)
			}
			header = true
			continue
		case !aligned:
			if strings.Trim(status, "-:") != "" {
				return nil, fmt.Errorf("line %d: no alignment row under the header: %q", i+1, line)
			}
			aligned = true
			continue
		}
		n, err := strconv.Atoi(status)
		if err != nil || n < 100 || n > 599 {
			return nil, fmt.Errorf("line %d: status %q is not an HTTP status", i+1, status)
		}
		if len(code) < 3 || code[0] != '`' || code[len(code)-1] != '`' || strings.Count(code, "`") != 2 {
			return nil, fmt.Errorf("line %d: %q is not one backticked code", i+1, code)
		}
		p := errorPair{status: n, code: code[1 : len(code)-1]}
		if seen[p] {
			return nil, fmt.Errorf("line %d: %s has a second row", i+1, p)
		}
		seen[p] = true
		rows = append(rows, protocolErrorRow{pair: p, reserved: strings.Contains(when, reservedRowMarker)})
	}
	if !header {
		return nil, errors.New("no table under the heading")
	}
	return rows, nil
}

// sentErrorPairs is every status and code pair internal/api's handlers send,
// each with where it is sent, failing the test on any error write the scan
// cannot read.
func sentErrorPairs(t *testing.T) map[errorPair][]string {
	t.Helper()
	scan, err := scanErrorPairs(".", declaredFieldFlows)
	if err != nil {
		t.Fatalf("scan internal/api: %v", err)
	}
	if len(scan.unresolved) > 0 {
		t.Fatalf("the guard cannot read the status or the code of %d error write(s):\n  %s\n\n"+
			"Write each as a constant, or as a parameter every caller passes a constant. A value only "+
			"another package's source holds goes in declaredFieldFlows, with the reason.",
			len(scan.unresolved), strings.Join(scan.unresolved, "\n  "))
	}
	if len(scan.pairs) < 40 {
		t.Fatalf("found only %d status and code pairs in internal/api; the scan is broken "+
			"and these guards would pass vacuously", len(scan.pairs))
	}
	return scan.pairs
}

// fieldFlow declares a status and code this parser cannot follow by itself:
// a writer call in fn passes the statusField and codeField of value, which
// another package builds. Their values are still read from source, as the
// fields of every typeName composite literal in dir.
type fieldFlow struct {
	fn, value              string
	dir, typeName          string
	statusField, codeField string
}

// declaredFieldFlows is every fieldFlow internal/api has, and the guard's one
// exception. proxyUPnP writes the Status and Code of the
// *upnpproxy.PreStreamError that Proxy.Serve returns, and only a type checker
// knows perr's type; this guard runs none, since checking internal/api would
// mean checking every dependency from source. internal/upnpproxy builds each
// PreStreamError as a literal, so those literals are the pairs. A flow that
// no longer matches a writer call is reported.
var declaredFieldFlows = []fieldFlow{{
	fn: "proxyUPnP", value: "perr",
	dir: "../upnpproxy", typeName: "PreStreamError",
	statusField: "Status", codeField: "Code",
}}

// errorWriters write the error envelope: the positions of their status and
// code parameters. Their own bodies pass their parameters on and are not read.
var errorWriters = map[string][2]int{
	"writeError":        {1, 2},
	"writeErrorLog":     {2, 3},
	"writeFileErrorLog": {2, 3},
}

// jsonWriters write any JSON body with a status: the positions of the status
// and the body. A body is an error body when it is a literal of a struct whose
// field is tagged json:"error"; any other body under a 4xx or 5xx status is
// one the guard cannot read.
var jsonWriters = map[string][2]int{
	"writeJSON":     {1, 2},
	"writeJSONGzip": {2, 3},
}

// errorPairScan is what scanErrorPairs found.
type errorPairScan struct {
	pairs      map[errorPair][]string // each pair, with the places that send it
	unresolved []string               // each error write it could not read, and why
}

func (s *errorPairScan) add(pairs []errorPair, site string) {
	for _, p := range pairs {
		if !slices.Contains(s.pairs[p], site) {
			s.pairs[p] = append(s.pairs[p], site)
		}
	}
}

// scanErrorPairs reads every status and code pair the Go package in dir
// writes through errorWriters, and through jsonWriters under a 4xx or 5xx
// status. A status or code is read when it is a constant (a literal, a
// constant of the package, one of net/http's Status constants, or a sum of
// them), a parameter, read at each call to its function so a status and a
// code stay paired, or a local set from a call whose results are those;
// flows name the values another package builds. Anything else is
// unresolved, never skipped.
func scanErrorPairs(dir string, flows []fieldFlow) (errorPairScan, error) {
	scan := errorPairScan{pairs: map[errorPair][]string{}}
	pkg, err := loadSourcePackage(dir)
	if err != nil {
		return scan, err
	}
	used := make([]bool, len(flows))
	var flowErr error
	pkg.eachCall(func(call *ast.CallExpr, at place) {
		id, ok := call.Fun.(*ast.Ident)
		if !ok || at.shadow[id.Name] || flowErr != nil {
			return
		}
		ew, isErr := errorWriters[id.Name]
		jw, isJSON := jsonWriters[id.Name]
		if (!isErr && !isJSON) || (at.fn != nil && at.fn.Recv == nil && isWriterName(at.fn.Name.Name)) {
			return
		}
		site := pkg.site(call, at)
		pos := ew
		if isJSON {
			pos = jw
		}
		if len(call.Args) <= pos[1] {
			scan.unresolved = append(scan.unresolved, site+": too few arguments")
			return
		}
		statusExpr, second := call.Args[pos[0]], call.Args[pos[1]]
		if isErr {
			if i := matchFlow(flows, at, statusExpr, second); i >= 0 {
				used[i] = true
				flowErr = addFlowPairs(&scan, flows[i], site)
				return
			}
			pkg.send(&scan, site, at, func(f *frame) ([]errorPair, error) {
				statuses, err := pkg.eval(statusExpr, f, 0)
				if err != nil {
					return nil, err
				}
				codes, err := pkg.eval(second, f, 0)
				if err != nil {
					return nil, err
				}
				return pairsOf(statuses, codes)
			})
			return
		}
		pkg.send(&scan, site, at, func(f *frame) ([]errorPair, error) {
			statuses, err := pkg.eval(statusExpr, f, 0)
			if err != nil {
				return nil, err
			}
			// Only a 4xx or 5xx body is an error body: a 2xx one may carry a
			// field tagged json:"error" as data (a batch row's).
			var failing []constant.Value
			for _, v := range statuses {
				if v.Kind() != constant.Int {
					return nil, fmt.Errorf("status %s is not an HTTP status", v)
				}
				if n, ok := constant.Int64Val(v); !ok || n >= 400 {
					failing = append(failing, v)
				}
			}
			if len(failing) == 0 {
				return nil, nil
			}
			codeExpr, codeFrame, err := pkg.errorBodyCode(second, f)
			if err != nil {
				return nil, err
			}
			if codeExpr == nil {
				return nil, fmt.Errorf("writes a %s JSON body whose error code it cannot read; "+
					"write it as a literal of a struct with a json:\"error\" field", failing[0])
			}
			codes, err := pkg.eval(codeExpr, codeFrame, 0)
			if err != nil {
				return nil, err
			}
			return pairsOf(failing, codes)
		})
	})
	if flowErr != nil {
		return scan, flowErr
	}
	for i, fl := range flows {
		if !used[i] {
			scan.unresolved = append(scan.unresolved, fmt.Sprintf(
				"the declared flow of %s.%s and %s.%s in %s matches no error write; remove it",
				fl.value, fl.statusField, fl.value, fl.codeField, fl.fn))
		}
	}
	return scan, nil
}

func isWriterName(name string) bool {
	_, e := errorWriters[name]
	_, j := jsonWriters[name]
	return e || j
}

// matchFlow returns the flow a writer call's status and code read, or -1.
func matchFlow(flows []fieldFlow, at place, status, code ast.Expr) int {
	if at.fn == nil {
		return -1
	}
	field := func(e ast.Expr, value, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == value
	}
	for i, fl := range flows {
		if at.fn.Name.Name == fl.fn && field(status, fl.value, fl.statusField) && field(code, fl.value, fl.codeField) {
			return i
		}
	}
	return -1
}

// addFlowPairs adds, at site, the status and code of every fl.typeName
// literal in fl.dir.
func addFlowPairs(scan *errorPairScan, fl fieldFlow, site string) error {
	pkg, err := loadSourcePackage(fl.dir)
	if err != nil {
		return err
	}
	literals := 0
	pkg.eachLiteral(fl.typeName, func(lit *ast.CompositeLit, at place) {
		literals++
		litSite := site + ", from " + pkg.site(lit, at)
		status, code := keyedField(lit, fl.statusField), keyedField(lit, fl.codeField)
		if status == nil || code == nil {
			scan.unresolved = append(scan.unresolved, litSite+": the literal sets no "+fl.statusField+" or no "+fl.codeField)
			return
		}
		pkg.send(scan, litSite, at, func(f *frame) ([]errorPair, error) {
			statuses, err := pkg.eval(status, f, 0)
			if err != nil {
				return nil, err
			}
			codes, err := pkg.eval(code, f, 0)
			if err != nil {
				return nil, err
			}
			return pairsOf(statuses, codes)
		})
	})
	if literals == 0 {
		scan.unresolved = append(scan.unresolved, fmt.Sprintf("%s: no %s literal in %s", site, fl.typeName, fl.dir))
	}
	return nil
}

// pairsOf pairs the statuses and codes one write can take. An empty code is
// dropped (a caller writes only past a check for one), but a write whose code
// is only ever empty is refused, and so is one whose status and code both vary,
// which this guard cannot pair.
func pairsOf(statuses, codes []constant.Value) ([]errorPair, error) {
	var ns []int
	for _, v := range statuses {
		if v.Kind() != constant.Int {
			return nil, fmt.Errorf("status %s is not an HTTP status", v)
		}
		n, ok := constant.Int64Val(v)
		if !ok || n < 100 || n > 599 {
			return nil, fmt.Errorf("status %s is not an HTTP status", v)
		}
		if !slices.Contains(ns, int(n)) {
			ns = append(ns, int(n))
		}
	}
	var cs []string
	for _, v := range codes {
		if v.Kind() != constant.String {
			return nil, fmt.Errorf("code %s is not a string", v)
		}
		if c := constant.StringVal(v); c != "" && !slices.Contains(cs, c) {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		return nil, errors.New("its code is always empty")
	}
	if len(ns) > 1 && len(cs) > 1 {
		return nil, errors.New("its status and its code each take several values, which the guard cannot pair")
	}
	var out []errorPair
	for _, n := range ns {
		for _, c := range cs {
			out = append(out, errorPair{status: n, code: c})
		}
	}
	return out, nil
}

// sourcePackage is one directory's non-test Go source, parsed, with the
// declarations a scan looks names up in.
type sourcePackage struct {
	fset       *token.FileSet
	files      []*ast.File
	fileOf     map[*ast.FuncDecl]*ast.File
	consts     map[string]constDecl
	funcs      map[string][]*ast.FuncDecl
	errorField map[string]string // a struct type's field tagged json:"error"
	callers    map[*ast.FuncDecl][]callSite
}

// constDecl is a package-level constant's value and the file it is in.
type constDecl struct {
	value ast.Expr
	file  *ast.File
}

// place is where an expression sits: its file, its function (nil at package
// level), and the parameters of the function literals around it, which hide
// names of the function.
type place struct {
	file   *ast.File
	fn     *ast.FuncDecl
	shadow map[string]bool
}

// callSite is one call and where it is made.
type callSite struct {
	call *ast.CallExpr
	at   place
}

// frame is the function an expression is read in, with its parameters bound
// to one call's arguments, which are read in from. A frame with no args is
// the outermost one, whose callers a read of a parameter has to enumerate.
type frame struct {
	at   place
	args []ast.Expr
	from *frame
}

// needCallers is the error a read of an unbound parameter returns.
type needCallers struct{ fn *ast.FuncDecl }

func (e needCallers) Error() string { return "reads a parameter of " + e.fn.Name.Name }

const (
	maxCallChain = 4  // calls a read follows outward from a write
	maxEvalDepth = 64 // nested reads in one evaluation
)

// loadSourcePackage parses dir's non-test Go files. A name the go tool ignores
// (a leading "." or "_", which an editor's lock file has) is skipped before it
// is opened.
func loadSourcePackage(dir string) (*sourcePackage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pkg := &sourcePackage{
		fset:       token.NewFileSet(),
		fileOf:     map[*ast.FuncDecl]*ast.File{},
		consts:     map[string]constDecl{},
		funcs:      map[string][]*ast.FuncDecl{},
		errorField: map[string]string{},
		callers:    map[*ast.FuncDecl][]callSite{},
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(pkg.fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		pkg.files = append(pkg.files, f)
		pkg.index(f)
	}
	if len(pkg.files) == 0 {
		return nil, fmt.Errorf("no Go source in %s", dir)
	}
	pkg.eachCall(func(call *ast.CallExpr, at place) {
		if callee := pkg.calleeOf(call, at); callee != nil {
			pkg.callers[callee] = append(pkg.callers[callee], callSite{call: call, at: at})
		}
	})
	return pkg, nil
}

// index records f's package-level constants, functions and error body types.
func (p *sourcePackage) index(f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			p.funcs[d.Name.Name] = append(p.funcs[d.Name.Name], d)
			p.fileOf[d] = f
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					if d.Tok != token.CONST || len(s.Values) != len(s.Names) {
						continue
					}
					for i, n := range s.Names {
						p.consts[n.Name] = constDecl{value: s.Values[i], file: f}
					}
				case *ast.TypeSpec:
					if st, ok := s.Type.(*ast.StructType); ok {
						if field := jsonErrorField(st); field != "" {
							p.errorField[s.Name.Name] = field
						}
					}
				}
			}
		}
	}
}

// jsonErrorField names the field of st tagged json:"error", or "".
func jsonErrorField(st *ast.StructType) string {
	for _, fld := range st.Fields.List {
		if fld.Tag == nil || len(fld.Names) != 1 {
			continue
		}
		tag, err := strconv.Unquote(fld.Tag.Value)
		if err != nil {
			continue
		}
		if name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ","); name == "error" {
			return fld.Names[0].Name
		}
	}
	return ""
}

// eachCall visits every call in the package with where it is made.
func (p *sourcePackage) eachCall(visit func(*ast.CallExpr, place)) {
	p.walk(func(n ast.Node, at place) {
		if call, ok := n.(*ast.CallExpr); ok {
			visit(call, at)
		}
	})
}

// eachLiteral visits every composite literal of the named type.
func (p *sourcePackage) eachLiteral(typeName string, visit func(*ast.CompositeLit, place)) {
	p.walk(func(n ast.Node, at place) {
		if lit, ok := n.(*ast.CompositeLit); ok {
			if id, ok := lit.Type.(*ast.Ident); ok && id.Name == typeName {
				visit(lit, at)
			}
		}
	})
}

// walk visits every node of the package's declarations with its place.
func (p *sourcePackage) walk(visit func(ast.Node, place)) {
	for _, f := range p.files {
		for _, decl := range f.Decls {
			at := place{file: f}
			var root ast.Node = decl
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if fd.Body == nil {
					continue
				}
				at.fn, root = fd, fd.Body
			}
			walkNode(root, at, visit)
		}
	}
}

func walkNode(root ast.Node, at place, visit func(ast.Node, place)) {
	ast.Inspect(root, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			if n != nil {
				visit(n, at)
			}
			return true
		}
		inner := at
		inner.shadow = maps.Clone(at.shadow)
		if inner.shadow == nil {
			inner.shadow = map[string]bool{}
		}
		for _, fields := range []*ast.FieldList{lit.Type.Params, lit.Type.Results} {
			for _, name := range fieldNames(fields) {
				inner.shadow[name] = true
			}
		}
		walkNode(lit.Body, inner, visit)
		return false
	})
}

// fieldNames lists a parameter or result list's names, "" for an unnamed one.
func fieldNames(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, fld := range fl.List {
		if len(fld.Names) == 0 {
			out = append(out, "")
		}
		for _, n := range fld.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// site names a node's place for a message: its file and line, and its function.
func (p *sourcePackage) site(n ast.Node, at place) string {
	pos := p.fset.Position(n.Pos())
	fn := "package level"
	if at.fn != nil {
		fn = at.fn.Name.Name
	}
	return fmt.Sprintf("%s:%d (%s)", filepath.Base(pos.Filename), pos.Line, fn)
}

// send records the pairs read returns at site. A read that needs a parameter
// of the outermost function is tried again at each call to that function,
// with the chain of calls in the site's name.
func (p *sourcePackage) send(scan *errorPairScan, site string, at place, read func(*frame) ([]errorPair, error)) {
	queue := [][]callSite{nil}
	for len(queue) > 0 {
		chain := queue[0]
		queue = queue[1:]
		label := site
		for _, c := range chain {
			label += " via " + p.site(c.call, c.at)
		}
		pairs, err := read(chainFrame(at, chain))
		var need needCallers
		switch {
		case errors.As(err, &need):
			callers := p.callers[need.fn]
			switch {
			case len(chain) >= maxCallChain:
				scan.unresolved = append(scan.unresolved, label+": "+err.Error()+", more than "+strconv.Itoa(maxCallChain)+" calls out")
			case len(callers) == 0:
				scan.unresolved = append(scan.unresolved, label+": "+err.Error()+", and the guard finds no call to it to follow")
			}
			if len(chain) < maxCallChain {
				for _, c := range callers {
					queue = append(queue, append(slices.Clip(chain), c))
				}
			}
		case err != nil:
			scan.unresolved = append(scan.unresolved, label+": "+err.Error())
		default:
			scan.add(pairs, label)
		}
	}
}

// chainFrame builds the frame a write at at is read in when its function is
// called through chain: chain[0] calls at's function, chain[1] calls the
// function chain[0] is in, and so on out. The function chain's last call is in
// is the outermost, and unbound.
func chainFrame(at place, chain []callSite) *frame {
	if len(chain) == 0 {
		return &frame{at: at}
	}
	outer := &frame{at: chain[len(chain)-1].at}
	for i := len(chain) - 1; i >= 1; i-- {
		outer = &frame{at: chain[i-1].at, args: chain[i].call.Args, from: outer}
	}
	return &frame{at: at, args: chain[0].call.Args, from: outer}
}

// eval returns every constant value e can hold in frame f.
func (p *sourcePackage) eval(e ast.Expr, f *frame, depth int) ([]constant.Value, error) {
	if depth > maxEvalDepth {
		return nil, fmt.Errorf("%s is nested too deeply to read", exprString(e))
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(e.Value, e.Kind, 0)
		if v.Kind() == constant.Unknown {
			return nil, fmt.Errorf("cannot read the literal %s", e.Value)
		}
		return []constant.Value{v}, nil
	case *ast.ParenExpr:
		return p.eval(e.X, f, depth+1)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			break
		}
		xs, err := p.eval(e.X, f, depth+1)
		if err != nil {
			return nil, err
		}
		ys, err := p.eval(e.Y, f, depth+1)
		if err != nil {
			return nil, err
		}
		var out []constant.Value
		for _, x := range xs {
			for _, y := range ys {
				if x.Kind() != y.Kind() {
					return nil, fmt.Errorf("%s adds a %s to a %s", exprString(e), x.Kind(), y.Kind())
				}
				out = append(out, constant.BinaryOp(x, token.ADD, y))
			}
		}
		return out, nil
	case *ast.Ident:
		return p.evalIdent(e, f, depth)
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok && importPath(f.at.file, pkg.Name) == "net/http" {
			statuses, err := netHTTPStatuses()
			if err != nil {
				return nil, err
			}
			if v, ok := statuses[e.Sel.Name]; ok {
				return []constant.Value{v}, nil
			}
		}
	}
	return nil, fmt.Errorf("%s is not a constant the guard can read", exprString(e))
}

// evalIdent reads a name: a parameter of f's function, a local of it, or a
// package-level constant.
func (p *sourcePackage) evalIdent(id *ast.Ident, f *frame, depth int) ([]constant.Value, error) {
	name := id.Name
	if f.at.shadow[name] {
		return nil, fmt.Errorf("%s is a parameter of a function literal", name)
	}
	if fn := f.at.fn; fn != nil {
		i, variadic, isParam := paramIndex(fn, name)
		defs, declared, err := localDefs(fn, name)
		if err != nil {
			return nil, err
		}
		if !isParam && !declared && len(defs) > 0 {
			return nil, fmt.Errorf("%s is a package variable, which any code may change", name)
		}
		if isParam || declared {
			// A parameter is its argument, and whatever the body sets it to.
			var out []constant.Value
			if isParam {
				switch {
				case variadic:
					return nil, fmt.Errorf("%s is a variadic parameter", name)
				case f.args == nil:
					return nil, needCallers{fn: fn}
				case i >= len(f.args):
					return nil, fmt.Errorf("a call passes no argument for %s", name)
				}
				vs, err := p.eval(f.args[i], f.from, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, vs...)
			}
			for _, d := range defs {
				var vs []constant.Value
				if d.call != nil {
					vs, err = p.evalResult(d.call, d.index, f, depth+1)
				} else {
					vs, err = p.eval(d.expr, f, depth+1)
				}
				if err != nil {
					return nil, err
				}
				out = append(out, vs...)
			}
			if len(out) == 0 {
				return nil, fmt.Errorf("%s is never set to a value the guard can read", name)
			}
			return out, nil
		}
	}
	if c, ok := p.consts[name]; ok {
		return p.eval(c.value, &frame{at: place{file: c.file}}, depth+1)
	}
	return nil, fmt.Errorf("%s is not a constant, a parameter or a local the guard can read", name)
}

// localDef is one value a local is set to: an expression, or one result of a
// call.
type localDef struct {
	expr  ast.Expr
	call  *ast.CallExpr
	index int
}

// localDefs lists every value fn's body sets name to, and whether the body
// declares it. A name the body changes any other way (an operator assignment,
// a range, a type switch, ++) is an error; a declaration without a value sets
// nothing. A function literal that takes the name as a parameter is another
// variable and is not read.
func localDefs(fn *ast.FuncDecl, name string) ([]localDef, bool, error) {
	var defs []localDef
	var err error
	declared := false
	set := func(lhs []ast.Expr, rhs []ast.Expr, i int, tok token.Token) {
		if tok == token.DEFINE || tok == token.CONST || tok == token.VAR {
			declared = true
		}
		switch {
		case tok != token.DEFINE && tok != token.ASSIGN && tok != token.CONST && tok != token.VAR:
			err = fmt.Errorf("%s is changed with %s", name, tok)
		case len(rhs) == len(lhs):
			defs = append(defs, localDef{expr: rhs[i]})
		case len(rhs) == 1:
			if call, ok := rhs[0].(*ast.CallExpr); ok {
				defs = append(defs, localDef{call: call, index: i})
				return
			}
			err = fmt.Errorf("%s is set from %s", name, exprString(rhs[0]))
		case len(rhs) != 0:
			err = fmt.Errorf("%s is set by an assignment the guard cannot read", name)
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if err != nil {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			for _, fields := range []*ast.FieldList{n.Type.Params, n.Type.Results} {
				if slices.Contains(fieldNames(fields), name) {
					return false
				}
			}
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					set(n.Lhs, n.Rhs, i, n.Tok)
				}
			}
		case *ast.GenDecl:
			for _, spec := range n.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if id.Name != name {
						continue
					}
					if n.Tok == token.CONST && len(vs.Values) == 0 {
						err = fmt.Errorf("%s repeats an earlier constant's expression", name)
						return false
					}
					lhs := make([]ast.Expr, len(vs.Names))
					set(lhs, vs.Values, i, n.Tok)
				}
			}
		case *ast.RangeStmt:
			for _, x := range []ast.Expr{n.Key, n.Value} {
				if id, ok := x.(*ast.Ident); ok && id.Name == name {
					err = fmt.Errorf("%s is set by a range", name)
				}
			}
		case *ast.IncDecStmt:
			if id, ok := n.X.(*ast.Ident); ok && id.Name == name {
				err = fmt.Errorf("%s is changed with %s", name, n.Tok)
			}
		case *ast.TypeSwitchStmt:
			if a, ok := n.Assign.(*ast.AssignStmt); ok {
				if id, ok := a.Lhs[0].(*ast.Ident); ok && id.Name == name {
					err = fmt.Errorf("%s is set by a type switch", name)
				}
			}
		}
		return err == nil
	})
	return defs, declared, err
}

// evalResult reads result index of call: what it is at each return of the
// function called, with that function's parameters bound to call's arguments.
func (p *sourcePackage) evalResult(call *ast.CallExpr, index int, f *frame, depth int) ([]constant.Value, error) {
	callee := p.calleeOf(call, f.at)
	if callee == nil {
		return nil, fmt.Errorf("its value comes from %s, a call the guard cannot follow", exprString(call.Fun))
	}
	results := len(fieldNames(callee.Type.Results))
	inner := &frame{at: place{file: p.fileOf[callee], fn: callee}, args: call.Args, from: f}
	var out []constant.Value
	var err error
	returns := 0
	ast.Inspect(callee.Body, func(n ast.Node) bool {
		if err != nil {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returns++
			if len(n.Results) != results || index >= results {
				err = fmt.Errorf("%s has a return the guard cannot read", callee.Name.Name)
				return false
			}
			var vs []constant.Value
			vs, err = p.eval(n.Results[index], inner, depth+1)
			out = append(out, vs...)
		}
		return err == nil
	})
	if err == nil && returns == 0 {
		err = fmt.Errorf("%s has no return to read", callee.Name.Name)
	}
	return out, err
}

// calleeOf is the function a call calls, when the source says which: a
// package function called by its name, or a method called on the calling
// method's own receiver. Any other call is nil.
func (p *sourcePackage) calleeOf(call *ast.CallExpr, at place) *ast.FuncDecl {
	var name, recvType string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		if !ok || at.fn == nil || at.shadow[x.Name] || receiverName(at.fn) != x.Name {
			return nil
		}
		name, recvType = fun.Sel.Name, receiverType(at.fn)
	default:
		return nil
	}
	var found *ast.FuncDecl
	for _, fd := range p.funcs[name] {
		if receiverType(fd) != recvType {
			continue
		}
		if found != nil {
			return nil
		}
		found = fd
	}
	if found == nil || recvType != "" {
		return found
	}
	// A function called by its name: unless a parameter or a local of the
	// calling function takes that name.
	if at.shadow[name] {
		return nil
	}
	if at.fn != nil {
		if _, _, ok := paramIndex(at.fn, name); ok {
			return nil
		}
		if _, declared, err := localDefs(at.fn, name); err != nil || declared {
			return nil
		}
	}
	return found
}

// paramIndex is the position of fn's parameter name, and whether it is the
// variadic one.
func paramIndex(fn *ast.FuncDecl, name string) (int, bool, bool) {
	i := 0
	for _, fld := range fn.Type.Params.List {
		_, variadic := fld.Type.(*ast.Ellipsis)
		if len(fld.Names) == 0 {
			i++
			continue
		}
		for _, n := range fld.Names {
			if n.Name == name {
				return i, variadic, true
			}
			i++
		}
	}
	return 0, false, false
}

// receiverName is the name a method calls its receiver, or "".
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// receiverType is the type a method is declared on, or "" for a function.
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// errorBodyCode is the expression a JSON body sets its error code to, and the
// frame to read it in, when the body is a literal (or the address of one) of
// an error body type, or a parameter a call passes one in; otherwise nil.
func (p *sourcePackage) errorBodyCode(body ast.Expr, f *frame) (ast.Expr, *frame, error) {
	for {
		if u, ok := body.(*ast.UnaryExpr); ok && u.Op == token.AND {
			body = u.X
		}
		id, ok := body.(*ast.Ident)
		if !ok || f.at.fn == nil || f.at.shadow[id.Name] {
			break
		}
		i, variadic, isParam := paramIndex(f.at.fn, id.Name)
		if !isParam || variadic {
			break
		}
		if f.args == nil {
			return nil, nil, needCallers{fn: f.at.fn}
		}
		if i >= len(f.args) {
			return nil, nil, fmt.Errorf("a call passes no argument for %s", id.Name)
		}
		body, f = f.args[i], f.from
	}
	lit, ok := body.(*ast.CompositeLit)
	if !ok {
		return nil, nil, nil
	}
	id, ok := lit.Type.(*ast.Ident)
	if !ok || p.errorField[id.Name] == "" {
		return nil, nil, nil
	}
	if code := keyedField(lit, p.errorField[id.Name]); code != nil {
		return code, f, nil
	}
	return nil, nil, fmt.Errorf("its %s literal sets no %s, so it sends no code", id.Name, p.errorField[id.Name])
}

// keyedField is the value a literal gives the named field, or nil.
func keyedField(lit *ast.CompositeLit, field string) ast.Expr {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
				return kv.Value
			}
		}
	}
	return nil
}

// importPath is the path a file imports under name, or "".
func importPath(f *ast.File, name string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := path.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == name {
			return p
		}
	}
	return ""
}

// netHTTPStatuses reads net/http's Status constants from the toolchain's own
// source, once, so a status is the value the handler compiles against.
var netHTTPStatuses = sync.OnceValues(func() (map[string]constant.Value, error) {
	src, err := build.Default.Import("net/http", "", build.FindOnly)
	if err != nil {
		return nil, fmt.Errorf("find net/http's source: %w", err)
	}
	entries, err := os.ReadDir(src.Dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	out := map[string]constant.Value{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(src.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse net/http: %w", err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "Status") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
						out[n.Name] = constant.MakeFromLiteral(lit.Value, lit.Kind, 0)
					}
				}
			}
		}
	}
	if out["StatusNotFound"] == nil || len(out) < 40 {
		return nil, fmt.Errorf("read %d Status constants from %s, without StatusNotFound", len(out), src.Dir)
	}
	return out, nil
})

// exprString prints an expression for a message.
func exprString(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return "an expression"
	}
	return b.String()
}

// TestErrorPairScanReadsEveryShapeOnAFixture pins what the scan reads, over a
// synthetic package: on a clean tree the two guards above find nothing wrong,
// so the tree cannot show the scan still reads each shape it must, or still
// refuses each it cannot. Pairs are exact: the two calls of a parameter-bound
// writer stay two pairs, never crossed. A test file, a name the go tool
// ignores and an editor's lock file are never read.
func TestErrorPairScanReadsEveryShapeOnAFixture(t *testing.T) {
	root := t.TempDir()
	apiDir, proxyDir := filepath.Join(root, "api"), filepath.Join(root, "proxy")
	files := map[string]string{
		filepath.Join(apiDir, "handlers.go"):      fixtureHandlers,
		filepath.Join(apiDir, "handlers_test.go"): fixtureWriteIn("from_a_test"),
		filepath.Join(apiDir, "_ignored.go"):      fixtureWriteIn("from_an_ignored_file"),
		filepath.Join(apiDir, ".#handlers.go"):    "user@host.1234:5678", // not Go: read, it fails the parse
		filepath.Join(proxyDir, "proxy.go"):       fixtureProxy,
	}
	for name, src := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	flow := fieldFlow{fn: "proxied", value: "perr", dir: proxyDir, typeName: "streamError",
		statusField: "Status", codeField: "Code"}
	stale := flow
	stale.fn = "gone"

	scan, err := scanErrorPairs(apiDir, []fieldFlow{flow, stale})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p := range scan.pairs {
		got = append(got, p.String())
	}
	sort.Strings(got)
	want := []string{
		"400 bad_input",      // a local set from a call's result
		"400 fallback",       // a parameter the body sets
		"400 given",          // and the argument its caller passes
		"404 literal",        // a literal and a net/http status
		"404 spec_not_found", // a parameter, at its first caller
		"404 wave_not_found", // and at its second
		"409 first",          // a status and a code, both parameters, at one call
		"409 in_a_body",      // a struct literal with a json:"error" field
		"409 through_a_parameter",
		"410 from_const_gone",
		"429 second", // and at the other call: not crossed with the first
		"502 upstream_gone",
		"503 from_const",
		"503 offline", // the declared flow, read from the other package's literals
		"507 in_a_pointer",
	}
	if !slices.Equal(got, want) {
		t.Errorf("pairs:\n got %v\nwant %v", got, want)
	}

	refused := []string{"(runtimeCode)", "(unreadBody)", "(closure)", "(uncalled)", "(bothVary)",
		"(viaVar)", "(setsVar)", "in gone"}
	for _, marker := range refused {
		n := 0
		for _, u := range scan.unresolved {
			if strings.Contains(u, marker) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("want one unresolved write matching %q, got %d", marker, n)
		}
	}
	if len(scan.unresolved) != len(refused) {
		t.Errorf("unresolved = %d, want %d:\n  %s", len(scan.unresolved), len(refused),
			strings.Join(scan.unresolved, "\n  "))
	}
}

// fixtureHandlers is TestErrorPairScanReadsEveryShapeOnAFixture's package:
// the shapes the scan reads, then the shapes it must refuse. It is parsed,
// never compiled.
const fixtureHandlers = `package fixture

import (
	"net/http"
	"strings"
)

const codeFromConst = "from_const"

func writeError(w any, status int, code, message string)          { writeJSON(w, status, envelope{Error: code}) }
func writeErrorLog(w, r any, status int, code, msg string, err error) { writeError(w, status, code, msg) }
func writeJSON(w any, status int, v any)                            {}

type envelope struct {
	Error string ` + "`json:\"error\"`" + `
}

type conflictBody struct {
	Error  string ` + "`json:\"error\"`" + `
	Detail string ` + "`json:\"detail,omitempty\"`" + `
}

type okBody struct {
	N int ` + "`json:\"n\"`" + `
}

type server struct{}

func literal()  { writeError(nil, http.StatusNotFound, "literal", "m") }
func viaConst() { writeError(nil, http.StatusServiceUnavailable, codeFromConst, "m") }
func concat()   { writeError(nil, http.StatusGone, codeFromConst+"_gone", "m") }

func (s *server) lookup(kind string) { writeError(nil, http.StatusNotFound, kind+"_not_found", "m") }
func (s *server) wave()              { s.lookup("wave") }
func (s *server) spec()              { s.lookup("spec") }

func classify(bad bool) (string, string) {
	if bad {
		return "bad_input", "m"
	}
	return "", ""
}

func viaReturn() {
	if code, msg := classify(true); code != "" {
		writeError(nil, http.StatusBadRequest, code, msg)
	}
}

func statusAndCode(status int, code string) { writeErrorLog(nil, nil, status, code, "m", nil) }

func twoCallers() {
	statusAndCode(http.StatusConflict, "first")
	statusAndCode(http.StatusTooManyRequests, "second")
}

func body()               { writeJSON(nil, http.StatusConflict, conflictBody{Error: "in_a_body", Detail: "d"}) }
func bodyPointer()        { writeJSON(nil, http.StatusInsufficientStorage, &conflictBody{Error: "in_a_pointer"}) }
func success()            { writeJSON(nil, http.StatusOK, okBody{N: 1}) }
func successValue(v okBody) { writeJSON(nil, http.StatusOK, v) }

func proxied() {
	perr := fetch()
	writeError(nil, perr.Status, perr.Code, "m")
}

func reassigned(code string) {
	if code == "" {
		code = "fallback"
	}
	writeError(nil, http.StatusBadRequest, code, "m")
}

func callsReassigned() { reassigned("given") }

func respond(status int, v any) { writeJSON(nil, status, v) }
func conflict()                 { respond(http.StatusConflict, conflictBody{Error: "through_a_parameter"}) }
func fine()                     { respond(http.StatusOK, okBody{N: 2}) }
func rowData(r rowBody)         { writeJSON(nil, http.StatusOK, rowBody{Error: r.Error}) }

type rowBody struct {
	Error string ` + "`json:\"error,omitempty\"`" + `
}

var codeVar = "from_a_var"

func viaVar()  { writeError(nil, http.StatusBadRequest, codeVar, "m") }
func setsVar() { codeVar = "set"; writeError(nil, http.StatusBadRequest, codeVar, "m") }

func runtimeCode(code string)   { writeError(nil, http.StatusBadRequest, strings.ToLower(code), "m") }
func unreadBody(v conflictBody) { writeJSON(nil, http.StatusConflict, v) }

func closure() {
	f := func(code string) { writeError(nil, http.StatusBadRequest, code, "m") }
	f("x")
}

func uncalled(code string) { writeError(nil, http.StatusBadRequest, code, "m") }

func pick(bad bool) (int, string) {
	if bad {
		return http.StatusBadRequest, "bad"
	}
	return http.StatusConflict, "conflict"
}

func bothVary() {
	status, code := pick(true)
	writeError(nil, status, code, "m")
}
`

// fixtureProxy is the other package of the fixture's declared flow.
const fixtureProxy = `package proxy

import "net/http"

type streamError struct {
	Status int
	Code   string
}

func open() *streamError { return &streamError{Status: http.StatusBadGateway, Code: "upstream_gone"} }
func busy() *streamError { return &streamError{Status: http.StatusServiceUnavailable, Code: "offline"} }
`

// fixtureWriteIn is a fixture file that writes code, for a file the scan
// must not read.
func fixtureWriteIn(code string) string {
	return "package fixture\n\nimport \"net/http\"\n\nfunc extra() { writeError(nil, http.StatusTeapot, \"" + code + "\", \"m\") }\n"
}

// TestParseErrorTableReadsRowsAndRefusesWhatItCannotPair pins the table's
// reading: a reserved row, a pipe inside the When cell, CRLF line ends, and
// the shapes it refuses rather than guess at.
func TestParseErrorTableReadsRowsAndRefusesWhatItCannotPair(t *testing.T) {
	const table = "## Error responses\n\nErrors are JSON.\n\n" +
		"| Status | `error` code | When |\n|-------:|---|---|\n" +
		"| 404 | `not_found` | a path, or `a|b` |\n" +
		"| 403 | `forbidden` | insufficient scope " + reservedRowMarker + " |\n" +
		"\nAfter the table.\n\n| 418 | `ignored` | a later table |\n\n## Next\n"
	want := []protocolErrorRow{
		{pair: errorPair{404, "not_found"}},
		{pair: errorPair{403, "forbidden"}, reserved: true},
	}
	for name, spec := range map[string]string{"LF": table, "CRLF": strings.ReplaceAll(table, "\n", "\r\n")} {
		rows, err := parseErrorTable(spec)
		if err != nil || !slices.Equal(rows, want) {
			t.Errorf("%s: rows = %v, err = %v; want %v", name, rows, err, want)
		}
	}

	const head = "## Error responses\n| Status | `error` code | When |\n|---|---|---|\n"
	for name, spec := range map[string]string{
		"no heading":          "## Errors\n| Status | `error` code | When |\n|---|---|---|\n| 404 | `a` | x |\n",
		"another table first": "## Error responses\n| Code | Meaning |\n|---|---|\n",
		"no alignment row":    "## Error responses\n| Status | `error` code | When |\n| 404 | `a` | x |\n",
		"two codes in a cell": head + "| 400 | `a` / `b` | x |\n",
		"a bare code":         head + "| 400 | a | x |\n",
		"not a status":        head + "| 4xx | `a` | x |\n",
		"a second row":        head + "| 400 | `a` | x |\n| 400 | `a` | y |\n",
		"no table":            "## Error responses\nProse only.\n## Next\n",
	} {
		if rows, err := parseErrorTable(spec); err == nil {
			t.Errorf("%s: read %v, want an error", name, rows)
		}
	}
}
