package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
)

// TestARestartRequestedFromTheConsoleExitsToBeRestarted — POST /api/restart
// (the console's Restart, and "Install & restart" after its install) stops
// serve through the cancel SIGINT and SIGTERM reach, and serve exits with
// supervision.RestartExitCode, which a supervisor relaunches. On main it
// exited 0, which the LaunchAgent `bridge init` writes (KeepAlive
// {SuccessfulExit: false}) and a Windows service do not relaunch, so the
// bridge stayed down (backlog B201).
func TestARestartRequestedFromTheConsoleExitsToBeRestarted(t *testing.T) {
	b := startConsoleBridge(t, "", nil)
	resp, err := b.console.Post(b.adminBase+"/api/restart", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(body), `"restarting":true`) {
		t.Fatalf("POST /api/restart: %d %s, want 202 restarting", resp.StatusCode, body)
	}
	select {
	case code := <-b.done:
		if code != supervision.RestartExitCode {
			t.Errorf("serve stopped by a restart request exited %d, want %d, which its supervisor starts again; stderr=%s",
				code, supervision.RestartExitCode, b.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return after a restart request; stderr=%s\nserve's goroutines:\n%s",
			b.stderr.String(), serveStacks())
	}
}

// TestAStopStillExitsZero — a stop (serve's context cancelled, as SIGINT and
// SIGTERM do, and as the Windows service handler does on the SCM's Stop)
// exits 0, which no supervisor starts again: only a restart request asks to
// be restarted.
func TestAStopStillExitsZero(t *testing.T) {
	b := startConsoleBridge(t, "", nil)
	b.stop()
	select {
	case code := <-b.done:
		if code != 0 {
			t.Errorf("serve stopped by its context exited %d, want 0; stderr=%s", code, b.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return after a stop; stderr=%s\nserve's goroutines:\n%s",
			b.stderr.String(), serveStacks())
	}
}

// TestServeRestartKeepsAFailureAndMarksOnlyAStop — the exit code a restart
// request leaves: a clean exit becomes supervision.RestartExitCode, a
// failure keeps its own code (a supervisor restarts it anyway, and it says
// more), and without a request nothing changes. The request cancels.
func TestServeRestartKeepsAFailureAndMarksOnlyAStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newServeRestart(cancel)
	for _, code := range []int{0, 1, 2} {
		if got := r.exitCode(code); got != code {
			t.Errorf("no request: exitCode(%d) = %d, want %d", code, got, code)
		}
	}
	r.request()
	if ctx.Err() == nil {
		t.Error("the request did not cancel serve")
	}
	r.request() // twice is harmless
	for code, want := range map[int]int{0: supervision.RestartExitCode, 1: 1, 2: 2} {
		if got := r.exitCode(code); got != want {
			t.Errorf("after a request: exitCode(%d) = %d, want %d", code, got, want)
		}
	}
}

// TestAWindowsServiceTellsARestartFromAFailure — the Windows service's serve
// function turns run's exit code into the error its handler answers the SCM
// with: nil for a clean exit, errRestartRequested for a restart (a
// service-specific exit code the recovery actions restart), and an error
// naming any other code.
func TestAWindowsServiceTellsARestartFromAFailure(t *testing.T) {
	if err := serviceServeResult(0); err != nil {
		t.Errorf("exit 0: %v, want nil", err)
	}
	if err := serviceServeResult(supervision.RestartExitCode); !errors.Is(err, errRestartRequested) {
		t.Errorf("exit %d: %v, want errRestartRequested", supervision.RestartExitCode, err)
	}
	err := serviceServeResult(1)
	if err == nil || errors.Is(err, errRestartRequested) || !strings.Contains(err.Error(), "code 1") {
		t.Errorf("exit 1: %v, want an error naming code 1", err)
	}
}

// TestEveryRestartInServeGoesThroughTheRestartRequest — runServe's two
// restart triggers, the console's (admin.Deps.Restart) and the
// auto-installer's (AutoInstallRestart), reach serve's cancel through
// restart.request, which marks the stop so serve exits to be restarted. A
// bare cancel there stops serve with exit 0, which launchd and the SCM do
// not relaunch (backlog B201). The console's is driven for real by
// TestARestartRequestedFromTheConsoleExitsToBeRestarted; the auto-installer's
// needs a release to install, so this reads the wiring.
func TestEveryRestartInServeGoesThroughTheRestartRequest(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	serve := topLevelFuncNamed(f, "runServe")
	if serve == nil {
		t.Fatal("main.go has no runServe")
	}
	var deps, autoInstall int
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Deps" || !isIdent(sel.X, "admin") {
				return true
			}
			forEachKeyedElement(x, func(key *ast.Ident, value ast.Expr) {
				if key.Name != "Restart" {
					return
				}
				deps++
				if !isSelector(value, "restart", "request") {
					t.Errorf("admin.Deps.Restart at %s is not restart.request", fset.Position(value.Pos()))
				}
			})
		case *ast.AssignStmt:
			if len(x.Lhs) != 1 || len(x.Rhs) != 1 || !isSelector(x.Lhs[0], "updOpts", "AutoInstallRestart") {
				return true
			}
			autoInstall++
			lit, ok := x.Rhs[0].(*ast.FuncLit)
			if !ok {
				t.Errorf("AutoInstallRestart at %s is not a function literal", fset.Position(x.Pos()))
				return true
			}
			var requests, cancels int
			ast.Inspect(lit.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					switch {
					case isSelector(call.Fun, "restart", "request"):
						requests++
					case isIdent(call.Fun, "cancel"):
						cancels++
					}
				}
				return true
			})
			if requests != 1 || cancels != 0 {
				t.Errorf("AutoInstallRestart at %s calls restart.request %d time(s) and cancel %d, want once and never",
					fset.Position(x.Pos()), requests, cancels)
			}
		}
		return true
	})
	if deps != 1 || autoInstall != 1 {
		t.Errorf("runServe wires admin.Deps.Restart %d time(s) and AutoInstallRestart %d, want each once", deps, autoInstall)
	}
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
