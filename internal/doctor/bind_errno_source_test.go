package doctor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// TestTheWindowsBindChecksNameTheWinsockErrnos pins, on every host, the
// Windows arm of serve's bind checks (backlog B660): IsAddrInUse must ask
// windows.WSAEADDRINUSE, IsBindPermission windows.WSAEACCES and
// IsAddrNotAvail windows.WSAEADDRNOTAVAIL (B660's second round), since a
// bare syscall errno is never what a Winsock bind answers, and a start
// refused by a port another process holds, or by an address the host does
// not have, would then count as a failed start of its install. The
// behaviour is run on Windows' CI leg (TestIsAddrInUseMatchesRealBindConflict,
// TestIsBindPermissionMatchesWhatTheHostAnswersARefusedBind,
// TestIsAddrNotAvailMatchesWhatTheHostAnswersAnAbsentAddress); this reads the
// source, which every host can.
func TestTheWindowsBindChecksNameTheWinsockErrnos(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0): no source position")
	}
	path := filepath.Join(filepath.Dir(thisFile), "doctor_windows.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"IsAddrInUse": "WSAEADDRINUSE", "IsBindPermission": "WSAEACCES", "IsAddrNotAvail": "WSAEADDRNOTAVAIL"}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || want[fn.Name.Name] == "" {
			continue
		}
		var named []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "windows" {
					named = append(named, sel.Sel.Name)
				}
			}
			return true
		})
		if !slices.Contains(named, want[fn.Name.Name]) {
			t.Errorf("%s in doctor_windows.go asks %v of x/sys/windows, want windows.%s among them", fn.Name.Name, named, want[fn.Name.Name])
		}
		delete(want, fn.Name.Name)
	}
	for name := range want {
		t.Errorf("doctor_windows.go declares no %s", name)
	}
}
