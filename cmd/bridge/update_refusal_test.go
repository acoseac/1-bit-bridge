package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// fakeLatestRelease is a GitHub Releases API whose latest release is tag.
func fakeLatestRelease(t *testing.T, tag string) *updater.Updater {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/fake/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.invalid/release"}`, tag)
	}))
	t.Cleanup(srv.Close)
	upd := updater.New(updater.Options{
		RepoOverride: "fake/repo",
		Client:       updater.NewClient("fake/repo", 5*time.Second).WithBaseURL(srv.URL),
	})
	if st, err := upd.CheckNow(t.Context()); err != nil || !st.UpdateAvailable {
		t.Fatalf("the fake release %s is not an update: %+v (%v)", tag, st, err)
	}
	return upd
}

// TestTheConsoleShowsAVersionHeldBackForRefusingTheSettings: a version the
// updater held back for refusing this bridge's settings is shown on the
// console's Updates panel, in the row an auto-install deferral uses, whether
// or not the auto-installer has run in this process, and so after a restart.
func TestTheConsoleShowsAVersionHeldBackForRefusingTheSettings(t *testing.T) {
	upd := fakeLatestRelease(t, "v9.9.9")
	dataDir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reason := "9.9.9 refuses this bridge's settings: line 1: a whole number is written with a leading zero"
	if err := updater.SaveState(dataDir, updater.State{RejectedVersion: "9.9.9", RejectedReason: reason}); err != nil {
		t.Fatal(err)
	}
	a := updateInfoAdapter{u: upd, dataDir: dataDir, configPath: cfg}
	st := a.Status()
	if !strings.Contains(st.DeferredReason, "line 1:") || !strings.Contains(st.DeferredReason, "9.9.9") {
		t.Errorf("DeferredReason = %q, want the recorded refusal", st.DeferredReason)
	}
	if st.RejectedVersion != "9.9.9" || st.RejectedReason != reason {
		t.Errorf("RejectedVersion, RejectedReason = %q, %q; want the record's", st.RejectedVersion, st.RejectedReason)
	}

	// An operator's rollback keeps the auto-installer's own wording, which
	// the panel shows once that has run, as before.
	if err := updater.SaveState(dataDir, updater.State{RejectedVersion: "9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Status().DeferredReason; got != "" {
		t.Errorf("a rollback's DeferredReason = %q before any auto-install cycle, want none, as before", got)
	}
}

// TestTheUpdaterReadsWhatThisBridgesSettingsCheckSays crosses the exec
// boundary the way an install does: internal/updater's check runs a copy of
// this test binary as the bridge, whose serve --validate-config is this
// tree's, and reads its answer. The codes and lines between the two are the
// contract a later release answers an earlier updater with.
func TestTheUpdaterReadsWhatThisBridgesSettingsCheckSays(t *testing.T) {
	name := "bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := installBridgeCopy(t, filepath.Join(t.TempDir(), name), "0.0.0-candidate")

	valid, _ := checkedSettings(t, "")
	if got := updater.CheckConfig(t.Context(), candidate, valid); got.Verdict != updater.ConfigValid {
		t.Errorf("settings this bridge loads: %+v, want valid", got)
	}
	refused, _ := checkedSettings(t, "scanIntervalSec: 010\n")
	got := updater.CheckConfig(t.Context(), candidate, refused)
	if got.Verdict != updater.ConfigRefused || len(got.Reasons) != 1 ||
		!strings.HasPrefix(got.Reasons[0], "line 4: a whole number is written with a leading zero") {
		t.Errorf("settings with a leading zero: %+v, want refused, naming line 4", got)
	}
	if got := updater.CheckConfig(t.Context(), candidate, filepath.Join(t.TempDir(), "bridge.yaml")); got.Verdict != updater.ConfigCheckFailed {
		t.Errorf("a missing settings file: %+v, want a check that did not answer, never a refusal", got)
	}
}

// installRefusal is an error of the shape Install returns for a candidate
// that refuses the settings: its sentence, matching updater.ErrConfigRefused.
type installRefusal string

func (e installRefusal) Error() string { return string(e) }
func (e installRefusal) Unwrap() error { return updater.ErrConfigRefused }

// TestAnInstallTheSettingsRefuseIsTheConsolesOwnAnswer: the console tells it
// from a failed install by its code (admin.ErrUpdateConfigRefused, a 409),
// and shows the updater's sentence once, not after its own words again.
func TestAnInstallTheSettingsRefuseIsTheConsolesOwnAnswer(t *testing.T) {
	sentence := "9.9.9 refuses this bridge's settings, so it was not installed: line 409: a whole number is written with a leading zero"
	mapped := mapUpdaterError(installRefusal(sentence))
	if !errors.Is(mapped, admin.ErrUpdateConfigRefused) || mapped.Error() != sentence {
		t.Errorf("mapped = %q (is %v), want %q as admin.ErrUpdateConfigRefused", mapped, errors.Is(mapped, admin.ErrUpdateConfigRefused), sentence)
	}
	failed := fmt.Errorf("%w: 9.9.9: it exited with status 3", updater.ErrConfigCheckFailed)
	mapped = mapUpdaterError(failed)
	if !errors.Is(mapped, admin.ErrUpdateConfigCheckFailed) || strings.Count(mapped.Error(), updater.ErrConfigCheckFailed.Error()) != 1 {
		t.Errorf("mapped = %q, want admin.ErrUpdateConfigCheckFailed saying it once", mapped)
	}
}

func TestBridgeUpdateSaysWhatTheNewVersionRefusedAndWhatToFix(t *testing.T) {
	var out bytes.Buffer
	sentence := "9.9.9 refuses this bridge's settings, so it was not installed: line 4: a whole number is written with a leading zero"
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	reportInstallFailure(&out, installRefusal(sentence), cfg)
	if got := out.String(); !strings.Contains(got, "Install failed: "+sentence) || !strings.Contains(got, "Fix "+cfg) {
		t.Errorf("bridge update said:\n%s\nwant the refusal by line and the file to fix", got)
	}
	out.Reset()
	reportInstallFailure(&out, errors.New("download: boom"), cfg)
	if got := out.String(); got != "Install failed: download: boom\n" {
		t.Errorf("another failure: %q", got)
	}
}

// TestEveryInstallTheBridgeStartsNamesItsSettings: each updater.InstallOptions
// the CLI and serve build for an install names the settings file
// (ConfigPath), which an empty value would leave unchecked; the one built
// for a rollback installs nothing and needs none.
func TestEveryInstallTheBridgeStartsNamesItsSettings(t *testing.T) {
	fset := token.NewFileSet()
	installs := 0
	for _, name := range nonTestGoFilesInPackage(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "InstallOptions" || !isIdent(sel.X, "updater") {
				return true
			}
			if len(stack) >= 2 {
				if call, ok := stack[len(stack)-2].(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.SelectorExpr); ok && fn.Sel.Name == "Rollback" {
						return true
					}
				}
			}
			installs++
			named := false
			forEachKeyedElement(lit, func(key *ast.Ident, _ ast.Expr) {
				if key.Name == "ConfigPath" {
					named = true
				}
			})
			if !named {
				t.Errorf("the updater.InstallOptions at %s names no ConfigPath: its install skips the settings check", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if installs < 3 {
		t.Fatalf("found %d installs, want at least the console's, the auto-installer's and the CLI's", installs)
	}
}
