package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"strings"
	"sync"
	"testing"
)

// fixedName returns a constant nameSource closure for tests.
func fixedName(s string) func() string { return func() string { return s } }

// TestMDNSLifecycleSetIsIdempotentOnRepeatedEnable: Set(true)
// twice in a row must NOT spawn a second advertiser (would
// double-register the Bonjour record + leak the first
// advertiser's goroutines).
func TestMDNSLifecycleSetIsIdempotentOnRepeatedEnable(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	m := newMDNSLifecycle(17788, 1, fixedName("Test Library"), stdout, stderr)
	defer m.Close()

	m.Set(true)
	first := m.advertiser
	m.Set(true) // second Set with the same state — should be a no-op
	if m.advertiser != first {
		t.Error("second Set(true) replaced the advertiser; want idempotent no-op")
	}
	m.Set(false)
	if m.advertiser != nil {
		t.Error("Set(false) didn't clear advertiser")
	}
	m.Set(false) // second Set(false) — no-op
	// (no further state to assert; reaching here without panic is the pin)
}

// TestMDNSLifecycleCloseIsSafeFromMultipleGoroutines: Close +
// Set racing must not deadlock or double-close.
func TestMDNSLifecycleCloseIsSafeFromMultipleGoroutines(t *testing.T) {
	m := newMDNSLifecycle(17789, 1, fixedName("Test"), &bytes.Buffer{}, &bytes.Buffer{})
	m.Set(true)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				m.Set(false)
			} else {
				m.Close()
			}
		}(i)
	}
	wg.Wait()
	if m.advertiser != nil {
		t.Error("advertiser should be nil after the race")
	}
}

// TestMDNSLifecycleSetReportsViaStdout pins the operator-facing
// log line so a panic-clicker in the admin UI sees the runtime
// state confirming their action landed.
func TestMDNSLifecycleSetReportsViaStdout(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	m := newMDNSLifecycle(17790, 1, fixedName("Logged Lib"), stdout, stderr)
	defer m.Close()
	m.Set(true)
	if !strings.Contains(stdout.String(), "advertising") {
		t.Errorf("Set(true) didn't log; stdout=%q", stdout.String())
	}
	m.Set(false)
	if !strings.Contains(stdout.String(), "stopped") {
		t.Errorf("Set(false) didn't log; stdout=%q", stdout.String())
	}
}

// TestLANInterfaceSourcePrintsAFailureOncePerStreak pins the mDNS
// InterfaceSource's printing. The advertiser asks it on every rebind tick,
// once a minute, so a picker that keeps failing (a host with no
// LAN-eligible interface) must print when the streak starts and when its
// error changes, never per call; a success ends the streak, and the next
// failure prints again.
func TestLANInterfaceSourcePrintsAFailureOncePerStreak(t *testing.T) {
	en0 := &net.Interface{Index: 14, Name: "en0"}
	var answer struct {
		iface *net.Interface
		err   error
	}
	stderr := &bytes.Buffer{}
	source := lanInterfaceSource(func() (*net.Interface, error) { return answer.iface, answer.err }, stderr)
	lines := func() int { return strings.Count(stderr.String(), "\n") }

	answer.err = errors.New("no LAN-eligible interface found")
	for range 5 {
		if got := source(); got != nil {
			t.Fatalf("a failed pick answered %v, want nil", got)
		}
	}
	if lines() != 1 {
		t.Fatalf("five failed ticks printed %d lines, want 1:\n%s", lines(), stderr)
	}
	if !strings.Contains(stderr.String(), "no LAN-eligible interface found") {
		t.Errorf("the line does not name the failure:\n%s", stderr)
	}

	answer.err = errors.New("net.Interfaces: operation not permitted")
	source()
	source()
	if lines() != 2 {
		t.Fatalf("a changed failure printed %d lines in all, want 2:\n%s", lines(), stderr)
	}

	answer.iface, answer.err = en0, nil
	if got := source(); got != en0 {
		t.Fatalf("a successful pick answered %v, want en0", got)
	}
	if lines() != 2 {
		t.Fatalf("a success printed a line:\n%s", stderr)
	}

	answer.iface, answer.err = nil, errors.New("no LAN-eligible interface found")
	source()
	if lines() != 3 {
		t.Fatalf("the failure after a success printed %d lines in all, want 3:\n%s", lines(), stderr)
	}
}

// TestMDNSInterfaceSourceIsTheOncePerStreakOne pins the one line the test
// above cannot see: every bridgemdns.Config literal in mdns_lifecycle.go
// builds its InterfaceSource with a lanInterfaceSource call. The
// advertiser asks the source on every rebind tick, so a closure that
// printed each failure itself (as the one here did until 2026-09-28, when
// it was asked only on a rebuild) would print a line a minute on a host
// with no LAN-eligible interface, while the lifecycle tests, run on hosts
// that have one, stay green.
//
// AST rather than a text scan: the commentary beside the literal names
// the function too.
func TestMDNSInterfaceSourceIsTheOncePerStreakOne(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mdns_lifecycle.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	literals := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isBridgeMDNSConfig(lit.Type) {
			return true
		}
		literals++
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "InterfaceSource" {
				continue
			}
			if !callsIdent(kv.Value, "lanInterfaceSource") {
				t.Errorf("the bridgemdns.Config at %s takes its InterfaceSource from something other "+
					"than a lanInterfaceSource(...) call, which would print every failed pick, once a minute",
					fset.Position(kv.Value.Pos()))
			}
			return true
		}
		t.Errorf("the bridgemdns.Config at %s sets no InterfaceSource", fset.Position(lit.Pos()))
		return true
	})
	// Floor: a sweep that finds no literal passes whatever the file says.
	if literals == 0 {
		t.Fatal("found no bridgemdns.Config literal in mdns_lifecycle.go")
	}
}

// isBridgeMDNSConfig reports whether a composite literal's type is
// `bridgemdns.Config`.
func isBridgeMDNSConfig(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Config" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "bridgemdns"
}

// callsIdent reports whether x is a call of the function named name.
func callsIdent(x ast.Expr, name string) bool {
	call, ok := x.(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	return ok && fn.Name == name
}
