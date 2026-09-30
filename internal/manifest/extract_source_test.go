package manifest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// libraryFileReaders are the functions in this package that may open or read
// a file through fsutil (OpenAsFile, ReadAsFile), each with what it reads.
// Every one but openAudioSource reads something that is not the audio file an
// extractor reads.
var libraryFileReaders = map[string]string{
	"openAudioSource":       "the audio file every extractor reads (openAudioFile)",
	"openSACDContainer":     "an .iso container, read under sacdReadOutcome's rule",
	"ExpandSACDISO":         "an .iso container, outside a scan",
	"readFolderArt":         "a folder's cover",
	"readSidecarCandidate":  "a lyrics sidecar",
	"rescaleOneArtworkFile": "a cover in the artwork cache",
	"jpegHeaderDimensions":  "a cover in the artwork cache",
	"EnsureThumb":           "a cover in the artwork cache",
}

// TestEveryAudioFileReadGoesThroughOpenAudioFile: a file the scan could not
// read whole keeps its row (keepUnread) only because every extractor opens
// its audio file through openAudioFile, whose faultNotingSource notes a read
// that did not complete, whatever the parser then does with it. An extractor
// that opened the file with fsutil.OpenAsFile would read around it, and a read
// failing there would be written as the path's guess again (backlog B134). So
// a function of this package that opens or reads a file through fsutil is one
// of libraryFileReaders, which read other files.
func TestEveryAudioFileReadGoesThroughOpenAudioFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files int
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		files++
		for fn, call := range fsutilReadersIn(t, name) {
			if _, ok := libraryFileReaders[fn]; !ok {
				t.Errorf("%s: %s calls fsutil.%s: an extractor opens its audio file with openAudioFile, "+
					"or a read that does not complete is written as the path's guess (add a function that reads "+
					"another file to libraryFileReaders)", name, fn, call)
			}
			seen[fn] = true
		}
	}
	// A sweep that parsed nothing, or found none of the readers it names,
	// passes over nothing: 53 production files and every reader named when
	// this was written.
	if files < 40 {
		t.Fatalf("parsed %d production files, want the package's", files)
	}
	var missing []string
	for fn := range libraryFileReaders {
		if !seen[fn] {
			missing = append(missing, fn)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("libraryFileReaders names functions that read nothing through fsutil any more: %v", missing)
	}
}

// fsutilReadersIn returns, for the Go file name, each function that calls
// fsutil.OpenAsFile or fsutil.ReadAsFile, with the call, by the name the file
// imports fsutil under.
func fsutilReadersIn(t *testing.T, name string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	local := importedAs(f, "github.com/acoseac/1-bit-bridge/internal/fsutil")
	if local == "" {
		return out
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call := fsutilReadCall(n, local); call != "" {
				out[fd.Name.Name] = call
			}
			return true
		})
	}
	return out
}

// importedAs is the name the file f imports path under, "" when it does not.
func importedAs(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return filepath.Base(path)
	}
	return ""
}

// fsutilReadCall names the fsutil read n calls (OpenAsFile, ReadAsFile), by
// the name local its file imports fsutil under, and "" for any other node.
func fsutilReadCall(n ast.Node, local string) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != local {
		return ""
	}
	if sel.Sel.Name == "OpenAsFile" || sel.Sel.Name == "ReadAsFile" {
		return sel.Sel.Name
	}
	return ""
}

// TestAudioFileReadSweepSeesAReadAroundOpenAudioFile runs the sweep's parse
// over a source that opens an audio file with fsutil.OpenAsFile in an
// extractor, as a fixture, so the sweep's own negative control stays in the
// tree: a reader the allowlist does not name is found, under the name the
// file imports fsutil by.
func TestAudioFileReadSweepSeesAReadAroundOpenAudioFile(t *testing.T) {
	dir := t.TempDir()
	src := `package manifest

import fu "github.com/acoseac/1-bit-bridge/internal/fsutil"

func extractNewFormat(absPath string) error {
	f, _, err := fu.OpenAsFile(absPath)
	if err != nil {
		return err
	}
	return f.Close()
}
`
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fsutilReadersIn(t, p)
	if got["extractNewFormat"] != "OpenAsFile" || len(got) != 1 {
		t.Errorf("found %v, want extractNewFormat's OpenAsFile", got)
	}
}
