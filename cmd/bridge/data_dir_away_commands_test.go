package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// name is the command as its refusal names it.
	name string
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
		{name: "tsnet", extra: "tailscale:\n    mode: tsnet\n", run: func(t *testing.T, a awayDataDir) (int, string) {
			// The gate tsnet auth and status take before they build a tsnet
			// server, whose state dir is in the data dir: run whole, either
			// would bring a node up on the tailnet.
			var stderr bytes.Buffer
			if _, err := loadConfigAndRequireTsnetMode([]string{"--config", a.cfg}, &stderr); err != nil {
				return 1, stderr.String()
			}
			return 0, stderr.String()
		}},
		{name: "init", run: cli("init", "--dir", "BASE", "--yes", "--force", "--library", "LIB", "--no-service", "--skip-doctor")},
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
		t.Run(c.name, func(t *testing.T) {
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
// reads the data dir, and init's run that keeps the config, refuse nothing
// and make nothing there. bridge doctor's data-dir line fails the state in
// serve's words (TestBothDoctorsFailADataDirItsRecordNamesWhileItsVolumeIsAway).
func TestTheDiagnosticsStayAvailableWhileTheDataDirsVolumeIsAway(t *testing.T) {
	for _, c := range []struct {
		name string
		args func(a awayDataDir) []string
		code int
	}{
		{"token list", func(a awayDataDir) []string { return []string{"token", "list", "--config", a.cfg} }, 0},
		{"cert info", func(a awayDataDir) []string { return []string{"cert", "info", "--config", a.cfg} }, 1},
		{"status", func(a awayDataDir) []string { return []string{"status", "--config", a.cfg} }, 1},
		{"init keeping the config", func(a awayDataDir) []string {
			return []string{"init", "--dir", a.base, "--yes", "--library", a.lib, "--no-service", "--skip-doctor"}
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAwayDataDir(t, 1, "")
			cfgBefore, recordBefore := mustRead(t, a.cfg), mustRead(t, dataDirRecordFile(a.cfg))
			code, out := runCLI(t, c.args(a)...)
			if code != c.code {
				t.Errorf("exit %d, want %d:\n%s", code, c.code, out)
			}
			if strings.Contains(out, "held this bridge's database") {
				t.Errorf("a read refused the data dir:\n%s", out)
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
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "updateCmd" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.IfStmt:
				if b, ok := n.Cond.(*ast.BinaryExpr); ok && refuse == token.NoPos {
					if call, ok := b.Y.(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuseDataDirAway" {
							refuse = call.Pos()
							if u, ok := b.X.(*ast.UnaryExpr); ok && u.Op == token.NOT {
								if s, ok := u.X.(*ast.StarExpr); ok {
									if id, ok := s.X.(*ast.Ident); ok {
										refuseCond = "!*" + id.Name
									}
								}
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
