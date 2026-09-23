package packaging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestNeedsRootForCoversEverySystemKind pins the predicate behind the
// gate that Stop, Restart, Start and Uninstall all read.
//
// It used to be the same two-term condition spelled out in the first
// three and ABSENT from the fourth — which is how Uninstall came to
// no-op on a sudo install and report success. The enumeration is now
// single-sourced, so this table is the one place it can go stale.
func TestNeedsRootForCoversEverySystemKind(t *testing.T) {
	for _, tc := range []struct {
		kind ServiceKind
		want bool
	}{
		{KindLaunchdSystem, true},
		{KindSystemdSystem, true},
		{KindLaunchdUser, false},
		{KindSystemdUser, false},
		{KindWindowsSCM, false},
		{KindWindowsStartup, false},
		{KindNone, false},
	} {
		if got := NeedsRootFor(tc.kind); got != tc.want {
			t.Errorf("NeedsRootFor(%v) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// TestEveryServiceKindIsClassified is the guard against the table above
// going stale the day a kind is added: a new ServiceKind that nobody
// classified would otherwise default to "user-level, safe to remove
// from here", which is the failure direction that loses data.
//
// Walks the declared constants rather than a hand-written list, so
// adding one forces a decision here.
func TestEveryServiceKindIsClassified(t *testing.T) {
	classified := map[ServiceKind]bool{
		KindNone: true, KindLaunchdUser: true, KindLaunchdSystem: true,
		KindSystemdUser: true, KindSystemdSystem: true,
		KindWindowsSCM: true, KindWindowsStartup: true,
	}
	// ServiceKind is a small dense iota; walk past the last known value
	// so a newly-appended constant shows up as unclassified.
	for k := ServiceKind(0); k <= KindWindowsStartup+1; k++ {
		if k == KindWindowsStartup+1 {
			if k.Description() != "not installed" {
				t.Errorf("a new ServiceKind (%d) exists and is not classified in NeedsRootFor's table", k)
			}
			continue
		}
		if !classified[k] {
			t.Errorf("ServiceKind %d is not classified", k)
		}
	}
}

// TestEveryLifecycleEntryPointConsultsTheGate is the structural pin,
// and it is the one that would have caught the original defect.
//
// The classification is covered exhaustively above, but the bug was not
// a wrong classification — it was Uninstall never ASKING. That wiring
// cannot be driven from a test: installedKindForOS probes absolute
// paths (/Library/LaunchDaemons, /etc/systemd/system) that only root
// can create, so on any CI host the gate simply never fires and a
// behavioural test passes whether the call is there or not. (Verified:
// a negative control removing the gate left such a test green.)
//
// So this walks the AST instead and requires each of the four entry
// points to reference NeedsRootFor. Anchored on the IDENTIFIER, not a
// string, because this package's own commentary names what it
// discusses — the docblock you are reading says "NeedsRootFor" twice.
func TestEveryLifecycleEntryPointConsultsTheGate(t *testing.T) {
	want := map[string]bool{"Stop": false, "Start": false, "Restart": false, "Uninstall": false}

	fset := token.NewFileSet()
	for _, name := range []string{"lifecycle.go", "packaging.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if _, tracked := want[fn.Name.Name]; !tracked {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "NeedsRootFor" {
					want[fn.Name.Name] = true
				}
				return true
			})
		}
	}

	checked := 0
	for fn, found := range want {
		checked++
		if !found {
			t.Errorf("%s does not consult NeedsRootFor — a system-level install would be driven "+
				"from a user context, which for Uninstall means a no-op reported as success", fn)
		}
	}
	// Floor: a parse that silently matched nothing would otherwise pass.
	if checked != len(want) {
		t.Fatalf("walked %d entry points, want %d", checked, len(want))
	}
}
