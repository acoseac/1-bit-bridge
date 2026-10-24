package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

func TestEveryPlayerUUIDMinterUsesFloorChecks(t *testing.T) {
	root := repoRootForCitations(t)
	calls := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return moduleDirRule(root, path, d.Name())
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, strings.ReplaceAll(string(b), "\r\n", "\n"), 0)
		if err != nil {
			return err
		}
		aliases := map[string]bool{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "github.com/acoseac/1-bit-bridge/internal/config" {
				name := "config"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == "." {
					t.Errorf("%s dot-imports config, hiding UUID minter references", path)
				}
				aliases[name] = true
			}
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				// Config's own callers need no selector. Permit the primitive's
				// declaration alone; other references would bypass the wrapper.
				if id, ok := n.(*ast.Ident); ok && f.Name.Name == "config" && id.Name == "EnsurePlayerUUID" && (fn == nil || fn.Name != id) {
					t.Errorf("%s calls the UUID minter inside config", path)
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "EnsurePlayerUUID" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || !aliases[id.Name] {
					return true
				}
				calls++
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s bypasses checked UUID wrapper", rel)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("found %d raw UUID minter references, want no calls to the unchecked primitive", calls)
	}
}

func TestDACBindRefusesUnsafePlayerIdentity(t *testing.T) {
	for _, kind := range []string{"orphan_queue", "orphan_queue_with_floor", "uuid_without_floor", "marker_without_floor", "bad_floor"} {
		t.Run(kind, func(t *testing.T) {
			root := dacTree(t)
			cfgPath := dacConfig(t, "  player:\n    enabled: true\n")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			put := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(cfg.DataDir, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "orphan_queue", "orphan_queue_with_floor":
				if kind == "orphan_queue_with_floor" {
					put("player-ids.json", `{"version":2,"uuid":"`+id+`","ids":8192,"tokens":8192}`)
				}
				put("player-queue.json", `{"version":1}`)
			case "uuid_without_floor":
				put("player-uuid", id)
			case "marker_without_floor":
				put("player-initialized", `{"version":1}`)
			case "bad_floor":
				put("player-ids.json", "{")
			}
			configBefore, _ := os.ReadFile(cfgPath)
			before, readErr := os.ReadFile(filepath.Join(cfg.DataDir, config.PlayerUUIDFile))
			out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
			if code == 0 || !strings.Contains(errOut, "bridge reset-player") {
				t.Errorf("unsafe bind: exit %d stdout %q stderr %q", code, out, errOut)
			}
			configAfter, _ := os.ReadFile(cfgPath)
			if string(configBefore) != string(configAfter) {
				t.Error("refused bind saved config")
			}
			after, err := os.ReadFile(filepath.Join(cfg.DataDir, config.PlayerUUIDFile))
			if string(before) != string(after) || os.IsNotExist(readErr) != os.IsNotExist(err) {
				t.Error("bind minted or changed unsafe identity")
			}
		})
	}
}

func TestDACBindKeepsOrRestampsTheBoxFloor(t *testing.T) {
	for _, kind := range []string{"pristine", "existing", "reset"} {
		t.Run(kind, func(t *testing.T) {
			root := dacTree(t)
			cfgPath := dacConfig(t, "  player:\n    enabled: true\n")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			const old = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			if kind != "pristine" {
				if err := os.WriteFile(filepath.Join(cfg.DataDir, "player-ids.json"), []byte(`{"version":2,"uuid":"`+old+`","ids":2000000000,"tokens":2000000001}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "existing" {
				if err := os.WriteFile(filepath.Join(cfg.DataDir, "player-uuid"), []byte(old), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, errs, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
			if code != 0 {
				t.Fatalf("bind: %d %s %s", code, out, errs)
			}
			id, err := config.ReadPlayerUUID(cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "existing" && id != old || kind == "reset" && id == old {
				t.Fatalf("identity: %s", id)
			}
			r, err := engine.ReadReservation(filepath.Join(cfg.DataDir, "player-ids.json"))
			if err != nil {
				t.Fatal(err)
			}
			if r.UUID != id || kind != "pristine" && (r.IDs != 2000000000 || r.Tokens != 2000000001) {
				t.Fatalf("floor changed: %+v", r)
			}
			if _, err := os.Stat(filepath.Join(cfg.DataDir, "player-initialized")); err != nil {
				t.Fatalf("no marker: %v", err)
			}
		})
	}
}
