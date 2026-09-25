package packaging

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestEmbeddedUnitTemplatesMatchDisk pins tmplFS to the unit templates on
// disk: every *.tmpl beside this file is embedded, and nothing else is. The
// disk side skips a leading "." for the reason the pattern refuses one: an
// editor's lock (.#systemd.service.tmpl) or a .DS_Store is not a template.
func TestEmbeddedUnitTemplatesMatchDisk(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, e := range entries {
		if n := e.Name(); !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".tmpl") {
			onDisk = append(onDisk, n)
		}
	}

	embedded, err := fs.ReadDir(tmplFS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range embedded {
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		t.Fatal("tmplFS embeds nothing: the embed directive is broken")
	}
	if !slices.Equal(names, onDisk) {
		t.Errorf("tmplFS embeds %v, but the templates on disk are %v", names, onDisk)
	}
}
