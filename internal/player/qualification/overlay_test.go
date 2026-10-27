package qualification

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestQualificationOverlayFailsClosed(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("python3 unavailable")
	}
	script, e := os.ReadFile("../../../scripts/native-integration-overlay.py")
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"valid", "tag_changed", "empty_support", "extra_tag"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "cmd/bridge", "internal/alsapcm"} {
				if e := os.MkdirAll(filepath.Join(root, dir), 0755); e != nil {
					t.Fatal(e)
				}
			}
			put := func(path, text string) {
				t.Helper()
				if e := os.WriteFile(filepath.Join(root, path), []byte(text), 0600); e != nil {
					t.Fatal(e)
				}
			}
			put("scripts/native-integration-overlay.py", string(script))
			if name != "empty_support" {
				put("internal/alsapcm/fake_test.go", "package alsapcm\n")
			}
			tag := "//go:build nativeintegration\n"
			if name == "tag_changed" {
				tag = "//go:build (nativeintegration)\n"
			}
			if name == "extra_tag" {
				tag += "// +build nativeintegration\n"
			}
			put("cmd/bridge/output_dsdtest_integration_test.go", tag+"\npackage main\n")
			out, e := exec.Command(python, filepath.Join(root, "scripts/native-integration-overlay.py"), filepath.Join(root, "overlay.json")).CombinedOutput()
			if (e == nil) != (name == "valid") {
				t.Fatalf("overlay generation err=%v output=%s", e, out)
			}
		})
	}
}
