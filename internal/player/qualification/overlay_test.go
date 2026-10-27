package qualification

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeOverlayRequiresPlaybackSuite(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	script, err := os.ReadFile("../../../scripts/native-integration-overlay.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{false, true} {
		name := "missing"
		if present {
			name = "present"
		}
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sources := map[string]string{
				"scripts/native-integration-overlay.py":         string(script),
				"internal/alsapcm/fake_test.go":                 "package alsapcm\n",
				"cmd/bridge/output_dsdtest_integration_test.go": "//go:build nativeintegration\n\npackage main\n",
			}
			const playback = "internal/player/playback/native_integration_test.go"
			if present {
				sources[playback] = "//go:build nativeintegration\n\npackage playback\n"
			}
			for name, text := range sources {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(root, "overlay.json")
			text, err := exec.Command(python, filepath.Join(root, "scripts/native-integration-overlay.py"), output).CombinedOutput()
			if !present {
				if err == nil || !strings.Contains(string(text), "playback integration test missing") {
					t.Fatalf("missing suite accepted: %v: %s", err, text)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("partial overlay published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("overlay: %v: %s", err, text)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var overlay struct{ Replace map[string]string }
			if err := json.Unmarshal(data, &overlay); err != nil {
				t.Fatal(err)
			}
			if overlay.Replace[filepath.Join(root, playback)] == "" || len(overlay.Replace) != 3 {
				t.Fatalf("incomplete overlay: %s", data)
			}
		})
	}
}

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
			for _, dir := range []string{"scripts", "cmd/bridge", "internal/alsapcm", "internal/player/playback"} {
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
			put("internal/player/playback/native_integration_test.go", "//go:build nativeintegration\n\npackage playback\n")
			out, e := exec.Command(python, filepath.Join(root, "scripts/native-integration-overlay.py"), filepath.Join(root, "overlay.json")).CombinedOutput()
			if (e == nil) != (name == "valid") {
				t.Fatalf("overlay generation err=%v output=%s", e, out)
			}
		})
	}
}
