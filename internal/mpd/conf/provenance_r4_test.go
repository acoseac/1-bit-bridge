package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNestedIncludeLocationsUnderLinkedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "physical")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"main.conf": "include \"child.conf\"\n", "child.conf": "include \"grandchild.conf\"\n", "grandchild.conf": "audio_output {\n name \"alsa\"\n type \"alsa\"\n}\ninclude \"missing.conf\"\n"} {
		if err := os.WriteFile(filepath.Join(root, "etc", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink("physical", alias); err != nil {
		t.Skip(err)
	}
	f, err := ReadHost(alias, filepath.Join(alias, "etc/main.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Problems) != 1 || !strings.HasPrefix(f.Problems[0], "/etc/grandchild.conf:") {
		t.Fatalf("nested problem location=%v", f.Problems)
	}
}
