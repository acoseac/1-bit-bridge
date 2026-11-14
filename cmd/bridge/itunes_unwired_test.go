package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// productionCalls walks every Go file of the module the go tool builds, but
// tests, skipping what the citation guard skips, and hands match each call:
// its function as written (a selector's package or receiver expression and
// name, or a bare name) and the file's slash path below the root.
func productionCalls(t *testing.T, match func(call *ast.CallExpr, rel string)) {
	t.Helper()
	productionCallsIn(t, filepath.WalkDir, repoRootForCitations(t), match)
}

// productionCallsIn is productionCalls over the tree at root, walked by walk,
// which a test gives a fixture of its own, and a sweeptest.Removal's WalkDir
// for filepath.WalkDir where it stands in for another process removing a
// directory mid-walk.
func productionCallsIn(t *testing.T, walk func(root string, fn fs.WalkDirFunc) error, root string, match func(call *ast.CallExpr, rel string)) {
	t.Helper()
	fset := token.NewFileSet()
	err := walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipsForCitations(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				match(call, filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheWireManifestIsTheProvidersAlone: enrich.enabled reaches the
// manifest's enrichmentProgress block through the Provider's two builders
// (Provider.SetEnrichmentEnabledSource), and the package-level BuildManifest,
// WriteManifest and BuildManifestPage, which carry the block whatever the
// switch says, have no production caller: a call of one outside a test would
// serve the block while enrichment is off (backlog B617). The walk must see
// the API's call through its ManifestProvider, which is the Provider's.
func TestTheWireManifestIsTheProvidersAlone(t *testing.T) {
	builders := map[string]bool{"BuildManifest": true, "WriteManifest": true, "BuildManifestPage": true}
	var calls []string
	sawProvider := false
	productionCalls(t, func(call *ast.CallExpr, rel string) {
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "manifest" && builders[fun.Sel.Name] {
				calls = append(calls, rel+": manifest."+fun.Sel.Name)
			}
			if recv, ok := fun.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "manifest" && fun.Sel.Name == "WriteManifest" {
				sawProvider = true
			}
		case *ast.Ident:
			if strings.HasPrefix(rel, "internal/manifest/") && builders[fun.Name] {
				calls = append(calls, rel+": "+fun.Name)
			}
		}
	})
	if !sawProvider {
		t.Fatal("the walk saw no call of the API's ManifestProvider: it reads nothing the wire serves")
	}
	if len(calls) != 0 {
		t.Errorf("a production path calls a builder that carries enrichmentProgress whatever enrich.enabled says:\n%s",
			strings.Join(calls, "\n"))
	}
}

// TestNoProductionPathAsksITunes: the enricher's iTunes fallback is wired by
// Enricher.WithITunes over a client from enrich.NewITunesClient, and no
// production path calls either, so the bridge as shipped asks iTunes
// nothing: PROTOCOL.md, the settings and the privacy text name the services
// enrichment asks, and iTunes is not one (backlog B617). A call of either
// outside a test is a new service those texts must name first. The walk
// reads every Go file of the module the go tool builds, skipping what the
// citation guard skips, and must see the one call that builds the enricher.
func TestNoProductionPathAsksITunes(t *testing.T) {
	var calls []string
	sawEnricher := false
	productionCalls(t, func(call *ast.CallExpr, rel string) {
		var fn string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			fn = fun.Sel.Name
		case *ast.Ident:
			fn = fun.Name
		}
		switch fn {
		case "WithITunes", "NewITunesClient":
			calls = append(calls, rel+": "+fn)
		case "NewEnricher":
			sawEnricher = true
		}
	})
	if !sawEnricher {
		t.Fatal("the walk saw no call of NewEnricher: it reads nothing serve builds")
	}
	if len(calls) != 0 {
		t.Errorf("a production path wires the iTunes fallback, which no text names as a service enrichment asks:\n%s",
			strings.Join(calls, "\n"))
	}
}
