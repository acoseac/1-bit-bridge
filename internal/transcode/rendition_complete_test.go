package transcode

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestACutStreamIsTheOutputSidesUnlessTheVolumeSaysOtherwise pins
// classifyCut on what a volume can answer the probe that follows a stream
// cut short, on every platform: nothing (it takes the bytes: the fault went
// as the tool exited), a permission (fs.ErrPermission), or a cause that is
// no fault of the host, which keeps the job's strike. The errno causes are
// in rendition_complete_unix_test.go.
func TestACutStreamIsTheOutputSidesUnlessTheVolumeSaysOtherwise(t *testing.T) {
	cut := errors.New("cut short")
	denied := &os.PathError{Op: "write", Path: "x.tmp", Err: os.ErrPermission}
	for _, tc := range []struct {
		name   string
		probe  error
		kind   outputFaultKind // 0: not the output side's
		reason string
	}{
		{name: "the volume takes the bytes", probe: nil, kind: outputFull, reason: reasonWriteFailedThenRoom},
		{name: "a permission", probe: denied, kind: outputDenied},
		{name: "a cause no table names", probe: errors.New("an answer no table names")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyCut(outputVariants, "/variants/Album", cut, tc.probe)
			if !errors.Is(err, cut) {
				t.Errorf("classifyCut = %v, want the cut wrapped", err)
			}
			checkOutputFault(t, err, tc.kind, tc.reason, outputVariants, "/variants/Album")
			if tc.probe != nil && !strings.Contains(err.Error(), tc.probe.Error()) {
				t.Errorf("%q does not say what the volume answered (%v)", err, tc.probe)
			}
		})
	}
}

// checkOutputFault asserts err is marked as the output side's fault of kind
// at where and dir, with reason when one is given, or is not marked when
// kind is 0.
func checkOutputFault(t *testing.T, err error, kind outputFaultKind, reason, where, dir string) {
	t.Helper()
	f, marked := unwritableOutput(err)
	if marked != (kind != 0) {
		t.Errorf("%v: marked = %v, want kind %v", err, marked, kind)
		return
	}
	if marked && (f.kind != kind || f.where != where || f.dir != dir) {
		t.Errorf("marked as %+v, want kind %v at %s %s", f, kind, where, dir)
	}
	if reason != "" && f.reason != reason {
		t.Errorf("reason %q, want %q", f.reason, reason)
	}
}

// TestAShortScratchIsTheOutputSidesOnlyWhenItsVolumeRefuses pins
// classifyScratch: a scratch volume that takes the bytes says the source
// decoded short, which strikes as it always did; one that refuses for a
// cause the host's table names is the scratch's outage.
func TestAShortScratchIsTheOutputSidesOnlyWhenItsVolumeRefuses(t *testing.T) {
	short := errors.New("decoded short")
	if err := classifyScratch("/tmp/1-bit-bridge-render", short, nil); err != short {
		t.Errorf("room: classifyScratch = %v, want the error as it was", err)
	}
	if err := classifyScratch("/tmp/1-bit-bridge-render", short, errors.New("an answer no table names")); err != short {
		t.Errorf("an unnamed refusal: classifyScratch = %v, want the error as it was", err)
	}
	err := classifyScratch("/tmp/1-bit-bridge-render", short, &os.PathError{Op: "write", Path: "x", Err: os.ErrPermission})
	if f, ok := unwritableOutput(err); !ok || f.where != outputScratch || f.kind != outputDenied || !errors.Is(err, short) {
		t.Errorf("a permission: classifyScratch = %v (%+v, %v), want the scratch's, the error wrapped", err, f, ok)
	}
}

// TestProbeTempDirRoomClosesBeforeItRemoves pins the two defers on the
// guard probe: Remove is registered first and Close second, so a panic
// or an early return closes the handle and then deletes the name.
// Windows cannot remove a file that is still open. An explicit Close
// stays on the error paths, and KeepOwner stays, because a root CLI
// otherwise leaves the probe owned by root.
func TestProbeTempDirRoomClosesBeforeItRemoves(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "rendition_complete.go", nil, 0)
	if err != nil {
		t.Fatalf("parse rendition_complete.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Name.Name == "probeTempDirRoom" && fd.Body != nil {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("probeTempDirRoom is not in rendition_complete.go")
	}
	var defers []string
	explicitClose := 0
	keepOwner := false
	for _, stmt := range fn.Body.List {
		if d, ok := stmt.(*ast.DeferStmt); ok {
			defers = append(defers, callSel(d.Call))
			continue
		}
		ast.Inspect(stmt, func(n ast.Node) bool {
			if _, ok := n.(*ast.DeferStmt); ok {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel := callSel(call)
			if sel == "Close" {
				explicitClose++
			}
			if sel == "KeepOwner" {
				keepOwner = true
			}
			return true
		})
	}
	if len(defers) < 2 || defers[0] != "Remove" || defers[1] != "Close" {
		t.Fatalf("probeTempDirRoom defers = %v, want Remove then Close (Close registered second runs first)", defers)
	}
	if !keepOwner {
		t.Error("probeTempDirRoom no longer calls KeepOwner")
	}
	if explicitClose == 0 {
		t.Error("probeTempDirRoom has no Close outside a defer; the write path has to return that error")
	}

	dir := t.TempDir()
	if err := probeTempDirRoom(dir); err != nil {
		t.Fatalf("probeTempDirRoom: %v", err)
	}
	left, err := filepath.Glob(filepath.Join(dir, ".guard-probe-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("probe left %v", left)
	}
}

func callSel(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return sel.Sel.Name
}
