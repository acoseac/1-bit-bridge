package engine

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// alsaTrees are the import paths of the ALSA packages: the box's reader and
// the player's PCM layer.
var alsaTrees = []string{
	"github.com/acoseac/1-bit-bridge/internal/alsa",
	"github.com/acoseac/1-bit-bridge/internal/alsapcm",
}

// TestTheEngineImportsNoALSAPackage pins that the engine imports neither ALSA
// package (B470 PR 5b): it knows a busy device by its own sentinel,
// ErrDeviceBusy, which the backend maps from alsapcm's busy reason, so it
// stays hardware-free and its darwin and windows builds stay as they are.
// Every source file is read, whatever its build constraint; the package's
// tests are not, since a test may quote a device layer's error.
func TestTheEngineImportsNoALSAPackage(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		read++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
			}
			for _, tree := range alsaTrees {
				if p == tree || strings.HasPrefix(p, tree+"/") {
					t.Errorf("%s imports %s: the engine imports no ALSA package, and knows a busy device by ErrDeviceBusy alone", name, p)
				}
			}
		}
	}
	if read < 5 {
		t.Fatalf("read %d source files: the glob found too few", read)
	}
}
