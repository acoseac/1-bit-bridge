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
)

func TestEveryPlayerUUIDMinterUsesHistoryChecks(t *testing.T) {
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
				if filepath.ToSlash(rel) != "internal/backup/player_history.go" || fn == nil || fn.Name.Name != "EnsurePlayerUUID" {
					t.Errorf("%s bypasses checked UUID wrapper", rel)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("found %d raw UUID minter references, want the checked wrapper alone", calls)
	}
}

func TestDACBindRefusesUnsafePlayerIdentity(t *testing.T) {
	for _, kind := range []string{"orphan_reservation", "orphan_queue", "pending", "given_up_live", "given_up_reservation", "covered_without_floor"} {
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
			mark := `{"version":2,"uuid":"` + id + `","ids":90000,"tokens":80000}`
			switch kind {
			case "orphan_reservation":
				put("player-ids.json", mark)
			case "orphan_queue":
				put("player-queue.json", `{"version":1,"entries":[]}`)
			case "pending":
				put("player-history.json", `{"version":1,"resetPending":true}`)
			case "covered_without_floor":
				put("player-history.json", `{"version":1,"covered":["`+id+`"]}`)
			case "given_up_live":
				put("player-uuid", id)
				put("player-history.json", `{"version":1,"givenUp":["`+id+`"]}`)
			case "given_up_reservation":
				put("player-uuid", "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee")
				put("player-ids.json", mark)
				put("player-history.json", `{"version":1,"givenUp":["`+id+`"]}`)
			}
			before, readErr := os.ReadFile(filepath.Join(cfg.DataDir, config.PlayerUUIDFile))
			out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
			if code == 0 || !strings.Contains(errOut, "bridge reset-player") {
				t.Errorf("unsafe bind: exit %d stdout %q stderr %q", code, out, errOut)
			}
			after, err := os.ReadFile(filepath.Join(cfg.DataDir, config.PlayerUUIDFile))
			if string(before) != string(after) || os.IsNotExist(readErr) != os.IsNotExist(err) {
				t.Error("bind minted or changed unsafe identity")
			}
		})
	}
}
