package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
	"github.com/acoseac/1-bit-bridge/internal/tsnet"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// Backlog B727's second item: a command other than serve, run while the
// volume a data dir is on is not mounted. B651 gave serve a refusal of a data
// dir its record names that holds neither its database nor its lock file, and
// left the other commands as they were: manifest.OpenStore made the data dir
// and an empty bridge.db under the bare mount point (scan, the render
// commands, analyze, variants, artwork, enrichment, duplicates, library), a
// token or credential write made tokens.json or adminauth.json there (pair,
// token, admin), cert rotate a TLS pair, a command that takes the data dir's
// lock (restore, reset-player, manifest clear-missing, the DAC bind) its
// server.lock, and init's rewrite the data dir and a pair. A bridge.db or a
// server.lock there has the next serve take the bare mount point for the data
// dir it served from, and start on an empty database with a new TLS pair:
// B651's own defect, reached through another command. Each now refuses before
// it makes, opens or writes anything there, in serve's words, naming how to
// go on; a diagnostic that only reads stays available.

// awayDataDir is an install whose record names its data dir, and whose data
// dir is on a volume mounted at mnt that is away: mnt is a bare directory and
// the data dir below it is not there.
type awayDataDir struct {
	base, mnt, dataDir, cfg string
	// lib is the library root the config names, lib2 a second directory,
	// named as a root where the install has two.
	lib, lib2 string
}

// newAwayDataDir writes the install with roots library roots (1 or 2) and
// extra YAML after the rest, and its record, as a serve that opened its
// database in the data dir writes it.
func newAwayDataDir(t *testing.T, roots int, extra string) awayDataDir {
	t.Helper()
	base := t.TempDir()
	a := awayDataDir{base: base, mnt: filepath.Join(base, "usb"), lib: filepath.Join(base, "Music"), lib2: filepath.Join(base, "More")}
	for _, d := range []string{a.mnt, a.lib, a.lib2} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a.dataDir = filepath.Join(a.mnt, "bridge")
	a.cfg = filepath.Join(base, "bridge.yaml")
	rootsYAML := "  - " + a.lib + "\n"
	if roots == 2 {
		rootsYAML += "  - " + a.lib2 + "\n"
	}
	body := fmt.Sprintf("libraryRoots:\n%sdataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: %s\ndisableHttp3: true\n"+
		"mdns:\n    enabled: false\nupscale:\n    enabled: true\nanalysis:\n    enabled: true\n%s",
		rootsYAML, a.dataDir, closedAdminAddr(t), extra)
	if err := os.WriteFile(a.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDataDirRecord(t, a.cfg, a.dataDir)
	return a
}

// runCLI runs `bridge args...` through run's dispatch and returns its exit
// code and everything it printed.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// awayCommand is one command over an away install, its confirmation skipped
// and any input given.
type awayCommand struct {
	// name is the command as its refusal names it, and label, where it is
	// not "", tells a run of it from another run of it in the test's name.
	name, label string
	// roots and extra shape the install (newAwayDataDir).
	roots int
	extra string
	run   func(t *testing.T, a awayDataDir) (int, string)
}

// awayCommands are every command other than serve that makes, opens or writes
// the data dir. `update` is not run here (its install asks GitHub for a
// release first): TestUpdateAsksTheRecordBeforeItAsksForARelease pins it.
func awayCommands(snap, dacRoot string) []awayCommand {
	cli := func(args ...string) func(t *testing.T, a awayDataDir) (int, string) {
		return func(t *testing.T, a awayDataDir) (int, string) {
			full := make([]string, 0, len(args)+2)
			for _, arg := range args {
				switch arg {
				case "CFG":
					full = append(full, "--config", a.cfg)
				case "BASE":
					full = append(full, a.base)
				case "LIB":
					full = append(full, a.lib)
				case "LIB2":
					full = append(full, a.lib2)
				case "ELSEWHERE":
					full = append(full, filepath.Join(a.base, "elsewhere"))
				default:
					full = append(full, arg)
				}
			}
			return runCLI(t, full...)
		}
	}
	return []awayCommand{
		{name: "scan", run: cli("scan", "CFG")},
		{name: "pair", run: cli("pair", "CFG", "--name", "phone")},
		{name: "upscale", run: cli("upscale", "CFG", "--gc")},
		{name: "optimize", run: cli("optimize", "CFG", "--gc")},
		{name: "render", run: cli("render", "CFG", "--gc")},
		{name: "analyze", run: cli("analyze", "CFG", "--gc")},
		{name: "variants move", run: cli("variants", "move", "CFG", "--to", "ELSEWHERE", "--confirm", "MOVE")},
		{name: "artwork", run: cli("artwork", "CFG", "--gc", "--dry-run")},
		{name: "enrichment misses", run: cli("enrichment", "misses", "CFG")},
		{name: "enrichment retry", run: cli("enrichment", "retry", "CFG")},
		{name: "duplicates", run: cli("duplicates", "CFG")},
		{name: cmdNameManifestClearMissing, run: cli("manifest", "clear-missing", "CFG", "--yes")},
		{name: "library add", roots: 1, run: cli("library", "add", "CFG", "LIB2")},
		{name: "library remove", roots: 2, run: cli("library", "remove", "CFG", "LIB2")},
		{name: "token rotate", run: cli("token", "rotate", "CFG", "token-id")},
		{name: "token expire", run: cli("token", "expire", "CFG", "--in", "1h", "token-id")},
		{name: "token revoke", run: cli("token", "revoke", "CFG", "token-id")},
		{name: "admin reset-password", run: func(t *testing.T, a awayDataDir) (int, string) {
			var stdout, stderr bytes.Buffer
			code := adminResetPasswordCmd([]string{"--config", a.cfg, "--from-stdin"}, strings.NewReader("a new password\n"), &stdout, &stderr)
			return code, stdout.String() + stderr.String()
		}},
		{name: "admin sign-out-everywhere", run: cli("admin", "sign-out-everywhere", "CFG")},
		{name: "admin login-link", run: cli("admin", "login-link", "CFG")},
		{name: "cert rotate", run: cli("cert", "rotate", "CFG", "--yes")},
		{name: "backup", run: cli("backup", "CFG")},
		{name: "restore", run: func(t *testing.T, a awayDataDir) (int, string) {
			code, out, errOut := runRestoreYes(t, a.cfg, snap)
			return code, out + errOut
		}},
		{name: "reset-player", run: cli("reset-player", "CFG", "--yes")},
		{name: "output dac bind", run: func(t *testing.T, a awayDataDir) (int, string) {
			out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", a.cfg}, treeFlags(dacRoot)...)...)
			return code, out + errOut
		}},
		{name: "tsnet logout", extra: "tailscale:\n    mode: tsnet\n", run: cli("tsnet", "logout", "CFG", "--force")},
		{name: "tsnet auth", extra: "tailscale:\n    mode: tsnet\n", run: tsnetWithoutItsServer("auth")},
		{name: "tsnet status", extra: "tailscale:\n    mode: tsnet\n", run: tsnetWithoutItsServer("status")},
		{name: "init", label: "rewriting the config",
			run: cli("init", "--dir", "BASE", "--yes", "--force", "--library", "LIB", "--no-service", "--skip-doctor")},
		// A run that keeps the config prepares the data dir its service needs
		// (backlog B664), so it asks first, as a rewrite does.
		{name: "init", label: "keeping the config",
			run: cli("init", "--dir", "BASE", "--yes", "--library", "LIB", "--no-service", "--skip-doctor")},
	}
}

// tsnetWithoutItsServer runs `bridge tsnet sub --config <cfg>` through run's
// dispatch, its exit code the command's own, with the tsnet server the
// command builds past its gate stood in for (tsnetCommandServer): a gate
// that let it through brings no node up on the tailnet and makes no state
// dir in the data dir, and fails the test.
func tsnetWithoutItsServer(sub string) func(t *testing.T, a awayDataDir) (int, string) {
	return func(t *testing.T, a awayDataDir) (int, string) {
		built := false
		prev := tsnetCommandServer
		tsnetCommandServer = func(*config.Config, io.Writer) (*tsnet.Server, error) {
			built = true
			return nil, errors.New("the test's stand-in for the tsnet server")
		}
		t.Cleanup(func() { tsnetCommandServer = prev })
		code, out := runCLI(t, "tsnet", sub, "--config", a.cfg)
		if built {
			t.Errorf("bridge tsnet %s went past its gate to build its tsnet server over a data dir whose volume is away", sub)
		}
		return code, out
	}
}

// requireNothingMadeThere fails the test unless the bare mount point holds
// nothing, and the config and its record are as the install wrote them.
func requireNothingMadeThere(t *testing.T, a awayDataDir, cfgBefore, recordBefore string) {
	t.Helper()
	entries, err := os.ReadDir(a.mnt)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		_ = filepath.WalkDir(a.mnt, func(p string, _ os.DirEntry, _ error) error {
			if rel, err := filepath.Rel(a.mnt, p); err == nil && rel != "." {
				names = append(names, filepath.ToSlash(rel))
			}
			return nil
		})
		t.Errorf("made %v under the mount point of the data dir's volume while it was away", names)
	}
	if got := mustRead(t, a.cfg); got != cfgBefore {
		t.Errorf("changed bridge.yaml:\n%s", got)
	}
	if got := mustRead(t, dataDirRecordFile(a.cfg)); got != recordBefore {
		t.Errorf("changed the record beside the config: %q", got)
	}
}

// TestEveryCommandThatWritesTheDataDirRefusesItWhileItsVolumeIsAway runs
// each of awayCommands over an install whose data dir's volume is away: each
// exits 1 with serve's refusal under its own name, saying to mount the volume
// and how to go on from a data dir moved or removed on purpose, and makes
// nothing under the bare mount point and changes neither the config nor its
// record. On main each went on: most made the data dir there, a bridge.db, a
// tokens.json, an adminauth.json, a TLS pair or a server.lock in it.
func TestEveryCommandThatWritesTheDataDirRefusesItWhileItsVolumeIsAway(t *testing.T) {
	_, _, snap := seedRestorableInstall(t, closedAdminAddr(t))
	dacRoot := dacTree(t)
	for _, c := range awayCommands(snap, dacRoot) {
		name := c.name
		if c.label != "" {
			name += " " + c.label
		}
		t.Run(name, func(t *testing.T) {
			roots := c.roots
			if roots == 0 {
				roots = 1
			}
			a := newAwayDataDir(t, roots, c.extra)
			cfgBefore, recordBefore := mustRead(t, a.cfg), mustRead(t, dataDirRecordFile(a.cfg))
			code, out := c.run(t, a)
			lead := c.name + ": the data directory " + a.dataDir + " held this bridge's database"
			switch {
			case code != 1:
				t.Errorf("%s over a data dir whose volume is away exited %d, want 1:\n%s", c.name, code, out)
			case !strings.Contains(out, lead):
				t.Errorf("%s did not refuse in serve's words, want %q:\n%s", c.name, lead, out)
			default:
				if want := "bridge " + c.name + " makes and writes nothing there: mount the volume and run it again"; !strings.Contains(out, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, out)
				}
				mustRefuseAsConfigured(t, out, a.dataDir, dataDirRecordFile(a.cfg))
			}
			requireNothingMadeThere(t, a, cfgBefore, recordBefore)
		})
	}
}

// TestTheDiagnosticsStayAvailableWhileTheDataDirsVolumeIsAway: what only
// reads the data dir refuses nothing and makes nothing there: token list,
// cert info, status, logs (a log of the test's own) and update --check (its
// release check asked of a server of the test's own). bridge doctor, --fix or
// not, fails its data-dir line in serve's words and makes nothing there
// either: every open of the store it makes asks first whether the database is
// there (TestBothDoctorsFailADataDirItsRecordNamesWhileItsVolumeIsAway reads
// the line itself). init's run that keeps the config was one of these until
// backlog B664: it prepares the data dir its service needs now, and is one of
// awayCommands.
func TestTheDiagnosticsStayAvailableWhileTheDataDirsVolumeIsAway(t *testing.T) {
	cli := func(args ...string) func(t *testing.T, a awayDataDir) (int, string) {
		return func(t *testing.T, a awayDataDir) (int, string) {
			full := make([]string, 0, len(args))
			for _, arg := range args {
				switch arg {
				case "CFG":
					full = append(full, a.cfg)
				case "BASE":
					full = append(full, a.base)
				case "LIB":
					full = append(full, a.lib)
				default:
					full = append(full, arg)
				}
			}
			return runCLI(t, full...)
		}
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T, a awayDataDir) (int, string)
		code int
		// says is what the run says where its job is to say the refusal
		// (bridge doctor's data-dir line); "" for a read that says nothing of
		// it.
		says string
	}{
		{name: "token list", run: cli("token", "list", "--config", "CFG"), code: 0},
		{name: "cert info", run: cli("cert", "info", "--config", "CFG"), code: 1},
		{name: "status", run: cli("status", "--config", "CFG"), code: 1},
		{name: "doctor", run: cli("doctor", "--config", "CFG"), code: 1,
			says: "bridge serve will not start: the data directory "},
		{name: "doctor --fix", run: cli("doctor", "--fix", "--config", "CFG"), code: 1,
			says: "bridge serve will not start: the data directory "},
		{name: "logs", code: 0, run: func(t *testing.T, a awayDataDir) (int, string) {
			// The log a service start wrote, where this test's home puts it.
			home := filepath.Join(a.base, "home")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_STATE_HOME", "")
			t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
			path, err := packaging.DefaultLogPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("a line the bridge logged\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			code, out := runCLI(t, "logs")
			if !strings.Contains(out, "a line the bridge logged") {
				t.Errorf("bridge logs printed no line of the log:\n%s", out)
			}
			return code, out
		}},
		{name: "update --check", code: 0, run: func(t *testing.T, a awayDataDir) (int, string) {
			var asked atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked.Add(1)
				if r.URL.Path != "/repos/"+updater.DefaultRepo+"/releases/latest" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"tag_name":"v99.0.0","name":"v99.0.0",`+
					`"html_url":"https://github.com/`+updater.DefaultRepo+`/releases/tag/v99.0.0","assets":[]}`)
			}))
			t.Cleanup(srv.Close)
			prev := updateReleases
			updateReleases = updater.NewClient(updater.DefaultRepo, 5*time.Second).WithBaseURL(srv.URL)
			t.Cleanup(func() { updateReleases = prev })
			code, out := runCLI(t, "update", "--check", "--config", a.cfg)
			if asked.Load() == 0 {
				t.Errorf("bridge update --check asked no release of the test's server:\n%s", out)
			}
			return code, out
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAwayDataDir(t, 1, "")
			cfgBefore, recordBefore := mustRead(t, a.cfg), mustRead(t, dataDirRecordFile(a.cfg))
			code, out := c.run(t, a)
			out = stripANSI(out)
			if code != c.code {
				t.Errorf("exit %d, want %d:\n%s", code, c.code, out)
			}
			switch {
			case c.says == "" && strings.Contains(out, "held this bridge's database"):
				t.Errorf("a read refused the data dir:\n%s", out)
			case c.says != "" && !strings.Contains(out, c.says):
				t.Errorf("the run does not say %q:\n%s", c.says, out)
			}
			requireNothingMadeThere(t, a, cfgBefore, recordBefore)
		})
	}
}

// TestACommandOverAnInstallWithNoRecordMakesItsDataDirAsBefore — the
// controls, which pass on main: no record (a fresh install, or one no serve
// has run on) asks nothing, so scan makes the data dir and its database; and
// a record naming another data dir (dataDir moved on purpose and set where it
// is now) refuses nothing either.
func TestACommandOverAnInstallWithNoRecordMakesItsDataDirAsBefore(t *testing.T) {
	for _, recorded := range []string{"", "elsewhere"} {
		t.Run(map[string]string{"": "no record", "elsewhere": "a record of another data dir"}[recorded], func(t *testing.T) {
			a := newAwayDataDir(t, 1, "")
			switch recorded {
			case "":
				if err := os.Remove(dataDirRecordFile(a.cfg)); err != nil {
					t.Fatal(err)
				}
			default:
				writeDataDirRecord(t, a.cfg, filepath.Join(a.base, "old-disk", "bridge"))
			}
			code, out := runCLI(t, "scan", "--config", a.cfg)
			if code != 0 || strings.Contains(out, "held this bridge's database") {
				t.Fatalf("scan exited %d over an install its record does not name the data dir of:\n%s", code, out)
			}
			if _, err := os.Stat(filepath.Join(a.dataDir, "bridge.db")); err != nil {
				t.Errorf("scan made no database: %v", err)
			}
		})
	}
}

// TestUpdateAsksTheRecordBeforeItAsksForARelease reads updateCmd: `bridge
// update` asks GitHub for the release first, so no test runs it over an away
// data dir (a regression would ask the real API, and could install over the
// test binary). Its refusal (refuseDataDirAway, the helper the other commands
// call) must come before the release check (CheckNow) and so before the
// install, and leave --check, a read, to go on.
func TestUpdateAsksTheRecordBeforeItAsksForARelease(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "update.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var refuse, check token.Pos
	var refuseCond string
	// refusalIn is the call of refuseDataDirAway in an if's condition.
	refusalIn := func(cond ast.Expr) *ast.CallExpr {
		var found *ast.CallExpr
		ast.Inspect(cond, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && found == nil {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuseDataDirAway" {
					found = call
				}
			}
			return found == nil
		})
		return found
	}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "updateCmd" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.IfStmt:
				call := refusalIn(n.Cond)
				if call == nil || refuse != token.NoPos {
					break
				}
				refuse = call.Pos()
				// The guard: `!*check && refuseDataDirAway(…)`.
				if b, ok := n.Cond.(*ast.BinaryExpr); ok && b.Op == token.LAND && b.Y == call {
					if u, ok := b.X.(*ast.UnaryExpr); ok && u.Op == token.NOT {
						if s, ok := u.X.(*ast.StarExpr); ok {
							if id, ok := s.X.(*ast.Ident); ok {
								refuseCond = "!*" + id.Name
							}
						}
					}
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckNow" && check == token.NoPos {
					check = n.Pos()
				}
			}
			return true
		})
		return false
	})
	switch {
	case refuse == token.NoPos:
		t.Fatal("updateCmd asks no record of its data dir (refuseDataDirAway)")
	case check == token.NoPos:
		t.Fatal("updateCmd asks for no release (CheckNow): this test reads the wrong function")
	case refuse > check:
		t.Errorf("updateCmd asks for a release (line %d) before it asks the record (line %d)",
			fset.Position(check).Line, fset.Position(refuse).Line)
	}
	if refuseCond != "!*check" {
		t.Errorf("updateCmd's refusal is guarded by %q, want !*check: --check only reads", refuseCond)
	}
}
